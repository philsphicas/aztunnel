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
	"github.com/philsphicas/aztunnel/internal/muxconfig"
	"github.com/philsphicas/aztunnel/internal/protocol"
	"github.com/xtaci/smux"
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
		{
			// MaxProtocolVersion=1 makes the listener reject v2
			// MuxHandshakes with the same wire shape a v1-only
			// listener would emit. The sender's
			// isMuxUnsupportedRejection recognises this exact string
			// and falls back to the v1 path. Locking the response
			// shape here protects the rolling-deployment scenario
			// from a silent rename of the rejection string.
			name: "reject-mux-when-listener-pinned-to-v1",
			cfg: Config{
				ConnectTimeout:     5 * time.Second,
				TCPKeepAlive:       5 * time.Second,
				Logger:             slog.New(slog.NewTextHandler(io.Discard, nil)),
				Metrics:            metrics.New(),
				ListenerID:         listenerID,
				MaxProtocolVersion: 1,
			},
			send: func(ctx context.Context, ws *websocket.Conn) error {
				data, _ := json.Marshal(protocol.MuxHandshake{
					Version: protocol.MuxVersion,
					Mode:    protocol.MuxMode,
				})
				return ws.Write(ctx, websocket.MessageText, data)
			},
			wantOK:  false,
			wantErr: "unsupported protocol version",
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
		streamSem := newConnSemaphore(cfg.MaxConnections)
		pendingMax := 0
		if cfg.MaxConnections > 0 {
			pendingMax = cfg.MaxConnections * 2
		}
		pendingSem := newConnSemaphore(pendingMax)
		handleConnection(r.Context(), ws, cfg, streamSem, pendingSem)
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
		streamSem := newConnSemaphore(cfg.MaxConnections)
		pendingMax := 0
		if cfg.MaxConnections > 0 {
			pendingMax = cfg.MaxConnections * 2
		}
		pendingSem := newConnSemaphore(pendingMax)
		handleConnection(r.Context(), ws, cfg, streamSem, pendingSem)
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

// --- v2 mux path tests ---

// muxTestRig wires a smux client/server pair over a TCP socket pair,
// runs handleMuxSession on the server side, and returns the client
// session so tests can OpenStream + drive the envelope exchange
// directly. The returned cancel tears the rig down.
//
// TCP socket pair (not net.Pipe): smux relies on the transport
// buffering small frames (SYN/FIN/keepalive) so multiple streams can
// be opened without head-of-line blocking. net.Pipe is strictly
// synchronous and stalls smux in subtle ways.
type muxTestRig struct {
	clientSess *smux.Session
	cancel     context.CancelFunc
	wg         *sync.WaitGroup
	cfg        Config
	streamSem  *connSemaphore
	pendingSem *connSemaphore
}

func newMuxTestRig(t *testing.T, cfg Config) *muxTestRig {
	t.Helper()

	if cfg.ConnectTimeout == 0 {
		cfg.ConnectTimeout = 5 * time.Second
	}
	if cfg.TCPKeepAlive == 0 {
		cfg.TCPKeepAlive = 30 * time.Second
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}

	// Use a TCP socket pair rather than net.Pipe: smux relies on the
	// transport buffering small frames (SYN/FIN/keepalive) so multiple
	// streams can be opened without head-of-line blocking. net.Pipe is
	// strictly synchronous and stalls smux in subtle ways.
	clientPipe, serverPipe := tcpSocketPair(t)
	clientSess, err := smux.Client(clientPipe, muxTestSmuxCfg())
	if err != nil {
		t.Fatalf("smux.Client: %v", err)
	}
	serverSess, err := smux.Server(serverPipe, muxTestSmuxCfg())
	if err != nil {
		t.Fatalf("smux.Server: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	streamSem := newConnSemaphore(cfg.MaxConnections)
	pendingMax := 0
	if cfg.MaxConnections > 0 {
		pendingMax = cfg.MaxConnections * 2
	}
	pendingSem := newConnSemaphore(pendingMax)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		runMuxAcceptLoop(ctx, serverSess, cfg, streamSem, pendingSem)
	}()

	t.Cleanup(func() {
		cancel()
		_ = clientSess.Close()
		_ = serverSess.Close()
		_ = clientPipe.Close()
		_ = serverPipe.Close()
		wg.Wait()
	})

	return &muxTestRig{
		clientSess: clientSess,
		cancel:     cancel,
		wg:         &wg,
		cfg:        cfg,
		streamSem:  streamSem,
		pendingSem: pendingSem,
	}
}

// waitForPendingSem blocks until the rig's pendingSem reaches the
// requested holder count, or fails the test if the state isn't
// observed within timeout. This replaces fixed-duration sleeps that
// could falsely pass on a slow listener without exercising the
// intended envelope-pending state.
func (r *muxTestRig) waitForPendingSem(t *testing.T, want int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if r.pendingSem.held() == want {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("pendingSem.held() = %d, want %d after %s", r.pendingSem.held(), want, timeout)
}

// runMuxAcceptLoop is the body of handleMuxSession minus the outer ws
// handshake — letting tests exercise the AcceptStream → handleMuxStream
// path without faking a websocket. Keep the rejection branch
// behaviourally identical to handleMuxSession (listener.go's accept
// loop), including the metric call AND the goroutine dispatch + write
// deadline on the rejection write, so tests of that branch actually
// cover the production path.
func runMuxAcceptLoop(ctx context.Context, sess *smux.Session, cfg Config, streamSem, pendingSem *connSemaphore) {
	var wg sync.WaitGroup
	defer wg.Wait()
	rejectSem := make(chan struct{}, muxRejectInflightCap)
	for {
		stream, err := sess.AcceptStream()
		if err != nil {
			return
		}
		if !pendingSem.tryAcquire() {
			cfg.Metrics.ConnectionError("listener", metrics.ReasonListenerAtCapacity)
			select {
			case rejectSem <- struct{}{}:
				wg.Add(1)
				go func() {
					defer wg.Done()
					defer func() { <-rejectSem }()
					defer stream.Close() //nolint:errcheck // test cleanup
					writeMuxRejection(stream, cfg, "listener busy")
				}()
			default:
				return
			}
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer stream.Close() //nolint:errcheck // test cleanup
			handleMuxStream(ctx, stream, cfg, streamSem, pendingSem)
		}()
	}
}

func muxTestSmuxCfg() *smux.Config {
	return muxconfig.SmuxConfig()
}

// tcpSocketPair returns two connected TCP sockets via a loopback
// listener — a more faithful substitute for the smux-over-WebSocket
// transport used in production than net.Pipe (which is fully synchronous
// and stalls smux's SYN/FIN/keepalive flow).
func tcpSocketPair(t *testing.T) (net.Conn, net.Conn) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close() //nolint:errcheck // best-effort cleanup

	type result struct {
		c   net.Conn
		err error
	}
	acceptCh := make(chan result, 1)
	go func() {
		c, err := ln.Accept()
		acceptCh <- result{c, err}
	}()

	client, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	res := <-acceptCh
	if res.err != nil {
		_ = client.Close()
		t.Fatalf("accept: %v", res.err)
	}
	return client, res.c
}

func startEchoTarget(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close() //nolint:errcheck // test cleanup
				_, _ = io.Copy(c, c)
			}(c)
		}
	}()
	return ln.Addr().String()
}

// TestMuxStream_HappyPath_EchoesData verifies the full v2 stream path
// against a real local target: open a stream, send envelope, expect OK,
// then bridge bytes through.
func TestMuxStream_HappyPath_EchoesData(t *testing.T) {
	target := startEchoTarget(t)
	rig := newMuxTestRig(t, Config{
		MaxConnections: 10,
		// nil allowlist → all targets allowed (test-only mode)
	})

	stream, err := rig.clientSess.OpenStream()
	if err != nil {
		t.Fatalf("OpenStream: %v", err)
	}
	defer stream.Close() //nolint:errcheck // test cleanup

	if err := protocol.WriteStreamEnvelope(stream, protocol.ConnectEnvelope{
		Version: protocol.CurrentVersion,
		Target:  target,
	}); err != nil {
		t.Fatalf("WriteStreamEnvelope: %v", err)
	}
	resp, err := protocol.ReadStreamResponse(stream)
	if err != nil {
		t.Fatalf("ReadStreamResponse: %v", err)
	}
	if !resp.OK {
		t.Fatalf("response not OK: %q", resp.Error)
	}

	const payload = "hello mux\n"
	if _, err := stream.Write([]byte(payload)); err != nil {
		t.Fatalf("stream.Write: %v", err)
	}
	_ = stream.SetReadDeadline(time.Now().Add(3 * time.Second))
	got := make([]byte, len(payload))
	if _, err := io.ReadFull(stream, got); err != nil {
		t.Fatalf("stream.Read: %v", err)
	}
	if string(got) != payload {
		t.Errorf("echo = %q, want %q", got, payload)
	}
}

// TestMuxStream_RejectsMissingTarget covers the case where a sender opens
// a stream but sends an envelope with no target. The listener must reply
// with an error (length-prefixed) and not deadlock.
func TestMuxStream_RejectsMissingTarget(t *testing.T) {
	rig := newMuxTestRig(t, Config{MaxConnections: 10})

	stream, err := rig.clientSess.OpenStream()
	if err != nil {
		t.Fatalf("OpenStream: %v", err)
	}
	defer stream.Close() //nolint:errcheck // test cleanup

	if err := protocol.WriteStreamEnvelope(stream, protocol.ConnectEnvelope{
		Version: protocol.CurrentVersion,
		Target:  "",
	}); err != nil {
		t.Fatalf("WriteStreamEnvelope: %v", err)
	}
	_ = stream.SetReadDeadline(time.Now().Add(3 * time.Second))
	resp, err := protocol.ReadStreamResponse(stream)
	if err != nil {
		t.Fatalf("ReadStreamResponse: %v", err)
	}
	if resp.OK {
		t.Fatal("expected rejection")
	}
	if !strings.Contains(resp.Error, "missing target") {
		t.Errorf("error = %q, want it to contain 'missing target'", resp.Error)
	}
}

// TestMuxStream_AllowlistEnforced verifies the allowlist applies to every
// stream individually, not just the outer handshake.
func TestMuxStream_AllowlistEnforced(t *testing.T) {
	rig := newMuxTestRig(t, Config{
		MaxConnections: 10,
		AllowList:      []string{"10.0.0.1:22"},
	})

	stream, err := rig.clientSess.OpenStream()
	if err != nil {
		t.Fatalf("OpenStream: %v", err)
	}
	defer stream.Close() //nolint:errcheck // test cleanup

	if err := protocol.WriteStreamEnvelope(stream, protocol.ConnectEnvelope{
		Version: protocol.CurrentVersion,
		Target:  "10.0.0.99:22",
	}); err != nil {
		t.Fatalf("WriteStreamEnvelope: %v", err)
	}
	_ = stream.SetReadDeadline(time.Now().Add(3 * time.Second))
	resp, err := protocol.ReadStreamResponse(stream)
	if err != nil {
		t.Fatalf("ReadStreamResponse: %v", err)
	}
	if resp.OK {
		t.Fatal("expected rejection by allowlist")
	}
	if !strings.Contains(resp.Error, "not allowed") {
		t.Errorf("error = %q, want 'not allowed'", resp.Error)
	}
}

// TestMuxStream_StreamCapRejectsOverflow is the critical regression test
// for the resource-control gap originally flagged by the rubber-duck and
// re-flagged by the GPT-5.5/Sonnet reviewers under v2: a single accepted
// relay WebSocket could spawn unbounded target dials, and multiple mux
// sessions multiplied the effective budget. The listener-wide stream
// semaphore (capped at MaxConnections) must reject streams beyond the
// cap regardless of how many mux sessions are sharing it.
//
// We hold one stream open (bridging to a live target), then open a second
// stream which should be rejected with "at max concurrent streams" after
// completing envelope/allowlist validation (the cap is now acquired
// post-envelope, so the second stream must send a valid envelope before
// it reaches the streamSem gate).
func TestMuxStream_StreamCapRejectsOverflow(t *testing.T) {
	target := startEchoTarget(t)
	rig := newMuxTestRig(t, Config{
		MaxConnections: 1, // strict cap
	})

	// First stream — should succeed and hold its slot open.
	hold, err := rig.clientSess.OpenStream()
	if err != nil {
		t.Fatalf("OpenStream #1: %v", err)
	}
	defer hold.Close() //nolint:errcheck // test cleanup
	if err := protocol.WriteStreamEnvelope(hold, protocol.ConnectEnvelope{
		Version: protocol.CurrentVersion,
		Target:  target,
	}); err != nil {
		t.Fatalf("WriteStreamEnvelope #1: %v", err)
	}
	resp1, err := protocol.ReadStreamResponse(hold)
	if err != nil {
		t.Fatalf("ReadStreamResponse #1: %v", err)
	}
	if !resp1.OK {
		t.Fatalf("first stream not OK: %q", resp1.Error)
	}

	// Second stream — should be rejected. The streamSem gate runs AFTER
	// envelope/allowlist validation in handleMuxStream, so the client
	// must send a valid envelope before the rejection is produced.
	overflow, err := rig.clientSess.OpenStream()
	if err != nil {
		t.Fatalf("OpenStream #2: %v", err)
	}
	defer overflow.Close() //nolint:errcheck // test cleanup
	if err := protocol.WriteStreamEnvelope(overflow, protocol.ConnectEnvelope{
		Version: protocol.CurrentVersion,
		Target:  target,
	}); err != nil {
		t.Fatalf("WriteStreamEnvelope #2: %v", err)
	}
	_ = overflow.SetReadDeadline(time.Now().Add(3 * time.Second))
	resp2, err := protocol.ReadStreamResponse(overflow)
	if err != nil {
		t.Fatalf("ReadStreamResponse #2: %v", err)
	}
	if resp2.OK {
		t.Fatal("second stream should have been rejected at the cap")
	}
	if !strings.Contains(resp2.Error, "max concurrent streams") {
		t.Errorf("error = %q, want 'max concurrent streams'", resp2.Error)
	}
}

// TestMuxStream_EnvelopePendingDoesNotConsumeStreamCap is the regression
// test for the TOCTOU fix where pre-fix, MaxConnections slots were
// reserved BEFORE the per-stream envelope was read or validated, letting
// a malicious or slow peer hold slots for up to muxStreamReadTimeout
// without dialing any target. After the fix, streamSem is acquired only
// after a successful envelope+allowlist check, so envelope-pending
// streams cannot starve legitimate connections of the active-stream cap.
func TestMuxStream_EnvelopePendingDoesNotConsumeStreamCap(t *testing.T) {
	target := startEchoTarget(t)
	rig := newMuxTestRig(t, Config{
		MaxConnections: 1, // strict cap
	})

	// Open a stream but DELAY sending the envelope. Under the old code
	// this would have already consumed the streamSem slot the moment
	// the accept loop dispatched the stream. Under the new code, the
	// slot is only acquired after envelope+allowlist validation.
	stalled, err := rig.clientSess.OpenStream()
	if err != nil {
		t.Fatalf("OpenStream #1 (stalled): %v", err)
	}
	defer stalled.Close() //nolint:errcheck // test cleanup

	// Wait for the listener to AcceptStream + dispatch the handler;
	// the handler is now blocked in ReadStreamEnvelope, holding
	// pendingSem (streamSem is NOT held yet — that's acquired only
	// after envelope+allowlist validation).
	rig.waitForPendingSem(t, 1, 2*time.Second)

	// Open a SECOND stream and send a valid envelope. With
	// MaxConnections=1, this only works if envelope-pending stream #1
	// has not consumed the cap.
	active, err := rig.clientSess.OpenStream()
	if err != nil {
		t.Fatalf("OpenStream #2 (active): %v", err)
	}
	defer active.Close() //nolint:errcheck // test cleanup
	if err := protocol.WriteStreamEnvelope(active, protocol.ConnectEnvelope{
		Version: protocol.CurrentVersion,
		Target:  target,
	}); err != nil {
		t.Fatalf("WriteStreamEnvelope #2: %v", err)
	}
	_ = active.SetReadDeadline(time.Now().Add(3 * time.Second))
	resp, err := protocol.ReadStreamResponse(active)
	if err != nil {
		t.Fatalf("ReadStreamResponse #2: %v", err)
	}
	if !resp.OK {
		t.Fatalf("envelope-pending stream consumed the cap; active stream rejected: %q", resp.Error)
	}

	// Sanity: bridge is live — echo a byte through.
	if _, err := active.Write([]byte("p")); err != nil {
		t.Fatalf("write payload: %v", err)
	}
	buf := make([]byte, 1)
	_ = active.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := active.Read(buf); err != nil {
		t.Fatalf("read echo: %v", err)
	}
	if string(buf) != "p" {
		t.Errorf("echo = %q, want %q", buf, "p")
	}
}

// TestMuxStream_PendingCapRejectsOverflow is the regression test for
// the envelope-pending DoS protection. With `MaxConnections=1` (so
// pendingMax=2), opening 2 streams that withhold their envelopes
// fills `pendingSem`. A 3rd OpenStream must receive a "listener busy"
// response before being closed by the accept loop, and the cap must
// not leak — closing the held streams must let a fresh stream proceed.
func TestMuxStream_PendingCapRejectsOverflow(t *testing.T) {
	target := startEchoTarget(t)
	rig := newMuxTestRig(t, Config{
		MaxConnections: 1, // pendingMax = MaxConnections * 2 = 2
	})

	// Open 2 streams that withhold their envelopes — they sit in
	// handleMuxStream blocked on ReadStreamEnvelope, each holding a
	// pendingSem slot.
	pending := make([]*smux.Stream, 0, 2)
	for i := range 2 {
		s, err := rig.clientSess.OpenStream()
		if err != nil {
			t.Fatalf("OpenStream #%d (pending): %v", i, err)
		}
		pending = append(pending, s)
	}

	// Wait until both handlers have dispatched and are blocked in
	// ReadStreamEnvelope holding their pendingSem slots.
	rig.waitForPendingSem(t, 2, 2*time.Second)

	// A 3rd OpenStream must be rejected by the accept loop with
	// "listener busy" before any handler runs (no envelope read).
	rejected, err := rig.clientSess.OpenStream()
	if err != nil {
		t.Fatalf("OpenStream #3 (rejected): %v", err)
	}
	defer rejected.Close() //nolint:errcheck // test cleanup

	_ = rejected.SetReadDeadline(time.Now().Add(3 * time.Second))
	resp, err := protocol.ReadStreamResponse(rejected)
	if err != nil {
		t.Fatalf("ReadStreamResponse #3: %v", err)
	}
	if resp.OK {
		t.Fatalf("3rd stream was accepted; expected pendingSem rejection")
	}
	if !strings.Contains(resp.Error, "listener busy") {
		t.Errorf("rejection error = %q, want substring %q", resp.Error, "listener busy")
	}

	// Close the held streams to free pendingSem — their handlers
	// should exit (envelope read fails on closed stream) and release
	// their pendingSem slots.
	for _, s := range pending {
		_ = s.Close()
	}
	// Wait for the handlers to observe the close and release their
	// pendingSem slots.
	rig.waitForPendingSem(t, 0, 2*time.Second)

	// A subsequent stream must now succeed — proving pendingSem is not
	// leaked by either the rejection path or the closed-pending-stream
	// path.
	fresh, err := rig.clientSess.OpenStream()
	if err != nil {
		t.Fatalf("OpenStream #4 (after recovery): %v", err)
	}
	defer fresh.Close() //nolint:errcheck // test cleanup
	if err := protocol.WriteStreamEnvelope(fresh, protocol.ConnectEnvelope{
		Version: protocol.CurrentVersion,
		Target:  target,
	}); err != nil {
		t.Fatalf("WriteStreamEnvelope #4: %v", err)
	}
	_ = fresh.SetReadDeadline(time.Now().Add(3 * time.Second))
	freshResp, err := protocol.ReadStreamResponse(fresh)
	if err != nil {
		t.Fatalf("ReadStreamResponse #4: %v", err)
	}
	if !freshResp.OK {
		t.Fatalf("post-recovery stream rejected: %q (pendingSem leaked)", freshResp.Error)
	}
}

// TestMuxStream_ListenerHalfCloseToTarget is the regression test for
// half-close propagation on the listener-side mux path: when the
// sender CloseWrites the smux stream (e.g. HTTP/1.0, SSH ProxyCommand
// stdin closure, "send EOF then read response"), the bridge must
// propagate that to the target conn via CloseWrite so the target
// observes EOF on its read.
func TestMuxStream_ListenerHalfCloseToTarget(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	// Target accepts, then blocks reading until EOF. If the listener
	// bridge fails to propagate the smux CloseWrite as a TCP FIN, the
	// target's io.Copy stays blocked forever and the test times out
	// on the eofObserved select below.
	eofObserved := make(chan struct{})
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()               //nolint:errcheck // test cleanup
		_, _ = io.Copy(io.Discard, c) // returns when listener-side half-closes
		close(eofObserved)
	}()

	rig := newMuxTestRig(t, Config{MaxConnections: 10})

	stream, err := rig.clientSess.OpenStream()
	if err != nil {
		t.Fatalf("OpenStream: %v", err)
	}
	defer stream.Close() //nolint:errcheck // test cleanup

	if err := protocol.WriteStreamEnvelope(stream, protocol.ConnectEnvelope{
		Version: protocol.CurrentVersion,
		Target:  ln.Addr().String(),
	}); err != nil {
		t.Fatalf("WriteStreamEnvelope: %v", err)
	}
	resp, err := protocol.ReadStreamResponse(stream)
	if err != nil {
		t.Fatalf("ReadStreamResponse: %v", err)
	}
	if !resp.OK {
		t.Fatalf("envelope rejected: %q", resp.Error)
	}

	// Send a request body, then half-close the sender → bridge must
	// CloseWrite the target conn so the target's io.Copy sees EOF.
	if _, err := stream.Write([]byte("REQUEST")); err != nil {
		t.Fatalf("write request: %v", err)
	}
	if err := stream.CloseWrite(); err != nil {
		t.Fatalf("CloseWrite stream: %v", err)
	}

	// The only thing we assert is that the target observed EOF — i.e.
	// the listener bridge translated stream.CloseWrite() into a
	// conn.CloseWrite() on the target side. The reverse direction
	// (response flowing back through the smux stream) is already
	// exercised by TestMuxStream_HappyPath_EchoesData and friends;
	// reading here would re-introduce a known smux v1.5.57
	// tryReadV2/die race on fully-half-closed streams.
	select {
	case <-eofObserved:
	case <-time.After(3 * time.Second):
		t.Fatal("target did not observe EOF: listener bridge did not propagate half-close from smux stream to target conn")
	}
}

// TestConnSemaphore exercises the listener-local connSemaphore in
// isolation.
func TestConnSemaphore(t *testing.T) {
	t.Run("unlimited when max<=0", func(t *testing.T) {
		s := newConnSemaphore(0)
		for range 100 {
			if !s.tryAcquire() {
				t.Fatal("unlimited semaphore should always acquire")
			}
		}
		// release is a no-op but must not panic.
		s.release()
	})

	t.Run("bounded acquire/release", func(t *testing.T) {
		s := newConnSemaphore(2)
		if !s.tryAcquire() {
			t.Fatal("first acquire should succeed")
		}
		if !s.tryAcquire() {
			t.Fatal("second acquire should succeed")
		}
		if s.tryAcquire() {
			t.Fatal("third acquire should fail")
		}
		s.release()
		if !s.tryAcquire() {
			t.Fatal("acquire after release should succeed")
		}
	})
}

// --- handleConnection dispatch tests ---

// newDispatchTestServer stands up an httptest server that runs the real
// handleConnection on each incoming WebSocket. It returns the ws:// URL
// the test client should dial, plus the shared streamSem (so tests can
// pre-fill it if they want to exercise rejection).
func newDispatchTestServer(t *testing.T, cfg Config) (string, *connSemaphore) {
	t.Helper()
	if cfg.MaxConnections == 0 {
		cfg.MaxConnections = 10
	}
	if cfg.ConnectTimeout == 0 {
		cfg.ConnectTimeout = 5 * time.Second
	}
	if cfg.TCPKeepAlive == 0 {
		cfg.TCPKeepAlive = 30 * time.Second
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	streamSem := newConnSemaphore(cfg.MaxConnections)
	pendingMax := 0
	if cfg.MaxConnections > 0 {
		pendingMax = cfg.MaxConnections * 2
	}
	pendingSem := newConnSemaphore(pendingMax)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer ws.CloseNow() //nolint:errcheck // test cleanup
		handleConnection(r.Context(), ws, cfg, streamSem, pendingSem)
	}))
	t.Cleanup(srv.Close)
	return "ws" + strings.TrimPrefix(srv.URL, "http"), streamSem
}

// TestHandleConnection_RejectsUnsupportedVersion verifies the exact
// "unsupported protocol version" rejection string that the sender's
// sticky v1-fallback detector (isMuxUnsupportedRejection) depends on.
// A regression here silently breaks rolling upgrades from v2 senders to
// older v1-only listeners.
func TestHandleConnection_RejectsUnsupportedVersion(t *testing.T) {
	wsURL, _ := newDispatchTestServer(t, Config{})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ws, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer ws.CloseNow() //nolint:errcheck // test cleanup

	env := protocol.ConnectEnvelope{Version: 99, Target: "127.0.0.1:1"}
	data, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := ws.Write(ctx, websocket.MessageText, data); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, resp, err := ws.Read(ctx)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var got protocol.ConnectResponse
	if err := json.Unmarshal(resp, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.OK {
		t.Fatal("expected !OK")
	}
	if !strings.Contains(got.Error, "unsupported protocol version") {
		t.Errorf("error = %q, want substring %q", got.Error, "unsupported protocol version")
	}
}

// TestHandleConnection_DispatchesV1 verifies that a v1 ConnectEnvelope
// reaches handleSingleConnection: an OK response is sent and the bridge
// to a local echo target is established (an end-to-end echo proves
// dispatch).
func TestHandleConnection_DispatchesV1(t *testing.T) {
	target := startEchoTarget(t)
	wsURL, _ := newDispatchTestServer(t, Config{})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ws, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer ws.CloseNow() //nolint:errcheck // test cleanup

	env := protocol.ConnectEnvelope{Version: protocol.CurrentVersion, Target: target}
	data, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := ws.Write(ctx, websocket.MessageText, data); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, resp, err := ws.Read(ctx)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var got protocol.ConnectResponse
	if err := json.Unmarshal(resp, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !got.OK {
		t.Fatalf("v1 dispatch failed: %q", got.Error)
	}
	// Bridge is now active; echo a payload through.
	const payload = "hello v1\n"
	if err := ws.Write(ctx, websocket.MessageBinary, []byte(payload)); err != nil {
		t.Fatalf("write payload: %v", err)
	}
	_, echoed, err := ws.Read(ctx)
	if err != nil {
		t.Fatalf("read echo: %v", err)
	}
	if string(echoed) != payload {
		t.Errorf("echo = %q, want %q", echoed, payload)
	}
}

// TestHandleConnection_DispatchesV2Mux verifies that a v2 MuxHandshake
// reaches handleMuxSession: an OK response is sent, the server hands the
// underlying WebSocket to smux, and a logical stream can be opened. The
// stream is exercised by sending a malformed envelope and observing the
// per-stream "unsupported protocol version" reply — that proves we
// actually reached handleMuxStream, not just sent OK and returned.
func TestHandleConnection_DispatchesV2Mux(t *testing.T) {
	wsURL, _ := newDispatchTestServer(t, Config{})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ws, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer ws.CloseNow() //nolint:errcheck // test cleanup

	hs := protocol.MuxHandshake{Version: protocol.MuxVersion, Mode: protocol.MuxMode}
	data, err := json.Marshal(hs)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := ws.Write(ctx, websocket.MessageText, data); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, resp, err := ws.Read(ctx)
	if err != nil {
		t.Fatalf("read handshake response: %v", err)
	}
	var got protocol.ConnectResponse
	if err := json.Unmarshal(resp, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !got.OK {
		t.Fatalf("v2 mux dispatch failed: %q", got.Error)
	}
	// Server has handed the ws to smux.Server. Complete the smux
	// handshake on the client side and open a stream.
	netConn := websocket.NetConn(ctx, ws, websocket.MessageBinary)
	sess, err := smux.Client(netConn, muxTestSmuxCfg())
	if err != nil {
		t.Fatalf("smux.Client: %v", err)
	}
	defer sess.Close() //nolint:errcheck // test cleanup
	stream, err := sess.OpenStream()
	if err != nil {
		t.Fatalf("OpenStream: %v", err)
	}
	defer stream.Close() //nolint:errcheck // test cleanup
	_ = stream.SetReadDeadline(time.Now().Add(3 * time.Second))
	if err := protocol.WriteStreamEnvelope(stream, protocol.ConnectEnvelope{
		Version: 99,
		Target:  "127.0.0.1:1",
	}); err != nil {
		t.Fatalf("WriteStreamEnvelope: %v", err)
	}
	sresp, err := protocol.ReadStreamResponse(stream)
	if err != nil {
		t.Fatalf("ReadStreamResponse: %v", err)
	}
	if sresp.OK || !strings.Contains(sresp.Error, "unsupported protocol version") {
		t.Errorf("mux stream response: OK=%v err=%q", sresp.OK, sresp.Error)
	}
}
