package relay

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// mockTokenProvider is a simple TokenProvider for control tests.
type mockTokenProvider struct {
	mu      sync.Mutex
	token   string
	err     error
	calls   int
	tokenFn func(ctx context.Context, resourceURI string) (string, error)
}

func (m *mockTokenProvider) GetToken(ctx context.Context, resourceURI string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls++
	if m.tokenFn != nil {
		return m.tokenFn(ctx, resourceURI)
	}
	return m.token, m.err
}

func (m *mockTokenProvider) getCalls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls
}

// testEndpoint returns bare host:port from a httptest.Server URL.
func testEndpoint(srv *httptest.Server) string {
	return strings.TrimPrefix(strings.TrimPrefix(srv.URL, "https://"), "http://")
}

func tlsServer(t *testing.T, handler http.Handler) *httptest.Server {
	t.Helper()
	srv := httptest.NewTLSServer(handler)
	t.Cleanup(srv.Close)
	return srv
}

func useInsecureTransport(t *testing.T) {
	t.Helper()
	origTransport := http.DefaultTransport
	http.DefaultTransport = &http.Transport{
		TLSClientConfig: &tls.Config{
			//nolint:gosec // G402: test-only, self-signed cert
			InsecureSkipVerify: true,
		},
	}
	t.Cleanup(func() { http.DefaultTransport = origTransport })
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(discard{}, nil))
}

type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }

// ---------- TestHandleAccept ----------

func TestHandleAccept(t *testing.T) {
	useInsecureTransport(t)

	t.Run("dials rendezvous and calls handler", func(t *testing.T) {
		handlerCalled := make(chan struct{})
		rendezvousSrv := tlsServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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

		cfg := ControlConfig{
			DialTimeout: 5 * time.Second,
			Logger:      discardLogger(),
			Handler: func(ctx context.Context, ws *websocket.Conn) {
				close(handlerCalled)
			},
		}

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		err := handleAccept(ctx, "wss://"+testEndpoint(rendezvousSrv), cfg, discardLogger())
		if err != nil {
			t.Fatalf("handleAccept returned error: %v", err)
		}

		select {
		case <-handlerCalled:
			// success
		default:
			t.Fatal("handler was not called")
		}
	})

	t.Run("returns error on dial failure", func(t *testing.T) {
		cfg := ControlConfig{
			DialTimeout: 1 * time.Second,
			Logger:      discardLogger(),
			Handler: func(ctx context.Context, ws *websocket.Conn) {
				t.Fatal("handler should not be called")
			},
		}

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		err := handleAccept(ctx, "ws://127.0.0.1:1", cfg, discardLogger())
		if err == nil {
			t.Fatal("expected error for bad address")
		}
		if !strings.Contains(err.Error(), "dial rendezvous") {
			t.Errorf("unexpected error: %v", err)
		}
	})
}

// ---------- TestRenewOnce ----------

func TestRenewOnce(t *testing.T) {
	useInsecureTransport(t)

	t.Run("successful renewal", func(t *testing.T) {
		received := make(chan map[string]interface{}, 1)
		srv := tlsServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ws, err := websocket.Accept(w, r, nil)
			if err != nil {
				return
			}
			defer ws.CloseNow()

			_, data, err := ws.Read(r.Context())
			if err != nil {
				return
			}
			var msg map[string]interface{}
			json.Unmarshal(data, &msg)
			received <- msg

			for {
				if _, _, err := ws.Read(r.Context()); err != nil {
					return
				}
			}
		}))

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		ws, _, err := websocket.Dial(ctx, "wss://"+testEndpoint(srv), nil)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		defer ws.CloseNow()

		tp := &mockTokenProvider{token: "renewed-token-123"}

		err = renewOnce(ctx, ws, "https://test.servicebus.windows.net/hc", tp, discardLogger())
		if err != nil {
			t.Fatalf("renewOnce returned error: %v", err)
		}

		select {
		case msg := <-received:
			rt, ok := msg["renewToken"].(map[string]interface{})
			if !ok {
				t.Fatalf("missing renewToken in message: %v", msg)
			}
			if rt["token"] != "renewed-token-123" {
				t.Errorf("token = %v, want %q", rt["token"], "renewed-token-123")
			}
		case <-time.After(3 * time.Second):
			t.Fatal("did not receive renewToken message")
		}
	})

	t.Run("retries on token failure then succeeds", func(t *testing.T) {
		received := make(chan map[string]interface{}, 1)
		srv := tlsServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ws, err := websocket.Accept(w, r, nil)
			if err != nil {
				return
			}
			defer ws.CloseNow()

			_, data, err := ws.Read(r.Context())
			if err != nil {
				return
			}
			var msg map[string]interface{}
			json.Unmarshal(data, &msg)
			received <- msg

			for {
				if _, _, err := ws.Read(r.Context()); err != nil {
					return
				}
			}
		}))

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		ws, _, err := websocket.Dial(ctx, "wss://"+testEndpoint(srv), nil)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		defer ws.CloseNow()

		var callCount atomic.Int32
		tp := &mockTokenProvider{
			tokenFn: func(ctx context.Context, resourceURI string) (string, error) {
				n := callCount.Add(1)
				if n < 3 {
					return "", fmt.Errorf("transient error %d", n)
				}
				return "success-after-retry", nil
			},
		}

		err = renewOnce(ctx, ws, "https://test.servicebus.windows.net/hc", tp, discardLogger())
		if err != nil {
			t.Fatalf("renewOnce returned error: %v", err)
		}

		if got := callCount.Load(); got != 3 {
			t.Errorf("GetToken called %d times, want 3", got)
		}

		select {
		case msg := <-received:
			rt := msg["renewToken"].(map[string]interface{})
			if rt["token"] != "success-after-retry" {
				t.Errorf("token = %v, want %q", rt["token"], "success-after-retry")
			}
		case <-time.After(3 * time.Second):
			t.Fatal("did not receive renewToken message")
		}
	})

	t.Run("returns error after max retries", func(t *testing.T) {
		srv := tlsServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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

		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()

		ws, _, err := websocket.Dial(ctx, "wss://"+testEndpoint(srv), nil)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		defer ws.CloseNow()

		tp := &mockTokenProvider{err: fmt.Errorf("permanent failure")}

		err = renewOnce(ctx, ws, "https://test.servicebus.windows.net/hc", tp, discardLogger())
		if err == nil {
			t.Fatal("expected error after max retries")
		}
		if !strings.Contains(err.Error(), "permanent failure") {
			t.Errorf("unexpected error: %v", err)
		}
		if tp.getCalls() != maxRenewRetries {
			t.Errorf("GetToken called %d times, want %d", tp.getCalls(), maxRenewRetries)
		}
	})

	t.Run("write failure returns immediately without retry", func(t *testing.T) {
		srv := tlsServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ws, err := websocket.Accept(w, r, nil)
			if err != nil {
				return
			}
			ws.Close(websocket.StatusNormalClosure, "bye")
		}))

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		ws, _, err := websocket.Dial(ctx, "wss://"+testEndpoint(srv), nil)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		defer ws.CloseNow()

		// Read the close frame so the connection knows it's closed.
		ws.Read(ctx)

		tp := &mockTokenProvider{token: "some-token"}

		err = renewOnce(ctx, ws, "https://test.servicebus.windows.net/hc", tp, discardLogger())
		if err == nil {
			t.Fatal("expected error on write failure")
		}
		if tp.getCalls() != 1 {
			t.Errorf("GetToken called %d times, want 1 (no retry on write failure)", tp.getCalls())
		}
	})
}

// ---------- TestPingLoop ----------

func TestPingLoop(t *testing.T) {
	useInsecureTransport(t)

	t.Run("ping failure calls cancel", func(t *testing.T) {
		// Create a server that accepts the WS and then shuts down entirely,
		// causing a network-level failure for the ping. The server must stay
		// alive long enough for the client to establish the WebSocket.
		srvReady := make(chan struct{})
		srv := tlsServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ws, err := websocket.Accept(w, r, nil)
			if err != nil {
				return
			}
			defer ws.CloseNow()
			close(srvReady)
			// Keep reading until context is cancelled (server shutdown).
			for {
				if _, _, err := ws.Read(r.Context()); err != nil {
					return
				}
			}
		}))

		ctx, cancel := context.WithTimeout(context.Background(), pingInterval+pingTimeout+10*time.Second)
		defer cancel()

		ws, _, err := websocket.Dial(ctx, "wss://"+testEndpoint(srv), nil)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		defer ws.CloseNow()

		// Wait for server handler to be ready, then shut down server.
		// This kills TCP connections, ensuring ping will fail.
		<-srvReady
		srv.Close()

		cancelCalled := make(chan struct{})
		loopCtx, loopCancel := context.WithCancel(ctx)
		defer loopCancel()

		done := make(chan struct{})
		go func() {
			defer close(done)
			pingLoop(loopCtx, ws, discardLogger(), func() {
				close(cancelCalled)
				loopCancel()
			})
		}()

		select {
		case <-cancelCalled:
			// success - ping failure triggered cancel
		case <-time.After(pingInterval + pingTimeout + 5*time.Second):
			t.Fatal("cancel was not called after ping failure")
		}

		<-done
	})

	t.Run("context cancel stops loop", func(t *testing.T) {
		srv := tlsServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		ws, _, err := websocket.Dial(ctx, "wss://"+testEndpoint(srv), nil)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		defer ws.CloseNow()

		loopCtx, loopCancel := context.WithCancel(ctx)

		done := make(chan struct{})
		go func() {
			defer close(done)
			pingLoop(loopCtx, ws, discardLogger(), func() {
				t.Error("cancel should not be called on context cancel")
			})
		}()

		loopCancel()

		select {
		case <-done:
			// success
		case <-time.After(3 * time.Second):
			t.Fatal("pingLoop did not exit after context cancel")
		}
	})
}

// ---------- TestRenewLoop ----------

func TestRenewLoop(t *testing.T) {
	useInsecureTransport(t)

	t.Run("exits on context cancel", func(t *testing.T) {
		// renewLoop uses renewInterval (45min) for its ticker, so we can't
		// easily trigger a tick in a test. Instead we verify that it exits
		// promptly when the context is cancelled.
		srv := tlsServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		ws, _, err := websocket.Dial(ctx, "wss://"+testEndpoint(srv), nil)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		defer ws.CloseNow()

		tp := &mockTokenProvider{err: fmt.Errorf("always fails")}

		loopCtx, loopCancel := context.WithCancel(ctx)

		done := make(chan struct{})
		cancelCalled := make(chan struct{})
		go func() {
			defer close(done)
			renewLoop(loopCtx, ws, "https://test.servicebus.windows.net/hc", tp, discardLogger(), func() {
				close(cancelCalled)
				loopCancel()
			})
		}()

		// Cancel the context; renewLoop should exit via <-ctx.Done().
		loopCancel()

		select {
		case <-done:
			// success
		case <-time.After(3 * time.Second):
			t.Fatal("renewLoop did not exit on context cancel")
		}
	})
}

// ---------- TestRunControlLoop ----------

func TestRunControlLoop(t *testing.T) {
	useInsecureTransport(t)

	t.Run("reads accept messages and spawns handlers", func(t *testing.T) {
		handlerCalls := make(chan string, 10)

		rendezvousSrv := tlsServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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

		rendezvousAddr := "wss://" + testEndpoint(rendezvousSrv)

		// Control server sends accept messages, waits for handlers, then closes.
		controlSrv := tlsServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ws, err := websocket.Accept(w, r, nil)
			if err != nil {
				return
			}
			defer ws.CloseNow()

			for i := range 2 {
				msg := map[string]interface{}{
					"accept": map[string]interface{}{
						"address": rendezvousAddr,
						"id":      fmt.Sprintf("test-id-%d", i),
					},
				}
				data, _ := json.Marshal(msg)
				if err := ws.Write(r.Context(), websocket.MessageText, data); err != nil {
					return
				}
			}

			// Wait until handlers have been called, then close to end the loop.
			deadline := time.After(3 * time.Second)
			for {
				select {
				case <-deadline:
					ws.Close(websocket.StatusNormalClosure, "timeout")
					return
				case <-time.After(50 * time.Millisecond):
					if len(handlerCalls) >= 2 {
						time.Sleep(100 * time.Millisecond)
						ws.Close(websocket.StatusNormalClosure, "done")
						return
					}
				}
			}
		}))

		// Use a short context timeout. After the server closes the WS,
		// runControlLoop's defer wg.Wait() blocks until the context
		// cancels the internal ping/renew goroutines.
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		tp := &mockTokenProvider{token: "test-token"}

		// Set Endpoint to bare host:port so EndpointToWSS prepends wss://.
		cfg := ControlConfig{
			Endpoint:      testEndpoint(controlSrv),
			EntityPath:    "test-entity",
			TokenProvider: tp,
			Handler: func(ctx context.Context, ws *websocket.Conn) {
				handlerCalls <- "called"
			},
			DialTimeout: 2 * time.Second,
			Logger:      discardLogger(),
		}

		_, err := runControlLoop(ctx, cfg)
		// runControlLoop returns error when server closes the WebSocket.
		if err == nil {
			t.Fatal("expected error from runControlLoop when server closes")
		}

		close(handlerCalls)
		var count int
		for range handlerCalls {
			count++
		}
		if count != 2 {
			t.Errorf("handler called %d times, want 2", count)
		}
	})

	t.Run("ignores non-accept messages", func(t *testing.T) {
		handlerCalls := make(chan string, 10)

		rendezvousSrv := tlsServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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

		controlSrv := tlsServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ws, err := websocket.Accept(w, r, nil)
			if err != nil {
				return
			}
			defer ws.CloseNow()

			// Non-accept message.
			ws.Write(r.Context(), websocket.MessageText, []byte(`{"status":"ok"}`))
			// Invalid JSON.
			ws.Write(r.Context(), websocket.MessageText, []byte(`not json`))
			// Null accept.
			ws.Write(r.Context(), websocket.MessageText, []byte(`{"accept":null}`))
			// Valid accept.
			msg := map[string]interface{}{
				"accept": map[string]interface{}{
					"address": "wss://" + testEndpoint(rendezvousSrv),
					"id":      "valid-id",
				},
			}
			data, _ := json.Marshal(msg)
			ws.Write(r.Context(), websocket.MessageText, data)

			deadline := time.After(5 * time.Second)
			for {
				select {
				case <-deadline:
					ws.Close(websocket.StatusNormalClosure, "timeout")
					return
				case <-time.After(50 * time.Millisecond):
					if len(handlerCalls) >= 1 {
						time.Sleep(100 * time.Millisecond)
						ws.Close(websocket.StatusNormalClosure, "done")
						return
					}
				}
			}
		}))

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		tp := &mockTokenProvider{token: "test-token"}
		cfg := ControlConfig{
			Endpoint:      testEndpoint(controlSrv),
			EntityPath:    "test-entity",
			TokenProvider: tp,
			Handler: func(ctx context.Context, ws *websocket.Conn) {
				handlerCalls <- "called"
			},
			DialTimeout: 2 * time.Second,
			Logger:      discardLogger(),
		}

		_, _ = runControlLoop(ctx, cfg)

		close(handlerCalls)
		var count int
		for range handlerCalls {
			count++
		}
		if count != 1 {
			t.Errorf("handler called %d times, want 1 (only valid accept)", count)
		}
	})

	t.Run("respects max connections", func(t *testing.T) {
		handlerStarted := make(chan struct{}, 10)

		rendezvousSrv := tlsServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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

		controlSrv := tlsServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ws, err := websocket.Accept(w, r, nil)
			if err != nil {
				return
			}
			defer ws.CloseNow()

			// Send 3 accept messages but max connections is 1.
			for i := range 3 {
				msg := map[string]interface{}{
					"accept": map[string]interface{}{
						"address": "wss://" + testEndpoint(rendezvousSrv),
						"id":      fmt.Sprintf("id-%d", i),
					},
				}
				data, _ := json.Marshal(msg)
				if err := ws.Write(r.Context(), websocket.MessageText, data); err != nil {
					return
				}
				// Delay to ensure the first handler is started and the semaphore is acquired
				// before subsequent accept messages arrive.
				time.Sleep(200 * time.Millisecond)
			}

			// Wait a bit, then close.
			time.Sleep(500 * time.Millisecond)
			ws.Close(websocket.StatusNormalClosure, "done")
		}))

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		tp := &mockTokenProvider{token: "test-token"}
		cfg := ControlConfig{
			Endpoint:       testEndpoint(controlSrv),
			EntityPath:     "test-entity",
			TokenProvider:  tp,
			MaxConnections: 1,
			Handler: func(ctx context.Context, ws *websocket.Conn) {
				handlerStarted <- struct{}{}
				// Block until context is cancelled (when runControlLoop exits).
				<-ctx.Done()
			},
			DialTimeout: 2 * time.Second,
			Logger:      discardLogger(),
		}

		_, err := runControlLoop(ctx, cfg)
		// runControlLoop returns error when server closes the WebSocket.
		// On return, defer loopCancel() cancels the context, which unblocks handlers.
		// Then defer wg.Wait() waits for handler goroutines to finish.
		if err == nil {
			t.Fatal("expected error from runControlLoop")
		}

		close(handlerStarted)
		var startCount int
		for range handlerStarted {
			startCount++
		}
		if startCount != 1 {
			t.Errorf("handler started %d times, want 1 (max connections = 1)", startCount)
		}
	})

	t.Run("get token failure returns error", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		tp := &mockTokenProvider{err: fmt.Errorf("auth failure")}

		cfg := ControlConfig{
			Endpoint:      "127.0.0.1:1",
			EntityPath:    "test-entity",
			TokenProvider: tp,
			Handler:       func(ctx context.Context, ws *websocket.Conn) {},
			DialTimeout:   1 * time.Second,
			Logger:        discardLogger(),
		}

		_, err := runControlLoop(ctx, cfg)
		if err == nil {
			t.Fatal("expected error on token failure")
		}
		if !strings.Contains(err.Error(), "get token") {
			t.Errorf("unexpected error: %v", err)
		}
	})

	t.Run("dial failure returns error", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		tp := &mockTokenProvider{token: "test-token"}

		cfg := ControlConfig{
			Endpoint:      "127.0.0.1:1",
			EntityPath:    "test-entity",
			TokenProvider: tp,
			Handler:       func(ctx context.Context, ws *websocket.Conn) {},
			DialTimeout:   1 * time.Second,
			Logger:        discardLogger(),
		}

		_, err := runControlLoop(ctx, cfg)
		if err == nil {
			t.Fatal("expected error on dial failure")
		}
		if !strings.Contains(err.Error(), "dial control") {
			t.Errorf("unexpected error: %v", err)
		}
	})
}

// ---------- TestListenAndServe ----------

func TestListenAndServe(t *testing.T) {
	t.Run("exits on context cancel", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())

		tp := &mockTokenProvider{err: fmt.Errorf("fail")}

		cfg := ControlConfig{
			Endpoint:      "127.0.0.1:1",
			EntityPath:    "test-entity",
			TokenProvider: tp,
			Handler:       func(ctx context.Context, ws *websocket.Conn) {},
			DialTimeout:   500 * time.Millisecond,
			Logger:        discardLogger(),
		}

		done := make(chan error, 1)
		go func() {
			done <- ListenAndServe(ctx, cfg)
		}()

		// Cancel immediately — the first runControlLoop call will fail (bad token),
		// then ListenAndServe checks ctx.Err() and returns.
		time.Sleep(100 * time.Millisecond)
		cancel()

		select {
		case err := <-done:
			if err != context.Canceled {
				t.Errorf("expected context.Canceled, got %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("ListenAndServe did not exit after context cancel")
		}
	})

	t.Run("reconnects after control loop failure", func(t *testing.T) {
		// Use a token provider that tracks call count. Each call to
		// runControlLoop calls GetToken, so the number of GetToken calls
		// equals the number of reconnect attempts. We make the token
		// provider return a token but point to an unreachable endpoint,
		// so runControlLoop fails at the dial step and returns immediately
		// (no 30s ping wait).
		var tokenCalls atomic.Int32
		tp := &mockTokenProvider{
			tokenFn: func(ctx context.Context, resourceURI string) (string, error) {
				tokenCalls.Add(1)
				return "test-token", nil
			},
		}

		cfg := ControlConfig{
			// Point to a port that refuses connections so dial fails fast.
			Endpoint:      "127.0.0.1:1",
			EntityPath:    "test-entity",
			TokenProvider: tp,
			Handler:       func(ctx context.Context, ws *websocket.Conn) {},
			DialTimeout:   500 * time.Millisecond,
			Logger:        discardLogger(),
		}

		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()

		done := make(chan error, 1)
		go func() {
			done <- ListenAndServe(ctx, cfg)
		}()

		// Wait for at least 2 token calls (proves reconnection loop).
		// First attempt is immediate, second is after 5s reconnect delay.
		deadline := time.After(12 * time.Second)
		for tokenCalls.Load() < 2 {
			select {
			case <-deadline:
				t.Fatalf("only %d reconnect attempts, want >= 2", tokenCalls.Load())
			case <-time.After(100 * time.Millisecond):
			}
		}

		cancel()

		select {
		case err := <-done:
			if err != context.Canceled {
				t.Errorf("expected context.Canceled, got %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("ListenAndServe did not exit")
		}
	})

	t.Run("sets default logger and dial timeout", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel() // cancel immediately

		tp := &mockTokenProvider{token: "test-token"}

		cfg := ControlConfig{
			Endpoint:      "127.0.0.1:1",
			EntityPath:    "test-entity",
			TokenProvider: tp,
			Handler:       func(ctx context.Context, ws *websocket.Conn) {},
			// Logger and DialTimeout intentionally not set.
		}

		err := ListenAndServe(ctx, cfg)
		if err != context.Canceled {
			t.Errorf("expected context.Canceled, got %v", err)
		}
	})
}

// ---------- TestControlSessionID ----------

// captureLogger returns a slog logger that writes JSON records to the
// returned recorder. Records are returned in the order they were
// emitted; the recorder is safe for concurrent writers.
type logRecorder struct {
	mu  sync.Mutex
	buf []byte
}

func (r *logRecorder) Write(p []byte) (int, error) {
	r.mu.Lock()
	r.buf = append(r.buf, p...)
	r.mu.Unlock()
	return len(p), nil
}

func (r *logRecorder) records(t *testing.T) []map[string]any {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	lines := strings.Split(strings.TrimRight(string(r.buf), "\n"), "\n")
	out := make([]map[string]any, 0, len(lines))
	for _, line := range lines {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("non-JSON log line: %q: %v", line, err)
		}
		out = append(out, rec)
	}
	return out
}

func captureLogger() (*slog.Logger, *logRecorder) {
	rec := &logRecorder{}
	return slog.New(slog.NewJSONHandler(rec, &slog.HandlerOptions{Level: slog.LevelDebug})), rec
}

// drivenControlLoop spins up a TLS test relay that accepts one
// rendezvous request then closes the control WebSocket, and runs
// runControlLoop with the supplied logger. The caller wires a
// logRecorder behind the logger to capture every log line emitted
// from the per-session function.
func drivenControlLoop(t *testing.T, logger *slog.Logger) {
	t.Helper()

	rendezvousSrv := tlsServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	rendezvousAddr := "wss://" + testEndpoint(rendezvousSrv)

	handlerDone := make(chan struct{}, 1)
	controlSrv := tlsServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer ws.CloseNow()
		msg := map[string]interface{}{
			"accept": map[string]interface{}{
				"address": rendezvousAddr,
				"id":      "session-id-test",
			},
		}
		data, _ := json.Marshal(msg)
		if err := ws.Write(r.Context(), websocket.MessageText, data); err != nil {
			return
		}
		// Wait until the listener's handler has been invoked, then
		// close so runControlLoop returns. Bounded so a stuck
		// handler can't hang the test indefinitely.
		select {
		case <-handlerDone:
		case <-time.After(3 * time.Second):
		}
		ws.Close(websocket.StatusNormalClosure, "done")
	}))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	cfg := ControlConfig{
		Endpoint:      testEndpoint(controlSrv),
		EntityPath:    "test-entity",
		TokenProvider: &mockTokenProvider{token: "test-token"},
		Handler: func(ctx context.Context, ws *websocket.Conn) {
			select {
			case handlerDone <- struct{}{}:
			default:
			}
		},
		DialTimeout: 2 * time.Second,
		Logger:      logger,
	}

	_, err := runControlLoop(ctx, cfg)
	if err == nil {
		t.Fatal("expected error from runControlLoop when server closes")
	}
}

// TestControlSessionID_StableWithinLoop drives one runControlLoop
// invocation end-to-end (connect → one accept → server-initiated
// close), captures every log line, and asserts that:
//
//   - every record carries a non-empty control_session_id;
//   - every record carries the SAME control_session_id;
//   - the canonical lifecycle lines ("control loop started",
//     "control channel connected", "control loop ended") are all
//     present and tagged.
//
// This is the per-session invariant operators rely on to mechanically
// separate one control-loop run from the next.
func TestControlSessionID_StableWithinLoop(t *testing.T) {
	useInsecureTransport(t)

	logger, rec := captureLogger()
	drivenControlLoop(t, logger)

	records := rec.records(t)
	if len(records) < 3 {
		t.Fatalf("expected at least 3 log records (started + connected + ended), got %d: %s", len(records), string(rec.buf))
	}

	var sessionID string
	for i, r := range records {
		id, _ := r["control_session_id"].(string)
		if id == "" {
			t.Errorf("record %d missing control_session_id: %v", i, r)
			continue
		}
		if sessionID == "" {
			sessionID = id
		} else if id != sessionID {
			t.Errorf("record %d has control_session_id=%q, want %q", i, id, sessionID)
		}
	}

	wantMsgs := []string{"control loop started", "control channel connected", "control loop ended"}
	for _, want := range wantMsgs {
		found := false
		for _, r := range records {
			if msg, _ := r["msg"].(string); msg == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("missing lifecycle log line %q in captured output:\n%s", want, string(rec.buf))
		}
	}
}

// TestControlSessionID_ChangesAcrossRestarts drives runControlLoop
// twice in sequence — each invocation captures into its own buffer —
// and asserts the two minted control_session_id values differ. This
// is the cross-restart invariant: every reconnect gets a fresh id so
// operators can tell "before the disconnect" from "after the
// disconnect" log streams.
func TestControlSessionID_ChangesAcrossRestarts(t *testing.T) {
	useInsecureTransport(t)

	logger1, rec1 := captureLogger()
	drivenControlLoop(t, logger1)
	id1 := extractSessionID(t, rec1)

	logger2, rec2 := captureLogger()
	drivenControlLoop(t, logger2)
	id2 := extractSessionID(t, rec2)

	if id1 == "" || id2 == "" {
		t.Fatalf("missing ids: id1=%q id2=%q", id1, id2)
	}
	if id1 == id2 {
		t.Fatalf("expected distinct control_session_id across runs, got %q twice", id1)
	}
}

func extractSessionID(t *testing.T, rec *logRecorder) string {
	t.Helper()
	for _, r := range rec.records(t) {
		if id, _ := r["control_session_id"].(string); id != "" {
			return id
		}
	}
	t.Fatalf("no control_session_id in captured records: %s", string(rec.buf))
	return ""
}

// ---------- TestAcceptID ----------

// capStore is the shared backing store for captureHandler clones
// produced by With/WithAttrs/WithGroup. Records always end up in the
// same slice no matter which derived handler emits them.
type capStore struct {
	mu      sync.Mutex
	records []capRecord
}

// capRecord is a single captured log emission with its attribute map
// flattened across With(...) and Record-level attrs. Tests query
// attrs by key (e.g. attrs["accept_id"]) instead of scraping the
// rendered text.
type capRecord struct {
	level slog.Level
	msg   string
	attrs map[string]any
}

// captureHandler is a slog.Handler that snapshots every record into a
// shared capStore. Used to assert structured attributes (accept_id,
// reason, ok) on lifecycle log lines.
type captureHandler struct {
	store *capStore
	attrs []slog.Attr
}

func newCaptureHandler() (*slog.Logger, *capStore) {
	s := &capStore{}
	return slog.New(&captureHandler{store: s}), s
}

func (h *captureHandler) Enabled(_ context.Context, _ slog.Level) bool { return true }

func (h *captureHandler) Handle(_ context.Context, r slog.Record) error {
	rec := capRecord{
		level: r.Level,
		msg:   r.Message,
		attrs: make(map[string]any, len(h.attrs)+r.NumAttrs()),
	}
	for _, a := range h.attrs {
		rec.attrs[a.Key] = a.Value.Any()
	}
	r.Attrs(func(a slog.Attr) bool {
		rec.attrs[a.Key] = a.Value.Any()
		return true
	})
	h.store.mu.Lock()
	h.store.records = append(h.store.records, rec)
	h.store.mu.Unlock()
	return nil
}

func (h *captureHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	combined := make([]slog.Attr, 0, len(h.attrs)+len(attrs))
	combined = append(combined, h.attrs...)
	combined = append(combined, attrs...)
	return &captureHandler{store: h.store, attrs: combined}
}

func (h *captureHandler) WithGroup(_ string) slog.Handler {
	// Groups are unused in the relay package; preserve attrs unchanged
	// so tests still see accept_id without group prefixing.
	return &captureHandler{store: h.store, attrs: h.attrs}
}

// snapshot returns a copy of the captured records under the store
// lock, so callers can iterate without racing the producer goroutines.
func (s *capStore) snapshot() []capRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]capRecord, len(s.records))
	copy(out, s.records)
	return out
}

// filterByMsg returns every captured record whose message equals msg.
func (s *capStore) filterByMsg(msg string) []capRecord {
	var out []capRecord
	for _, r := range s.snapshot() {
		if r.msg == msg {
			out = append(out, r)
		}
	}
	return out
}

// waitForRecord polls until at least one record matches msg, or the
// deadline expires. Returns the first match, or zero-value + false on
// timeout. Used to synchronise the test on lifecycle events (e.g.
// wait until "accept acquired" is logged before sending the next
// accept message) so the suite doesn't rely on time.Sleep heuristics.
func (s *capStore) waitForRecord(msg string, timeout time.Duration) (capRecord, bool) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if recs := s.filterByMsg(msg); len(recs) > 0 {
			return recs[0], true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return capRecord{}, false
}

// TestAcceptID_StableAcrossLifecycle drives one accept through the
// happy path (acquire → dial start → dial complete → release) and
// asserts every lifecycle log line carries the same accept_id, so
// operators can group them with a single filter.
func TestAcceptID_StableAcrossLifecycle(t *testing.T) {
	useInsecureTransport(t)

	rendezvousSrv := tlsServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer ws.CloseNow()
		// Block until the handler is done and the client closes.
		for {
			if _, _, err := ws.Read(r.Context()); err != nil {
				return
			}
		}
	}))
	rendezvousAddr := "wss://" + testEndpoint(rendezvousSrv)

	handlerDone := make(chan struct{})
	controlSrv := tlsServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer ws.CloseNow()

		msg := map[string]interface{}{
			"accept": map[string]interface{}{
				"address": rendezvousAddr,
				"id":      "test-id-0",
			},
		}
		data, _ := json.Marshal(msg)
		if err := ws.Write(r.Context(), websocket.MessageText, data); err != nil {
			return
		}
		// Wait for the in-process accept handler to finish, then close
		// the control channel so runControlLoop returns and any
		// deferred "accept released" line gets flushed before the
		// test inspects the capture store.
		select {
		case <-handlerDone:
		case <-time.After(3 * time.Second):
		}
		ws.Close(websocket.StatusNormalClosure, "done")
	}))

	// Cap test runtime well above the expected lifecycle round-trip
	// but cancel proactively as soon as the expected records are
	// observed (runControlLoop's deferred wg.Wait would otherwise sit
	// on the renew/ping goroutines, which only exit on ctx.Done()).
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	logger, store := newCaptureHandler()
	tp := &mockTokenProvider{token: "test-token"}
	cfg := ControlConfig{
		Endpoint:      testEndpoint(controlSrv),
		EntityPath:    "test-entity",
		TokenProvider: tp,
		Handler: func(ctx context.Context, ws *websocket.Conn) {
			close(handlerDone)
		},
		DialTimeout: 2 * time.Second,
		Logger:      logger,
	}

	loopErrCh := make(chan error, 1)
	go func() {
		_, err := runControlLoop(ctx, cfg)
		loopErrCh <- err
	}()

	// Wait for the deferred release line: that's the last lifecycle
	// event for the accepted attempt and arrives after the goroutine
	// returns. Once seen, cancel ctx so the renew/ping goroutines
	// exit and runControlLoop unwinds promptly.
	if _, ok := store.waitForRecord("accept released", 10*time.Second); !ok {
		cancel()
		<-loopErrCh
		t.Fatalf("never observed 'accept released' (records=%v)", store.snapshot())
	}
	cancel()
	if err := <-loopErrCh; err == nil {
		t.Fatal("expected error from runControlLoop")
	}

	lifecycle := []string{"accept acquired", "accept dial started", "accept dial complete", "accept released"}
	var acceptID string
	for _, msg := range lifecycle {
		recs := store.filterByMsg(msg)
		if len(recs) == 0 {
			t.Fatalf("no captured record with msg=%q (have %d total records)", msg, len(store.snapshot()))
		}
		id, ok := recs[0].attrs["accept_id"].(string)
		if !ok || id == "" {
			t.Fatalf("record msg=%q missing accept_id attribute (attrs=%v)", msg, recs[0].attrs)
		}
		if acceptID == "" {
			acceptID = id
		} else if id != acceptID {
			t.Fatalf("record msg=%q accept_id=%q differs from first accept_id=%q — lifecycle lines must share one ID",
				msg, id, acceptID)
		}
	}

	// Dial-complete on the happy path reports ok=true.
	dialComplete := store.filterByMsg("accept dial complete")
	if got, want := dialComplete[0].attrs["ok"], true; got != want {
		t.Errorf("accept dial complete: ok=%v, want %v", got, want)
	}

	// accept_id is a 16-char base32 [A-Z2-7] string (idgen contract).
	if len(acceptID) != 16 {
		t.Errorf("accept_id %q: want 16 base32 chars", acceptID)
	}
	for _, c := range acceptID {
		ok := (c >= 'A' && c <= 'Z') || (c >= '2' && c <= '7')
		if !ok {
			t.Errorf("accept_id %q has non-base32 char %q", acceptID, c)
			break
		}
	}
}

// TestAcceptID_PresentOnDrop saturates the listener-side semaphore
// (MaxConnections=1, first accept blocks in-handler), then sends a
// second accept that must drop. The dropped log line must carry an
// accept_id distinct from the accepted one, plus a structured drop
// reason — operators can filter on the dropped accept_id and find
// no further lifecycle lines for it.
func TestAcceptID_PresentOnDrop(t *testing.T) {
	useInsecureTransport(t)

	rendezvousSrv := tlsServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	rendezvousAddr := "wss://" + testEndpoint(rendezvousSrv)

	logger, store := newCaptureHandler()

	// Buffered so the in-process handler's send always completes —
	// the unbuffered+default form silently lost the only sync signal
	// when the receiver hadn't yet reached its select case.
	handlerEntered := make(chan struct{}, 1)
	controlSrv := tlsServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer ws.CloseNow()

		send := func(id string) error {
			msg := map[string]interface{}{
				"accept": map[string]interface{}{
					"address": rendezvousAddr,
					"id":      id,
				},
			}
			data, _ := json.Marshal(msg)
			return ws.Write(r.Context(), websocket.MessageText, data)
		}

		// First accept: must be acquired and held by the in-process
		// handler (which blocks on context).
		if err := send("first"); err != nil {
			return
		}
		// Wait until the first handler actually entered (i.e. the
		// semaphore slot is held) before sending the second accept.
		// This removes the time.Sleep race that a slow CI runner
		// could lose, where the second accept arrives before the
		// first handler grabs the semaphore.
		select {
		case <-handlerEntered:
		case <-time.After(5 * time.Second):
			ws.Close(websocket.StatusNormalClosure, "first handler never entered")
			return
		}
		// Second accept: semaphore is now full, must be dropped.
		if err := send("second"); err != nil {
			return
		}

		// Wait for the drop to be logged before closing so the test
		// can observe it on the capture store deterministically.
		if _, ok := store.waitForRecord("accept dropped", 5*time.Second); !ok {
			ws.Close(websocket.StatusNormalClosure, "drop never logged")
			return
		}
		ws.Close(websocket.StatusNormalClosure, "done")
	}))

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	tp := &mockTokenProvider{token: "test-token"}
	cfg := ControlConfig{
		Endpoint:       testEndpoint(controlSrv),
		EntityPath:     "test-entity",
		TokenProvider:  tp,
		MaxConnections: 1,
		Handler: func(ctx context.Context, ws *websocket.Conn) {
			// Signal that we entered (the semaphore slot is held)
			// then block until the control loop tears us down. The
			// buffered+default pattern keeps the send non-blocking
			// even though the buffer guarantees it always lands.
			select {
			case handlerEntered <- struct{}{}:
			default:
			}
			<-ctx.Done()
		},
		DialTimeout: 2 * time.Second,
		Logger:      logger,
	}

	loopErrCh := make(chan error, 1)
	go func() {
		_, err := runControlLoop(ctx, cfg)
		loopErrCh <- err
	}()

	// Wait for both the acquired and dropped log lines, then cancel
	// ctx to unwind runControlLoop promptly.
	if _, ok := store.waitForRecord("accept acquired", 10*time.Second); !ok {
		cancel()
		<-loopErrCh
		t.Fatalf("never observed 'accept acquired'")
	}
	if _, ok := store.waitForRecord("accept dropped", 10*time.Second); !ok {
		cancel()
		<-loopErrCh
		t.Fatalf("never observed 'accept dropped' (records=%v)", store.snapshot())
	}
	cancel()
	if err := <-loopErrCh; err == nil {
		t.Fatal("expected error from runControlLoop when server closes")
	}

	acquired := store.filterByMsg("accept acquired")
	dropped := store.filterByMsg("accept dropped")
	if len(acquired) == 0 {
		t.Fatalf("no 'accept acquired' record captured (have %d records total)", len(store.snapshot()))
	}
	if len(dropped) == 0 {
		t.Fatalf("no 'accept dropped' record captured (have %d records total)", len(store.snapshot()))
	}

	acqID, _ := acquired[0].attrs["accept_id"].(string)
	dropID, _ := dropped[0].attrs["accept_id"].(string)
	if acqID == "" {
		t.Errorf("accept acquired missing accept_id (attrs=%v)", acquired[0].attrs)
	}
	if dropID == "" {
		t.Errorf("accept dropped missing accept_id (attrs=%v)", dropped[0].attrs)
	}
	if acqID == dropID {
		t.Errorf("accept_id %q reused across accept lifecycle — each accept attempt must mint its own", acqID)
	}
	if got, want := dropped[0].attrs["reason"], "semaphore_full"; got != want {
		t.Errorf("accept dropped reason=%v, want %v", got, want)
	}
	if got, want := dropped[0].level, slog.LevelWarn; got != want {
		t.Errorf("accept dropped level=%v, want %v (drops are operator-actionable)", got, want)
	}
}
