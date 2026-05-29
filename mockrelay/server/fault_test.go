package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// silentServer constructs a Server whose log output is dropped.
// Fault-injection tests deliberately provoke server-side warnings
// that would otherwise pollute `go test -v` output.
func silentServer(t *testing.T, opts ...Option) *Server {
	t.Helper()
	cfg := Config{
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		SkipAuth: true,
	}
	s, err := NewServerForTesting(cfg, opts...)
	if err != nil {
		t.Fatalf("NewServerForTesting: %v", err)
	}
	return s
}

// newTestPending returns a fully-initialized pendingRendezvous. The
// fault-injection tests register one of these directly on the hub so
// they can drive handleAccept without standing up a real sender.
func newTestPending() *pendingRendezvous {
	return &pendingRendezvous{
		ready:      make(chan struct{}),
		paired:     make(chan struct{}),
		bridgeDone: make(chan struct{}),
		senderTook: make(chan struct{}),
	}
}

// dialAccept dials the accept-side rendezvous endpoint for an entity
// and id that the caller has already registered on the hub.
func dialAccept(t *testing.T, ctx context.Context, srvURL, entity, id string) *websocket.Conn {
	t.Helper()
	wsURL := strings.Replace(srvURL, "http://", "ws://", 1) + "/$hc/" + entity + "?sb-hc-action=accept&id=" + id
	ws, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("dial accept: %v", err)
	}
	return ws
}

// TestFaultInjection_CloseControlOnRenew verifies that
// WithCloseControlOnRenew closes the listener control channel on the
// next inbound renewToken message.
func TestFaultInjection_CloseControlOnRenew(t *testing.T) {
	s := silentServer(t, WithCloseControlOnRenew())
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	ws, _ := dialListener(t, ctx, srv.URL, "renew-entity")
	defer ws.CloseNow() //nolint:errcheck // best-effort cleanup

	renew, err := json.Marshal(map[string]any{
		"renewToken": map[string]string{"token": "irrelevant-for-mock"},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := ws.Write(ctx, websocket.MessageText, renew); err != nil {
		t.Fatalf("write renew: %v", err)
	}

	readCtx, readCancel := context.WithTimeout(ctx, 2*time.Second)
	defer readCancel()
	_, _, err = ws.Read(readCtx)
	if err == nil {
		t.Fatalf("read after renew returned nil; want close error")
	}
	var ce websocket.CloseError
	if !errors.As(err, &ce) {
		t.Fatalf("read error not a websocket.CloseError: %v", err)
	}
	if ce.Code != websocket.StatusGoingAway {
		t.Errorf("close code = %d, want %d (StatusGoingAway)", ce.Code, websocket.StatusGoingAway)
	}
}

// TestFaultInjection_CloseCodeOnAccept_1011 verifies that the accept-
// side WS emits the 1011 (server error) close code when the knob is
// armed with that value.
func TestFaultInjection_CloseCodeOnAccept_1011(t *testing.T) {
	assertCloseCodeOnAccept(t, 1011)
}

// TestFaultInjection_CloseCodeOnAccept_4400 verifies a 4xxx app-
// defined close code, exercising the same path with a different
// numeric value to catch a hard-coded constant.
func TestFaultInjection_CloseCodeOnAccept_4400(t *testing.T) {
	assertCloseCodeOnAccept(t, 4400)
}

func assertCloseCodeOnAccept(t *testing.T, code int) {
	t.Helper()
	s := silentServer(t, WithCloseCodeOnAccept(code))
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	const (
		entity = "accept-entity"
		id     = "deadbeefcafef00ddeadbeefcafef00d"
	)
	pending := newTestPending()
	s.hub.addPending(entity, id, pending)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ws := dialAccept(t, ctx, srv.URL, entity, id)
	defer ws.CloseNow() //nolint:errcheck // best-effort cleanup

	readCtx, readCancel := context.WithTimeout(ctx, 2*time.Second)
	defer readCancel()
	_, _, err := ws.Read(readCtx)
	if err == nil {
		t.Fatalf("read on accept WS returned nil; want close error")
	}
	var ce websocket.CloseError
	if !errors.As(err, &ce) {
		t.Fatalf("read error not a websocket.CloseError: %v", err)
	}
	if int(ce.Code) != code {
		t.Errorf("close code = %d, want %d", ce.Code, code)
	}

	// The fault aborts the pending entry; ready must be closed.
	select {
	case <-pending.ready:
	default:
		t.Errorf("pending.ready not closed after accept fault")
	}
	if pending.listenerWS != nil {
		t.Errorf("pending.listenerWS = %v, want nil (abort path)", pending.listenerWS)
	}
}

// TestFaultInjection_CloseCodeOnAccept_Rejected verifies that
// WithCloseCodeOnAccept rejects every close code coder/websocket
// would refuse to put on the wire: the four explicitly reserved
// codes (1004/1005/1006/1015) and the protocol-reserved range
// 1016-2999. A test that wants the 1006 "abnormal closure" path on
// the client must close the TCP socket abruptly instead.
func TestFaultInjection_CloseCodeOnAccept_Rejected(t *testing.T) {
	cfg := Config{
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		SkipAuth: true,
	}
	// Reserved by RFC 6455 §7.4.1 / coder/websocket validator.
	for _, code := range []int{1004, 1005, 1006, 1015} {
		_, err := NewServerForTesting(cfg, WithCloseCodeOnAccept(code))
		if err == nil {
			t.Errorf("WithCloseCodeOnAccept(%d) returned nil error; want rejection", code)
			continue
		}
		if !strings.Contains(err.Error(), "WithCloseCodeOnAccept") {
			t.Errorf("code %d: error %q does not mention the option name", code, err.Error())
		}
	}

	// Protocol-reserved range (1016-2999) and codes outside the WS
	// close-code space are also rejected — these would otherwise
	// silently fail at Close-frame send time inside coder/websocket.
	for _, code := range []int{0, 1, 999, 1016, 1500, 2999, 5000, 10000} {
		_, err := NewServerForTesting(cfg, WithCloseCodeOnAccept(code))
		if err == nil {
			t.Errorf("WithCloseCodeOnAccept(%d) returned nil error; want rejection", code)
		}
	}

	// Sanity: representative valid codes accept.
	for _, code := range []int{1000, 1011, 1014, 3000, 4400, 4999} {
		_, err := NewServerForTesting(cfg, WithCloseCodeOnAccept(code))
		if err != nil {
			t.Errorf("WithCloseCodeOnAccept(%d) returned error: %v", code, err)
		}
	}
}

// TestFaultInjection_RejectControlDial verifies that the next inbound
// listener control dial after the knob is armed gets HTTP 503,
// pre-upgrade.
func TestFaultInjection_RejectControlDial(t *testing.T) {
	s := silentServer(t, WithRejectControlDial())
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	// Use http.Get rather than websocket.Dial so we observe the raw
	// 503 instead of the WS library's "expected status 101" wrapping.
	resp, err := http.Get(srv.URL + "/$hc/reject-entity?sb-hc-action=listen")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close() //nolint:errcheck // best-effort cleanup
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want %d (ServiceUnavailable)", resp.StatusCode, http.StatusServiceUnavailable)
	}

	// Single-shot: the next dial must succeed (i.e. attempt the WS
	// upgrade) instead of returning 503 again. Without the
	// Upgrade/Connection headers, the WS library returns 426 / 400 /
	// 200 with an error body — anything other than 503 confirms the
	// fault disarmed.
	resp2, err := http.Get(srv.URL + "/$hc/reject-entity?sb-hc-action=listen")
	if err != nil {
		t.Fatalf("second GET: %v", err)
	}
	defer resp2.Body.Close() //nolint:errcheck // best-effort cleanup
	if resp2.StatusCode == http.StatusServiceUnavailable {
		t.Errorf("second dial also got 503; fault should have disarmed after first hit")
	}
}

// TestFaultInjection_OptionsDontLeakToProduction verifies that the
// production constructor (mockrelay/server.NewServer) leaves the
// fault knobs at their zero values — nothing armed.
func TestFaultInjection_OptionsDontLeakToProduction(t *testing.T) {
	s, err := NewServer(Config{
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		SkipAuth: true,
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	if got := s.faults.closeCodeOnAccept.Load(); got != 0 {
		t.Errorf("closeCodeOnAccept = %d, want 0", got)
	}
	if s.faults.closeControlOnRenew.Load() {
		t.Errorf("closeControlOnRenew = true, want false")
	}
	if s.faults.rejectControlDial.Load() {
		t.Errorf("rejectControlDial = true, want false")
	}
	if s.delayProfile != (DelayProfile{}) {
		t.Errorf("delayProfile = %+v, want zero DelayProfile", s.delayProfile)
	}

	// Behavioural sanity: a normal control dial succeeds.
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ws, _ := dialListener(t, ctx, srv.URL, "no-fault-entity")
	defer ws.CloseNow() //nolint:errcheck // best-effort cleanup
	// If WithRejectControlDial had leaked, the dial above would have
	// failed with HTTP 503 instead of upgrading to a WebSocket.
}

// TestFaultInjection_NewServerForTesting_NoOpts verifies that calling
// the testing constructor without options is equivalent to the
// production constructor: no faults armed, identical behaviour.
func TestFaultInjection_NewServerForTesting_NoOpts(t *testing.T) {
	s, err := NewServerForTesting(Config{
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		SkipAuth: true,
	})
	if err != nil {
		t.Fatalf("NewServerForTesting (no opts): %v", err)
	}
	if got := s.faults.closeCodeOnAccept.Load(); got != 0 {
		t.Errorf("closeCodeOnAccept = %d, want 0", got)
	}
	if s.faults.closeControlOnRenew.Load() {
		t.Errorf("closeControlOnRenew = true, want false")
	}
	if s.faults.rejectControlDial.Load() {
		t.Errorf("rejectControlDial = true, want false")
	}
	if s.delayProfile != (DelayProfile{}) {
		t.Errorf("delayProfile = %+v, want zero DelayProfile", s.delayProfile)
	}
}

// TestFaultInjection_NewServerForTesting_NilOption verifies that a
// nil Option returns a clear error rather than panicking.
func TestFaultInjection_NewServerForTesting_NilOption(t *testing.T) {
	_, err := NewServerForTesting(Config{SkipAuth: true}, nil)
	if err == nil {
		t.Fatal("NewServerForTesting(nil Option) returned nil error; want rejection")
	}
}
