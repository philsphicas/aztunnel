package parity

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/philsphicas/aztunnel/internal/listener"
	"github.com/philsphicas/aztunnel/internal/metrics"
	"github.com/philsphicas/aztunnel/internal/relay"
	"github.com/philsphicas/aztunnel/internal/sender"
	"github.com/philsphicas/aztunnel/mockrelay/server"
)

// TestMockOnly_WSCloseCodeOnEnvelopeRead exercises the listener-side
// "failed to read envelope" log enrichment by arming the mock relay's
// WithCloseCodeOnAccept fault knob. Each subtest drives one
// port-forward bridge attempt: the mock closes the listener-rendezvous
// WebSocket with the configured close code before the listener can
// read the connect envelope, and we assert the listener's
// envelope-read failure log carries close_code=<code>.
//
// Mockrelay-only because the knob is a testing-only fault-injection
// hook with no Azure analogue — Azure Relay does not surface
// configurable non-normal close codes on demand.
//
// The mock's accept knob fires before envelope round-trip, so the
// post-bridge "bridge ended" log path on the listener and the
// post-bridge "forward failed" / "socks5 failed" paths on the sender
// are NOT exercised here. Their close_code wiring is the same call
// shape as the path covered here (relay.WSCloseCode + slog attr
// append); the WSCloseCode helper itself is unit-tested in
// internal/relay/bridge_test.go.
func TestMockOnly_WSCloseCodeOnEnvelopeRead(t *testing.T) {
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

// runWSCloseCodeCase brings up an in-process listener + port-forward
// sender against a mock relay armed with WithCloseCodeOnAccept(code),
// triggers one connection attempt, and asserts the listener's
// "failed to read envelope" log line carries close_code=<code>.
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
	t.Cleanup(func() {
		cancel()
		wg.Wait()
	})

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

	// One bridge attempt is enough to consume the single-shot
	// close-code fault. The TCP dial completes immediately at the
	// sender bind; the relay-side failure surfaces later as a
	// closed-WS error on both ends.
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
	if !strings.Contains(line, fmt.Sprintf("close_code=%d", code)) {
		t.Fatalf("listener envelope-read line missing expected close_code=%d:\n%s", code, line)
	}
}

// startMockRelayWithCloseCode is a variant of startMockRelay that
// constructs the server via NewServerForTesting with
// WithCloseCodeOnAccept armed. Production callers cannot inject
// faults; this lives in test code only.
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
// test so the listener's AllowList has a real address to permit. The
// echo behaviour is incidental — the listener never actually bridges
// data through it because the WS gets force-closed by the fault knob
// before any payload reaches the target.
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
// shared mustEntityName helper, which embeds the full test name; for
// table-driven subtests that produces values with characters we'd
// rather keep out of the URL path.
func mockOnlyEntity(t *testing.T) string {
	t.Helper()
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return "ws-close-" + hex.EncodeToString(b[:])
}

// waitForLogMatching polls logs() until any newline-delimited line
// matches re, bounded by timeout. Returns the matching line. Fails
// the test on deadline.
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

// regexpInt returns the decimal form of n as a regexp-safe literal.
// All callers pass small positive close codes, so the decimal form
// contains no regex metacharacters and needs no escaping.
func regexpInt(n int) string { return fmt.Sprintf("%d", n) }
