package sender

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/philsphicas/aztunnel/internal/protocol"
)

// --- stdioConn tests ---

type fakeReadCloser struct {
	io.Reader
	closed bool
}

func (f *fakeReadCloser) Close() error {
	f.closed = true
	return nil
}

type fakeWriteCloser struct {
	io.Writer
	closed bool
}

func (f *fakeWriteCloser) Close() error {
	f.closed = true
	return nil
}

type errCloser struct {
	err error
}

func (e *errCloser) Read([]byte) (int, error)  { return 0, e.err }
func (e *errCloser) Write([]byte) (int, error) { return 0, e.err }
func (e *errCloser) Close() error              { return e.err }

func TestStdioConn(t *testing.T) {
	t.Run("ReadWriteClose", func(t *testing.T) {
		inData := []byte("hello from stdin")
		inBuf := &fakeReadCloser{Reader: bytes.NewReader(inData)}
		outBuf := &bytes.Buffer{}
		outCloser := &fakeWriteCloser{Writer: outBuf}

		conn := &stdioConn{in: inBuf, out: outCloser}

		// Test Read delegates to in.
		buf := make([]byte, 64)
		n, err := conn.Read(buf)
		if err != nil {
			t.Fatalf("Read: %v", err)
		}
		if string(buf[:n]) != "hello from stdin" {
			t.Errorf("Read got %q, want %q", string(buf[:n]), "hello from stdin")
		}

		// Test Write delegates to out.
		msg := []byte("hello to stdout")
		n, err = conn.Write(msg)
		if err != nil {
			t.Fatalf("Write: %v", err)
		}
		if n != len(msg) {
			t.Errorf("Write returned %d, want %d", n, len(msg))
		}
		if outBuf.String() != "hello to stdout" {
			t.Errorf("Write output %q, want %q", outBuf.String(), "hello to stdout")
		}

		// Test Close closes both.
		if err := conn.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
		if !inBuf.closed {
			t.Error("Close did not close input")
		}
		if !outCloser.closed {
			t.Error("Close did not close output")
		}
	})

	t.Run("CloseJoinsErrors", func(t *testing.T) {
		errIn := errors.New("in close error")
		errOut := errors.New("out close error")
		conn := &stdioConn{
			in:  &errCloser{err: errIn},
			out: &errCloser{err: errOut},
		}

		err := conn.Close()
		if err == nil {
			t.Fatal("Close should return error when both sides fail")
		}
		if !errors.Is(err, errIn) {
			t.Errorf("Close error should contain in error, got: %v", err)
		}
		if !errors.Is(err, errOut) {
			t.Errorf("Close error should contain out error, got: %v", err)
		}
	})

	t.Run("DeadlinesReturnNil", func(t *testing.T) {
		conn := &stdioConn{
			in:  &fakeReadCloser{Reader: strings.NewReader("")},
			out: &fakeWriteCloser{Writer: &bytes.Buffer{}},
		}

		if err := conn.SetDeadline(time.Now()); err != nil {
			t.Errorf("SetDeadline should return nil, got %v", err)
		}
		if err := conn.SetReadDeadline(time.Now()); err != nil {
			t.Errorf("SetReadDeadline should return nil, got %v", err)
		}
		if err := conn.SetWriteDeadline(time.Now()); err != nil {
			t.Errorf("SetWriteDeadline should return nil, got %v", err)
		}
	})

	t.Run("LocalAddrRemoteAddr", func(t *testing.T) {
		conn := &stdioConn{
			in:  &fakeReadCloser{Reader: strings.NewReader("")},
			out: &fakeWriteCloser{Writer: &bytes.Buffer{}},
		}

		local := conn.LocalAddr()
		remote := conn.RemoteAddr()

		if local.Network() != "stdio" {
			t.Errorf("LocalAddr().Network() = %q, want %q", local.Network(), "stdio")
		}
		if local.String() != "stdio" {
			t.Errorf("LocalAddr().String() = %q, want %q", local.String(), "stdio")
		}
		if remote.Network() != "stdio" {
			t.Errorf("RemoteAddr().Network() = %q, want %q", remote.Network(), "stdio")
		}
		if remote.String() != "stdio" {
			t.Errorf("RemoteAddr().String() = %q, want %q", remote.String(), "stdio")
		}
	})

	t.Run("ImplementsNetConn", func(t *testing.T) {
		conn := &stdioConn{
			in:  &fakeReadCloser{Reader: strings.NewReader("")},
			out: &fakeWriteCloser{Writer: &bytes.Buffer{}},
		}
		// Compile-time check that stdioConn implements net.Conn.
		var _ net.Conn = conn
	})
}

// --- stubAddr tests ---

func TestStubAddr(t *testing.T) {
	addr := stubAddr{}
	if addr.Network() != "stdio" {
		t.Errorf("Network() = %q, want %q", addr.Network(), "stdio")
	}
	if addr.String() != "stdio" {
		t.Errorf("String() = %q, want %q", addr.String(), "stdio")
	}
}

// --- sendEnvelopeAndCheck tests ---

func TestSendEnvelopeAndCheck(t *testing.T) {
	tests := []struct {
		name      string
		target    string
		respOK    bool
		respError string
		wantErr   string
	}{
		{
			name:   "success",
			target: "localhost:8080",
			respOK: true,
		},
		{
			name:      "rejected",
			target:    "badhost:9999",
			respOK:    false,
			respError: "connection refused",
			wantErr:   "connection rejected: connection refused",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			// Create a mock WebSocket server.
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				ws, err := websocket.Accept(w, r, nil)
				if err != nil {
					return
				}
				defer ws.CloseNow()

				// Read the envelope sent by the client.
				_, data, err := ws.Read(r.Context())
				if err != nil {
					return
				}

				// Verify it's a valid ConnectEnvelope.
				var env protocol.ConnectEnvelope
				if err := json.Unmarshal(data, &env); err != nil {
					t.Errorf("server: unmarshal envelope: %v", err)
					return
				}
				if env.Target != tt.target {
					t.Errorf("server: envelope target = %q, want %q", env.Target, tt.target)
				}
				if env.Version != protocol.CurrentVersion {
					t.Errorf("server: envelope version = %d, want %d", env.Version, protocol.CurrentVersion)
				}

				// Send response.
				resp := protocol.ConnectResponse{
					Version: protocol.CurrentVersion,
					OK:      tt.respOK,
					Error:   tt.respError,
				}
				respData, _ := json.Marshal(resp)
				if err := ws.Write(r.Context(), websocket.MessageText, respData); err != nil {
					return
				}

				// Keep connection open briefly so the client can read.
				ws.Close(websocket.StatusNormalClosure, "done")
			}))
			defer srv.Close()

			// Dial the mock server.
			wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
			ws, _, err := websocket.Dial(ctx, wsURL, nil)
			if err != nil {
				t.Fatalf("dial: %v", err)
			}
			defer ws.CloseNow()

			err = sendEnvelopeAndCheck(ctx, ws, tt.target)

			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("expected error containing %q, got nil", tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Errorf("error %q does not contain %q", err.Error(), tt.wantErr)
				}
			} else if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestSendEnvelopeAndCheck_WriteError(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Create a server that immediately closes the WebSocket.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		// Close immediately to cause a write error on the client side.
		ws.Close(websocket.StatusNormalClosure, "bye")
	}))
	defer srv.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	ws, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer ws.CloseNow()

	// Give the server a moment to send its close frame.
	time.Sleep(50 * time.Millisecond)

	err = sendEnvelopeAndCheck(ctx, ws, "localhost:80")
	if err == nil {
		t.Fatal("expected error when writing to closed websocket, got nil")
	}
	// The error should be about sending the envelope or reading the response.
	if !strings.Contains(err.Error(), "send envelope") && !strings.Contains(err.Error(), "read response") {
		t.Errorf("unexpected error message: %v", err)
	}
}

func TestSendEnvelopeAndCheck_InvalidResponse(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Create a server that sends invalid JSON as the response.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer ws.CloseNow()

		// Read the envelope.
		_, _, err = ws.Read(r.Context())
		if err != nil {
			return
		}

		// Send invalid JSON.
		if err := ws.Write(r.Context(), websocket.MessageText, []byte("not json")); err != nil {
			return
		}

		ws.Close(websocket.StatusNormalClosure, "done")
	}))
	defer srv.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	ws, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer ws.CloseNow()

	err = sendEnvelopeAndCheck(ctx, ws, "localhost:80")
	if err == nil {
		t.Fatal("expected error for invalid JSON response, got nil")
	}
	if !strings.Contains(err.Error(), "parse response") {
		t.Errorf("error %q does not contain %q", err.Error(), "parse response")
	}
}
