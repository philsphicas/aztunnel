package relay

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
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
		bridgeErr <- Bridge(ctx, ws, serverConn)
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
		bridgeErr <- Bridge(ctx, ws, serverConn)
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

	go Bridge(ctx, ws, serverConn)

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
