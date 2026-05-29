//go:build e2e

package mock

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/philsphicas/aztunnel/internal/listener"
	"github.com/philsphicas/aztunnel/internal/metrics"
	"github.com/philsphicas/aztunnel/internal/relay"
	"github.com/philsphicas/aztunnel/internal/sender"
	"github.com/philsphicas/aztunnel/mockrelay/server"
	dto "github.com/prometheus/client_model/go"
)

// TestMockEmulates_NoListenerReturns404 asserts the mock relay
// returns HTTP 404 pre-WebSocket-upgrade when a sender connects to
// a hyco with no registered listener. This is the wire-level
// contract DialWithRetry depends on for backoff. Carried over from
// the deleted mockrelay/server/integration_test.go.
func TestMockEmulates_NoListenerReturns404(t *testing.T) {
	host, srv := startEmulationMockRelay(t)
	tok := mintProbeToken(t, host, "nobody")
	resp, err := srv.Client().Get(srv.URL +
		"/$hc/nobody?sb-hc-action=connect&sb-hc-token=" + url.QueryEscape(tok))
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close() //nolint:errcheck // best-effort cleanup
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusNotFound)
	}
}

// TestMockEmulates_ControlChannel_ConnectionLostOnRenew arms the
// mock relay to send a polite close on the listener's control WS as
// soon as it sees the listener's first renewToken frame. The
// listener emits renew_ok (write to local TCP send buffer
// succeeds), then control_ended{reason=read_failed}.
//
// Naming note: this exercises a mock-only fault knob
// (server.WithCloseControlOnRenew). Renamed from
// TestMockEmulates_* to TestMockFeature_* would match the
// taxonomy more strictly; the spec calls it Emulates because the
// log shape (control_ended{reason=read_failed}) is what Azure
// would also emit on a real renew-time close. Keeping
// TestMockEmulates_ because the assertion is about aztunnel's log
// shape, which is portable.
func TestMockEmulates_ControlChannel_ConnectionLostOnRenew(t *testing.T) {
	host, copts := startFaultyMockRelay(t, server.WithCloseControlOnRenew())
	logs, _ := startFaultyListener(t, host, copts, 100*time.Millisecond)

	waitForLogContaining(t, logs, 10*time.Second,
		`msg=`+relay.EventRenewOK)
	waitForLogContaining(t, logs, 10*time.Second,
		`msg=`+relay.EventControlEnded,
		`reason=`+relay.ControlEndedReadFailed)
}

// TestMockEmulates_ControlChannel_RejectControlDial arms the mock
// relay to refuse the listener's control dial with HTTP 503.
// control_started is emitted only after a successful dial, so
// control_ended{reason=dial_failed} is the single lifecycle event
// for the failed attempt.
func TestMockEmulates_ControlChannel_RejectControlDial(t *testing.T) {
	host, copts := startFaultyMockRelay(t, server.WithRejectControlDial())
	logs, _ := startFaultyListener(t, host, copts, 0)

	waitForLogContaining(t, logs, 10*time.Second,
		`msg=`+relay.EventControlEnded,
		`reason=`+relay.ControlEndedDialFailed)
}

// TestMockEmulates_AcceptDropped_DialFailed drives an
// accept_dropped{reason=dial_failed} log line by standing up a
// custom TLS test server that emits an accept frame whose address
// field points at a closed TCP port. Bypasses mockrelay entirely
// to exercise the relay-package read loop → handleAccept dial path
// against a hand-rolled control-channel server.
func TestMockEmulates_AcceptDropped_DialFailed(t *testing.T) {
	refused := "ws://" + refusedAddrEmul(t)
	host, copts := startCustomControlServer(t, refused)
	logs, _ := startFaultyListener(t, host, copts, 0)

	waitForLogContaining(t, logs, 10*time.Second,
		`msg=`+relay.EventAcceptAttempted)
	waitForLogContaining(t, logs, 10*time.Second,
		`msg=`+relay.EventAcceptDropped,
		`reason=`+relay.AcceptDroppedDialFailed)
}

// TestMockEmulates_TokenFetchMetric verifies relay.WithMetrics
// applied to a real TokenProvider produces observations on the
// shared Metrics surface for both result=ok (success path through
// a listener + sender topology) and result=error (separately
// wrapped erroring provider).
func TestMockEmulates_TokenFetchMetric(t *testing.T) {
	host, clientOpts := startMockRelay(t, server.DelayProfileDefault)

	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	t.Cleanup(func() { cancel(); wg.Wait() })

	entity := mustEntityName(t)
	silentLogger := slog.New(slog.NewTextHandler(io.Discard, nil))
	m := metrics.New()
	sasInner := relay.TokenProvider(&relay.SASTokenProvider{
		KeyName: server.DefaultSASKeyName,
		Key:     server.DefaultSASKey,
	})
	listenerTP := relay.WithMetrics(sasInner, m, relay.ProviderSAS)
	senderTP := relay.WithMetrics(sasInner, m, relay.ProviderSAS)

	echoLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen echo: %v", err)
	}
	t.Cleanup(func() { _ = echoLn.Close() })
	go runEchoServerEmul(echoLn)

	wg.Add(1)
	go func() {
		defer wg.Done()
		err := listener.ListenAndServe(ctx, listener.Config{
			Endpoint:      host,
			EntityPath:    entity,
			TokenProvider: listenerTP,
			ClientOptions: clientOpts,
			AllowList:     []string{echoLn.Addr().String()},
			Logger:        silentLogger,
			Metrics:       m,
		})
		if err != nil && ctx.Err() == nil {
			t.Logf("listener exited: %v", err)
		}
	}()
	if !waitForGauge(m, "aztunnel_control_channel_connected", 1, 15*time.Second) {
		t.Fatalf("listener never reported control_channel_connected")
	}

	addrCh := make(chan net.Addr, 1)
	wg.Add(1)
	go func() {
		defer wg.Done()
		err := sender.PortForward(ctx, sender.PortForwardConfig{
			Endpoint:      host,
			EntityPath:    entity,
			TokenProvider: senderTP,
			ClientOptions: clientOpts,
			Target:        echoLn.Addr().String(),
			BindAddress:   "127.0.0.1:0",
			Logger:        silentLogger,
			Metrics:       m,
			Ready: func(a net.Addr) {
				select {
				case addrCh <- a:
				default:
				}
			},
		})
		if err != nil && ctx.Err() == nil {
			t.Logf("sender exited: %v", err)
		}
	}()

	var senderAddr net.Addr
	select {
	case senderAddr = <-addrCh:
	case <-time.After(15 * time.Second):
		t.Fatalf("sender never became ready")
	}

	conn, err := net.DialTimeout("tcp", senderAddr.String(), 5*time.Second)
	if err != nil {
		t.Fatalf("dial sender: %v", err)
	}
	defer conn.Close() //nolint:errcheck // best-effort cleanup
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	payload := []byte("token-fetch-metric\n")
	if _, err := conn.Write(payload); err != nil {
		t.Fatalf("write: %v", err)
	}
	buf := make([]byte, len(payload))
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatalf("read: %v", err)
	}

	// Drive the error path through a separately wrapped provider.
	errSentinel := errors.New("simulated upstream failure")
	failingTP := relay.WithMetrics(&erroringTokenProvider{err: errSentinel}, m, relay.ProviderSAS)
	for i := 0; i < 2; i++ {
		if _, err := failingTP.GetToken(ctx, "ignored"); !errors.Is(err, errSentinel) {
			t.Fatalf("failingTP.GetToken: got %v, want wraps %v", err, errSentinel)
		}
	}

	if !waitForCounter(m, "aztunnel_token_fetch_total", relay.ProviderSAS, "ok", 2, 5*time.Second) {
		t.Fatalf("token_fetch_total{provider=sas,result=ok} never reached 2 (got %v)",
			counterValueByProviderResult(m, "aztunnel_token_fetch_total", relay.ProviderSAS, "ok"))
	}
	if got := counterValueByProviderResult(m, "aztunnel_token_fetch_total", relay.ProviderSAS, "error"); got != 2 {
		t.Errorf("token_fetch_total{provider=sas,result=error} = %v, want 2", got)
	}
	if got := histogramCount(m, "aztunnel_token_fetch_seconds", relay.ProviderSAS, "ok"); got < 2 {
		t.Errorf("token_fetch_seconds{provider=sas,result=ok} count = %v, want >= 2", got)
	}
	if got := histogramCount(m, "aztunnel_token_fetch_seconds", relay.ProviderSAS, "error"); got != 2 {
		t.Errorf("token_fetch_seconds{provider=sas,result=error} count = %v, want 2", got)
	}
}

// TestMockEmulates_WSCloseCodeOnEnvelopeRead arms the mock relay's
// WithCloseCodeOnAccept fault knob to close the listener-rendezvous
// WebSocket with a configured close code before the listener reads
// the connect envelope. Asserts the listener's "failed to read
// envelope" log enrichment carries close_code=<code>. Mock-only
// because the fault knob has no Azure analogue.
func TestMockEmulates_WSCloseCodeOnEnvelopeRead(t *testing.T) {
	cases := []struct {
		name string
		code int
	}{
		{"Code1011_ServerError", 1011},
		{"Code4400_AppDefined", 4400},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runWSCloseCodeCase(t, tc.code)
		})
	}
}

// runWSCloseCodeCase brings up an in-process listener + port-
// forward sender against a mock relay armed with
// WithCloseCodeOnAccept(code), triggers one connection, and
// asserts the listener's "failed to read envelope" log carries
// close_code=<code>.
func runWSCloseCodeCase(t *testing.T, code int) {
	t.Helper()
	host, clientOpts := startMockRelayWithCloseCode(t, code)
	entity := mockOnlyEntity(t)
	tokenProvider := &relay.SASTokenProvider{
		KeyName: server.DefaultSASKeyName,
		Key:     server.DefaultSASKey,
	}

	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	t.Cleanup(func() { cancel(); wg.Wait() })

	echo := startCloseCodeEcho(t)

	listenerLogs := newCaptureBuffer()
	listenerMetrics := metrics.New()
	listenerLogger := slog.New(slog.NewTextHandler(listenerLogs, &slog.HandlerOptions{Level: slog.LevelDebug}))

	wg.Add(1)
	go func() {
		defer wg.Done()
		err := listener.ListenAndServe(ctx, listener.Config{
			Endpoint:      host,
			EntityPath:    entity,
			TokenProvider: tokenProvider,
			ClientOptions: clientOpts,
			AllowList:     []string{echo.Addr().String()},
			Logger:        listenerLogger,
			Metrics:       listenerMetrics,
		})
		if err != nil && ctx.Err() == nil {
			t.Logf("listener exited: %v", err)
		}
	}()
	if !waitForGauge(listenerMetrics, "aztunnel_control_channel_connected", 1, 15*time.Second) {
		t.Fatalf("listener never reported control_channel_connected")
	}

	senderLogs := newCaptureBuffer()
	senderLogger := slog.New(slog.NewTextHandler(senderLogs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	senderAddrCh := make(chan net.Addr, 1)
	wg.Add(1)
	go func() {
		defer wg.Done()
		err := sender.PortForward(ctx, sender.PortForwardConfig{
			Endpoint:      host,
			EntityPath:    entity,
			TokenProvider: tokenProvider,
			ClientOptions: clientOpts,
			Target:        echo.Addr().String(),
			BindAddress:   "127.0.0.1:0",
			Logger:        senderLogger,
			Metrics:       metrics.New(),
			Ready: func(a net.Addr) {
				select {
				case senderAddrCh <- a:
				default:
				}
			},
		})
		if err != nil && ctx.Err() == nil {
			t.Logf("sender exited: %v", err)
		}
	}()

	var senderAddr net.Addr
	select {
	case senderAddr = <-senderAddrCh:
	case <-time.After(15 * time.Second):
		t.Fatalf("sender Ready callback never fired")
	}

	conn, err := net.DialTimeout("tcp", senderAddr.String(), 5*time.Second)
	if err != nil {
		t.Fatalf("dial sender: %v", err)
	}
	_, _ = conn.Write([]byte("ping\n"))
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, _ = io.ReadAll(conn)
	_ = conn.Close()

	pattern := `msg="failed to read envelope".* close_code=` + regexpInt(code)
	line := waitForLogMatching(t, listenerLogs.String, 10*time.Second, pattern)
	if !strings.Contains(line, "close_code="+regexpInt(code)) {
		t.Fatalf("listener envelope-read line missing close_code=%d:\n%s", code, line)
	}
}

// startEmulationMockRelay starts a plain mock relay (no fault
// knobs) and returns the host + the httptest server for
// HTTP-level probes. Mirrors the legacy startMockRelay helper
// from the deleted server/integration_test.go.
func startEmulationMockRelay(t *testing.T) (host string, srv *httptest.Server) {
	t.Helper()
	rs, err := server.NewServer(server.Config{
		Logger:            slog.New(slog.NewTextHandler(io.Discard, nil)),
		RendezvousTimeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	srv = httptest.NewTLSServer(rs.Handler())
	u, _ := url.Parse(srv.URL)
	t.Cleanup(srv.Close)
	return u.Host, srv
}

// mintProbeToken builds a short-lived SAS token using the mock's
// default credentials so probe requests get past validateSAS.
func mintProbeToken(t *testing.T, host, entity string) string {
	t.Helper()
	tok, err := relay.GenerateSASToken(
		relay.ResourceURI(host, entity),
		server.DefaultSASKeyName,
		server.DefaultSASKey,
		1*time.Minute,
	)
	if err != nil {
		t.Fatalf("mint probe token: %v", err)
	}
	return tok
}

// startFaultyMockRelay stands up a mock relay with the supplied
// fault-injection options. Returns host + ClientOptions
// configured for InsecureSkipVerify.
func startFaultyMockRelay(t *testing.T, opts ...server.Option) (string, relay.ClientOptions) {
	t.Helper()
	rs, err := server.NewServerForTesting(server.Config{
		Logger:            slog.New(slog.NewTextHandler(io.Discard, nil)),
		RendezvousTimeout: 1 * time.Second,
	}, opts...)
	if err != nil {
		t.Fatalf("new mock relay: %v", err)
	}
	srv := httptest.NewTLSServer(rs.Handler())
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	return u.Host, relay.ClientOptions{
		TLSConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // test cert
	}
}

// startFaultyListener brings up an in-process aztunnel listener
// wired to host with the supplied RenewInterval (zero selects the
// package default). Returns the captured-log accessor and a Stop
// closure.
func startFaultyListener(t *testing.T, host string, opts relay.ClientOptions, renewInterval time.Duration) (func() string, func()) {
	t.Helper()
	logs := newCaptureBuffer()
	cfg := listener.Config{
		Endpoint:      host,
		EntityPath:    mustEntityName(t),
		TokenProvider: &relay.SASTokenProvider{KeyName: server.DefaultSASKeyName, Key: server.DefaultSASKey},
		ClientOptions: opts,
		Logger:        slog.New(slog.NewTextHandler(logs, nil)),
		Metrics:       metrics.New(),
		RenewInterval: renewInterval,
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = listener.ListenAndServe(ctx, cfg)
	}()
	var stopOnce sync.Once
	stop := func() {
		stopOnce.Do(func() {
			cancel()
			<-done
		})
	}
	t.Cleanup(stop)
	return logs.String, stop
}

// waitForLogContaining polls logs() at 20 ms until every needle is
// present or timeout elapses.
func waitForLogContaining(t *testing.T, logs func() string, timeout time.Duration, needles ...string) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		s := logs()
		hit := true
		for _, n := range needles {
			if !strings.Contains(s, n) {
				hit = false
				break
			}
		}
		if hit {
			return s
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("did not observe all needles %v within %s; captured:\n%s",
		needles, timeout, logs())
	return ""
}

// startCustomControlServer stands up a TLS HTTP server that
// accepts the listener's control-channel WS upgrade and sends a
// single accept frame whose address field points at acceptAddr.
func startCustomControlServer(t *testing.T, acceptAddr string) (string, relay.ClientOptions) {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer ws.CloseNow() //nolint:errcheck // best-effort cleanup
		payload, _ := json.Marshal(map[string]any{
			"accept": map[string]any{
				"address": acceptAddr,
				"id":      "test-id",
			},
		})
		if err := ws.Write(r.Context(), websocket.MessageText, payload); err != nil {
			return
		}
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	return u.Host, relay.ClientOptions{
		TLSConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // test cert
	}
}

// refusedAddrEmul binds a TCP listener to a free port, closes it,
// returns "127.0.0.1:N". A connect attempt gets ECONNREFUSED.
// Renamed from refusedAddr to avoid name collision with the
// scenarios package helper.
func refusedAddrEmul(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	addr := l.Addr().String()
	if err := l.Close(); err != nil {
		t.Fatalf("close listener: %v", err)
	}
	return addr
}

// startMockRelayWithCloseCode variant of startMockRelay that
// constructs the server via NewServerForTesting with
// WithCloseCodeOnAccept armed.
func startMockRelayWithCloseCode(t *testing.T, code int) (host string, opts relay.ClientOptions) {
	t.Helper()
	rs, err := server.NewServerForTesting(server.Config{
		Logger:            slog.New(slog.NewTextHandler(io.Discard, nil)),
		RendezvousTimeout: 1 * time.Second,
	}, server.WithCloseCodeOnAccept(code))
	if err != nil {
		t.Fatalf("NewServerForTesting: %v", err)
	}
	srv := httptest.NewTLSServer(rs.Handler())
	u, _ := url.Parse(srv.URL)
	host = u.Host
	opts = relay.ClientOptions{
		TLSConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // test cert
	}
	t.Cleanup(srv.Close)
	return host, opts
}

// startCloseCodeEcho stands up a tiny TCP echo server local to the
// test so the listener's AllowList has a real address to permit.
func startCloseCodeEcho(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("echo listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close() //nolint:errcheck // best-effort cleanup
				_, _ = io.Copy(c, c)
			}(c)
		}
	}()
	return ln
}

// mockOnlyEntity mints a short unique entity name without using the
// shared mustEntityName helper.
func mockOnlyEntity(t *testing.T) string {
	t.Helper()
	return "ws-close-" + mustEntityName(t)
}

// waitForLogMatching polls logs() until any line matches re,
// bounded by timeout.
func waitForLogMatching(t *testing.T, logs func() string, timeout time.Duration, pattern string) string {
	t.Helper()
	re := regexp.MustCompile(pattern)
	deadline := time.Now().Add(timeout)
	for {
		snapshot := logs()
		for _, line := range strings.Split(snapshot, "\n") {
			if re.MatchString(line) {
				return line
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %v waiting for log line matching %q\n--- logs ---\n%s",
				timeout, pattern, snapshot)
			return ""
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// regexpInt returns the decimal form of n. Used by close-code
// match patterns; small positive integers contain no regex
// metacharacters.
func regexpInt(n int) string { return strconvItoa(n) }

func strconvItoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// runEchoServerEmul accepts and echoes until ln is closed. Local
// rename to avoid colliding with any future scenarios helper.
func runEchoServerEmul(ln net.Listener) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		go func(conn net.Conn) {
			defer conn.Close() //nolint:errcheck // best-effort cleanup
			_, _ = io.Copy(conn, conn)
		}(c)
	}
}

// erroringTokenProvider always returns the configured error.
type erroringTokenProvider struct{ err error }

func (e *erroringTokenProvider) GetToken(context.Context, string) (string, error) {
	return "", e.err
}

// waitForCounter polls m's registry for the given counter to reach
// at least want under the (provider, result) labels before
// timeout. Returns true on success, false on deadline.
func waitForCounter(m *metrics.Metrics, name, provider, result string, want float64, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if counterValueByProviderResult(m, name, provider, result) >= want {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

// counterValueByProviderResult returns the single-sample value of
// a counter under the (provider, result) label pair.
func counterValueByProviderResult(m *metrics.Metrics, name, provider, result string) float64 {
	families, err := m.Registry.Gather()
	if err != nil {
		return 0
	}
	for _, f := range families {
		if f.GetName() != name {
			continue
		}
		for _, sample := range f.GetMetric() {
			if matchesProviderResult(sample.GetLabel(), provider, result) {
				return sample.GetCounter().GetValue()
			}
		}
	}
	return 0
}

// histogramCount returns the SampleCount of a histogram under the
// (provider, result) label pair.
func histogramCount(m *metrics.Metrics, name, provider, result string) uint64 {
	families, err := m.Registry.Gather()
	if err != nil {
		return 0
	}
	for _, f := range families {
		if f.GetName() != name {
			continue
		}
		for _, sample := range f.GetMetric() {
			if matchesProviderResult(sample.GetLabel(), provider, result) {
				return sample.GetHistogram().GetSampleCount()
			}
		}
	}
	return 0
}

// matchesProviderResult reports whether a metric sample's labels
// match the given (provider, result) pair exactly.
func matchesProviderResult(labels []*dto.LabelPair, provider, result string) bool {
	var sawProvider, sawResult bool
	for _, lp := range labels {
		switch lp.GetName() {
		case "provider":
			if lp.GetValue() != provider {
				return false
			}
			sawProvider = true
		case "result":
			if lp.GetValue() != result {
				return false
			}
			sawResult = true
		}
	}
	return sawProvider && sawResult
}
