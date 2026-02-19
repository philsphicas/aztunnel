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
	"sync/atomic"
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

	t.Run("failure logs warning", func(t *testing.T) {
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
	t.Run("succeeds on first attempt", func(t *testing.T) {
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
		logger := slog.New(slog.NewTextHandler(io.Discard, nil))

		var retryCalls int
		onRetry := func() { retryCalls++ }

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		ws, err := DialWithRetry(ctx, endpoint, "my-entity", tp, 3, onRetry, logger)
		if err != nil {
			t.Fatalf("DialWithRetry: %v", err)
		}
		defer ws.CloseNow()

		if retryCalls != 0 {
			t.Errorf("onRetry called %d times, want 0", retryCalls)
		}
	})

	t.Run("succeeds after retries", func(t *testing.T) {
		// Server that rejects the first two connections and accepts the third.
		var connCount atomic.Int32
		srv := dialTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			n := connCount.Add(1)
			if n < 3 {
				http.Error(w, "not ready", http.StatusServiceUnavailable)
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

		var logBuf strings.Builder
		logger := slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))

		var retryCalls int
		onRetry := func() { retryCalls++ }

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		// The retry delays are hardcoded exponential backoff (1s, 2s, ...),
		// so this test is designed to succeed on the 3rd attempt after two
		// automatic retries. With retries=3 (4 total attempts), the test
		// verifies both retry count and eventual success.
		ws, err := DialWithRetry(ctx, endpoint, "my-entity", tp, 3, onRetry, logger)
		if err != nil {
			t.Fatalf("DialWithRetry: %v", err)
		}
		defer ws.CloseNow()

		if retryCalls != 2 {
			t.Errorf("onRetry called %d times, want 2", retryCalls)
		}
		if !strings.Contains(logBuf.String(), "retrying relay dial") {
			t.Errorf("log missing retry message: %s", logBuf.String())
		}
	})

	t.Run("fails after all retries exhausted", func(t *testing.T) {
		tp := &mockTokenProvider{token: "test-token"}
		logger := slog.New(slog.NewTextHandler(io.Discard, nil))

		var retryCalls int
		onRetry := func() { retryCalls++ }

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		_, err := DialWithRetry(ctx, "127.0.0.1:1", "my-entity", tp, 2, onRetry, logger)
		if err == nil {
			t.Fatal("expected error, got nil")
		}

		if retryCalls != 2 {
			t.Errorf("onRetry called %d times, want 2", retryCalls)
		}
	})

	t.Run("zero retries means one attempt only", func(t *testing.T) {
		tp := &mockTokenProvider{token: "test-token"}
		logger := slog.New(slog.NewTextHandler(io.Discard, nil))

		var retryCalls int
		onRetry := func() { retryCalls++ }

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		_, err := DialWithRetry(ctx, "127.0.0.1:1", "my-entity", tp, 0, onRetry, logger)
		if err == nil {
			t.Fatal("expected error, got nil")
		}

		if retryCalls != 0 {
			t.Errorf("onRetry called %d times, want 0 for zero retries", retryCalls)
		}
	})

	t.Run("context cancelled stops retries", func(t *testing.T) {
		tp := &mockTokenProvider{token: "test-token"}
		logger := slog.New(slog.NewTextHandler(io.Discard, nil))

		ctx, cancel := context.WithCancel(context.Background())

		var retryCalls int
		onRetry := func() {
			retryCalls++
			// Cancel after first retry is triggered.
			cancel()
		}

		_, err := DialWithRetry(ctx, "127.0.0.1:1", "my-entity", tp, 5, onRetry, logger)
		if err == nil {
			t.Fatal("expected error for cancelled context, got nil")
		}

		// Should have triggered exactly one retry before context was cancelled.
		if retryCalls != 1 {
			t.Errorf("onRetry called %d times, want 1", retryCalls)
		}
	})

	t.Run("nil onRetry is safe", func(t *testing.T) {
		tp := &mockTokenProvider{token: "test-token"}
		logger := slog.New(slog.NewTextHandler(io.Discard, nil))

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		// Should not panic with nil onRetry.
		_, err := DialWithRetry(ctx, "127.0.0.1:1", "my-entity", tp, 1, nil, logger)
		if err == nil {
			t.Fatal("expected error, got nil")
		}
	})

	t.Run("nil logger uses default", func(t *testing.T) {
		tp := &mockTokenProvider{token: "test-token"}

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		// Should not panic with nil logger.
		_, err := DialWithRetry(ctx, "127.0.0.1:1", "my-entity", tp, 0, nil, nil)
		if err == nil {
			t.Fatal("expected error, got nil")
		}
	})
}
