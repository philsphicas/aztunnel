package mockbackend

import (
	"context"
	"crypto/tls"
	"encoding/json"
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

	"github.com/coder/websocket"

	"github.com/philsphicas/aztunnel/internal/listener"
	"github.com/philsphicas/aztunnel/internal/metrics"
	"github.com/philsphicas/aztunnel/internal/relay"
	"github.com/philsphicas/aztunnel/mockrelay/server"
)

// startFaultyMockRelay stands up an in-process mock relay with the
// supplied fault-injection options. Returns the host:port the
// listener should dial, plus the ClientOptions wired with
// InsecureSkipVerify so the listener accepts httptest's self-signed
// certificate — the only TLS path that matters in this test is the
// fault-injection one. The server is registered for cleanup on t.
func startFaultyMockRelay(t *testing.T, opts ...server.Option) (string, relay.ClientOptions) {
	t.Helper()
	rs, err := server.NewServerForTesting(server.Config{
		Logger:            slog.New(slog.NewTextHandler(io.Discard, nil)),
		RendezvousTimeout: 1 * time.Second,
	}, opts...)
	if err != nil {
		t.Fatalf("new mock relay: %v", err)
	}
	srv := httptest.NewTLSServer(rs.Handler())
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	return u.Host, relay.ClientOptions{
		TLSConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // test cert
	}
}

// startFaultyListener brings up an in-process aztunnel listener wired
// to host with the supplied RenewInterval (zero selects the package
// default). Returns the captured-log accessor and a Stop closure.
// The listener goroutine is joined by t.Cleanup so a panicking test
// doesn't leak it across tests.
func startFaultyListener(t *testing.T, host string, opts relay.ClientOptions, renewInterval time.Duration) (func() string, func()) {
	t.Helper()
	logs := newCaptureBuffer()
	cfg := listener.Config{
		Endpoint:      host,
		EntityPath:    mustEntityName(t),
		TokenProvider: &relay.SASTokenProvider{KeyName: server.DefaultSASKeyName, Key: server.DefaultSASKey},
		ClientOptions: opts,
		Logger:        slog.New(slog.NewTextHandler(logs, nil)),
		Metrics:       metrics.New(),
		RenewInterval: renewInterval,
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = listener.ListenAndServe(ctx, cfg)
	}()
	var stopOnce sync.Once
	stop := func() {
		stopOnce.Do(func() {
			cancel()
			<-done
		})
	}
	t.Cleanup(stop)
	return logs.String, stop
}

// waitForLogContaining polls logs() at 20ms until every needle is
// present in the captured output OR timeout elapses. Returns the
// snapshot at success; fails the test at deadline.
func waitForLogContaining(t *testing.T, logs func() string, timeout time.Duration, needles ...string) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		s := logs()
		hit := true
		for _, n := range needles {
			if !strings.Contains(s, n) {
				hit = false
				break
			}
		}
		if hit {
			return s
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("did not observe all needles %v within %s; captured:\n%s",
		needles, timeout, logs())
	return ""
}

// TestControl_Events_ConnectionLostOnRenew arms the mock relay to
// send a polite close on the listener's control WebSocket as soon as
// it sees the listener's first renewToken frame. The listener's
// write to the local TCP send buffer succeeds (so renew_ok is
// emitted), and the read loop then observes the close — emitting
// control_ended{reason=read_failed}. This pair is the
// operator-visible signature for "the control channel was dropped
// during a renew round-trip".
//
// The complementary code path where ws.Write itself fails (yielding
// renew_failed{code=connection_lost}) is exercised in
// internal/relay/control_test.go TestRenewOnce/write_failure_*.
func TestControl_Events_ConnectionLostOnRenew(t *testing.T) {
	host, copts := startFaultyMockRelay(t, server.WithCloseControlOnRenew())
	logs, _ := startFaultyListener(t, host, copts, 100*time.Millisecond)

	waitForLogContaining(t, logs, 10*time.Second,
		`msg=`+relay.EventRenewOK)
	waitForLogContaining(t, logs, 10*time.Second,
		`msg=`+relay.EventControlEnded,
		`reason=`+relay.ControlEndedReadFailed)
}

// TestControl_Events_RejectControlDial arms the mock relay to refuse
// the listener's control dial with HTTP 503. control_started is
// emitted only after a successful dial, so a refused control-channel
// dial leaves control_ended{reason=dial_failed} as the single
// lifecycle event for the failed attempt — that's the
// operator-visible signal the dial never reached the relay.
func TestControl_Events_RejectControlDial(t *testing.T) {
	host, copts := startFaultyMockRelay(t, server.WithRejectControlDial())
	logs, _ := startFaultyListener(t, host, copts, 0)

	waitForLogContaining(t, logs, 10*time.Second,
		`msg=`+relay.EventControlEnded,
		`reason=`+relay.ControlEndedDialFailed)
}

// TestControl_Events_AcceptDropped_DialFailed drives an
// accept_dropped{reason=dial_failed} log line by standing up a
// custom TLS test server that acts as the relay control channel,
// emits an accept frame whose `address` field points at a closed
// TCP port, then waits for the listener to log the drop event.
//
// The mockrelay fault knobs do not include a way to corrupt the
// accept frame address, so this test bypasses mockrelay entirely
// and exercises the relay-package read loop → handleAccept dial
// path against a hand-rolled control-channel server. The unit test
// TestHandleAccept/emits_accept_dropped_on_dial_failure covers the
// same code path in isolation; this test confirms the full
// listener.ListenAndServe wiring emits the event on the integrated
// path.
func TestControl_Events_AcceptDropped_DialFailed(t *testing.T) {
	refused := "ws://" + refusedAddr(t)
	host, copts := startCustomControlServer(t, refused)
	logs, _ := startFaultyListener(t, host, copts, 0)

	waitForLogContaining(t, logs, 10*time.Second,
		`msg=`+relay.EventAcceptAttempted)
	waitForLogContaining(t, logs, 10*time.Second,
		`msg=`+relay.EventAcceptDropped,
		`reason=`+relay.AcceptDroppedDialFailed)
}

// refusedAddr binds a TCP listener to a free port, closes it, and
// returns "127.0.0.1:N". A connect attempt to that address gets
// ECONNREFUSED.
func refusedAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	addr := l.Addr().String()
	if err := l.Close(); err != nil {
		t.Fatalf("close listener: %v", err)
	}
	return addr
}

// startCustomControlServer stands up a TLS HTTP server that
// accepts the listener's control-channel WS upgrade and sends a
// single accept frame whose `address` field points at acceptAddr.
// It returns the host:port to use as Endpoint and the
// ClientOptions wired to the test cert. The server is registered
// for cleanup on t.
func startCustomControlServer(t *testing.T, acceptAddr string) (string, relay.ClientOptions) {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer ws.CloseNow() //nolint:errcheck // best-effort cleanup
		payload, _ := json.Marshal(map[string]any{
			"accept": map[string]any{
				"address": acceptAddr,
				"id":      "test-id",
			},
		})
		if err := ws.Write(r.Context(), websocket.MessageText, payload); err != nil {
			return
		}
		// Hold the control channel open until the listener
		// cancels its context — this keeps the test focused on
		// the accept_dropped emission, not the loop teardown.
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	return u.Host, relay.ClientOptions{
		TLSConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // test cert
	}
}
