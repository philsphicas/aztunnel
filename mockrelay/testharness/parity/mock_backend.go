// Package parity wires the shared relay-parity scenario suite
// (internal/testharness/relayparity) up against an in-process mock relay.
//
// MockBackend brings up a real aztunnel listener + sender (the same
// code paths exercised against Azure Relay) talking to a mock relay
// server in-process. Tests written against the relayparity.Backend
// interface run unmodified against this backend, and the e2e module's
// azureBackend, so any behavioural divergence between mock and Azure
// shows up as a failing scenario in one but not the other.
package parity

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

	"github.com/philsphicas/aztunnel/internal/listener"
	"github.com/philsphicas/aztunnel/internal/metrics"
	"github.com/philsphicas/aztunnel/internal/relay"
	"github.com/philsphicas/aztunnel/internal/sender"
	"github.com/philsphicas/aztunnel/internal/testharness/relayparity"
	"github.com/philsphicas/aztunnel/mockrelay/server"
	dto "github.com/prometheus/client_model/go"
)

// MockBackend implements relayparity.Backend by standing up a mock
// relay server + aztunnel listener(s) + aztunnel sender(s) all in the
// same process. It is the fast, deterministic side of the parity
// matrix and runs in the default `go test ./mockrelay/...` job.
type MockBackend struct{}

// Name returns the backend identifier used in test sub-paths.
func (*MockBackend) Name() string { return "mock" }

// Setup brings up the in-process topology described by opts and blocks
// until every listener's control channel is attached and every sender
// bind is accepting TCP. All goroutines, the mock HTTP server, and
// the sender binds are released via t.Cleanup.
func (*MockBackend) Setup(t testing.TB, opts relayparity.SetupOptions) *relayparity.Tunnel {
	t.Helper()
	if opts.NumListeners < 1 {
		t.Fatalf("NumListeners must be >= 1, got %d", opts.NumListeners)
	}
	switch opts.SenderMode {
	case relayparity.ModePortForward, relayparity.ModeSOCKS5:
	default:
		t.Fatalf("unknown SenderMode %v", opts.SenderMode)
	}
	numSenders := opts.NumSenders
	if numSenders < 1 {
		numSenders = 1
	}

	host, clientOpts := startMockRelay(t)
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

	tun := &relayparity.Tunnel{}

	// startListener brings up a single listener goroutine and returns
	// its relayparity handle. Each listener owns a private metrics
	// surface so Completed() / Active() report only that listener's
	// bridges, and a private log buffer so Logs() returns only this
	// listener's lines (cross-process correlation tests rely on the
	// per-handle isolation).
	startListener := func(t testing.TB) *relayparity.Listener {
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

		return &relayparity.Listener{
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
	startOneSender := func() *relayparity.Sender {
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
			case relayparity.ModePortForward:
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
			case relayparity.ModeSOCKS5:
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

		return &relayparity.Sender{
			Addr:      addr.String(),
			Completed: counterReader(m, "aztunnel_connections_total"),
			Active:    gaugeReader(m, "aztunnel_active_connections"),
			Stop:      stop,
			Logs:      logs.String,
		}
	}

	for i := 0; i < numSenders; i++ {
		s := startOneSender()
		tun.Senders = append(tun.Senders, s)
		tun.SenderAddrs = append(tun.SenderAddrs, s.Addr)
	}
	tun.SenderAddr = tun.SenderAddrs[0]
	tun.AddListener = func(t *testing.T) *relayparity.Listener {
		t.Helper()
		l := startListener(t)
		tun.Listeners = append(tun.Listeners, l)
		return l
	}

	return tun
}

// startMockRelay starts a server.Server backed by httptest.NewTLSServer
// (aztunnel only dials TLS-protected relays) and returns the host:port
// plus a ClientOptions whose TLSConfig skips verification of the test
// certificate.
//
// RendezvousTimeout is deliberately short so MaxConn back-pressure
// scenarios — which intentionally provoke listeners to drop accept
// messages and rely on the rendezvous timing out — fail-fast on each
// retry instead of waiting the default. 1s is plenty for in-process
// rendezvous round-trips.
func startMockRelay(t testing.TB) (host string, opts relay.ClientOptions) {
	t.Helper()
	rs, err := server.NewServer(server.Config{
		Logger:            slog.New(slog.NewTextHandler(io.Discard, nil)),
		RendezvousTimeout: 1 * time.Second,
	})
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

// captureBuffer is a goroutine-safe io.Writer used as the destination
// for a slog TextHandler. slog.Handler implementations are themselves
// goroutine-safe only when their underlying writer is — bytes.Buffer
// is not — so observability parity scenarios that scrape the captured
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
