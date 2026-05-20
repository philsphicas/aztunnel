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

	type bridgeResult struct {
		stats BridgeStats
		err   error
	}
	ch := make(chan bridgeResult, 1)
	go func() {
		stats, err := Bridge(ctx, ws, serverConn)
		ch <- bridgeResult{stats, err}
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
		if res.stats.TCPToWS != 100 {
			t.Errorf("TCPToWS = %d, want 100", res.stats.TCPToWS)
		}
		// WSToTCP: 100 bytes echoed back from WS to TCP.
		if res.stats.WSToTCP != 100 {
			t.Errorf("WSToTCP = %d, want 100", res.stats.WSToTCP)
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

	stats, _ := Bridge(ctx, ws, serverConn)
	if stats.Cause != "peer_close" {
		t.Errorf("Cause = %q, want %q", stats.Cause, "peer_close")
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

	type result struct {
		stats BridgeStats
		err   error
	}
	ch := make(chan result, 1)
	go func() {
		s, e := Bridge(ctx, ws, serverConn)
		ch <- result{s, e}
	}()

	// Close the local side; tcp.Read returns EOF, tcpToWS returns
	// (tcp_read, nil) after ignoreEOF, and the bridge stamps
	// CauseLocalClose.
	_ = clientConn.Close()

	select {
	case res := <-ch:
		if res.stats.Cause != "local_close" {
			t.Errorf("Cause = %q, want %q", res.stats.Cause, "local_close")
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

	type result struct {
		stats BridgeStats
		err   error
	}
	ch := make(chan result, 1)
	go func() {
		s, e := Bridge(parent, ws, serverConn)
		ch <- result{s, e}
	}()

	parentCancel(bridgecause.CauseUserCancel)

	select {
	case res := <-ch:
		if res.stats.Cause != "user_cancel" {
			t.Errorf("Cause = %q, want %q", res.stats.Cause, "user_cancel")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("bridge did not terminate after parent cancel")
	}
}

// TestBridge_Cause_ExternalSiteCancel simulates the control loop tearing
// the bridge down on renew failure: the caller cancels the bridge ctx
// with CauseRenewFailure mid-flight. The Bridge sees its internal ctx
// done with that cause and reports stats.Cause == "renew_failure".
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

	type result struct {
		stats BridgeStats
		err   error
	}
	ch := make(chan result, 1)
	go func() {
		s, e := Bridge(bridgeCtx, ws, serverConn)
		ch <- result{s, e}
	}()

	bridgeCancel(bridgecause.CauseRenewFailure)

	select {
	case res := <-ch:
		if res.stats.Cause != "renew_failure" {
			t.Errorf("Cause = %q, want %q", res.stats.Cause, "renew_failure")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("bridge did not terminate after external cancel")
	}
}
