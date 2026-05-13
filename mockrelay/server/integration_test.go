package server_test

import (
	"bytes"
	"context"
	"crypto/tls"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/philsphicas/aztunnel/internal/listener"
	"github.com/philsphicas/aztunnel/internal/relay"
	"github.com/philsphicas/aztunnel/internal/sender"
	"github.com/philsphicas/aztunnel/mockrelay/server"
)

// startEchoServer starts a tiny TCP echo server on localhost:0 and
// returns its address. The server is stopped when ctx is cancelled.
func startEchoServer(t *testing.T, ctx context.Context) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() {
		<-ctx.Done()
		ln.Close()
	}()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				_, _ = io.Copy(c, c)
			}(conn)
		}
	}()
	return ln.Addr().String()
}

// pickFreePort returns a localhost port that was free at the moment of
// the call. There is an unavoidable TOCTOU window but it's adequate for
// tests.
func pickFreePort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("pick port: %v", err)
	}
	addr := ln.Addr().String()
	ln.Close()
	return addr
}

// startMockRelay starts a server.Server backed by httptest.NewTLSServer
// (aztunnel only dials TLS-protected relays). The returned ClientOptions
// includes a TLSConfig with InsecureSkipVerify so the in-process
// listener and sender accept the test cert. The returned host is suitable
// to pass to relay.ClientOptions / the --relay flag.
func startMockRelay(t *testing.T) (host string, opts relay.ClientOptions, srv *httptest.Server) {
	t.Helper()
	rs, err := server.NewServer(server.Config{
		Logger:            slog.New(slog.NewTextHandler(io.Discard, nil)),
		RendezvousTimeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatalf("new server: %v", err)
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

// mintProbeToken builds a short-lived SAS token using the mock's
// default credentials so probe requests get past validateSAS. Returns
// the bare token string (caller URL-encodes for the query param).
func mintProbeToken(t *testing.T, host, entity string) string {
	t.Helper()
	tok, err := relay.GenerateSASToken(
		relay.ResourceURI(host, entity),
		server.DefaultSASKeyName,
		server.DefaultSASKey,
		1*time.Minute,
	)
	if err != nil {
		t.Fatalf("mint probe token: %v", err)
	}
	return tok
}

// waitForControl polls until at least one listener has registered for
// the given entity, or the timeout elapses. The listener's control loop
// runs asynchronously after ListenAndServe is called.
func waitForControl(t *testing.T, srv *httptest.Server, entity string, timeout time.Duration) {
	t.Helper()
	httpClient := srv.Client()
	httpClient.Timeout = 1 * time.Second
	host := strings.TrimPrefix(srv.URL, "https://")
	// We probe with sb-hc-action=connect, which returns 404 when no
	// listener is registered (and would upgrade to WS otherwise — but
	// without the Upgrade header it returns 400 instead, signaling the
	// listener IS present). We watch for the transition from 404 to
	// 400/500-ish.
	probeURL := srv.URL + "/$hc/" + entity + "?sb-hc-action=connect&sb-hc-token=" + url.QueryEscape(mintProbeToken(t, host, entity))
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		resp, err := httpClient.Get(probeURL)
		if err == nil {
			status := resp.StatusCode
			resp.Body.Close()
			if status != http.StatusNotFound {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for listener on %s/%s", host, entity)
}

// runPortForward runs sender.PortForward in a background goroutine and
// returns the address the local port-forward listener bound to.
func runPortForward(t *testing.T, ctx context.Context, host, entity, target string, opts relay.ClientOptions) string {
	t.Helper()
	bind := pickFreePort(t)
	doneCh := make(chan struct{})
	go func() {
		defer close(doneCh)
		err := sender.PortForward(ctx, sender.PortForwardConfig{
			Endpoint:      host,
			EntityPath:    entity,
			TokenProvider: &relay.SASTokenProvider{KeyName: server.DefaultSASKeyName, Key: server.DefaultSASKey},
			ClientOptions: opts,
			Target:        target,
			BindAddress:   bind,
			Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		})
		if err != nil && ctx.Err() == nil {
			t.Logf("port-forward exited: %v", err)
		}
	}()
	// Wait for the port-forward listener to be ready.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		c, err := net.Dial("tcp", bind)
		if err == nil {
			c.Close()
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Cleanup(func() {
		<-doneCh
	})
	return bind
}

// runListener starts listener.ListenAndServe in a goroutine. The
// listener is stopped when ctx is cancelled.
func runListener(t *testing.T, ctx context.Context, host, entity string, opts relay.ClientOptions) {
	t.Helper()
	doneCh := make(chan struct{})
	go func() {
		defer close(doneCh)
		err := listener.ListenAndServe(ctx, listener.Config{
			Endpoint:      host,
			EntityPath:    entity,
			TokenProvider: &relay.SASTokenProvider{KeyName: server.DefaultSASKeyName, Key: server.DefaultSASKey},
			ClientOptions: opts,
			Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		})
		if err != nil && ctx.Err() == nil {
			t.Logf("listener exited: %v", err)
		}
	}()
	t.Cleanup(func() {
		<-doneCh
	})
}

// TestIntegration_PortForwardEcho is the headline end-to-end test: a
// real aztunnel listener and sender wire through the mock relay and a
// TCP echo round-trip succeeds.
func TestIntegration_PortForwardEcho(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	echoAddr := startEchoServer(t, ctx)
	host, opts, srv := startMockRelay(t)
	entity := "test-entity"

	runListener(t, ctx, host, entity, opts)
	waitForControl(t, srv, entity, 3*time.Second)

	bind := runPortForward(t, ctx, host, entity, echoAddr, opts)

	conn, err := net.Dial("tcp", bind)
	if err != nil {
		t.Fatalf("dial port-forward: %v", err)
	}
	defer conn.Close()

	want := []byte("hello, mock relay!\n")
	if _, err := conn.Write(want); err != nil {
		t.Fatalf("write: %v", err)
	}
	got := make([]byte, len(want))
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.ReadFull(conn, got); err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("echo mismatch:\n got=%q\nwant=%q", got, want)
	}
}

// TestIntegration_NoListener_Returns404 verifies that when no listener
// is registered, the server returns 404 to the sender pre-upgrade —
// this is the contract DialWithRetry depends on for backoff.
func TestIntegration_NoListener_Returns404(t *testing.T) {
	host, _, srv := startMockRelay(t)
	tok := mintProbeToken(t, host, "nobody")
	resp, err := srv.Client().Get(srv.URL + "/$hc/nobody?sb-hc-action=connect&sb-hc-token=" + url.QueryEscape(tok))
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusNotFound)
	}
}

// TestIntegration_MultipleMessages verifies the bridge preserves
// message boundaries across many round-trips — this is the property
// the ConnectEnvelope read depends on.
func TestIntegration_MultipleMessages(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	echoAddr := startEchoServer(t, ctx)
	host, opts, srv := startMockRelay(t)
	entity := "msg-bound"

	runListener(t, ctx, host, entity, opts)
	waitForControl(t, srv, entity, 3*time.Second)
	bind := runPortForward(t, ctx, host, entity, echoAddr, opts)

	conn, err := net.Dial("tcp", bind)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	// Write 10 small messages with intentional small delays so the
	// TCP stack flushes each as its own write, exercising the bridge
	// repeatedly.
	for i := 0; i < 10; i++ {
		msg := []byte("packet-" + strings.Repeat("x", i+1) + "\n")
		if _, err := conn.Write(msg); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
		buf := make([]byte, len(msg))
		conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		if _, err := io.ReadFull(conn, buf); err != nil {
			t.Fatalf("read %d: %v", i, err)
		}
		if !bytes.Equal(buf, msg) {
			t.Fatalf("msg %d mismatch:\n got=%q\nwant=%q", i, buf, msg)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
