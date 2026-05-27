package relay

import (
	"context"
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestTLSConfigForDial(t *testing.T) {
	t.Run("nil base populates cache and MinVersion", func(t *testing.T) {
		cfg := tlsConfigForDial(nil)
		if cfg.ClientSessionCache != sessionCache {
			t.Errorf("ClientSessionCache = %v, want shared sessionCache", cfg.ClientSessionCache)
		}
		if cfg.MinVersion != tls.VersionTLS13 {
			t.Errorf("MinVersion = %#x, want %#x", cfg.MinVersion, tls.VersionTLS13)
		}
	})

	t.Run("preserves caller InsecureSkipVerify", func(t *testing.T) {
		//nolint:gosec // test only
		base := &tls.Config{InsecureSkipVerify: true}
		cfg := tlsConfigForDial(base)
		if !cfg.InsecureSkipVerify {
			t.Error("InsecureSkipVerify was not preserved")
		}
		if cfg.ClientSessionCache != sessionCache {
			t.Error("shared session cache not attached")
		}
		if cfg.MinVersion != tls.VersionTLS13 {
			t.Errorf("MinVersion = %#x, want %#x", cfg.MinVersion, tls.VersionTLS13)
		}
	})

	t.Run("overrides caller-supplied session cache", func(t *testing.T) {
		userCache := tls.NewLRUClientSessionCache(8)
		base := &tls.Config{ClientSessionCache: userCache}
		cfg := tlsConfigForDial(base)
		if cfg.ClientSessionCache != sessionCache {
			t.Error("caller-supplied ClientSessionCache was not replaced with shared cache")
		}
	})

	t.Run("overrides caller-supplied MinVersion", func(t *testing.T) {
		base := &tls.Config{MinVersion: tls.VersionTLS12}
		cfg := tlsConfigForDial(base)
		if cfg.MinVersion != tls.VersionTLS13 {
			t.Errorf("MinVersion = %#x, want %#x (caller value must not lower the floor)", cfg.MinVersion, tls.VersionTLS13)
		}
	})

	t.Run("clones base — caller mutation does not affect returned cfg", func(t *testing.T) {
		base := &tls.Config{}
		cfg := tlsConfigForDial(base)
		//nolint:gosec // test only
		base.InsecureSkipVerify = true
		if cfg.InsecureSkipVerify {
			t.Error("returned cfg shares mutable state with base — Clone() not applied")
		}
	})
}

func TestWSDialOptionsAlwaysAttachesSessionCache(t *testing.T) {
	t.Run("nil base", func(t *testing.T) {
		opts := WSDialOptions(nil, nil)
		if opts == nil || opts.HTTPClient == nil {
			t.Fatal("WSDialOptions returned nil HTTPClient")
		}
		tr, ok := opts.HTTPClient.Transport.(*http.Transport)
		if !ok {
			t.Fatalf("Transport is %T, want *http.Transport", opts.HTTPClient.Transport)
		}
		if tr.TLSClientConfig == nil || tr.TLSClientConfig.ClientSessionCache == nil {
			t.Error("TLSClientConfig.ClientSessionCache not set")
		}
		if tr.TLSClientConfig.MinVersion != tls.VersionTLS13 {
			t.Errorf("MinVersion = %#x, want %#x", tr.TLSClientConfig.MinVersion, tls.VersionTLS13)
		}
	})

	t.Run("caller InsecureSkipVerify preserved", func(t *testing.T) {
		//nolint:gosec // test only
		opts := WSDialOptions(nil, &tls.Config{InsecureSkipVerify: true})
		tr := opts.HTTPClient.Transport.(*http.Transport)
		if !tr.TLSClientConfig.InsecureSkipVerify {
			t.Error("InsecureSkipVerify not preserved into dial options")
		}
		if tr.TLSClientConfig.ClientSessionCache == nil {
			t.Error("session cache missing")
		}
	})

	t.Run("preserves mutated http.DefaultTransport TLSClientConfig", func(t *testing.T) {
		orig := http.DefaultTransport
		t.Cleanup(func() { http.DefaultTransport = orig })

		//nolint:gosec // test only
		http.DefaultTransport = &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		}

		opts := WSDialOptions(nil, nil)
		tr := opts.HTTPClient.Transport.(*http.Transport)
		if !tr.TLSClientConfig.InsecureSkipVerify {
			t.Error("InsecureSkipVerify from http.DefaultTransport was lost")
		}
		if tr.TLSClientConfig.ClientSessionCache == nil {
			t.Error("session cache missing")
		}
	})

	t.Run("prefers http.DefaultClient.Transport over http.DefaultTransport", func(t *testing.T) {
		// When http.DefaultClient.Transport is an *http.Transport,
		// WSDialOptions uses it (preserving DefaultClient-level
		// transport tuning) instead of falling through to
		// http.DefaultTransport.
		origC := http.DefaultClient
		origT := http.DefaultTransport
		t.Cleanup(func() {
			http.DefaultClient = origC
			http.DefaultTransport = origT
		})

		sentinel := &http.Transport{
			MaxIdleConnsPerHost: 7,
			//nolint:gosec // test only
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		}
		http.DefaultClient = &http.Client{Transport: sentinel}
		http.DefaultTransport = &http.Transport{MaxIdleConnsPerHost: 13}

		opts := WSDialOptions(nil, nil)
		tr := opts.HTTPClient.Transport.(*http.Transport)
		if tr.MaxIdleConnsPerHost != 7 {
			t.Errorf("MaxIdleConnsPerHost = %d, want 7 (DefaultClient.Transport ignored)", tr.MaxIdleConnsPerHost)
		}
		if !tr.TLSClientConfig.InsecureSkipVerify {
			t.Error("DefaultClient.Transport TLSClientConfig was lost")
		}
	})

	t.Run("preserves caller headers", func(t *testing.T) {
		h := http.Header{}
		h.Set("X-Test", "value")
		opts := WSDialOptions(h, nil)
		if got := opts.HTTPHeader.Get("X-Test"); got != "value" {
			t.Errorf("HTTPHeader.X-Test = %q, want %q", got, "value")
		}
	})

	t.Run("does not mutate caller TLSConfig", func(t *testing.T) {
		//nolint:gosec // test only
		base := &tls.Config{InsecureSkipVerify: true}
		_ = WSDialOptions(nil, base)
		if base.ClientSessionCache != nil {
			t.Error("caller's *tls.Config was mutated (ClientSessionCache set)")
		}
		if base.MinVersion != 0 {
			t.Error("caller's *tls.Config was mutated (MinVersion set)")
		}
	})

	t.Run("uses shallow copy of http.DefaultClient", func(t *testing.T) {
		orig := http.DefaultClient
		t.Cleanup(func() { http.DefaultClient = orig })

		http.DefaultClient = &http.Client{
			Timeout: 7 * time.Second,
		}

		opts := WSDialOptions(nil, nil)
		if opts.HTTPClient.Timeout != 7*time.Second {
			t.Errorf("Timeout = %v, want 7s — http.DefaultClient.Timeout was not preserved", opts.HTTPClient.Timeout)
		}
		if opts.HTTPClient == http.DefaultClient {
			t.Error("returned HTTPClient is the SAME *http.Client as http.DefaultClient — should be a shallow copy")
		}
	})

	t.Run("does not panic when http.DefaultClient is nil", func(t *testing.T) {
		orig := http.DefaultClient
		t.Cleanup(func() { http.DefaultClient = orig })

		http.DefaultClient = nil

		opts := WSDialOptions(nil, nil)
		if opts == nil || opts.HTTPClient == nil {
			t.Fatal("WSDialOptions returned nil HTTPClient")
		}
		tr, ok := opts.HTTPClient.Transport.(*http.Transport)
		if !ok || tr.TLSClientConfig == nil || tr.TLSClientConfig.ClientSessionCache == nil {
			t.Error("session cache not attached when DefaultClient is nil")
		}
	})
}

// TestWSDialOptionsSessionResumption verifies that two sequential
// dials through WSDialOptions actually resume the TLS session: the
// second connection sees ConnectionState.DidResume == true on the
// server side. End-to-end proof that the shared session cache is
// wired correctly through coder/websocket → http.Transport.
func TestWSDialOptionsSessionResumption(t *testing.T) {
	var (
		mu        sync.Mutex
		resumedOn []int // 1-indexed dial numbers that resumed
		dialNum   atomic.Int32
	)

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := int(dialNum.Add(1))
		if r.TLS != nil && r.TLS.DidResume {
			mu.Lock()
			resumedOn = append(resumedOn, n)
			mu.Unlock()
		}
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		_ = ws.Close(websocket.StatusNormalClosure, "ok")
	})

	srv := httptest.NewTLSServer(handler)
	t.Cleanup(srv.Close)

	// Caller TLSConfig reused across dials. tlsConfigForDial stamps
	// the package sessionCache on the cloned config — that shared
	// cache is what enables resumption.
	//nolint:gosec // test server uses self-signed cert
	baseTLS := &tls.Config{InsecureSkipVerify: true}

	wssURL := "wss://" + strings.TrimPrefix(srv.URL, "https://")

	dial := func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		ws, _, err := websocket.Dial(ctx, wssURL, WSDialOptions(nil, baseTLS))
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		// Drain the close so the server-side handler completes and
		// records its DidResume observation before the next dial.
		// This read also drives the client past the TLS 1.3
		// post-handshake NewSessionTicket frame, so the ticket is
		// stored in the cache before the next dial begins.
		_, _, _ = ws.Read(ctx)
		_ = ws.CloseNow()
	}

	dial()
	dial()

	mu.Lock()
	defer mu.Unlock()
	if len(resumedOn) == 0 {
		t.Fatal("no dial reported DidResume — session cache not effective")
	}
	for _, n := range resumedOn {
		if n == 1 {
			t.Errorf("first dial reported DidResume — impossible without prior cache state")
		}
	}
}
