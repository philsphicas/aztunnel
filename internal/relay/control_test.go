package relay

import (
	"context"
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

// wsURL converts an httptest.Server URL to a ws:// URL.
func wsURL(srv *httptest.Server) string {
	return "ws" + strings.TrimPrefix(srv.URL, "http")
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(discard{}, nil))
}

type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }

// ---------- TestHandleAccept ----------

func TestHandleAccept(t *testing.T) {
	t.Run("dials rendezvous and calls handler", func(t *testing.T) {
		handlerCalled := make(chan struct{})
		rendezvousSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
		defer rendezvousSrv.Close()

		cfg := ControlConfig{
			DialTimeout: 5 * time.Second,
			Logger:      discardLogger(),
			Handler: func(ctx context.Context, ws *websocket.Conn) {
				close(handlerCalled)
			},
		}

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		err := handleAccept(ctx, wsURL(rendezvousSrv), cfg)
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

		err := handleAccept(ctx, "ws://127.0.0.1:1", cfg)
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
	t.Run("successful renewal", func(t *testing.T) {
		received := make(chan map[string]interface{}, 1)
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
		defer srv.Close()

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		ws, _, err := websocket.Dial(ctx, wsURL(srv), nil)
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
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
		defer srv.Close()

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		ws, _, err := websocket.Dial(ctx, wsURL(srv), nil)
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

		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()

		ws, _, err := websocket.Dial(ctx, wsURL(srv), nil)
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
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ws, err := websocket.Accept(w, r, nil)
			if err != nil {
				return
			}
			ws.Close(websocket.StatusNormalClosure, "bye")
		}))
		defer srv.Close()

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		ws, _, err := websocket.Dial(ctx, wsURL(srv), nil)
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
	t.Run("ping failure calls cancel", func(t *testing.T) {
		// Create a server that accepts the WS and then shuts down entirely,
		// causing a network-level failure for the ping. The server must stay
		// alive long enough for the client to establish the WebSocket.
		srvReady := make(chan struct{})
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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

		ws, _, err := websocket.Dial(ctx, wsURL(srv), nil)
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

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		ws, _, err := websocket.Dial(ctx, wsURL(srv), nil)
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
	t.Run("exits on context cancel", func(t *testing.T) {
		// renewLoop uses renewInterval (45min) for its ticker, so we can't
		// easily trigger a tick in a test. Instead we verify that it exits
		// promptly when the context is cancelled.
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

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		ws, _, err := websocket.Dial(ctx, wsURL(srv), nil)
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
	t.Run("reads accept messages and spawns handlers", func(t *testing.T) {
		handlerCalls := make(chan string, 10)

		rendezvousSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
		defer rendezvousSrv.Close()

		rendezvousAddr := wsURL(rendezvousSrv)

		// Control server sends accept messages, waits for handlers, then closes.
		controlSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
		defer controlSrv.Close()

		// Use a short context timeout. After the server closes the WS,
		// runControlLoop's defer wg.Wait() blocks until the context
		// cancels the internal ping/renew goroutines.
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		tp := &mockTokenProvider{token: "test-token"}

		// Set Endpoint to ws://host:port so EndpointToWSS returns it unchanged
		// (no "sb://" prefix to replace). The server accepts any path.
		cfg := ControlConfig{
			Endpoint:      wsURL(controlSrv),
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

		rendezvousSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
		defer rendezvousSrv.Close()

		controlSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
					"address": wsURL(rendezvousSrv),
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
		defer controlSrv.Close()

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		tp := &mockTokenProvider{token: "test-token"}
		cfg := ControlConfig{
			Endpoint:      wsURL(controlSrv),
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

		rendezvousSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
		defer rendezvousSrv.Close()

		controlSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ws, err := websocket.Accept(w, r, nil)
			if err != nil {
				return
			}
			defer ws.CloseNow()

			// Send 3 accept messages but max connections is 1.
			for i := range 3 {
				msg := map[string]interface{}{
					"accept": map[string]interface{}{
						"address": wsURL(rendezvousSrv),
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
		defer controlSrv.Close()

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		tp := &mockTokenProvider{token: "test-token"}
		cfg := ControlConfig{
			Endpoint:       wsURL(controlSrv),
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
			Endpoint:      "ws://127.0.0.1:1",
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
			Endpoint:      "ws://127.0.0.1:1",
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
			Endpoint:      "ws://127.0.0.1:1",
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
			Endpoint:      "ws://127.0.0.1:1",
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
			Endpoint:      "ws://127.0.0.1:1",
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
