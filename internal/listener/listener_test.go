package listener

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/philsphicas/aztunnel/internal/metrics"
	"github.com/philsphicas/aztunnel/internal/protocol"
)

func TestIsAllowed(t *testing.T) {
	tests := []struct {
		name      string
		target    string
		allowList []string
		want      bool
	}{
		{"wildcard", "10.0.0.1:22", []string{"*"}, true},
		{"exact match", "10.0.0.1:22", []string{"10.0.0.1:22"}, true},
		{"exact no match", "10.0.0.1:22", []string{"10.0.0.2:22"}, false},
		{"wrong port", "10.0.0.1:80", []string{"10.0.0.1:22"}, false},
		{"cidr match", "10.0.0.5:22", []string{"10.0.0.0/8:22"}, true},
		{"cidr wildcard port", "10.0.0.5:8080", []string{"10.0.0.0/8:*"}, true},
		{"cidr no match", "192.168.0.1:22", []string{"10.0.0.0/8:22"}, false},
		{"multiple entries", "10.0.0.5:22", []string{"192.168.0.0/16:*", "10.0.0.0/8:22"}, true},
		{"hostname exact", "myhost:22", []string{"myhost:22"}, true},
		{"hostname wrong", "myhost:22", []string{"other:22"}, false},
		{"empty target", "", []string{"*"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isAllowed(tt.target, tt.allowList)
			if got != tt.want {
				t.Errorf("isAllowed(%q, %v) = %v, want %v", tt.target, tt.allowList, got, tt.want)
			}
		})
	}
}

func TestSplitAllowEntry(t *testing.T) {
	tests := []struct {
		entry    string
		wantHost string
		wantPort string
		wantErr  bool
	}{
		{"10.0.0.1:22", "10.0.0.1", "22", false},
		{"10.0.0.0/8:*", "10.0.0.0/8", "*", false},
		{"myhost:22", "myhost", "22", false},
		{"nocolon", "", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.entry, func(t *testing.T) {
			h, p, err := splitAllowEntry(tt.entry)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr = %v", err, tt.wantErr)
			}
			if h != tt.wantHost {
				t.Errorf("host = %q, want %q", h, tt.wantHost)
			}
			if p != tt.wantPort {
				t.Errorf("port = %q, want %q", p, tt.wantPort)
			}
		})
	}
}

func TestClassifyDialError_Nil(t *testing.T) {
	if got := classifyDialError(nil); got != "" {
		t.Errorf("classifyDialError(nil) = %q, want %q", got, "")
	}
}

func TestClassifyDialError_DNSNotFound(t *testing.T) {
	err := &net.DNSError{Err: "no such host", Name: "nonexistent.invalid", IsNotFound: true}
	if got := classifyDialError(err); got != protocol.CodeDNSNotFound {
		t.Errorf("classifyDialError(DNSError{IsNotFound}) = %q, want %q", got, protocol.CodeDNSNotFound)
	}
}

func TestClassifyDialError_DNSTimeout(t *testing.T) {
	err := &net.DNSError{Err: "i/o timeout", Name: "slow.example", IsTimeout: true}
	if got := classifyDialError(err); got != protocol.CodeDNSTimeout {
		t.Errorf("classifyDialError(DNSError{IsTimeout}) = %q, want %q", got, protocol.CodeDNSTimeout)
	}
}

func TestClassifyDialError_DNSTemporary(t *testing.T) {
	// Non-timeout, non-not-found DNS errors (e.g. SERVFAIL) fall through
	// to CodeDNSNotFound under the current spec. This documents that
	// behaviour so a future refinement (e.g. a separate dns_failed code)
	// is a deliberate change rather than an accidental one.
	err := &net.DNSError{Err: "server misbehaving", Name: "example.invalid"}
	if got := classifyDialError(err); got != protocol.CodeDNSNotFound {
		t.Errorf("classifyDialError(DNSError{plain}) = %q, want %q", got, protocol.CodeDNSNotFound)
	}
}

func TestClassifyDialError_DNSWrappedInOpError(t *testing.T) {
	// net.Dialer wraps DNS failures inside *net.OpError; classifyDialError
	// must unwrap via errors.As to find the underlying *net.DNSError.
	dnsErr := &net.DNSError{Err: "no such host", Name: "nonexistent.invalid", IsNotFound: true}
	wrapped := &net.OpError{Op: "dial", Net: "tcp", Err: dnsErr}
	if got := classifyDialError(wrapped); got != protocol.CodeDNSNotFound {
		t.Errorf("classifyDialError(OpError wrapping DNSError) = %q, want %q", got, protocol.CodeDNSNotFound)
	}
}

func TestClassifyDialError_ContextDeadlineBeatsDNSTimeout(t *testing.T) {
	// When the dial error is both a DNS timeout AND ctx.DeadlineExceeded,
	// the context-deadline branch must win: the operator deliberately
	// cancelled, so CodeTimeout reflects that intent better than the
	// underlying DNS-layer detail.
	dnsErr := &net.DNSError{Err: "i/o timeout", Name: "slow.example", IsTimeout: true}
	combined := errors.Join(context.DeadlineExceeded, dnsErr)
	if got := classifyDialError(combined); got != protocol.CodeTimeout {
		t.Errorf("classifyDialError(Join(DeadlineExceeded, DNS timeout)) = %q, want %q",
			got, protocol.CodeTimeout)
	}
}

func TestClassifyDialError_OtherErrors(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{"refused", &net.OpError{Op: "dial", Err: syscall.ECONNREFUSED}, protocol.CodeConnectionRefused},
		{"host unreachable", &net.OpError{Op: "dial", Err: syscall.EHOSTUNREACH}, protocol.CodeHostUnreachable},
		{"net unreachable", &net.OpError{Op: "dial", Err: syscall.ENETUNREACH}, protocol.CodeNetworkUnreachable},
		{"context deadline", context.DeadlineExceeded, protocol.CodeTimeout},
		{"unclassified", errors.New("something broke"), ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := classifyDialError(tt.err); got != tt.want {
				t.Errorf("classifyDialError(%v) = %q, want %q", tt.err, got, tt.want)
			}
		})
	}
}

// --- listener_id tests ---

// TestApplyDefaults_MintsListenerID asserts a Config with empty
// ListenerID gets a non-empty value populated by applyDefaults — the
// production startup path mints exactly once per process and stamps
// every response with that value.
func TestApplyDefaults_MintsListenerID(t *testing.T) {
	cfg := Config{}
	applyDefaults(&cfg)
	if cfg.ListenerID == "" {
		t.Fatal("applyDefaults did not mint a ListenerID")
	}
}

// TestApplyDefaults_MintsDistinctIDs is the cross-instance invariant:
// two listener configs prepared independently must end up with
// different IDs, otherwise restart-driven failure modes are
// indistinguishable from "the same listener keeps misbehaving".
func TestApplyDefaults_MintsDistinctIDs(t *testing.T) {
	a, b := Config{}, Config{}
	applyDefaults(&a)
	applyDefaults(&b)
	if a.ListenerID == b.ListenerID {
		t.Fatalf("two listener configs got the same ID: %q", a.ListenerID)
	}
}

// TestApplyDefaults_RespectsCallerProvidedID ensures tests (and any
// future caller that wants deterministic IDs) can pre-populate the
// field and have applyDefaults leave it alone. The doc comment on
// Config.ListenerID is the contract; this test enforces it.
func TestApplyDefaults_RespectsCallerProvidedID(t *testing.T) {
	cfg := Config{ListenerID: "fixed-id-for-test"}
	applyDefaults(&cfg)
	if cfg.ListenerID != "fixed-id-for-test" {
		t.Fatalf("applyDefaults overwrote caller ID: got %q", cfg.ListenerID)
	}
}

// TestApplyDefaults_LoggerCarriesListenerID asserts every subsequent
// log line emitted via cfg.Logger automatically carries the
// listener_id attribute. Operators correlating sender-side
// observations against listener-side logs rely on this; without it,
// the listener_id would have to be threaded through every log call
// site manually (error-prone).
func TestApplyDefaults_LoggerCarriesListenerID(t *testing.T) {
	var buf bytes.Buffer
	cfg := Config{
		ListenerID: "tag-me-please",
		Logger:     slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})),
	}
	applyDefaults(&cfg)
	cfg.Logger.Info("anything")
	if !strings.Contains(buf.String(), "listener_id=tag-me-please") {
		t.Fatalf("log line missing listener_id attribute:\n  %s", buf.String())
	}
}

// TestHandleConnection_ResponseCarriesListenerID drives the full
// handleConnection path with an in-memory ws pair and asserts the
// success-path response carries the configured ListenerID. This is
// the smallest end-to-end check that wiring through sendResponse →
// sendResponseWithCode actually populates the field on the wire.
func TestHandleConnection_ResponseCarriesListenerID(t *testing.T) {
	// Start a one-shot target the listener will dial.
	target, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("target listen: %v", err)
	}
	defer target.Close() //nolint:errcheck // best-effort cleanup
	go func() {
		c, err := target.Accept()
		if err != nil {
			return
		}
		_ = c.Close()
	}()

	cfg := Config{
		AllowList:      []string{target.Addr().String()},
		ConnectTimeout: 5 * time.Second,
		TCPKeepAlive:   5 * time.Second,
		Logger:         slog.New(slog.NewTextHandler(io.Discard, nil)),
		Metrics:        metrics.New(),
		ListenerID:     "stable-test-id-1",
	}

	resp := driveOneHandshake(t, cfg, target.Addr().String())
	if !resp.OK {
		t.Fatalf("expected OK response, got error=%q code=%q", resp.Error, resp.Code)
	}
	if resp.ListenerID != "stable-test-id-1" {
		t.Errorf("response listener_id = %q, want %q", resp.ListenerID, "stable-test-id-1")
	}
}

// TestHandleConnection_StableAcrossRequests drives 10 sequential
// handshakes through the same Config and asserts every response
// carries the same listener_id. Two listener_id values inside a
// single instance's lifetime would indicate a regression where the
// mint happens per-request instead of per-instance.
func TestHandleConnection_StableAcrossRequests(t *testing.T) {
	target, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("target listen: %v", err)
	}
	defer target.Close() //nolint:errcheck // best-effort cleanup
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			c, err := target.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()

	cfg := Config{
		AllowList:      []string{target.Addr().String()},
		ConnectTimeout: 5 * time.Second,
		TCPKeepAlive:   5 * time.Second,
		Logger:         slog.New(slog.NewTextHandler(io.Discard, nil)),
		Metrics:        metrics.New(),
		ListenerID:     "stable-test-id-2",
	}

	for i := 0; i < 10; i++ {
		resp := driveOneHandshake(t, cfg, target.Addr().String())
		if resp.ListenerID != "stable-test-id-2" {
			t.Fatalf("iteration %d: listener_id = %q, want %q", i, resp.ListenerID, "stable-test-id-2")
		}
	}
}

// TestHandleConnection_FailurePathsCarryListenerID covers every
// sendResponse / sendResponseWithCode call site in handleConnection:
// invalid envelope, unsupported version, missing target, allowlist
// rejection, dial failure. Each must carry the listener_id so a
// rejected sender still sees which listener instance rejected it.
func TestHandleConnection_FailurePathsCarryListenerID(t *testing.T) {
	const listenerID = "stable-test-id-3"

	// Address that the OS will RST on connect — used for the dial-
	// failure case. Bind+immediate-close gives us a port that is
	// (typically) free in the seconds that follow.
	refused := func(t *testing.T) string {
		t.Helper()
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("listen: %v", err)
		}
		addr := ln.Addr().String()
		_ = ln.Close()
		return addr
	}

	tests := []struct {
		name    string
		cfg     Config
		send    func(ctx context.Context, ws *websocket.Conn) error
		wantOK  bool
		wantErr string
	}{
		{
			name: "invalid-envelope",
			cfg: Config{
				ConnectTimeout: 5 * time.Second,
				TCPKeepAlive:   5 * time.Second,
				Logger:         slog.New(slog.NewTextHandler(io.Discard, nil)),
				Metrics:        metrics.New(),
				ListenerID:     listenerID,
			},
			send: func(ctx context.Context, ws *websocket.Conn) error {
				return ws.Write(ctx, websocket.MessageText, []byte("not json"))
			},
			wantOK:  false,
			wantErr: "invalid envelope",
		},
		{
			name: "unsupported-version",
			cfg: Config{
				ConnectTimeout: 5 * time.Second,
				TCPKeepAlive:   5 * time.Second,
				Logger:         slog.New(slog.NewTextHandler(io.Discard, nil)),
				Metrics:        metrics.New(),
				ListenerID:     listenerID,
			},
			send: func(ctx context.Context, ws *websocket.Conn) error {
				data, _ := json.Marshal(protocol.ConnectEnvelope{Version: 999, Target: "x:1"})
				return ws.Write(ctx, websocket.MessageText, data)
			},
			wantOK:  false,
			wantErr: "unsupported protocol version",
		},
		{
			name: "missing-target",
			cfg: Config{
				ConnectTimeout: 5 * time.Second,
				TCPKeepAlive:   5 * time.Second,
				Logger:         slog.New(slog.NewTextHandler(io.Discard, nil)),
				Metrics:        metrics.New(),
				ListenerID:     listenerID,
			},
			send: func(ctx context.Context, ws *websocket.Conn) error {
				data, _ := json.Marshal(protocol.ConnectEnvelope{Version: protocol.CurrentVersion})
				return ws.Write(ctx, websocket.MessageText, data)
			},
			wantOK:  false,
			wantErr: "missing target",
		},
		{
			name: "allowlist-rejected",
			cfg: Config{
				AllowList:      []string{"10.255.255.255:1"},
				ConnectTimeout: 5 * time.Second,
				TCPKeepAlive:   5 * time.Second,
				Logger:         slog.New(slog.NewTextHandler(io.Discard, nil)),
				Metrics:        metrics.New(),
				ListenerID:     listenerID,
			},
			send: func(ctx context.Context, ws *websocket.Conn) error {
				data, _ := json.Marshal(protocol.ConnectEnvelope{Version: protocol.CurrentVersion, Target: "127.0.0.1:1"})
				return ws.Write(ctx, websocket.MessageText, data)
			},
			wantOK:  false,
			wantErr: "target not allowed",
		},
		{
			name: "dial-failure",
			cfg: Config{
				ConnectTimeout: 2 * time.Second,
				TCPKeepAlive:   5 * time.Second,
				Logger:         slog.New(slog.NewTextHandler(io.Discard, nil)),
				Metrics:        metrics.New(),
				ListenerID:     listenerID,
			},
			send: func(ctx context.Context, ws *websocket.Conn) error {
				addr := refused(t)
				data, _ := json.Marshal(protocol.ConnectEnvelope{Version: protocol.CurrentVersion, Target: addr})
				return ws.Write(ctx, websocket.MessageText, data)
			},
			wantOK:  false,
			wantErr: "connection failed",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp := driveCustomHandshake(t, tt.cfg, tt.send)
			if resp.OK != tt.wantOK {
				t.Errorf("ok = %v, want %v", resp.OK, tt.wantOK)
			}
			if !strings.Contains(resp.Error, tt.wantErr) {
				t.Errorf("error = %q, want substring %q", resp.Error, tt.wantErr)
			}
			if resp.ListenerID != listenerID {
				t.Errorf("listener_id = %q, want %q", resp.ListenerID, listenerID)
			}
		})
	}
}

// driveOneHandshake stands up an httptest WebSocket server that
// forwards the accepted connection to handleConnection(cfg), then
// dials it, sends a valid ConnectEnvelope for target, and returns
// the parsed response. The full success path runs end-to-end.
func driveOneHandshake(t *testing.T, cfg Config, target string) protocol.ConnectResponse {
	t.Helper()
	return driveCustomHandshake(t, cfg, func(ctx context.Context, ws *websocket.Conn) error {
		data, _ := json.Marshal(protocol.ConnectEnvelope{Version: protocol.CurrentVersion, Target: target})
		return ws.Write(ctx, websocket.MessageText, data)
	})
}

// driveCustomHandshake is the underlying driver for handshakes
// against a real handleConnection. send is the client-side step that
// writes whatever envelope the test wants to exercise.
//
// The helper calls applyDefaults so tests walk the same startup path
// as production (ConnectTimeout/TCPKeepAlive defaulting, logger
// wrapping with listener_id). Tests that need a deterministic
// listener_id should pre-populate cfg.ListenerID; applyDefaults
// leaves a non-empty value untouched.
func driveCustomHandshake(t *testing.T, cfg Config, send func(ctx context.Context, ws *websocket.Conn) error) protocol.ConnectResponse {
	t.Helper()
	applyDefaults(&cfg)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		// handleConnection takes ownership of the ws and closes
		// internal state on return.
		handleConnection(r.Context(), ws, cfg)
	}))
	defer srv.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	ws, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer ws.CloseNow() //nolint:errcheck // best-effort cleanup

	if err := send(ctx, ws); err != nil {
		t.Fatalf("send envelope: %v", err)
	}

	_, data, err := ws.Read(ctx)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	var resp protocol.ConnectResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		t.Fatalf("parse response: %v", err)
	}
	return resp
}

// runDialFailureHandler drives one handleConnection invocation against
// an in-process WebSocket pair, returning the captured slog output
// after the handler has fully returned. The injected dial stub is the
// seam these tests use to provoke specific dial outcomes without
// touching the real network.
//
// Synchronisation contract: the test closes the client side, the
// handler returns, the handler-side goroutine signals `done`, and
// only then does this helper read the slog buffer — so the log
// snapshot is happens-before-stable when callers grep it.
func runDialFailureHandler(t *testing.T, target string,
	dial func(ctx context.Context, network, addr string) (net.Conn, error),
) string {
	t.Helper()

	var logBuf bytes.Buffer
	cfg := Config{
		ConnectTimeout: 5 * time.Second,
		Logger:         slog.New(slog.NewTextHandler(&logBuf, nil)),
		Metrics:        metrics.New(),
		dialContext:    dial,
	}

	done := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(done)
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Errorf("server accept: %v", err)
			return
		}
		defer ws.CloseNow() //nolint:errcheck // best-effort cleanup
		handleConnection(r.Context(), ws, cfg)
	}))
	t.Cleanup(srv.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	ws, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("dial websocket: %v", err)
	}
	defer ws.CloseNow() //nolint:errcheck // best-effort cleanup

	env := protocol.ConnectEnvelope{
		Version:  protocol.CurrentVersion,
		Target:   target,
		BridgeID: "TESTBRIDGE2P25Z",
	}
	envBytes, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	if err := ws.Write(ctx, websocket.MessageText, envBytes); err != nil {
		t.Fatalf("write envelope: %v", err)
	}

	// Read the failure response so the handler has a clean exit path
	// rather than being torn down mid-write by the client close.
	if _, _, err := ws.Read(ctx); err != nil {
		t.Fatalf("read response: %v", err)
	}
	_ = ws.Close(websocket.StatusNormalClosure, "done")

	select {
	case <-done:
	case <-ctx.Done():
		t.Fatalf("handler did not return within deadline: %v", ctx.Err())
	}
	return logBuf.String()
}

func TestListener_DialFailed_LogIncludesCode_Refused(t *testing.T) {
	stub := func(ctx context.Context, network, addr string) (net.Conn, error) {
		return nil, &net.OpError{Op: "dial", Net: network, Err: syscall.ECONNREFUSED}
	}
	logs := runDialFailureHandler(t, "127.0.0.1:9", stub)

	assertDialFailureLog(t, logs, "127.0.0.1:9", protocol.CodeConnectionRefused)
}

func TestListener_DialFailed_LogIncludesCode_Timeout(t *testing.T) {
	stub := func(ctx context.Context, network, addr string) (net.Conn, error) {
		return nil, &net.OpError{Op: "dial", Net: network, Err: context.DeadlineExceeded}
	}
	logs := runDialFailureHandler(t, "192.0.2.1:9", stub)

	assertDialFailureLog(t, logs, "192.0.2.1:9", protocol.CodeTimeout)
}

func TestListener_DialFailed_LogIncludesCode_Unclassified(t *testing.T) {
	stub := func(ctx context.Context, network, addr string) (net.Conn, error) {
		return nil, errors.New("synthetic failure")
	}
	logs := runDialFailureHandler(t, "10.0.0.1:22", stub)

	// Empty code means the classifier did not match; slog renders it
	// as code="" so operators see explicit evidence rather than a
	// silently absent attribute.
	assertDialFailureLog(t, logs, "10.0.0.1:22", "")
}

// assertDialFailureLog requires the captured listener output to
// contain at least one "dial target failed" line for the given target,
// and that the first such line carries the expected code attribute
// (rendered as code=VALUE for non-empty values and code="" for the
// unclassified case). Each test drives exactly one failed dial through
// runDialFailureHandler, so the helper does not need to enforce
// single-match uniqueness explicitly.
func assertDialFailureLog(t *testing.T, logs, target, code string) {
	t.Helper()
	var hit string
	for _, line := range strings.Split(logs, "\n") {
		if !strings.Contains(line, `msg="dial target failed"`) {
			continue
		}
		if !strings.Contains(line, "target="+target) {
			continue
		}
		hit = line
		break
	}
	if hit == "" {
		t.Fatalf("no 'dial target failed' line for target=%s in logs:\n%s", target, logs)
	}
	want := `code=` + code
	if code == "" {
		want = `code=""`
	}
	if !strings.Contains(hit, want) {
		t.Fatalf("dial-failure log missing %q:\n%s", want, hit)
	}
}
