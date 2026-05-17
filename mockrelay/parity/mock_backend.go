// Package parity wires the shared relay-parity scenario suite
// (internal/relayparity) up against an in-process mock relay.
//
// MockBackend is intended for use inside the mockrelay module's test
// binary; it brings up a real aztunnel listener + sender (the same
// code paths exercised against Azure Relay) talking to a mock relay
// server in-process. Tests written against the relayparity.Backend
// interface run unmodified against this backend, and the e2e module's
// AzureBackend, so any behavioural divergence between mock and Azure
// shows up as a failing scenario in one but not the other.
package parity

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/philsphicas/aztunnel/internal/listener"
	"github.com/philsphicas/aztunnel/internal/relay"
	"github.com/philsphicas/aztunnel/internal/relayparity"
	"github.com/philsphicas/aztunnel/internal/sender"
	"github.com/philsphicas/aztunnel/mockrelay/server"
)

// MockBackend implements relayparity.Backend by standing up a mock
// relay server + aztunnel listener(s) + aztunnel sender all in the
// same process. It is the fast, deterministic side of the parity
// matrix and runs in the default `go test ./mockrelay/...` job.
type MockBackend struct{}

// Name returns the backend identifier used in test sub-paths.
func (*MockBackend) Name() string { return "mock" }

// Setup brings up the in-process topology described by opts and
// blocks until every listener has registered with the mock relay and
// the sender's local bind is accepting TCP. All goroutines, the mock
// HTTP server, and the sender bind are released via t.Cleanup.
func (*MockBackend) Setup(t *testing.T, opts relayparity.SetupOptions) *relayparity.Tunnel {
	t.Helper()
	if opts.NumListeners < 1 {
		t.Fatalf("NumListeners must be >= 1, got %d", opts.NumListeners)
	}

	ctx, cancel := context.WithCancel(context.Background())
	// Register cleanup BEFORE spawning any goroutines so an early
	// t.Fatalf in a readiness gate still drains anything we managed to
	// start. wg.Wait() on a zero-counter wg returns immediately, so
	// this is safe when Setup fails before any goroutine launches.
	var wg sync.WaitGroup
	t.Cleanup(func() {
		cancel()
		wg.Wait()
	})

	host, clientOpts, srv := startMockRelay(t)
	// Entity name is unique per scenario so concurrent t.Parallel
	// sub-tests cannot bleed into each other if they ever land on the
	// same mock-relay instance — and gives test failure messages a
	// useful breadcrumb to the originating scenario.
	entity := mustEntityName(t)

	tokenProvider := &relay.SASTokenProvider{
		KeyName: server.DefaultSASKeyName,
		Key:     server.DefaultSASKey,
	}
	silentLogger := slog.New(slog.NewTextHandler(io.Discard, nil))

	for i := 0; i < opts.NumListeners; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := listener.ListenAndServe(ctx, listener.Config{
				Endpoint:       host,
				EntityPath:     entity,
				TokenProvider:  tokenProvider,
				ClientOptions:  clientOpts,
				AllowList:      opts.AllowedTargets,
				MaxConnections: opts.MaxConnections,
				Logger:         silentLogger,
			})
			if err != nil && ctx.Err() == nil {
				t.Logf("listener exited: %v", err)
			}
		}()
	}
	// Wait for *all* listeners to register their accept side with the
	// mock so scenarios that happen to land on a not-yet-attached
	// listener don't see a transient 404. The mock's probe transitions
	// from 404 -> 400 once any listener is registered; we don't have a
	// strict per-listener probe, so a small extra settle after the
	// first ready signal covers the others.
	waitForControl(t, srv, entity, 3*time.Second)
	if opts.NumListeners > 1 {
		// One additional poll cycle is enough on the mock: every
		// listener registers in a single tick of its dial loop.
		time.Sleep(100 * time.Millisecond)
	}

	// Validate SenderMode synchronously so an unknown mode fails the
	// test immediately instead of being caught by the waitForTCP
	// timeout below — the timeout would mask the real cause and add
	// ~3 s of avoidable delay per misconfigured scenario.
	switch opts.SenderMode {
	case relayparity.ModePortForward, relayparity.ModeSOCKS5:
	default:
		t.Fatalf("unknown SenderMode %v", opts.SenderMode)
	}

	bind := pickFreePort(t)
	wg.Add(1)
	go func() {
		defer wg.Done()
		var err error
		switch opts.SenderMode {
		case relayparity.ModePortForward:
			err = sender.PortForward(ctx, sender.PortForwardConfig{
				Endpoint:      host,
				EntityPath:    entity,
				TokenProvider: tokenProvider,
				ClientOptions: clientOpts,
				Target:        opts.Target,
				BindAddress:   bind,
				Logger:        silentLogger,
			})
		case relayparity.ModeSOCKS5:
			err = sender.SOCKS5Proxy(ctx, sender.SOCKS5Config{
				Endpoint:      host,
				EntityPath:    entity,
				TokenProvider: tokenProvider,
				ClientOptions: clientOpts,
				BindAddress:   bind,
				Logger:        silentLogger,
			})
		}
		if err != nil && ctx.Err() == nil {
			t.Logf("sender exited: %v", err)
		}
	}()

	if !waitForTCP(bind, 3*time.Second) {
		t.Fatalf("sender bind %s never became reachable", bind)
	}

	return &relayparity.Tunnel{SenderAddr: bind}
}

// startMockRelay starts a server.Server backed by httptest.NewTLSServer
// (aztunnel only dials TLS-protected relays) and returns the host:port
// plus a ClientOptions whose TLSConfig skips verification of the test
// certificate. Mirrors the helper in mockrelay/server/integration_test.go
// so behaviour is identical.
func startMockRelay(t *testing.T) (host string, opts relay.ClientOptions, srv *httptest.Server) {
	t.Helper()
	rs, err := server.NewServer(server.Config{
		Logger:            slog.New(slog.NewTextHandler(io.Discard, nil)),
		RendezvousTimeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatalf("new mock relay: %v", err)
	}
	srv = httptest.NewTLSServer(rs.Handler())
	u, _ := url.Parse(srv.URL)
	host = u.Host
	opts = relay.ClientOptions{
		TLSConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // test cert
	}
	t.Cleanup(srv.Close)
	return host, opts, srv
}

// pickFreePort returns a localhost host:port that was free at the
// moment of the call. There is an unavoidable TOCTOU window between
// this Listen+Close pair and the sender's own net.Listen on the same
// addr: another process can grab the port in between. If it loses
// the race, the sender returns immediately with a bind error (the
// senders do not retry), the sender goroutine exits, and Setup's
// downstream waitForTCP times out after 3 s with a "never became
// reachable" failure. In practice the window is microseconds-long
// on a quiet test host, so this has not flaked, but the failure
// mode is loud rather than retried.
func pickFreePort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("pick port: %v", err)
	}
	addr := ln.Addr().String()
	ln.Close() //nolint:errcheck // best-effort cleanup
	return addr
}

// waitForTCP polls until a TCP connection to addr succeeds or the
// timeout elapses. Used to make Backend.Setup block until the sender
// is actually accepting.
//
// TODO(slice-2B): in ModePortForward, every accepted TCP connection
// triggers a forward attempt through the relay. The probe socket
// immediately closes, so the bridge tears down — but it does briefly
// occupy a slot. Once MaxConnections=N scenarios land, the probe will
// consume the only available slot and the scenario's real dial will
// be rejected on the mock backend (azureBackend uses log-gated
// readiness and is unaffected). Replace this probe with a non-
// forwarding readiness signal (custom logger handler that captures
// the "port-forward listening" log line, or a sender-side ready
// channel) before MaxConnections scenarios merge.
func waitForTCP(addr string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		c, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err == nil {
			c.Close() //nolint:errcheck // best-effort cleanup
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}

// waitForControl polls the mock relay until at least one listener has
// registered for the given entity. The sb-hc-action=connect probe
// without an Upgrade header returns 404 when no listener is attached
// and 400-ish once one is, so we watch for the transition away from
// 404. Copy of the helper in mockrelay/server/integration_test.go.
func waitForControl(t *testing.T, srv *httptest.Server, entity string, timeout time.Duration) {
	t.Helper()
	// Build our own client over the test server's TLS transport
	// rather than mutating srv.Client(), which is shared and could
	// surprise other code paths that reach for the default timeout.
	httpClient := &http.Client{
		Transport: srv.Client().Transport,
		Timeout:   1 * time.Second,
	}
	host := strings.TrimPrefix(srv.URL, "https://")
	tok, err := relay.GenerateSASToken(
		relay.ResourceURI(host, entity),
		server.DefaultSASKeyName,
		server.DefaultSASKey,
		1*time.Minute,
	)
	if err != nil {
		t.Fatalf("mint probe token: %v", err)
	}
	probeURL := srv.URL + "/$hc/" + entity + "?sb-hc-action=connect&sb-hc-token=" + url.QueryEscape(tok)
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		resp, err := httpClient.Get(probeURL)
		if err == nil {
			status := resp.StatusCode
			resp.Body.Close() //nolint:errcheck // best-effort cleanup
			if status != http.StatusNotFound {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for listener on %s/%s", host, entity)
}

// mustEntityName returns a short random suffix appended to the
// caller's test name. Keeps entities unique across scenarios.
func mustEntityName(t *testing.T) string {
	t.Helper()
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatalf("rand: %v", err)
	}
	// Sanitize the test name into something safe for a URL path.
	safe := strings.NewReplacer("/", "-", " ", "-", "#", "-").Replace(t.Name())
	return safe + "-" + hex.EncodeToString(b[:])
}
