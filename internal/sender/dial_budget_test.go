package sender

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/philsphicas/aztunnel/internal/protocol"
	"github.com/philsphicas/aztunnel/internal/relay"
)

func TestDialBudget_DefaultsWhenZeroOrNegative(t *testing.T) {
	if got := dialBudget(0); got != defaultDialBudget {
		t.Errorf("dialBudget(0) = %v, want %v", got, defaultDialBudget)
	}
	if got := dialBudget(-time.Second); got != defaultDialBudget {
		t.Errorf("dialBudget(-1s) = %v, want %v (negative must not produce already-expired ctx)", got, defaultDialBudget)
	}
	if got := dialBudget(5 * time.Second); got != 5*time.Second {
		t.Errorf("dialBudget(5s) = %v, want 5s", got)
	}
}

type budgetTokenProvider struct{}

func (budgetTokenProvider) GetToken(context.Context, string) (string, error) {
	return "test-token", nil
}

// tcpPairForBudget mirrors test helpers in the package without taking a
// dependency on the prior PR's test scaffolding.
func tcpPairForBudget(t *testing.T) (local, peer net.Conn) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	accepted := make(chan net.Conn, 1)
	acceptErr := make(chan error, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			acceptErr <- err
			return
		}
		accepted <- c
	}()

	peer, err = net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	select {
	case err := <-acceptErr:
		_ = peer.Close()
		t.Fatalf("accept: %v", err)
	case local = <-accepted:
	}
	return local, peer
}

// TestForwardConnection_DialBudgetBoundsRetry confirms the
// per-connection dial+retry duration is capped by cfg.DialBudget when
// the relay never accepts. Before the fix, the retry loop ran until
// the *parent* (process-lifetime) ctx was cancelled, producing the
// ghost-rendezvous behaviour described in issue #94.
func TestForwardConnection_DialBudgetBoundsRetry(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("url.Parse: %v", err)
	}

	local, peer := tcpPairForBudget(t)
	defer local.Close()
	defer peer.Close()

	cfg := PortForwardConfig{
		Endpoint:      u.Host,
		EntityPath:    "test-hc",
		TokenProvider: budgetTokenProvider{},
		ClientOptions: relay.ClientOptions{
			TLSConfig: srv.Client().Transport.(*http.Transport).TLSClientConfig,
		},
		Target:     "example.internal:443",
		Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		DialBudget: 250 * time.Millisecond,
	}

	// Parent context is intentionally long-lived to model the
	// process-lifetime ctx; the per-connection budget must do the
	// bounding.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errCh := make(chan error, 1)
	start := time.Now()
	go func() {
		errCh <- forwardConnection(ctx, local, cfg.Target, cfg)
	}()

	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("forwardConnection returned nil; want budget-bounded error")
		}
		// The exact error wording comes from DialWithRetry; what
		// matters is that we did not run beyond the budget waiting
		// for retries.
		if elapsed := time.Since(start); elapsed > 2*time.Second {
			t.Fatalf("forwardConnection returned after %v; budget=%v should have aborted much sooner", elapsed, cfg.DialBudget)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("forwardConnection did not return within 3s; dial budget is not being honoured")
	}
}

// TestForwardConnection_HappyPathSurvivesDialBudget guards against
// the regression that sank PR #103: if the dial succeeds, the bridge
// must continue to use the parent context, not a context that was
// cancelled to bound the dial phase. Without this test, an
// implementation that accidentally passes dialCtx (rather than the
// outer ctx) to TrackedBridge would tear down every successful
// connection with cause=user_cancel before transferring any bytes.
//
// The test stands up a minimal relay-side mock that completes the
// envelope handshake and then bridges bytes back, then asserts a
// full round-trip after the dial+envelope phase that the budget
// covered.
func TestForwardConnection_HappyPathSurvivesDialBudget(t *testing.T) {
	const payload = "hello bridge\n"

	var connOpened atomic.Int32
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Errorf("server: websocket.Accept: %v", err)
			return
		}
		connOpened.Add(1)
		defer ws.CloseNow()

		_, env, err := ws.Read(r.Context())
		if err != nil {
			t.Errorf("server: read envelope: %v", err)
			return
		}
		var ce protocol.ConnectEnvelope
		if err := json.Unmarshal(env, &ce); err != nil {
			t.Errorf("server: unmarshal envelope: %v", err)
			return
		}
		resp, _ := json.Marshal(protocol.ConnectResponse{
			Version:    protocol.CurrentVersion,
			OK:         true,
			ListenerID: "TESTLISTENERID01",
		})
		if err := ws.Write(r.Context(), websocket.MessageText, resp); err != nil {
			t.Errorf("server: write response: %v", err)
			return
		}

		// Echo the first payload back as a single binary frame so
		// the client side can verify bytes actually flowed after
		// dial budget expiry.
		_, msg, err := ws.Read(r.Context())
		if err != nil {
			t.Errorf("server: read payload: %v", err)
			return
		}
		if err := ws.Write(r.Context(), websocket.MessageBinary, msg); err != nil {
			t.Errorf("server: echo: %v", err)
			return
		}
		// Hold the connection open until the client closes it so
		// the bridge tears down naturally.
		_, _, _ = ws.Read(r.Context())
	}))
	srv.StartTLS()
	defer srv.Close()

	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("url.Parse: %v", err)
	}

	local, peer := tcpPairForBudget(t)
	defer local.Close()
	defer peer.Close()

	cfg := PortForwardConfig{
		Endpoint:      u.Host,
		EntityPath:    "happy-path",
		TokenProvider: budgetTokenProvider{},
		ClientOptions: relay.ClientOptions{
			TLSConfig: srv.Client().Transport.(*http.Transport).TLSClientConfig,
		},
		Target:     "example.internal:443",
		Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		DialBudget: 5 * time.Second,
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- forwardConnection(ctx, local, cfg.Target, cfg)
	}()

	// Drive the bridge: write a payload from the app side, read
	// the echo back. If the dial-budget cancellation leaked into
	// the bridge ctx (the PR #103 regression), the bridge would
	// fail with cause=user_cancel before any byte made it through.
	if err := peer.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatalf("peer.SetDeadline: %v", err)
	}
	if _, err := peer.Write([]byte(payload)); err != nil {
		t.Fatalf("peer.Write: %v", err)
	}
	got := make([]byte, len(payload))
	if _, err := io.ReadFull(peer, got); err != nil {
		t.Fatalf("peer.Read echo (bridge ctx may have been cancelled by dial budget): %v", err)
	}
	if string(got) != payload {
		t.Errorf("echo mismatch: got %q want %q", got, payload)
	}

	if n := connOpened.Load(); n != 1 {
		t.Errorf("server saw %d WS opens, want 1", n)
	}

	// Close from the app side; expect a clean teardown.
	if err := peer.Close(); err != nil {
		t.Fatalf("peer.Close: %v", err)
	}
	select {
	case err := <-errCh:
		// Bridge end after a clean app-side close may surface as
		// nil or a benign network error depending on which side
		// races to EOF first; what we are asserting is the absence
		// of context-cancellation regression, which is implied by
		// having read the echo above.
		_ = err
	case <-time.After(5 * time.Second):
		t.Fatal("forwardConnection did not return after peer.Close()")
	}
}

// TestForwardConnection_NegativeBudgetUsesDefault guards the
// dialBudget(<=0) fallback at the call-site level: a config with a
// negative DialBudget must not produce an already-expired dialCtx
// that makes the very first dial fail spuriously. We exercise this
// with the same 404 server as TestForwardConnection_DialBudgetBoundsRetry
// but a negative budget; the resulting elapsed time must be on the
// order of defaultDialBudget, not zero.
func TestForwardConnection_NegativeBudgetUsesDefault(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping ~retry-attempt sized test in -short mode")
	}

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("url.Parse: %v", err)
	}

	local, peer := tcpPairForBudget(t)
	defer local.Close()
	defer peer.Close()

	cfg := PortForwardConfig{
		Endpoint:      u.Host,
		EntityPath:    "neg-budget",
		TokenProvider: budgetTokenProvider{},
		ClientOptions: relay.ClientOptions{
			TLSConfig: srv.Client().Transport.(*http.Transport).TLSClientConfig,
		},
		Target:     "example.internal:443",
		Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		DialBudget: -1 * time.Second,
	}

	// Use a short parent ctx to bound the test even though the
	// negative-budget code path falls back to defaultDialBudget
	// (which is much longer than this test's parent timeout).
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()

	errCh := make(chan error, 1)
	start := time.Now()
	go func() {
		errCh <- forwardConnection(ctx, local, cfg.Target, cfg)
	}()

	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("forwardConnection returned nil; want parent ctx cancellation")
		}
		// We expect the parent ctx to be the bound (since
		// defaultDialBudget > parent timeout). What matters is
		// the negative budget did NOT short-circuit dialing — the
		// elapsed time should be close to the parent timeout, not
		// near zero.
		if elapsed := time.Since(start); elapsed < 500*time.Millisecond {
			t.Fatalf("forwardConnection returned after %v; negative DialBudget appears to have produced an already-expired dialCtx (want fallback to defaultDialBudget so the parent ctx bounds the test)", elapsed)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("forwardConnection did not return within 3s")
	}
}
