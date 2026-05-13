package server

import (
	"context"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// dialListener opens a listener control channel against the test
// server. Returns the client-side WS conn and a cleanup func.
func dialListener(t *testing.T, ctx context.Context, srvURL, entity string) (*websocket.Conn, func()) {
	t.Helper()
	wsURL := strings.Replace(srvURL, "http://", "ws://", 1) + "/$hc/" + entity + "?sb-hc-action=listen"
	ws, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("dial listener: %v", err)
	}
	return ws, func() { _ = ws.CloseNow() }
}

// TestHandleListen_PingResetsIdleTimer verifies that incoming WebSocket
// pings reset the listener idle timer. The previous implementation
// used per-iteration context timeouts on ws.Read and did NOT see pings
// as activity, which silently broke documented behavior.
func TestHandleListen_PingResetsIdleTimer(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping idle-timeout test in -short mode")
	}
	idle := 250 * time.Millisecond
	s, err := NewServer(Config{
		ListenerIdleTimeout: idle,
		Logger:              slog.New(slog.NewTextHandler(testDiscard{}, nil)),
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	ws, closeWS := dialListener(t, ctx, srv.URL, "entity-ping")
	defer closeWS()

	// coder/websocket Ping() needs an active reader on this side so the
	// pong frame is processed and unblocks the Ping caller. Run a read
	// loop in a goroutine; the server doesn't push messages, so the
	// goroutine just waits for the eventual close.
	readDone := make(chan error, 1)
	go func() {
		for {
			if _, _, err := ws.Read(ctx); err != nil {
				readDone <- err
				return
			}
		}
	}()

	// Ping every 80ms for ~600ms — well beyond the 250ms idle timeout.
	// If the timeout is reset by pings, the connection survives.
	deadline := time.Now().Add(600 * time.Millisecond)
	pingNum := 0
	for time.Now().Before(deadline) {
		pingCtx, pingCancel := context.WithTimeout(ctx, 1*time.Second)
		err := ws.Ping(pingCtx)
		pingCancel()
		if err != nil {
			t.Fatalf("ping %d failed: %v", pingNum, err)
		}
		pingNum++
		time.Sleep(80 * time.Millisecond)
	}
	if pingNum == 0 {
		t.Fatalf("no pings sent")
	}

	// Now stop pinging and verify the server closes us within ~3x idle.
	select {
	case err := <-readDone:
		var ce websocket.CloseError
		if asCE(err, &ce) {
			if ce.Code != websocket.StatusPolicyViolation {
				t.Errorf("close code=%d, want StatusPolicyViolation", ce.Code)
			}
		}
	case <-time.After(4 * idle):
		t.Fatalf("server did not close idle listener within %v", 4*idle)
	}
}

// TestHandleListen_IdleClosesWithoutActivity verifies that an idle
// listener (no traffic, no pings) is closed by the server after the
// configured timeout. This is the "negative control" for the
// ping-resets-timer test.
func TestHandleListen_IdleClosesWithoutActivity(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping idle-timeout test in -short mode")
	}
	idle := 200 * time.Millisecond
	s, err := NewServer(Config{
		ListenerIdleTimeout: idle,
		Logger:              slog.New(slog.NewTextHandler(testDiscard{}, nil)),
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	ws, closeWS := dialListener(t, ctx, srv.URL, "entity-idle")
	defer closeWS()

	start := time.Now()
	readCtx, readCancel := context.WithTimeout(ctx, 5*idle)
	defer readCancel()
	_, _, err = ws.Read(readCtx)
	if err == nil {
		t.Fatalf("expected idle close, got nil error")
	}
	elapsed := time.Since(start)
	if elapsed > 4*idle {
		t.Errorf("idle close took %v, want < %v", elapsed, 4*idle)
	}
}

// asCE is a tiny errors.As wrapper local to this test file.
func asCE(err error, target *websocket.CloseError) bool {
	for err != nil {
		if ce, ok := err.(websocket.CloseError); ok {
			*target = ce
			return true
		}
		type unwrapper interface{ Unwrap() error }
		u, ok := err.(unwrapper)
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

// testDiscard is an io.Writer that drops all output, used to silence
// slog output during tests.
type testDiscard struct{}

func (testDiscard) Write(p []byte) (int, error) { return len(p), nil }
