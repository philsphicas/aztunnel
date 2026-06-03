// Package mock wires the shared e2e scenario suite
// (github.com/philsphicas/aztunnel/e2e/scenarios) up against an in-process mock relay.
//
// MockBackend brings up a real aztunnel listener + sender (the same
// code paths exercised against Azure Relay) talking to a mock relay
// server in-process. Tests written against the scenarios.Backend
// interface run unmodified against this backend, and the e2e module's
// azureBackend, so any behavioural divergence between mock and Azure
// shows up as a failing scenario in one but not the other.
package mock

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http/httptest"
	"net/url"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/philsphicas/aztunnel/e2e/scenarios"
	"github.com/philsphicas/aztunnel/internal/listener"
	"github.com/philsphicas/aztunnel/internal/metrics"
	"github.com/philsphicas/aztunnel/internal/relay"
	"github.com/philsphicas/aztunnel/internal/sender"
	"github.com/philsphicas/aztunnel/mockrelay/server"
	dto "github.com/prometheus/client_model/go"
)

// Auth method names for the MockBackend auth axis. They mirror the
// `provider` metric label values (relay.ProviderSAS / ProviderEntra)
// so a cell's name and its token-fetch metric line up. They are
// exported because the e2e entry point (env_test.go) and the feature
// tests (features_test.go) live in the external mock_test package and
// pin auth methods by name.
const (
	AuthSAS   = "sas"
	AuthEntra = "entra"
)

// MockBackend implements scenarios.Backend by standing up a mock
// relay server + aztunnel listener(s) + aztunnel sender(s) all in the
// same process. It is the fast, deterministic side of the mock-vs-
// Azure conformance matrix and runs in the default
// `go test ./mockrelay/...` job.
//
// The zero value runs SAS-only with no axis layer (what directly-
// constructed callers want). Construct via NewMatrixBackend to fan
// over the {sas, entra} auth axis and/or a delay-profile axis,
// mirroring the Azure backend's matrix.
type MockBackend struct {
	// DelayProfile parameterizes the synthetic per-step sleeps the
	// mock relay applies on every leg of the rendezvous protocol.
	// The zero DelayProfile (i.e., the zero value of MockBackend)
	// means no synthetic delay: rendezvous completes in the
	// in-process baseline (~6 ms).
	//
	// For e2e suites whose timing thresholds were calibrated against
	// Azure Relay (e.g. e2e_test.go), set DelayProfile explicitly to
	// server.DelayProfileDefault so the mock approximates the
	// wireshark-observed wall-clock shape.
	DelayProfile server.DelayProfile

	// authName is the auth method this backend speaks: AuthSAS (the
	// zero value / default) or AuthEntra. It selects the token provider
	// built in newTokenProvider; on the entra path the one-off cold
	// token-acquisition cost is taken from DelayProfile.TokenAcquire.
	authName string

	// authAxis and delayAxis put this backend in factory mode for the
	// dimension they describe: when non-nil, Axes() advertises that
	// dimension and Cell() pins it per cell. They are set only by
	// NewMatrixBackend, and only for a dimension that varies (more than
	// one value). A single-valued dimension is pre-pinned into authName
	// / DelayProfile and carries a nil axis, so it adds no sub-test
	// layer. A directly-constructed MockBackend{...} has both nil and
	// therefore no axes, so the historical pinned usage (features_test,
	// entracred_test) is unchanged.
	authAxis  *namedAxis
	delayAxis *namedAxis
}

// namedAxis is a scenarios.Axis with a fixed name and value set. The
// mock backend uses one instance per matrix dimension it varies over
// (auth, delay). The values are the cell keys the harness passes back
// to Cell: auth-method names for the auth axis, registry profile names
// for the delay axis.
type namedAxis struct {
	name   string
	values []string
}

func (a *namedAxis) Name() string { return a.name }

// Values returns a defensive copy so callers (and the harness) cannot
// mutate the axis's backing slice.
func (a *namedAxis) Values() []string {
	out := make([]string, len(a.values))
	copy(out, a.values)
	return out
}

// NewMatrixBackend returns a factory MockBackend that fans the e2e
// scenario suite over the cartesian product of the named auth methods
// and delay profiles. Use it from a test entry point (TestE2E_Mock) so
// the mock runs the full suite once per (auth, delay) cell, mirroring
// the Azure backend's matrix.
//
// Each dimension is advertised as an axis only when it has more than
// one value; a single-valued dimension is pinned directly (into
// authName or DelayProfile) and adds no sub-test layer. So
// NewMatrixBackend([]string{AuthSAS}, []string{"default"}) is a fully
// pinned backend with no axes, while NewMatrixBackend([]string{AuthSAS,
// AuthEntra}, []string{"zero", "default"}) advertises both axes (auth
// outermost, mirroring Azure) for four cells.
//
// Panics on an empty list, an unknown auth method, or an unregistered
// profile — all caller bugs (the E2E_AUTH / E2E_DELAY entry point
// validates input first).
func NewMatrixBackend(authNames, delayNames []string) *MockBackend {
	if len(authNames) == 0 {
		panic("NewMatrixBackend: authNames must be non-empty")
	}
	if len(delayNames) == 0 {
		panic("NewMatrixBackend: delayNames must be non-empty")
	}
	for _, a := range authNames {
		if a != AuthSAS && a != AuthEntra {
			panic("NewMatrixBackend: unknown auth method " + a)
		}
	}
	for _, n := range delayNames {
		if _, err := server.ProfileByName(n); err != nil {
			panic("NewMatrixBackend: " + err.Error())
		}
	}

	b := &MockBackend{}
	if len(authNames) == 1 {
		b.authName = authNames[0]
	} else {
		b.authAxis = &namedAxis{name: "auth", values: append([]string(nil), authNames...)}
	}
	if len(delayNames) == 1 {
		// ProfileByName already validated this name above.
		b.DelayProfile, _ = server.ProfileByName(delayNames[0])
	} else {
		b.delayAxis = &namedAxis{name: "delay", values: append([]string(nil), delayNames...)}
	}
	return b
}

// Name returns the backend identifier (always "mock"). The harness
// fills sub-test paths from axis values rather than the name; scenarios
// and external callers may still surface it in debug output.
func (*MockBackend) Name() string { return "mock" }

// Axes returns the matrix dimensions this backend varies over, in the
// order [auth, delay] (auth outermost, mirroring the Azure backend). A
// dimension pinned to a single value is omitted, so a fully pinned
// backend returns nil and the harness runs scenarios directly under the
// entry point with no axis sub-path layer.
func (b *MockBackend) Axes() []scenarios.Axis {
	var axes []scenarios.Axis
	if b.authAxis != nil {
		axes = append(axes, b.authAxis)
	}
	if b.delayAxis != nil {
		axes = append(axes, b.delayAxis)
	}
	return axes
}

// Cell returns a fresh *MockBackend pinned to the cell described by
// values. It reads exactly the keys for the axes this backend
// advertises (see Axes): "auth" when the auth axis is live, "delay"
// when the delay axis is live. Pinned dimensions are carried through
// from the receiver. An axis-less backend accepts only an empty map and
// returns a clone. Panics on a missing key, an unknown value, or an
// unexpected number of values — all harness-contract violations.
func (b *MockBackend) Cell(values map[string]string) scenarios.Backend {
	cell := &MockBackend{
		DelayProfile: b.DelayProfile,
		authName:     b.authName,
	}
	want := 0
	if b.authAxis != nil {
		want++
		v, ok := values["auth"]
		if !ok {
			panic(`MockBackend.Cell: missing required axis key "auth"`)
		}
		if v != AuthSAS && v != AuthEntra {
			panic("MockBackend.Cell: unknown auth method " + v)
		}
		cell.authName = v
	}
	if b.delayAxis != nil {
		want++
		v, ok := values["delay"]
		if !ok {
			panic(`MockBackend.Cell: missing required axis key "delay"`)
		}
		p, err := server.ProfileByName(v)
		if err != nil {
			panic("MockBackend.Cell: " + err.Error())
		}
		cell.DelayProfile = p
	}
	if len(values) != want {
		panic(fmt.Sprintf("MockBackend.Cell: expected %d axis value(s), got %d", want, len(values)))
	}
	return cell
}

// mockLatencyFloor is the minimum per-connection latency ceiling. It
// keeps near-zero profiles stable against CI scheduling jitter; slower
// profiles scale above it via mockLatencyBudget.
const mockLatencyFloor = 3 * time.Second

// mockLatencyBudget derives the per-connection latency ceiling for a
// pinned profile and auth method. It is affine in the profile's
// predicted cost — one cold rendezvous (on the given auth path, so the
// Entra leg pays EntraValidate instead of AuthInternal) plus one bridge
// echo, the shape the ConnectLatency scenarios measure (dial -> 1-byte
// echo) — so a slow profile gets a proportionally larger budget that
// still trips on a real regression, rather than a flat constant that
// would mask seconds-scale slowdowns. The 3 s floor preserves the
// historical budget for the zero/default profiles (both predict well
// under it).
func mockLatencyBudget(p server.DelayProfile, entra bool) time.Duration {
	predicted := p.PredictedRendezvousFor(entra) + p.PredictedBridgeEcho()
	budget := predicted*3/2 + 2*time.Second
	if budget < mockLatencyFloor {
		budget = mockLatencyFloor
	}
	return budget
}

// mockWarmRequestFloor is the minimum warm-request budget. It preserves
// the historical flat per-request wall allowance (500 ms) for the
// zero/default profiles, whose PredictedBridgeEcho sits well under it.
const mockWarmRequestFloor = 500 * time.Millisecond

// mockWarmRequestBudget derives the warm-request term of the workload
// scenarios' per-round budget (roundBudget) for a pinned profile. A
// warm request is a single write→read on an already-bridged
// connection, so it is modelled on the profile's predicted bridge-echo
// round-trip with a 3/2 headroom factor, floored at mockWarmRequestFloor
// so near-zero profiles keep the historical 500 ms allowance. A slow
// DelayProfile (PredictedBridgeEcho above the floor) scales the budget
// up accordingly, keeping roundBudget's sanity ceiling honest instead
// of tripping on the profile's own modelled latency.
func mockWarmRequestBudget(p server.DelayProfile) time.Duration {
	budget := p.PredictedBridgeEcho() * 3 / 2
	if budget < mockWarmRequestFloor {
		budget = mockWarmRequestFloor
	}
	return budget
}

// ConnectLatencyThreshold returns the per-connection connect-latency
// ceiling for the Performance suite, derived from this backend's
// pinned DelayProfile so the budget scales with the profile (see
// mockLatencyBudget). A directly-constructed zero-profile backend
// yields the 3 s floor — the historical value.
func (b *MockBackend) ConnectLatencyThreshold() time.Duration {
	return mockLatencyBudget(b.DelayProfile, b.authName == AuthEntra)
}

// WarmRequestBudget returns the warm-request term of the workload
// scenarios' per-round budget, derived from this backend's pinned
// DelayProfile (see mockWarmRequestBudget). A zero/default profile
// yields the 500 ms floor — the historical flat value.
func (b *MockBackend) WarmRequestBudget() time.Duration {
	return mockWarmRequestBudget(b.DelayProfile)
}

// ConnectLatencyPolicy returns a strict, spike-free quantile gate for
// the mock backend. The in-process mock is deterministic — every dial
// pays the same modelled rendezvous delay with negligible jitter — so
// it must NOT inherit Azure's spike tolerance, which would mask a real
// regression in mock-exercised code. All three thresholds collapse to
// the single deterministic budget (mockLatencyBudget): the upper-median
// and soft-tail samples both sit at ~the modelled latency, comfortably
// under the budget, and any regression that lifts the bulk of samples
// past the budget trips the gate. Iterations is kept at 10 since extra
// samples buy nothing against a deterministic backend.
func (b *MockBackend) ConnectLatencyPolicy() scenarios.ConnectLatencyPolicy {
	budget := mockLatencyBudget(b.DelayProfile, b.authName == AuthEntra)
	return scenarios.ConnectLatencyPolicy{
		Iterations:   10,
		NormalP50:    budget,
		SoftTail:     budget,
		SpikeCeiling: budget,
	}
}

// ColdStartLatencyThreshold returns the per-backend ceiling for the
// first connection through a freshly-started sender. The warm budget is
// mockLatencyBudget (every dial pays the same rendezvous delay). The
// entra path additionally pays a one-off cold token acquisition on that
// first dial — the modelled AAD round trip in DelayProfile.TokenAcquire,
// absorbed thereafter by the client token cache — so its budget is
// widened by entraColdStartHeadroom. With the zero profile TokenAcquire
// is 0, so entra and sas share the same (floored) cold-start budget,
// keeping "zero means instant everywhere" intact.
func (b *MockBackend) ColdStartLatencyThreshold() time.Duration {
	budget := mockLatencyBudget(b.DelayProfile, b.authName == AuthEntra)
	if b.authName == AuthEntra {
		budget += entraColdStartHeadroom(b.DelayProfile)
	}
	return budget
}

// entraColdStartHeadroom is the extra cold-start ceiling the entra path
// gets on top of the warm rendezvous budget, to keep comfortable
// headroom over the modelled token acquisition on noisy CI. It scales
// with the profile's TokenAcquire (4x the modelled fetch ~= 1.8 s for
// the default profile's 450 ms, recovering the historical ~5 s entra
// ceiling) and is exactly 0 for the zero profile.
func entraColdStartHeadroom(p server.DelayProfile) time.Duration {
	return 4 * p.TokenAcquire
}

// newTokenProvider builds a fresh token provider for one aztunnel
// instance (one listener, sender, or connect invocation), wrapped to
// record the aztunnel_token_fetch_* metrics on m under this cell's
// provider label. Each call returns an INDEPENDENT provider so every
// instance owns its own credential cache — mirroring separate aztunnel
// processes, which is what makes the entra cell's per-process cold-start
// cost visible (a shared provider would warm a sibling's cache and mask
// it).
//
// SAS cell (and the zero value): a real relay.SASTokenProvider, which
// re-signs locally per call (free). Entra cell: a real
// relay.EntraTokenProvider backed by fakeEntraCredential, so the
// production token cache is exercised end-to-end and the modelled
// DelayProfile.TokenAcquire cost is paid only on the cold cache miss
// (zero for the zero profile, so entra is then instant too).
func (b *MockBackend) newTokenProvider(m *metrics.Metrics) relay.TokenProvider {
	switch b.authName {
	case AuthEntra:
		inner, _ := newFakeEntraProvider(b.DelayProfile.TokenAcquire)
		return relay.WithMetrics(inner, m, relay.ProviderEntra)
	default: // AuthSAS or "" (zero value)
		inner := &relay.SASTokenProvider{
			KeyName: server.DefaultSASKeyName,
			Key:     server.DefaultSASKey,
		}
		return relay.WithMetrics(inner, m, relay.ProviderSAS)
	}
}

// Setup brings up the in-process topology described by opts and blocks
// until every listener's control channel is attached and every sender
// bind is accepting TCP. All goroutines, the mock HTTP server, and
// the sender binds are released via t.Cleanup.
func (b *MockBackend) Setup(t testing.TB, opts scenarios.SetupOptions) *scenarios.Tunnel {
	t.Helper()
	if opts.NumListeners < 0 {
		t.Fatalf("NumListeners must be >= 0, got %d", opts.NumListeners)
	}
	switch opts.SenderMode {
	case scenarios.ModePortForward, scenarios.ModeSOCKS5, scenarios.ModeConnect:
	default:
		t.Fatalf("unknown SenderMode %v", opts.SenderMode)
	}
	numSenders := opts.NumSenders
	if numSenders < 1 {
		numSenders = 1
	}
	// ModeConnect spawns senders on demand via Tunnel.OpenConnect.
	// No persistent sender goroutine at Setup time.
	if opts.SenderMode == scenarios.ModeConnect {
		numSenders = 0
	}

	host, clientOpts := startMockRelay(t, b.DelayProfile)
	// Cleanup ordering: startMockRelay's own t.Cleanup registers
	// srv.Close. Register cancel+wg.Wait AFTER it so LIFO teardown
	// runs cancel first → drains the listener / sender goroutines
	// → THEN closes the mock relay. This stops the "listener
	// exited: <error>" log lines that fired when the relay
	// disappeared while listeners were still running.
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	t.Cleanup(func() {
		cancel()
		wg.Wait()
	})

	entity := mustEntityName(t)

	tun := &scenarios.Tunnel{}

	// startListener brings up a single listener goroutine and returns
	// its scenarios handle. Each listener owns a private metrics
	// surface so Completed() / Active() report only that listener's
	// bridges, and a private log buffer so Logs() returns only this
	// listener's lines (cross-process correlation tests rely on the
	// per-handle isolation).
	startListener := func(t testing.TB) *scenarios.Listener {
		t.Helper()
		m := metrics.New()
		logs := newCaptureBuffer()
		lctx, lcancel := context.WithCancel(ctx)
		done := make(chan struct{})

		cfg := listener.Config{
			Endpoint:       host,
			EntityPath:     entity,
			TokenProvider:  b.newTokenProvider(m),
			ClientOptions:  clientOpts,
			AllowList:      opts.AllowedTargets,
			MaxConnections: opts.MaxConnections,
			ConnectTimeout: opts.ConnectTimeout,
			Logger:         slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
			Metrics:        m,
		}

		wg.Add(1)
		go func() {
			defer wg.Done()
			defer close(done)
			err := listener.ListenAndServe(lctx, cfg)
			if err != nil && lctx.Err() == nil && ctx.Err() == nil {
				t.Logf("listener exited: %v", err)
			}
		}()

		// Wait for the control_channel_connected gauge to flip to 1:
		// it is set inside relay.ListenAndServe's OnConnect, which is
		// the canonical "control channel attached" signal — the same
		// log line the Azure backend waits for. Polling the gauge
		// avoids any need to scrape /metrics over HTTP.
		if !waitForGauge(m, "aztunnel_control_channel_connected", 1, 15*time.Second) {
			lcancel()
			<-done
			t.Fatalf("listener never reported control_channel_connected")
		}

		var stopOnce sync.Once
		stop := func() {
			stopOnce.Do(func() {
				lcancel()
				<-done
			})
		}

		return &scenarios.Listener{
			Addr:             "",
			Completed:        counterReader(m, "aztunnel_connections_total"),
			Active:           gaugeReader(m, "aztunnel_active_connections"),
			ConnectionErrors: connectionErrorReader(m),
			Stop:             stop,
			Logs:             logs.String,
		}
	}

	for i := 0; i < opts.NumListeners; i++ {
		tun.Listeners = append(tun.Listeners, startListener(t))
	}

	// Each sender goroutine has its own free :0 bind and its own
	// metrics surface. Ready is signalled via the
	// PortForwardConfig.Ready / SOCKS5Config.Ready callback, which
	// fires immediately after net.Listen — there is no probe TCP
	// connection that would consume a listener slot under
	// MaxConnections.
	startOneSender := func() *scenarios.Sender {
		m := metrics.New()
		senderTP := b.newTokenProvider(m)
		logs := newCaptureBuffer()
		senderLogger := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
		sctx, scancel := context.WithCancel(ctx)
		done := make(chan struct{})
		addrCh := make(chan net.Addr, 1)
		ready := func(a net.Addr) {
			select {
			case addrCh <- a:
			default:
			}
		}

		wg.Add(1)
		go func() {
			defer wg.Done()
			defer close(done)
			var err error
			switch opts.SenderMode {
			case scenarios.ModePortForward:
				err = sender.PortForward(sctx, sender.PortForwardConfig{
					Endpoint:      host,
					EntityPath:    entity,
					TokenProvider: senderTP,
					ClientOptions: clientOpts,
					Target:        opts.Target,
					BindAddress:   "127.0.0.1:0",
					Logger:        senderLogger,
					Metrics:       m,
					Ready:         ready,
				})
			case scenarios.ModeSOCKS5:
				err = sender.SOCKS5Proxy(sctx, sender.SOCKS5Config{
					Endpoint:      host,
					EntityPath:    entity,
					TokenProvider: senderTP,
					ClientOptions: clientOpts,
					BindAddress:   "127.0.0.1:0",
					Logger:        senderLogger,
					Metrics:       m,
					Ready:         ready,
				})
			}
			if err != nil && sctx.Err() == nil && ctx.Err() == nil {
				t.Logf("sender exited: %v", err)
			}
		}()

		var addr net.Addr
		select {
		case addr = <-addrCh:
		case <-time.After(15 * time.Second):
			scancel()
			<-done
			t.Fatalf("sender Ready callback never fired")
		}

		var stopOnce sync.Once
		stop := func() {
			stopOnce.Do(func() {
				scancel()
				<-done
			})
		}

		return &scenarios.Sender{
			Addr:                addr.String(),
			Completed:           counterReader(m, "aztunnel_connections_total"),
			Active:              gaugeReader(m, "aztunnel_active_connections"),
			DialDurationSamples: histogramSampleCount(m, "aztunnel_dial_duration_seconds"),
			TokenFetchOK:        tokenFetchOKReader(m),
			Stop:                stop,
			Logs:                logs.String,
		}
	}

	for i := 0; i < numSenders; i++ {
		s := startOneSender()
		tun.Senders = append(tun.Senders, s)
		tun.SenderAddrs = append(tun.SenderAddrs, s.Addr)
	}
	if len(tun.SenderAddrs) > 0 {
		tun.SenderAddr = tun.SenderAddrs[0]
	}
	tun.AddListener = func(t *testing.T) *scenarios.Listener {
		t.Helper()
		l := startListener(t)
		tun.Listeners = append(tun.Listeners, l)
		return l
	}

	if opts.SenderMode == scenarios.ModeConnect {
		tun.SetOpenConnect(b.makeOpenConnect(ctx, &wg, host, entity, clientOpts, b.newTokenProvider))
	}

	return tun
}

// makeOpenConnect returns the closure that Tunnel.OpenConnect calls
// when SenderMode==ModeConnect. Each call spawns one in-process
// sender.Connect goroutine with pipe-backed stdio. The returned
// ConnectClient bridges the OTHER ends of those pipes; closing it
// cancels the sender's context, closes both pipes, and drains the
// goroutine via the parent wg.
//
// newProvider builds the token provider for a single connect
// invocation against that invocation's private metrics surface; it is
// called once per OpenConnect so each connect "process" owns its own
// credential cache (mirroring the per-instance contract in Setup).
func (b *MockBackend) makeOpenConnect(
	parentCtx context.Context,
	wg *sync.WaitGroup,
	host, entity string,
	clientOpts relay.ClientOptions,
	newProvider func(*metrics.Metrics) relay.TokenProvider,
) func(t testing.TB, target string) scenarios.ConnectClient {
	return func(t testing.TB, target string) scenarios.ConnectClient {
		t.Helper()
		// Two pipes: one for sender's stdin (test writes, sender
		// reads), one for sender's stdout (sender writes, test reads).
		stdinR, stdinW := io.Pipe()
		stdoutR, stdoutW := io.Pipe()
		logs := newCaptureBuffer()
		senderLogger := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))

		// Per-call cancel chained off the backend's parent ctx so
		// teardown order (test cleanup) still works.
		ctx, cancel := context.WithCancel(parentCtx)
		done := make(chan struct{})
		var exitErr error

		wg.Add(1)
		go func() {
			defer wg.Done()
			defer close(done)
			// sender.Connect dereferences cfg.Metrics.InstrumentedDial
			// unconditionally despite the doc-comment claiming Metrics
			// is optional. Pass a fresh metrics.New() per call and build
			// this connect's provider against it.
			cm := metrics.New()
			exitErr = sender.Connect(ctx, sender.ConnectConfig{
				Endpoint:      host,
				EntityPath:    entity,
				TokenProvider: newProvider(cm),
				ClientOptions: clientOpts,
				Target:        target,
				Stdin:         stdinR,
				Stdout:        stdoutW,
				Logger:        senderLogger,
				Metrics:       cm,
			})
			// Close the stdout writer so Read on the other end
			// returns EOF instead of blocking forever.
			_ = stdoutW.Close()
		}()

		cc := &mockConnectClient{
			stdinW:  stdinW,
			stdoutR: stdoutR,
			logs:    logs,
			cancel:  cancel,
			done:    done,
			err:     func() error { return exitErr },
		}
		t.Cleanup(func() { _ = cc.Close() })
		return cc
	}
}

// mockConnectClient is the mock backend's scenarios.ConnectClient
// implementation. Read drains the in-process sender's stdout pipe,
// Write feeds its stdin pipe; Logs returns the sender's slog buffer;
// Wait blocks on the sender goroutine; Close cancels and closes the
// pipes (idempotent).
type mockConnectClient struct {
	stdinW   *io.PipeWriter
	stdoutR  *io.PipeReader
	logs     *captureBuffer
	cancel   context.CancelFunc
	done     chan struct{}
	err      func() error
	closeOne sync.Once
}

func (c *mockConnectClient) Read(p []byte) (int, error)  { return c.stdoutR.Read(p) }
func (c *mockConnectClient) Write(p []byte) (int, error) { return c.stdinW.Write(p) }
func (c *mockConnectClient) Logs() string                { return c.logs.String() }

func (c *mockConnectClient) Wait(ctx context.Context) error {
	select {
	case <-c.done:
		return c.err()
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *mockConnectClient) Close() error {
	c.closeOne.Do(func() {
		c.cancel()
		_ = c.stdinW.Close()
		_ = c.stdoutR.Close()
		// Don't block on done here — the parent wg waits at backend
		// teardown. Close is allowed to return before the goroutine
		// fully exits.
	})
	return nil
}

// SetupExpectingFailure brings up the topology described by opts and
// EXPECTS something to fail. Trigger semantics follow the Backend
// contract: listener-side auth override → wait for listener to log
// control-channel failure; sender-side auth override → start sender
// and either wait for its failure log or perform one client dial to
// trigger it; ModeConnect failure → no sender started, caller drives
// via Tunnel.OpenConnect.
//
// On the mock backend the failure is reproduced by signing the auth
// token with the override's BadSASKey, which the mock relay rejects
// the same way Azure does (mockrelay/server/auth_test.go verifies
// the parity).
func (b *MockBackend) SetupExpectingFailure(t testing.TB, opts scenarios.SetupOptions) scenarios.FailureHandle {
	t.Helper()
	switch opts.SenderMode {
	case scenarios.ModePortForward, scenarios.ModeSOCKS5, scenarios.ModeConnect:
	default:
		t.Fatalf("unknown SenderMode %v", opts.SenderMode)
	}
	if opts.OverrideListenerAuth == nil && opts.OverrideSenderAuth == nil && opts.OverrideHycoName == "" {
		t.Fatalf("SetupExpectingFailure requires at least one override (ListenerAuth, SenderAuth, or HycoName)")
	}
	// UseOppositeSASDirection exercises Azure's per-key claim
	// enforcement, which the mock relay does not model (the mock
	// has a single shared key for both directions). Scenarios that
	// set this MUST scope to AzureOnly; the defensive skip here
	// catches any future caller that forgets.
	if (opts.OverrideListenerAuth != nil && opts.OverrideListenerAuth.UseOppositeSASDirection) ||
		(opts.OverrideSenderAuth != nil && opts.OverrideSenderAuth.UseOppositeSASDirection) {
		t.Skipf("UseOppositeSASDirection requires per-direction SAS keys not modelled by the mock relay")
	}

	host, clientOpts := startMockRelay(t, b.DelayProfile)

	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	t.Cleanup(func() {
		cancel()
		wg.Wait()
	})

	entity := mustEntityName(t)
	listenerLogs := newCaptureBuffer()
	senderLogs := newCaptureBuffer()

	goodProvider := relay.TokenProvider(&relay.SASTokenProvider{
		KeyName: server.DefaultSASKeyName,
		Key:     server.DefaultSASKey,
	})
	listenerProvider := goodProvider
	senderProvider := goodProvider
	if opts.OverrideListenerAuth != nil && opts.OverrideListenerAuth.BadSASKey != "" {
		listenerProvider = &relay.SASTokenProvider{
			KeyName: server.DefaultSASKeyName,
			Key:     opts.OverrideListenerAuth.BadSASKey,
		}
	}
	if opts.OverrideSenderAuth != nil && opts.OverrideSenderAuth.BadSASKey != "" {
		senderProvider = &relay.SASTokenProvider{
			KeyName: server.DefaultSASKeyName,
			Key:     opts.OverrideSenderAuth.BadSASKey,
		}
	}

	// Listener-side failure path: spin up the listener with bad
	// creds, wait for its control-channel failure log, return.
	if opts.OverrideListenerAuth != nil {
		listenerLogger := slog.New(slog.NewTextHandler(listenerLogs, &slog.HandlerOptions{Level: slog.LevelDebug}))
		lctx, lcancel := context.WithCancel(ctx)
		// Per-listener context is released via parent ctx; the
		// explicit cancel handle exists only for the fast-fail
		// branch below.
		t.Cleanup(lcancel)
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := listener.ListenAndServe(lctx, listener.Config{
				Endpoint:      host,
				EntityPath:    entity,
				TokenProvider: listenerProvider,
				ClientOptions: clientOpts,
				AllowList:     opts.AllowedTargets,
				Logger:        listenerLogger,
				Metrics:       metrics.New(),
			})
			if err != nil && lctx.Err() == nil && ctx.Err() == nil {
				listenerLogger.Debug("listener exited", "err", err)
			}
		}()
		// Wait for a control-channel failure log line. The
		// production wording is "control channel disconnected"
		// (control loop teardown).
		if !waitForLogString(listenerLogs.String, "control channel disconnected", 30*time.Second) {
			lcancel()
			t.Fatalf("listener never logged a control-channel failure within 30s\n--- logs ---\n%s", listenerLogs.String())
		}
		return &mockFailureHandle{
			listenerLogs: listenerLogs.String,
			senderLogs:   func() string { return "" },
		}
	}

	// Sender-side failure path: bring up a healthy listener (or
	// none, if ModeConnect's contract leaves the listener absent),
	// then start the sender with bad creds and observe its
	// failure.
	if opts.NumListeners > 0 {
		listenerLogger := slog.New(slog.NewTextHandler(listenerLogs, &slog.HandlerOptions{Level: slog.LevelDebug}))
		m := metrics.New()
		lctx, lcancel := context.WithCancel(ctx)
		t.Cleanup(lcancel)
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := listener.ListenAndServe(lctx, listener.Config{
				Endpoint:       host,
				EntityPath:     entity,
				TokenProvider:  listenerProvider,
				ClientOptions:  clientOpts,
				AllowList:      opts.AllowedTargets,
				MaxConnections: opts.MaxConnections,
				ConnectTimeout: opts.ConnectTimeout,
				Logger:         listenerLogger,
				Metrics:        m,
			})
			if err != nil && lctx.Err() == nil && ctx.Err() == nil {
				listenerLogger.Debug("listener exited", "err", err)
			}
		}()
		if !waitForGauge(m, "aztunnel_control_channel_connected", 1, 15*time.Second) {
			t.Fatalf("listener never reported control_channel_connected during SetupExpectingFailure")
		}
	}

	if opts.SenderMode == scenarios.ModeConnect {
		// Auth-rejection scenarios pin the credential explicitly
		// (good or deliberately-bad SAS) regardless of the cell's
		// auth method — this path tests data-plane SAS validation,
		// not provider selection, matching the Azure backend's
		// BadSASKey handling. Wrap the fixed provider in a factory so
		// every OpenConnect call reuses it.
		return &mockFailureHandle{
			listenerLogs: listenerLogs.String,
			senderLogs:   senderLogs.String,
			openConnect: b.makeOpenConnect(ctx, &wg, host, entity, clientOpts,
				func(*metrics.Metrics) relay.TokenProvider { return senderProvider }),
		}
	}

	senderLogger := slog.New(slog.NewTextHandler(senderLogs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	sctx, scancel := context.WithCancel(ctx)
	t.Cleanup(scancel)
	addrCh := make(chan net.Addr, 1)
	ready := func(a net.Addr) {
		select {
		case addrCh <- a:
		default:
		}
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		var err error
		switch opts.SenderMode {
		case scenarios.ModePortForward:
			err = sender.PortForward(sctx, sender.PortForwardConfig{
				Endpoint:      host,
				EntityPath:    entity,
				TokenProvider: senderProvider,
				ClientOptions: clientOpts,
				Target:        opts.Target,
				BindAddress:   "127.0.0.1:0",
				Logger:        senderLogger,
				Metrics:       metrics.New(),
				Ready:         ready,
			})
		case scenarios.ModeSOCKS5:
			err = sender.SOCKS5Proxy(sctx, sender.SOCKS5Config{
				Endpoint:      host,
				EntityPath:    entity,
				TokenProvider: senderProvider,
				ClientOptions: clientOpts,
				BindAddress:   "127.0.0.1:0",
				Logger:        senderLogger,
				Metrics:       metrics.New(),
				Ready:         ready,
			})
		}
		if err != nil && sctx.Err() == nil && ctx.Err() == nil {
			senderLogger.Debug("sender exited", "err", err)
		}
	}()

	// Wait for sender bind ready. The port-forward / SOCKS5 sender
	// only dials the relay when a client connects; we trigger that
	// dial with one local connect attempt.
	var addr net.Addr
	select {
	case addr = <-addrCh:
	case <-time.After(15 * time.Second):
		t.Fatalf("sender Ready callback never fired during SetupExpectingFailure")
	}

	conn, err := net.DialTimeout("tcp", addr.String(), 5*time.Second)
	if err != nil {
		t.Fatalf("dial sender during SetupExpectingFailure: %v", err)
	}
	triggerSenderRelayDial(conn, opts.SenderMode)
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, _ = io.ReadAll(conn)
	_ = conn.Close()

	if !waitForLogString(senderLogs.String, "relay dial failed", 30*time.Second) {
		t.Fatalf("sender never logged 'relay dial failed' within 30s\n--- logs ---\n%s", senderLogs.String())
	}
	return &mockFailureHandle{
		listenerLogs: listenerLogs.String,
		senderLogs:   senderLogs.String,
	}
}

// triggerSenderRelayDial sends the minimal byte sequence each
// sender mode requires to provoke its relay dial. PortForward
// dials the relay on first client data; SOCKS5 requires the
// SOCKS5 greeting + CONNECT request before it dials. ModeConnect
// has no client-protocol layer here — the caller invokes
// OpenConnect, which starts the sender and dials directly.
func triggerSenderRelayDial(conn net.Conn, mode scenarios.SenderMode) {
	switch mode {
	case scenarios.ModeSOCKS5:
		// SOCKS5 greeting: VER=5, NMETHODS=1, METHOD=0 (no auth).
		_, _ = conn.Write([]byte{0x05, 0x01, 0x00})
		// Drain the server's method selection so the sender
		// moves to the CONNECT phase.
		_ = conn.SetReadDeadline(time.Now().Add(1 * time.Second))
		drain := make([]byte, 2)
		_, _ = io.ReadFull(conn, drain)
		// CONNECT request: VER=5, CMD=1 (CONNECT), RSV=0,
		// ATYP=1 (IPv4), DST=127.0.0.1, PORT=9999.
		_, _ = conn.Write([]byte{0x05, 0x01, 0x00, 0x01, 127, 0, 0, 1, 0x27, 0x0F})
	default:
		_, _ = conn.Write([]byte("trigger\n"))
	}
}

// mockFailureHandle is the mock backend's scenarios.FailureHandle
// implementation. Logs accessors are snapshots; Close is a no-op
// because the parent t.Cleanup already cancels and drains.
type mockFailureHandle struct {
	listenerLogs func() string
	senderLogs   func() string
	openConnect  func(t testing.TB, target string) scenarios.ConnectClient
}

func (h *mockFailureHandle) ListenerLogs() string { return h.listenerLogs() }
func (h *mockFailureHandle) SenderLogs() string   { return h.senderLogs() }
func (h *mockFailureHandle) Close()               {}

// OpenConnect lets ModeConnect failure scenarios drive a connect
// invocation against this handle's pre-configured (possibly faulty)
// auth credentials. Returns nil if the handle was not built for a
// ModeConnect scenario.
func (h *mockFailureHandle) OpenConnect(t testing.TB, target string) scenarios.ConnectClient {
	t.Helper()
	if h.openConnect == nil {
		t.Fatalf("FailureHandle.OpenConnect called on a non-ModeConnect handle")
	}
	return h.openConnect(t, target)
}

// waitForLogString polls logs() at 50 ms until the substring appears
// or timeout elapses. Returns true on hit, false on deadline.
func waitForLogString(logs func() string, substr string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if strings.Contains(logs(), substr) {
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return false
}

// startMockRelay starts a server.Server backed by httptest.NewTLSServer
// (aztunnel only dials TLS-protected relays) and returns the host:port
// plus a ClientOptions whose TLSConfig skips verification of the test
// certificate.
//
// profile selects the per-step synthetic delay the relay applies; the
// zero value (server.DelayProfileZero) means no delay and the
// rendezvous completes in the in-process ~6 ms baseline. Pass
// server.DelayProfileDefault for e2e suites whose thresholds were
// calibrated against Azure Relay.
//
// RendezvousTimeout is a flat 5 s — generous enough for any reasonable
// DelayProfile while keeping MaxConn back-pressure scenarios
// fail-fast.
func startMockRelay(t testing.TB, profile server.DelayProfile) (host string, opts relay.ClientOptions) {
	t.Helper()
	rs, err := server.NewServerForTesting(server.Config{
		Logger:            slog.New(slog.NewTextHandler(io.Discard, nil)),
		RendezvousTimeout: 5 * time.Second,
	}, server.WithDelayProfile(profile))
	if err != nil {
		t.Fatalf("new mock relay: %v", err)
	}
	srv := httptest.NewTLSServer(rs.Handler())
	u, _ := url.Parse(srv.URL)
	host = u.Host
	opts = relay.ClientOptions{
		TLSConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // test cert
	}
	t.Cleanup(srv.Close)
	return host, opts
}

// mustEntityName returns a short random suffix appended to the
// caller's test name. Keeps entities unique across scenarios.
func mustEntityName(t testing.TB) string {
	t.Helper()
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatalf("rand: %v", err)
	}
	safe := strings.NewReplacer("/", "-", " ", "-", "#", "-").Replace(t.Name())
	return safe + "-" + hex.EncodeToString(b[:])
}

// waitForGauge polls m's registry for a gauge to reach at least want
// before timeout. Returns true on success, false on deadline.
func waitForGauge(m *metrics.Metrics, name string, want float64, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if gaugeValue(m, name) >= want {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

// counterReader returns a closure that sums every sample for the given
// counter name across all label combinations on m's registry. Used to
// expose listener / sender connection counters as Completed() closures.
//
// Summing across labels (target, status) is deliberate: each per-
// listener / per-sender metrics surface only sees its own connections,
// so there is no risk of cross-listener bleed in the sum.
func counterReader(m *metrics.Metrics, name string) func() int64 {
	return func() int64 {
		families, err := m.Registry.Gather()
		if err != nil {
			return 0
		}
		var total float64
		for _, f := range families {
			if f.GetName() != name {
				continue
			}
			for _, sample := range f.GetMetric() {
				if c := sample.GetCounter(); c != nil {
					total += c.GetValue()
				}
			}
		}
		return int64(total)
	}
}

// gaugeReader returns a closure that sums every sample for the given
// gauge name across all label combinations on m's registry.
func gaugeReader(m *metrics.Metrics, name string) func() int64 {
	return func() int64 {
		families, err := m.Registry.Gather()
		if err != nil {
			return 0
		}
		var total float64
		for _, f := range families {
			if f.GetName() != name {
				continue
			}
			for _, sample := range f.GetMetric() {
				if g := sample.GetGauge(); g != nil {
					total += g.GetValue()
				}
			}
		}
		return int64(total)
	}
}

// connectionErrorReader returns a closure that sums every sample of
// aztunnel_connection_errors_total whose reason label equals the
// requested reason, on m's registry. Each per-listener metrics surface
// only sees its own connection errors, so summing across the other
// labels (role) is safe.
//
// Returns 0 when the counter has no samples for that reason — Prometheus
// counters are not initialized until the first observation.
func connectionErrorReader(m *metrics.Metrics) func(reason string) int64 {
	return func(reason string) int64 {
		families, err := m.Registry.Gather()
		if err != nil {
			return 0
		}
		var total float64
		for _, f := range families {
			if f.GetName() != "aztunnel_connection_errors_total" {
				continue
			}
			for _, sample := range f.GetMetric() {
				if !labelMatches(sample.GetLabel(), "reason", reason) {
					continue
				}
				if c := sample.GetCounter(); c != nil {
					total += c.GetValue()
				}
			}
		}
		return int64(total)
	}
}

// labelMatches reports whether any of pairs has the given name and value.
func labelMatches(pairs []*dto.LabelPair, name, value string) bool {
	for _, lp := range pairs {
		if lp.GetName() == name && lp.GetValue() == value {
			return true
		}
	}
	return false
}

// gaugeValue returns the single-sample value of a gauge by name. Used
// for the control_channel_connected readiness probe, which has no
// labels.
func gaugeValue(m *metrics.Metrics, name string) float64 {
	families, err := m.Registry.Gather()
	if err != nil {
		return 0
	}
	for _, f := range families {
		if f.GetName() != name {
			continue
		}
		for _, sample := range f.GetMetric() {
			if g := sample.GetGauge(); g != nil {
				return g.GetValue()
			}
		}
	}
	return 0
}

// histogramSampleCount returns a closure that sums SampleCount
// across every label combination of histogram `name` on m's
// registry. Used by per-handle DialDurationSamples accessors that
// need a single scalar "did this histogram observe anything" value.
func histogramSampleCount(m *metrics.Metrics, name string) func() uint64 {
	return func() uint64 {
		families, err := m.Registry.Gather()
		if err != nil {
			return 0
		}
		var total uint64
		for _, f := range families {
			if f.GetName() != name {
				continue
			}
			for _, sample := range f.GetMetric() {
				if h := sample.GetHistogram(); h != nil {
					total += h.GetSampleCount()
				}
			}
		}
		return total
	}
}

// tokenFetchOKReader returns a closure that reports one
// scenarios.TokenFetchObservation per `provider` label observed with
// result="ok" on m's registry, reading the counter
// aztunnel_token_fetch_total and the histogram
// aztunnel_token_fetch_seconds (its _count) for that sender. Returns
// nil before any token fetch has been recorded, matching the optional-
// nil contract on Sender.TokenFetchOK.
//
// Counter and histogram are read from a SINGLE Registry.Gather()
// snapshot so a concurrent token fetch can never be seen half-applied
// (counter incremented but histogram not, or vice versa); the
// observability wrapper records both per call, so within one snapshot
// they agree.
func tokenFetchOKReader(m *metrics.Metrics) func() []scenarios.TokenFetchObservation {
	const (
		counterFamily = "aztunnel_token_fetch_total"
		histFamily    = "aztunnel_token_fetch_seconds"
	)
	return func() []scenarios.TokenFetchObservation {
		families, err := m.Registry.Gather()
		if err != nil {
			return nil
		}
		counters := map[string]uint64{}
		hists := map[string]uint64{}
		for _, f := range families {
			switch f.GetName() {
			case counterFamily:
				for _, sample := range f.GetMetric() {
					if !labelMatches(sample.GetLabel(), "result", "ok") {
						continue
					}
					if c := sample.GetCounter(); c != nil {
						counters[providerLabel(sample.GetLabel())] += uint64(c.GetValue())
					}
				}
			case histFamily:
				for _, sample := range f.GetMetric() {
					if !labelMatches(sample.GetLabel(), "result", "ok") {
						continue
					}
					if h := sample.GetHistogram(); h != nil {
						hists[providerLabel(sample.GetLabel())] += h.GetSampleCount()
					}
				}
			}
		}
		if len(counters) == 0 && len(hists) == 0 {
			return nil
		}
		seen := map[string]struct{}{}
		for p := range counters {
			seen[p] = struct{}{}
		}
		for p := range hists {
			seen[p] = struct{}{}
		}
		providers := make([]string, 0, len(seen))
		for p := range seen {
			providers = append(providers, p)
		}
		sort.Strings(providers)
		out := make([]scenarios.TokenFetchObservation, 0, len(providers))
		for _, p := range providers {
			out = append(out, scenarios.TokenFetchObservation{
				Provider:       p,
				CounterValue:   counters[p],
				HistogramCount: hists[p],
			})
		}
		return out
	}
}

// providerLabel returns the value of the `provider` label, or "" when
// absent.
func providerLabel(pairs []*dto.LabelPair) string {
	for _, lp := range pairs {
		if lp.GetName() == "provider" {
			return lp.GetValue()
		}
	}
	return ""
}

// captureBuffer is a goroutine-safe io.Writer used as the destination
// for a slog TextHandler. slog.Handler implementations are themselves
// goroutine-safe only when their underlying writer is — bytes.Buffer
// is not — so observability e2e scenarios that scrape the captured
// output across multiple bridges need this mutex-guarded wrapper.
type captureBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func newCaptureBuffer() *captureBuffer { return &captureBuffer{} }

// Write appends p to the buffer under a mutex. The mutex is what
// makes the wrapped slog handler safe for concurrent use across the
// listener / sender goroutines that share this buffer.
func (c *captureBuffer) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.Write(p)
}

// String returns the captured output as a single string. Safe to
// call from any goroutine; a snapshot of the buffer at call time.
func (c *captureBuffer) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.String()
}
