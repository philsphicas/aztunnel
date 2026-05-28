package relay

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/philsphicas/aztunnel/internal/bridgecause"
)

func TestBridge(t *testing.T) {
	// Create a WebSocket server that echoes all received binary data.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer ws.CloseNow()

		for {
			typ, data, err := ws.Read(r.Context())
			if err != nil {
				return
			}
			if err := ws.Write(r.Context(), typ, data); err != nil {
				return
			}
		}
	}))
	defer srv.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	ws, _, err := websocket.Dial(context.Background(), wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer ws.CloseNow()

	// Create a pipe to simulate TCP.
	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()
	defer serverConn.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Run bridge in background.
	bridgeErr := make(chan error, 1)
	go func() {
		_, err := Bridge(ctx, ws, serverConn)
		bridgeErr <- err
	}()

	// Write data through the "TCP" side, read it back (echoed by WS server).
	msg := []byte("hello bridge")
	if _, err := clientConn.Write(msg); err != nil {
		t.Fatalf("write: %v", err)
	}

	buf := make([]byte, 64)
	clientConn.SetReadDeadline(time.Now().Add(2 * time.Second))
	n, err := clientConn.Read(buf)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(buf[:n]) != "hello bridge" {
		t.Errorf("got %q, want %q", string(buf[:n]), "hello bridge")
	}

	// Close client side to end bridge.
	clientConn.Close()
	select {
	case err := <-bridgeErr:
		if err != nil {
			t.Logf("bridge ended with: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("bridge did not terminate")
	}
}

func TestBridge_ByteCounts(t *testing.T) {
	// WebSocket server that echoes data back.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer ws.CloseNow()

		for {
			typ, data, err := ws.Read(r.Context())
			if err != nil {
				return
			}
			if err := ws.Write(r.Context(), typ, data); err != nil {
				return
			}
		}
	}))
	defer srv.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	ws, _, err := websocket.Dial(context.Background(), wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer ws.CloseNow()

	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()
	defer serverConn.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	type bridgeOutcome struct {
		result BridgeResult
		err    error
	}
	ch := make(chan bridgeOutcome, 1)
	go func() {
		result, err := Bridge(ctx, ws, serverConn)
		ch <- bridgeOutcome{result, err}
	}()

	// Send 100 bytes through the TCP side.
	payload := strings.Repeat("X", 100)
	if _, err := clientConn.Write([]byte(payload)); err != nil {
		t.Fatalf("write: %v", err)
	}

	// Read the echoed data back.
	buf := make([]byte, 200)
	clientConn.SetReadDeadline(time.Now().Add(2 * time.Second))
	n, err := clientConn.Read(buf)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if n != 100 {
		t.Fatalf("read %d bytes, want 100", n)
	}

	// Close to end bridge.
	clientConn.Close()

	select {
	case res := <-ch:
		// TCPToWS: 100 bytes sent from TCP to WS (the payload we wrote).
		if res.result.Stats.TCPToWS != 100 {
			t.Errorf("Stats.TCPToWS = %d, want 100", res.result.Stats.TCPToWS)
		}
		// WSToTCP: 100 bytes echoed back from WS to TCP.
		if res.result.Stats.WSToTCP != 100 {
			t.Errorf("Stats.WSToTCP = %d, want 100", res.result.Stats.WSToTCP)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("bridge did not terminate")
	}
}

func TestBridge_ContextCancel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer ws.CloseNow()
		// Just block until closed.
		for {
			if _, _, err := ws.Read(r.Context()); err != nil {
				return
			}
		}
	}))
	defer srv.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	ws, _, err := websocket.Dial(context.Background(), wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer ws.CloseNow()

	_, serverConn := net.Pipe()
	defer serverConn.Close()

	ctx, cancel := context.WithCancel(context.Background())

	bridgeErr := make(chan error, 1)
	go func() {
		_, err := Bridge(ctx, ws, serverConn)
		bridgeErr <- err
	}()

	// Cancel context; bridge should exit promptly.
	cancel()

	select {
	case <-bridgeErr:
		// success
	case <-time.After(3 * time.Second):
		t.Fatal("bridge did not terminate after context cancel")
	}
}

func TestBridge_ZeroLengthData(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer ws.CloseNow()

		// Send empty message then a real message.
		ws.Write(r.Context(), websocket.MessageBinary, []byte{})
		ws.Write(r.Context(), websocket.MessageBinary, []byte("after-empty"))
		// Read until closed.
		for {
			if _, _, err := ws.Read(r.Context()); err != nil {
				return
			}
		}
	}))
	defer srv.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	ws, _, err := websocket.Dial(context.Background(), wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer ws.CloseNow()

	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()
	defer serverConn.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	go func() { _, _ = Bridge(ctx, ws, serverConn) }()

	// Should receive "after-empty" even though empty message was sent first.
	buf := make([]byte, 64)
	clientConn.SetReadDeadline(time.Now().Add(2 * time.Second))

	// Read all available data.
	var total []byte
	for {
		n, err := clientConn.Read(buf)
		total = append(total, buf[:n]...)
		if strings.Contains(string(total), "after-empty") {
			break
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("read: %v", err)
		}
	}

	if !strings.Contains(string(total), "after-empty") {
		t.Errorf("did not receive expected data, got %q", string(total))
	}
}

func TestWSCloseCode_Nil_ReturnsFalse(t *testing.T) {
	code, ok := WSCloseCode(nil)
	if ok {
		t.Errorf("ok=true on nil error; want false")
	}
	if code != 0 {
		t.Errorf("code=%d on nil error; want 0", code)
	}
}

func TestWSCloseCode_Plain_ReturnsFalse(t *testing.T) {
	code, ok := WSCloseCode(errors.New("synthetic"))
	if ok {
		t.Errorf("ok=true on plain error; want false")
	}
	if code != 0 {
		t.Errorf("code=%d on plain error; want 0", code)
	}
}

func TestWSCloseCode_DirectCloseError(t *testing.T) {
	err := websocket.CloseError{Code: 1006, Reason: "abnormal"}
	code, ok := WSCloseCode(err)
	if !ok {
		t.Fatalf("ok=false on websocket.CloseError; want true")
	}
	if code != 1006 {
		t.Errorf("code=%d; want 1006", code)
	}
}

func TestWSCloseCode_WrappedCloseError(t *testing.T) {
	inner := websocket.CloseError{Code: 1011, Reason: "server error"}
	wrapped := fmt.Errorf("read response: %w", inner)
	code, ok := WSCloseCode(wrapped)
	if !ok {
		t.Fatalf("ok=false on wrapped CloseError; want true")
	}
	if code != 1011 {
		t.Errorf("code=%d; want 1011", code)
	}
}

// TestBridge_Cause_PeerNormalClose drives a bridge where the WS server
// sends a normal close frame. The wsToTCP pump returns first with op=ws_read
// and a nil error (normal close is filtered to nil by ignoreNormalClose),
// so the bridge stamps CausePeerClose.
func TestBridge_Cause_PeerNormalClose(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		// Send a normal close to the client and exit. The bridge's
		// wsToTCP pump observes a normal close and returns (ws_read, nil).
		_ = ws.Close(websocket.StatusNormalClosure, "bye")
	}))
	defer srv.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	ws, _, err := websocket.Dial(context.Background(), wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer ws.CloseNow()

	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()
	defer serverConn.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	result, _ := Bridge(ctx, ws, serverConn)
	if result.EndCause != "peer_close" {
		t.Errorf("EndCause = %q, want %q", result.EndCause, "peer_close")
	}
}

// TestBridge_Cause_LocalEOF closes the local TCP side; the tcpToWS pump
// reads EOF and returns (tcp_read, nil), stamping CauseLocalClose.
func TestBridge_Cause_LocalEOF(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer ws.CloseNow()
		for {
			if _, _, err := ws.Read(r.Context()); err != nil {
				return
			}
		}
	}))
	defer srv.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	ws, _, err := websocket.Dial(context.Background(), wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer ws.CloseNow()

	clientConn, serverConn := net.Pipe()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	type outcome struct {
		result BridgeResult
		err    error
	}
	ch := make(chan outcome, 1)
	go func() {
		r, e := Bridge(ctx, ws, serverConn)
		ch <- outcome{r, e}
	}()

	// Close the local side; tcp.Read returns EOF, tcpToWS returns
	// (tcp_read, nil) after ignoreEOF, and the bridge stamps
	// CauseLocalClose.
	_ = clientConn.Close()

	select {
	case res := <-ch:
		if res.result.EndCause != "local_close" {
			t.Errorf("EndCause = %q, want %q", res.result.EndCause, "local_close")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("bridge did not terminate")
	}
	_ = serverConn.Close()
}

// TestBridge_Cause_ParentCanceled cancels the bridge's parent context
// with CauseUserCancel via WithCancelCause. The cause propagates down
// to Bridge's internal child ctx, so context.Cause(internalCtx) returns
// CauseUserCancel and bridgecause.Name maps it to "user_cancel".
func TestBridge_Cause_ParentCanceled(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer ws.CloseNow()
		for {
			if _, _, err := ws.Read(r.Context()); err != nil {
				return
			}
		}
	}))
	defer srv.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	ws, _, err := websocket.Dial(context.Background(), wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer ws.CloseNow()

	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()
	defer serverConn.Close()

	parent, parentCancel := context.WithCancelCause(context.Background())

	type outcome struct {
		result BridgeResult
		err    error
	}
	ch := make(chan outcome, 1)
	go func() {
		r, e := Bridge(parent, ws, serverConn)
		ch <- outcome{r, e}
	}()

	parentCancel(bridgecause.CauseUserCancel)

	select {
	case res := <-ch:
		if res.result.EndCause != "user_cancel" {
			t.Errorf("EndCause = %q, want %q", res.result.EndCause, "user_cancel")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("bridge did not terminate after parent cancel")
	}
}

// TestBridge_Cause_ExternalSiteCancel simulates the control loop tearing
// the bridge down on renew failure: the caller cancels the bridge ctx
// with CauseRenewFailure mid-flight. The Bridge sees its internal ctx
// done with that cause and reports EndCause == "renew_failure".
func TestBridge_Cause_ExternalSiteCancel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer ws.CloseNow()
		for {
			if _, _, err := ws.Read(r.Context()); err != nil {
				return
			}
		}
	}))
	defer srv.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	ws, _, err := websocket.Dial(context.Background(), wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer ws.CloseNow()

	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()
	defer serverConn.Close()

	bridgeCtx, bridgeCancel := context.WithCancelCause(context.Background())

	type outcome struct {
		result BridgeResult
		err    error
	}
	ch := make(chan outcome, 1)
	go func() {
		r, e := Bridge(bridgeCtx, ws, serverConn)
		ch <- outcome{r, e}
	}()

	bridgeCancel(bridgecause.CauseRenewFailure)

	select {
	case res := <-ch:
		if res.result.EndCause != "renew_failure" {
			t.Errorf("EndCause = %q, want %q", res.result.EndCause, "renew_failure")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("bridge did not terminate after external cancel")
	}
}

// TestBridge_Result_BothNormal verifies the "no real per-direction
// error" contract of BridgeResult: when neither pump returns a real
// I/O error, both per-direction fields resolve to nil. The WS peer
// sends StatusNormalClosure on Accept and the wsToTCP pump reads it
// cleanly via ignoreNormalClose; the tcpToWS pump is blocked on a
// quiet net.Pipe and exits via the bridge's induced SetReadDeadline,
// which isInducedCancellation resolves to nil. The asymmetry between
// the two pumps' exit paths is intentional — each pump's clean-
// close path is independently covered by TestBridge_Cause_*.
func TestBridge_Result_BothNormal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		_ = ws.Close(websocket.StatusNormalClosure, "bye")
	}))
	defer srv.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	ws, _, err := websocket.Dial(context.Background(), wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer ws.CloseNow()

	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()
	defer serverConn.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	result, bridgeErr := Bridge(ctx, ws, serverConn)
	if result.TCPToWS != nil {
		t.Errorf("TCPToWS = %v, want nil (normal close)", result.TCPToWS)
	}
	if result.WSToTCP != nil {
		t.Errorf("WSToTCP = %v, want nil (normal close)", result.WSToTCP)
	}
	if bridgeErr != nil {
		t.Errorf("bridgeErr = %v, want nil (normal close)", bridgeErr)
	}
	if result.EndCause != "peer_close" {
		t.Errorf("EndCause = %q, want %q", result.EndCause, "peer_close")
	}
}

// TestBridge_Result_TCPSideErrors injects a non-EOF I/O failure on the
// TCP side via a custom net.Conn whose Read returns a sentinel that
// does NOT match isInducedCancellation. After Bridge returns,
// result.TCPToWS must carry the sentinel and result.WSToTCP must be
// nil (it was cancelled by the bridge's internal ctx).
func TestBridge_Result_TCPSideErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer ws.CloseNow()
		for {
			if _, _, err := ws.Read(r.Context()); err != nil {
				return
			}
		}
	}))
	defer srv.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	ws, _, err := websocket.Dial(context.Background(), wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer ws.CloseNow()

	sentinel := errors.New("synthetic tcp read failure")
	tcp := newScriptedConn()
	defer tcp.Close()
	tcp.failReadAfter(0, sentinel)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	result, bridgeErr := Bridge(ctx, ws, tcp)
	if !errors.Is(result.TCPToWS, sentinel) {
		t.Errorf("TCPToWS = %v, want %v", result.TCPToWS, sentinel)
	}
	if result.WSToTCP != nil {
		t.Errorf("WSToTCP = %v, want nil (induced ws cancellation)", result.WSToTCP)
	}
	if !errors.Is(bridgeErr, sentinel) {
		t.Errorf("bridgeErr = %v, want %v", bridgeErr, sentinel)
	}
}

// TestBridge_Result_WSSideErrors injects a non-normal close on the WS
// side (StatusInternalError, 1011) which the ws-layer surfaces as a
// CloseError that is NOT filtered as a normal close. The tcpToWS pump
// is on a blocking custom net.Conn; the bridge interrupts it via
// SetReadDeadline. The pump's tcp.Read therefore returns the deadline
// timeout (induced) which is filtered to nil; result.WSToTCP carries
// the CloseError.
func TestBridge_Result_WSSideErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		_ = ws.Close(websocket.StatusInternalError, "boom")
	}))
	defer srv.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	ws, _, err := websocket.Dial(context.Background(), wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer ws.CloseNow()

	tcp := newScriptedConn()
	defer tcp.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	result, _ := Bridge(ctx, ws, tcp)
	if result.WSToTCP == nil {
		t.Fatalf("WSToTCP = nil, want CloseError (1011)")
	}
	var closeErr websocket.CloseError
	if !errors.As(result.WSToTCP, &closeErr) || closeErr.Code != websocket.StatusInternalError {
		t.Errorf("WSToTCP = %v, want CloseError{Code=%d}", result.WSToTCP, websocket.StatusInternalError)
	}
	if result.TCPToWS != nil {
		t.Errorf("TCPToWS = %v, want nil (induced tcp deadline)", result.TCPToWS)
	}
}

// TestBridge_Result_SecondPumpSuppressed proves the bridge only reports
// the first pump's terminal error. Even when the drained second pump
// returns a real sentinel error, BridgeResult keeps that side nil.
func TestBridge_Result_SecondPumpSuppressed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		_ = ws.Close(websocket.StatusInternalError, "boom")
	}))
	defer srv.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	ws, _, err := websocket.Dial(context.Background(), wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer ws.CloseNow()

	sentinel := errors.New("synthetic tcp read failure")
	tcp := newScriptedConn()
	tcp.ignoreDeadline = true
	defer tcp.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	type outcome struct {
		result BridgeResult
		err    error
	}
	ch := make(chan outcome, 1)
	go func() {
		r, e := Bridge(ctx, ws, tcp)
		ch <- outcome{r, e}
	}()

	// Wait for the bridge to call tcp.SetReadDeadline, which it
	// only does AFTER the first pump exits. With wsToTCP as the
	// only pump that can finish unprompted (the WS server already
	// sent the CloseError; tcp.Read is parked on c.releaseCh),
	// reaching this signal proves wsToTCP has already returned
	// its CloseError. Releasing the sentinel after this point
	// makes the test deterministic without timing-based sleeps.
	<-tcp.deadlineCalled
	tcp.releaseRead(sentinel)

	select {
	case res := <-ch:
		if res.result.WSToTCP == nil {
			t.Errorf("WSToTCP = nil, want CloseError")
		}
		if res.result.TCPToWS != nil {
			t.Errorf("TCPToWS = %v, want nil (second-pump collateral)", res.result.TCPToWS)
		}
		if res.err == nil {
			t.Errorf("bridgeErr = nil, want non-nil")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("bridge did not terminate")
	}
}

// scriptedConn is a net.Conn whose Read blocks until releaseRead /
// failReadAfter unblocks it, whose Write succeeds silently, and
// whose Close + deadline calls are no-ops. The dedicated harness
// keeps TestBridge_Result_* deterministic: the bridge's
// SetReadDeadline-based unblock can be defeated via ignoreDeadline,
// so per-direction tests can deliver a chosen sentinel error to
// tcp.Read at exactly the moment they want.
//
// deadlineCalled fires on the first non-zero SetReadDeadline call.
// Bridge only calls SetReadDeadline after the first pump exits, so
// the channel is a sync point tests can wait on to know wsToTCP has
// already returned before they release the TCP Read sentinel.
type scriptedConn struct {
	releaseCh      chan error
	closed         chan struct{}
	deadlineCalled chan struct{}
	deadlineOnce   sync.Once
	ignoreDeadline bool
}

func newScriptedConn() *scriptedConn {
	return &scriptedConn{
		releaseCh:      make(chan error, 1),
		closed:         make(chan struct{}),
		deadlineCalled: make(chan struct{}),
	}
}

func (c *scriptedConn) Read(p []byte) (int, error) {
	select {
	case err := <-c.releaseCh:
		if err == nil {
			return 0, io.EOF
		}
		return 0, err
	case <-c.closed:
		return 0, io.EOF
	}
}

func (c *scriptedConn) Write(p []byte) (int, error) {
	select {
	case <-c.closed:
		return 0, io.ErrClosedPipe
	default:
		return len(p), nil
	}
}

func (c *scriptedConn) Close() error {
	select {
	case <-c.closed:
	default:
		close(c.closed)
	}
	return nil
}

func (c *scriptedConn) LocalAddr() net.Addr  { return &net.IPAddr{} }
func (c *scriptedConn) RemoteAddr() net.Addr { return &net.IPAddr{} }

func (c *scriptedConn) SetDeadline(t time.Time) error      { return nil }
func (c *scriptedConn) SetWriteDeadline(t time.Time) error { return nil }
func (c *scriptedConn) SetReadDeadline(t time.Time) error {
	if t.IsZero() {
		return nil
	}
	c.deadlineOnce.Do(func() { close(c.deadlineCalled) })
	if c.ignoreDeadline {
		return nil
	}
	// Default behavior: complete the parked Read with a Timeout()
	// net.Error so the bridge's induced-cancellation filter
	// classifies it as nil.
	select {
	case c.releaseCh <- &timeoutError{}:
	default:
	}
	return nil
}

func (c *scriptedConn) failReadAfter(d time.Duration, err error) {
	if d == 0 {
		c.releaseCh <- err
		return
	}
	go func() {
		time.Sleep(d)
		c.releaseCh <- err
	}()
}

func (c *scriptedConn) releaseRead(err error) {
	c.releaseCh <- err
}

// timeoutError satisfies net.Error and reports Timeout() = true so the
// bridge classifies it as an induced cancellation via
// isInducedCancellation.
type timeoutError struct{}

func (*timeoutError) Error() string   { return "scripted-conn: deadline exceeded" }
func (*timeoutError) Timeout() bool   { return true }
func (*timeoutError) Temporary() bool { return true }
