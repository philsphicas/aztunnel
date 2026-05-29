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
	"io"
	"log/slog"
	"net"
	"net/http/httptest"
	"net/url"
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

// MockBackend implements scenarios.Backend by standing up a mock
// relay server + aztunnel listener(s) + aztunnel sender(s) all in the
// same process. It is the fast, deterministic side of the mock-vs-
// Azure conformance matrix and runs in the default
// `go test ./mockrelay/...` job.
type MockBackend struct {
	// DelayProfile parameterizes the synthetic per-step sleeps the
	// mock relay applies on every leg of the rendezvous protocol.
	// The zero DelayProfile (i.e., the zero value of MockBackend)
	// means no synthetic delay: rendezvous completes in the
	// in-process baseline (~6 ms), which is what bench_test.go wants.
	//
	// For e2e suites whose timing thresholds were calibrated against
	// Azure Relay (e.g. e2e_test.go), set DelayProfile explicitly to
	// server.DelayProfileDefault so the mock approximates the
	// wireshark-observed wall-clock shape.
	DelayProfile server.DelayProfile
}

// Name returns the backend identifier (always "mock"). The harness
// does not embed it in sub-test paths — MockBackend has no axes,
// so scenarios run directly under the test entry point — but
// scenarios and external callers may surface it in debug output.
func (*MockBackend) Name() string { return "mock" }

// Axes returns the matrix dimensions this backend varies over. The
// mock has none — it only speaks SAS against an in-process server,
// so the harness runs scenarios directly under the test entry point
// with no axis sub-path layer.
func (*MockBackend) Axes() []scenarios.Axis { return nil }

// Cell returns the backend pinned to the cell described by values.
// MockBackend has no axes so values must be empty; Cell returns the
// receiver unchanged.
func (m *MockBackend) Cell(values map[string]string) scenarios.Backend {
	if len(values) != 0 {
		panic("MockBackend.Cell: no axes, expected empty values")
	}
	return m
}

// ConnectLatencyThreshold returns the per-backend connect-latency
// ceiling for the Performance suite. 3 s leaves comfortable headroom
// for CI scheduling noise on any reasonable DelayProfile without
// masking regressions of the order of seconds.
//
// The mock returns one value regardless of cell — MockBackend has
// no axes, and the DelayProfile field affects timing but is not
// itself an axis the harness enumerates over.
func (*MockBackend) ConnectLatencyThreshold() time.Duration {
	return 3 * time.Second
}

// ColdStartLatencyThreshold returns the per-backend ceiling for the
// first connection through a freshly-started sender. The mock has
// no per-process credential cache to warm — every dial pays the
// same rendezvous delay regardless of order — so the cold-start
// budget intentionally matches ConnectLatencyThreshold.
func (*MockBackend) ColdStartLatencyThreshold() time.Duration {
	return 3 * time.Second
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
	tokenProvider := &relay.SASTokenProvider{
		KeyName: server.DefaultSASKeyName,
		Key:     server.DefaultSASKey,
	}

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
			TokenProvider:  tokenProvider,
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
					TokenProvider: tokenProvider,
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
					TokenProvider: tokenProvider,
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
		tun.SetOpenConnect(b.makeOpenConnect(ctx, &wg, host, entity, clientOpts, tokenProvider))
	}

	return tun
}

// makeOpenConnect returns the closure that Tunnel.OpenConnect calls
// when SenderMode==ModeConnect. Each call spawns one in-process
// sender.Connect goroutine with pipe-backed stdio. The returned
// ConnectClient bridges the OTHER ends of those pipes; closing it
// cancels the sender's context, closes both pipes, and drains the
// goroutine via the parent wg.
func (b *MockBackend) makeOpenConnect(
	parentCtx context.Context,
	wg *sync.WaitGroup,
	host, entity string,
	clientOpts relay.ClientOptions,
	tokenProvider relay.TokenProvider,
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
			// is optional. Pass a fresh metrics.New() per call.
			exitErr = sender.Connect(ctx, sender.ConnectConfig{
				Endpoint:      host,
				EntityPath:    entity,
				TokenProvider: tokenProvider,
				ClientOptions: clientOpts,
				Target:        target,
				Stdin:         stdinR,
				Stdout:        stdoutW,
				Logger:        senderLogger,
				Metrics:       metrics.New(),
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
		// The test will drive the failure via Tunnel.OpenConnect.
		return &mockFailureHandle{
			listenerLogs: listenerLogs.String,
			senderLogs:   senderLogs.String,
			openConnect:  b.makeOpenConnect(ctx, &wg, host, entity, clientOpts, senderProvider),
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
