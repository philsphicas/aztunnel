package relay

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// dialTestServer creates a TLS httptest server and configures http.DefaultTransport
// to trust its certificate. It returns the server and a cleanup function that
// restores the original transport.
func dialTestServer(t *testing.T, handler http.Handler) *httptest.Server {
	t.Helper()
	srv := httptest.NewTLSServer(handler)

	// Inject the test server's TLS config into http.DefaultTransport so that
	// websocket.Dial (which uses http.DefaultClient when options are nil)
	// trusts the self-signed cert.
	origTransport := http.DefaultTransport
	http.DefaultTransport = &http.Transport{
		TLSClientConfig: &tls.Config{
			//nolint:gosec // G402: test-only, self-signed cert from httptest
			InsecureSkipVerify: true,
		},
	}
	t.Cleanup(func() {
		http.DefaultTransport = origTransport
		srv.Close()
	})

	return srv
}

func TestDial(t *testing.T) {
	t.Run("successful connection", func(t *testing.T) {
		var gotPath string
		var gotToken string

		srv := dialTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotPath = r.URL.Path
			gotToken = r.URL.Query().Get("sb-hc-token")

			ws, err := websocket.Accept(w, r, nil)
			if err != nil {
				t.Logf("accept: %v", err)
				return
			}
			defer ws.CloseNow()
			<-r.Context().Done()
		}))

		tp := &mockTokenProvider{token: "test-sas-token"}
		// Strip scheme so EndpointToWSS prepends wss://.
		endpoint := strings.TrimPrefix(srv.URL, "https://")

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		ws, err := Dial(ctx, endpoint, "my-entity", tp)
		if err != nil {
			t.Fatalf("Dial: %v", err)
		}
		defer ws.CloseNow()

		wantPath := "/$hc/" + url.PathEscape("my-entity")
		if gotPath != wantPath {
			t.Errorf("path = %q, want %q", gotPath, wantPath)
		}
		if gotToken == "" {
			t.Error("expected sb-hc-token query parameter, got empty")
		}
		if tp.getCalls() != 1 {
			t.Errorf("token provider calls = %d, want 1", tp.getCalls())
		}
	})

	t.Run("token provider error", func(t *testing.T) {
		tp := &mockTokenProvider{err: fmt.Errorf("auth failed")}

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		_, err := Dial(ctx, "test.servicebus.windows.net", "my-entity", tp)
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		if !strings.Contains(err.Error(), "get token") {
			t.Errorf("error %q does not contain %q", err.Error(), "get token")
		}
	})

	t.Run("dial error for unreachable host", func(t *testing.T) {
		tp := &mockTokenProvider{token: "test-token"}

		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()

		_, err := Dial(ctx, "127.0.0.1:1", "my-entity", tp)
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		if !strings.Contains(err.Error(), "dial relay") {
			t.Errorf("error %q does not contain %q", err.Error(), "dial relay")
		}
	})

	t.Run("cancelled context", func(t *testing.T) {
		tp := &mockTokenProvider{token: "test-token"}

		ctx, cancel := context.WithCancel(context.Background())
		cancel() // cancel immediately

		_, err := Dial(ctx, "127.0.0.1:1", "my-entity", tp)
		if err == nil {
			t.Fatal("expected error for cancelled context, got nil")
		}
	})

	t.Run("entity path with special characters", func(t *testing.T) {
		var gotRawPath string

		srv := dialTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotRawPath = r.URL.RawPath
			if gotRawPath == "" {
				gotRawPath = r.URL.Path
			}

			ws, err := websocket.Accept(w, r, nil)
			if err != nil {
				return
			}
			defer ws.CloseNow()
			<-r.Context().Done()
		}))

		tp := &mockTokenProvider{token: "tok"}
		endpoint := strings.TrimPrefix(srv.URL, "https://")

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		ws, err := Dial(ctx, endpoint, "my entity/path", tp)
		if err != nil {
			t.Fatalf("Dial: %v", err)
		}
		defer ws.CloseNow()

		// Verify entity path is present (URL-encoded) in the request path.
		if !strings.Contains(gotRawPath, "my%20entity") {
			t.Errorf("entity path not properly escaped in URL: %q", gotRawPath)
		}
	})
}

func TestDialWithLogger(t *testing.T) {
	t.Run("success logs debug messages", func(t *testing.T) {
		var logBuf strings.Builder
		logger := slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))

		srv := dialTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ws, err := websocket.Accept(w, r, nil)
			if err != nil {
				return
			}
			defer ws.CloseNow()
			<-r.Context().Done()
		}))

		tp := &mockTokenProvider{token: "test-token"}
		endpoint := strings.TrimPrefix(srv.URL, "https://")

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		ws, err := DialWithLogger(ctx, endpoint, "test-entity", tp, logger)
		if err != nil {
			t.Fatalf("DialWithLogger: %v", err)
		}
		defer ws.CloseNow()

		output := logBuf.String()
		if !strings.Contains(output, "dialing relay") {
			t.Errorf("log output missing %q: %s", "dialing relay", output)
		}
		if !strings.Contains(output, "relay connected") {
			t.Errorf("log output missing %q: %s", "relay connected", output)
		}
	})

	t.Run("non-retryable failure logs warning", func(t *testing.T) {
		var logBuf strings.Builder
		logger := slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))

		tp := &mockTokenProvider{err: fmt.Errorf("token error")}

		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()

		_, err := DialWithLogger(ctx, "127.0.0.1:1", "test-entity", tp, logger)
		if err == nil {
			t.Fatal("expected error, got nil")
		}

		output := logBuf.String()
		if !strings.Contains(output, "dialing relay") {
			t.Errorf("log output missing %q: %s", "dialing relay", output)
		}
		if !strings.Contains(output, "relay dial failed") {
			t.Errorf("log output missing %q: %s", "relay dial failed", output)
		}
	})
}

func TestDialWithRetry(t *testing.T) {
	t.Run("retries on 404 then succeeds", func(t *testing.T) {
		var mu sync.Mutex
		attempts := 0

		srv := dialTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			attempts++
			n := attempts
			mu.Unlock()

			if n <= 2 {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			ws, err := websocket.Accept(w, r, nil)
			if err != nil {
				return
			}
			defer ws.CloseNow()
			<-r.Context().Done()
		}))

		tp := &mockTokenProvider{token: "test-token"}
		endpoint := strings.TrimPrefix(srv.URL, "https://")
		logger := slog.New(slog.NewTextHandler(io.Discard, nil))

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		ws, err := DialWithRetry(ctx, endpoint, "test-entity", tp, logger)
		if err != nil {
			t.Fatalf("DialWithRetry: %v", err)
		}
		defer ws.CloseNow()

		mu.Lock()
		got := attempts
		mu.Unlock()
		if got < 3 {
			t.Errorf("expected at least 3 attempts, got %d", got)
		}
	})

	t.Run("retries on 503 then succeeds", func(t *testing.T) {
		var mu sync.Mutex
		attempts := 0

		srv := dialTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			attempts++
			n := attempts
			mu.Unlock()

			if n == 1 {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			ws, err := websocket.Accept(w, r, nil)
			if err != nil {
				return
			}
			defer ws.CloseNow()
			<-r.Context().Done()
		}))

		tp := &mockTokenProvider{token: "test-token"}
		endpoint := strings.TrimPrefix(srv.URL, "https://")
		logger := slog.New(slog.NewTextHandler(io.Discard, nil))

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		ws, err := DialWithRetry(ctx, endpoint, "test-entity", tp, logger)
		if err != nil {
			t.Fatalf("DialWithRetry: %v", err)
		}
		defer ws.CloseNow()

		mu.Lock()
		got := attempts
		mu.Unlock()
		if got != 2 {
			t.Errorf("expected 2 attempts, got %d", got)
		}
	})

	t.Run("context cancelled during retry returns last error", func(t *testing.T) {
		srv := dialTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNotFound)
		}))

		tp := &mockTokenProvider{token: "test-token"}
		endpoint := strings.TrimPrefix(srv.URL, "https://")
		logger := slog.New(slog.NewTextHandler(io.Discard, nil))

		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()

		_, err := DialWithRetry(ctx, endpoint, "test-entity", tp, logger)
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		if !strings.Contains(err.Error(), "dial relay") {
			t.Errorf("error %q should contain 'dial relay'", err.Error())
		}
	})

	t.Run("401 fails immediately without retry", func(t *testing.T) {
		var mu sync.Mutex
		attempts := 0

		srv := dialTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			attempts++
			mu.Unlock()
			w.WriteHeader(http.StatusUnauthorized)
		}))

		tp := &mockTokenProvider{token: "test-token"}
		endpoint := strings.TrimPrefix(srv.URL, "https://")
		logger := slog.New(slog.NewTextHandler(io.Discard, nil))

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		_, err := DialWithRetry(ctx, endpoint, "test-entity", tp, logger)
		if err == nil {
			t.Fatal("expected error, got nil")
		}

		mu.Lock()
		got := attempts
		mu.Unlock()
		if got != 1 {
			t.Errorf("expected exactly 1 attempt (no retry), got %d", got)
		}
	})

	t.Run("connection refused fails immediately without retry", func(t *testing.T) {
		tp := &mockTokenProvider{token: "test-token"}
		logger := slog.New(slog.NewTextHandler(io.Discard, nil))

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		_, err := DialWithRetry(ctx, "127.0.0.1:1", "test-entity", tp, logger)
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		if !strings.Contains(err.Error(), "dial relay") {
			t.Errorf("error %q should contain 'dial relay'", err.Error())
		}
	})

	t.Run("logs retry attempts", func(t *testing.T) {
		var mu sync.Mutex
		attempts := 0

		srv := dialTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			attempts++
			n := attempts
			mu.Unlock()

			if n == 1 {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			ws, err := websocket.Accept(w, r, nil)
			if err != nil {
				return
			}
			defer ws.CloseNow()
			<-r.Context().Done()
		}))

		var logBuf strings.Builder
		logger := slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))

		tp := &mockTokenProvider{token: "test-token"}
		endpoint := strings.TrimPrefix(srv.URL, "https://")

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		ws, err := DialWithRetry(ctx, endpoint, "test-entity", tp, logger)
		if err != nil {
			t.Fatalf("DialWithRetry: %v", err)
		}
		defer ws.CloseNow()

		output := logBuf.String()
		if !strings.Contains(output, "retrying") {
			t.Errorf("log output missing retry message: %s", output)
		}
		if !strings.Contains(output, "relay connected") {
			t.Errorf("log output missing connected message: %s", output)
		}
	})

	t.Run("GetToken failure on retry returns immediately", func(t *testing.T) {
		var mu sync.Mutex
		attempts := 0

		srv := dialTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNotFound)
		}))

		tp := &mockTokenProvider{
			tokenFn: func(_ context.Context, _ string) (string, error) {
				mu.Lock()
				defer mu.Unlock()
				attempts++
				if attempts > 1 {
					return "", fmt.Errorf("token renewal failed")
				}
				return "test-token", nil
			},
		}

		endpoint := strings.TrimPrefix(srv.URL, "https://")
		logger := slog.New(slog.NewTextHandler(io.Discard, nil))

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		_, err := DialWithRetry(ctx, endpoint, "test-entity", tp, logger)
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		if !strings.Contains(err.Error(), "get token") {
			t.Errorf("error %q should contain 'get token'", err.Error())
		}
	})
}
