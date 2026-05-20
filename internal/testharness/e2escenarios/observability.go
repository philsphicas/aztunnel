package e2escenarios

import (
	"bytes"
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"
)

// RunObservabilityScenarios runs the cross-backend observability e2e
// scenarios against b. Each scenario asserts a log-shape contract
// that operators rely on for cross-process correlation (e.g. a
// bridge_id present on both ends).
//
// Backends that do not capture per-handle logs (Listener.Logs /
// Sender.Logs are nil) will trip the t.Fatal in the scenario itself;
// adding a new backend therefore forces the implementer to wire
// log capture so the parity claim stays honest.
func RunObservabilityScenarios(t *testing.T, b Backend) {
	t.Helper()
	scenarios := []struct {
		name string
		run  func(*testing.T, Backend)
	}{
		{"BridgeID_Correlation", ScenarioBridgeID_Correlation},
		{"ControlSessionID_OnConnectedLine", ScenarioControlSessionID_OnConnectedLine},
		{"SenderLogsCode_OnConnectFailure", ScenarioSenderLogsCode_OnConnectFailure},
		{"ListenerDialFailureLog_CarriesCode", ScenarioListenerDialFailureLog_CarriesCode},
		{"BridgeCauseLogs", ScenarioBridgeCauseLogs},
		{"BridgePerDirection_NormalClose", ScenarioBridgePerDirection_NormalClose},
	}
	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			sc.run(t, b)
		})
	}
}

// bridgeIDRe matches a bridge_id=VALUE token in slog TextHandler
// output. The base32 NoPadding charset ([A-Z2-7]) is safe to leave
// unquoted in slog output so an unquoted match is sufficient — and
// is exactly what the operator's grep one-liner will look like.
var bridgeIDRe = regexp.MustCompile(`bridge_id=([A-Z2-7]+)`)

// ScenarioBridgeID_Correlation drives one successful bridge and one
// failed bridge through a SOCKS5 sender (so a single sender can pick
// per-connection targets), then asserts that the bridge_id slog
// attribute the sender minted appears on the matching listener-side
// log line for each bridge.
//
// Success case: sender emits "connection requested" with
// bridge_id=X; listener emits "connection requested" with the same
// bridge_id=X. Failure case: sender emits the same outbound log,
// listener emits "dial target failed" with the same bridge_id=Y. The
// scenario does not assert on the failure log on the sender side
// because the sender's failure surface depends on backend timing
// (port-forward vs SOCKS5, refused vs timeout); the success-side
// outbound log is enough to anchor each ID.
//
// Cross-backend: identical on mock and Azure because BridgeID is an
// application-level envelope field that Azure Relay just forwards
// verbatim.
func ScenarioBridgeID_Correlation(t *testing.T, b Backend) {
	t.Helper()
	AssertNoLeaks(t)

	echo := StartPlainEcho(t)
	refused := refusedAddr(t)

	tun := b.Setup(t, SetupOptions{
		NumListeners:   1,
		SenderMode:     ModeSOCKS5,
		AllowedTargets: []string{echo.Addr(), refused},
		ConnectTimeout: 5 * time.Second,
	})
	requireLogs(t, tun)

	// 1. Successful bridge through the echo target. Drive a round-trip
	//    so the listener has unambiguously processed the envelope
	//    before we scrape its logs.
	{
		conn, err := dialSOCKS5WithRetry(tun.SenderAddr, echo.Addr(), 15*time.Second)
		if err != nil {
			t.Fatalf("socks5 dial echo: %v", err)
		}
		want := []byte("p5 bridge id\n")
		writeAll(t, conn, want)
		got := readN(t, conn, len(want), 10*time.Second)
		if !bytes.Equal(got, want) {
			t.Fatalf("echo mismatch: got=%q want=%q", got, want)
		}
		_ = conn.Close()
	}

	// 2. Failed bridge to a refused target — allowlisted so the
	//    listener actually attempts the dial (and logs "dial target
	//    failed"), rather than rejecting the target outright.
	{
		_, err := DialSOCKS5(tun.SenderAddr, refused, 15*time.Second)
		var sErr *SOCKS5Error
		if !errors.As(err, &sErr) {
			t.Fatalf("socks5 dial refused: expected SOCKS5Error, got %T: %v", err, err)
		}
		if sErr.Rep != 0x05 {
			t.Fatalf("socks5 REP for refused = %#x, want 0x05", sErr.Rep)
		}
	}

	senderLogs := tun.Senders[0].Logs()
	listenerLogs := waitForListenerCorrelations(t, tun.Listeners[0].Logs, 2, 10*time.Second)

	dumpOnFail := func() {
		t.Logf("--- sender logs ---\n%s", senderLogs)
		t.Logf("--- listener logs ---\n%s", listenerLogs)
	}

	successID := bridgeIDForTarget(senderLogs, echo.Addr())
	if successID == "" {
		dumpOnFail()
		t.Fatalf("no bridge_id in sender log for echo target %s", echo.Addr())
	}
	failureID := bridgeIDForTarget(senderLogs, refused)
	if failureID == "" {
		dumpOnFail()
		t.Fatalf("no bridge_id in sender log for refused target %s", refused)
	}
	if successID == failureID {
		dumpOnFail()
		t.Fatalf("expected distinct bridge_ids per bridge, both = %q", successID)
	}

	if !listenerHasBridge(listenerLogs, "connection requested", successID, echo.Addr()) {
		dumpOnFail()
		t.Fatalf("listener missing 'connection requested' for bridge_id=%s target=%s",
			successID, echo.Addr())
	}
	if !listenerHasBridge(listenerLogs, "dial target failed", failureID, refused) {
		dumpOnFail()
		t.Fatalf("listener missing 'dial target failed' for bridge_id=%s target=%s",
			failureID, refused)
	}
}

// requireLogs fails the scenario if either side of the tunnel has no
// log capture wired up. This is the parity gate: an observability
// scenario silently passing because the backend dropped the logs
// would defeat the point of the test.
func requireLogs(t *testing.T, tun *Tunnel) {
	t.Helper()
	for i, l := range tun.Listeners {
		if l.Logs == nil {
			t.Fatalf("backend Listener[%d].Logs is nil; observability scenarios require log capture", i)
		}
	}
	for i, s := range tun.Senders {
		if s.Logs == nil {
			t.Fatalf("backend Sender[%d].Logs is nil; observability scenarios require log capture", i)
		}
	}
}

// bridgeIDForTarget scans logs for the first "connection requested"
// line whose target field matches target and returns its bridge_id.
// Empty string when no match. Anchoring on the deterministic
// "connection requested" line avoids double-counting bridges that
// emit multiple bridge_id-tagged lines (debug logs, bridge-end
// logs, etc.).
func bridgeIDForTarget(logs, target string) string {
	for _, line := range strings.Split(logs, "\n") {
		if !strings.Contains(line, `msg="connection requested"`) {
			continue
		}
		if !strings.Contains(line, "target="+target) {
			continue
		}
		m := bridgeIDRe.FindStringSubmatch(line)
		if m != nil {
			return m[1]
		}
	}
	return ""
}

// listenerHasBridge reports whether the listener log stream contains
// a line with the given message, bridge_id, and target — the exact
// shape an operator's correlation grep would look for.
func listenerHasBridge(logs, msg, bridgeID, target string) bool {
	needleMsg := `msg="` + msg + `"`
	needleID := "bridge_id=" + bridgeID
	needleTarget := "target=" + target
	for _, line := range strings.Split(logs, "\n") {
		if strings.Contains(line, needleMsg) &&
			strings.Contains(line, needleID) &&
			strings.Contains(line, needleTarget) {
			return true
		}
	}
	return false
}

// waitForListenerCorrelations polls the listener log capture until at
// least min lines carrying a bridge_id appear, bounded by timeout.
// Returns the snapshot captured on success or at deadline. The poll
// defends against the goroutine-scheduling window between sender
// completion and the listener-side handler returning — slog itself
// is synchronous, but the listener's handler runs in a goroutine
// distinct from the sender's request path.
func waitForListenerCorrelations(t *testing.T, snapshot func() string, min int, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var logs string
	for time.Now().Before(deadline) {
		logs = snapshot()
		if len(bridgeIDRe.FindAllStringIndex(logs, -1)) >= min {
			return logs
		}
		time.Sleep(50 * time.Millisecond)
	}
	return logs
}

// controlSessionIDRe matches a control_session_id=VALUE token in
// slog TextHandler output. Uses the same [A-Z2-7] charset as
// bridgeIDRe — both ids are minted through idgen, which encodes
// base32 NoPadding so neither needs slog-quoting.
var controlSessionIDRe = regexp.MustCompile(`control_session_id=([A-Z2-7]+)`)

// ScenarioControlSessionID_OnConnectedLine asserts that the
// listener's control_started log line carries a non-empty
// control_session_id field. control_started fires once per
// control-loop attempt, immediately after the dial succeeds and at
// the same operational milestone the e2e test harness blocks on
// during Setup. The id on this line is the same id every other
// per-session log record (renew_*, accept_*, control_ended) carries
// across the rest of the loop.
//
// Cross-backend: identical on mock and Azure because the binding
// happens listener-side before any data crosses the relay.
func ScenarioControlSessionID_OnConnectedLine(t *testing.T, b Backend) {
	t.Helper()
	AssertNoLeaks(t)

	echo := StartPlainEcho(t)
	tun := b.Setup(t, SetupOptions{
		NumListeners:   1,
		SenderMode:     ModePortForward,
		Target:         echo.Addr(),
		AllowedTargets: []string{echo.Addr()},
	})
	requireLogs(t, tun)

	lst := tun.Listeners[0]

	// Backend.Setup blocks until control_started has been logged,
	// so a short poll on lst.Logs is sufficient — keep a small
	// grace window for pipe-flushing on subprocess backends.
	startedLine := waitForLogLineContaining(t, lst.Logs, 5*time.Second, "control_started")
	if !controlSessionIDRe.MatchString(startedLine) {
		t.Fatalf("control_started line missing control_session_id field:\n%s", startedLine)
	}
}

// waitForLogLineContaining returns the first newline-delimited line
// from logs() that contains every needle in needles, polling at 50ms
// until timeout. Variadic needles let callers anchor a search to
// both a message and a correlation id, which is how the scenarios
// here distinguish the lifecycle line of one session from another.
// Failing the test rather than returning a sentinel keeps the
// scenario's stack trace pointing at the missing line.
func waitForLogLineContaining(t *testing.T, logs func() string, timeout time.Duration, needles ...string) string {
	t.Helper()
	if len(needles) == 0 {
		t.Fatal("waitForLogLineContaining requires at least one needle")
		return ""
	}
	deadline := time.Now().Add(timeout)
	for {
		for _, line := range strings.Split(logs(), "\n") {
			if containsAll(line, needles) {
				return line
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %v waiting for log line containing %v\n--- logs ---\n%s",
				timeout, needles, logs())
			return ""
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// containsAll reports whether s contains every needle. Defined
// inline (instead of using slices.ContainsFunc + strings.Contains)
// to keep the helper readable in its single call site.
func containsAll(s string, needles []string) bool {
	for _, n := range needles {
		if !strings.Contains(s, n) {
			return false
		}
	}
	return true
}

// ScenarioSenderLogsCode_OnConnectFailure asserts that the listener's
// machine-readable classification code from ConnectResponse.Code
// surfaces on the sender's rejection warn line, so operators see the
// same classification on both ends of the tunnel without parsing the
// human-readable error text.
//
// The scenario drives one SOCKS5 connection through to refusedAddr
// (allowlisted so the listener actually attempts the dial and
// classifies the OS-level ECONNREFUSED), then polls the sender's
// log capture for the rejection warn line carrying target=<refused>
// and code=connection_refused.
//
// Cross-backend: identical on mock and Azure. The classification is
// performed listener-side from the OS dial error; the relay just
// forwards the resulting protocol.Code string verbatim.
func ScenarioSenderLogsCode_OnConnectFailure(t *testing.T, b Backend) {
	t.Helper()
	AssertNoLeaks(t)

	refused := refusedAddr(t)
	tun := b.Setup(t, SetupOptions{
		NumListeners:   1,
		SenderMode:     ModeSOCKS5,
		AllowedTargets: []string{refused},
		ConnectTimeout: 5 * time.Second,
	})
	requireLogs(t, tun)

	_, err := DialSOCKS5(tun.SenderAddr, refused, 15*time.Second)
	var sErr *SOCKS5Error
	if !errors.As(err, &sErr) {
		t.Fatalf("socks5 dial refused: expected SOCKS5Error, got %T: %v", err, err)
	}
	if sErr.Rep != 0x05 {
		t.Fatalf("socks5 REP for refused = %#x, want 0x05", sErr.Rep)
	}

	waitForLogLineContaining(t, tun.Senders[0].Logs, 10*time.Second,
		`msg="listener refused connection"`,
		"target="+refused,
		"code=connection_refused",
	)
}

// ScenarioListenerDialFailureLog_CarriesCode asserts that the
// listener's "dial target failed" slog line carries the same
// machine-readable classification (code=...) the listener already
// returns in its ConnectResponse. Operators triaging a dispatcher
// trace can read this code straight from the log without round-
// tripping through the metric surface or a packet capture.
//
// Two sub-cases share one tunnel:
//
//   - Refused: 127.0.0.1:<closed-port> always classifies to the
//     exact code=connection_refused on every backend.
//   - Unreachable: 192.0.2.1:9 (RFC 5737 TEST-NET-1) classifies to
//     one of code=timeout / code=host_unreachable /
//     code=network_unreachable depending on whether the host sees
//     ICMP-unreachable before ConnectTimeout fires; all three are
//     valid wirings of the field.
func ScenarioListenerDialFailureLog_CarriesCode(t *testing.T, b Backend) {
	t.Helper()
	AssertNoLeaks(t)

	refused := refusedAddr(t)
	const unreachable = "192.0.2.1:9"
	tun := b.Setup(t, SetupOptions{
		NumListeners:   1,
		SenderMode:     ModeSOCKS5,
		AllowedTargets: []string{refused, unreachable},
		ConnectTimeout: 4 * time.Second,
	})
	requireLogs(t, tun)

	t.Run("Refused", func(t *testing.T) {
		_, err := DialSOCKS5(tun.SenderAddr, refused, 15*time.Second)
		var sErr *SOCKS5Error
		if !errors.As(err, &sErr) {
			t.Fatalf("socks5 dial refused: expected SOCKS5Error, got %T: %v", err, err)
		}
		if sErr.Rep != 0x05 {
			t.Fatalf("socks5 REP for refused = %#x, want 0x05", sErr.Rep)
		}

		// waitForLogLineContaining is the assertion: it fails the test
		// unless a single line contains all three needles.
		waitForLogLineContaining(t, tun.Listeners[0].Logs, 10*time.Second,
			`msg="dial target failed"`, "target="+refused, "code=connection_refused")
	})

	t.Run("Unreachable", func(t *testing.T) {
		_, err := DialSOCKS5(tun.SenderAddr, unreachable, 30*time.Second)
		var sErr *SOCKS5Error
		if !errors.As(err, &sErr) {
			t.Fatalf("socks5 dial unreachable: expected SOCKS5Error, got %T: %v", err, err)
		}

		line := waitForLogLineContaining(t, tun.Listeners[0].Logs, 10*time.Second,
			`msg="dial target failed"`, "target="+unreachable)

		accepted := []string{"code=timeout", "code=host_unreachable", "code=network_unreachable"}
		ok := false
		for _, want := range accepted {
			if strings.Contains(line, want) {
				ok = true
				break
			}
		}
		if !ok {
			t.Fatalf("listener dial-failure log for unreachable carried no accepted code (want one of %v):\n%s",
				accepted, line)
		}
	})
}

// ScenarioBridgeCauseLogs drives one port-forward bridge to completion
// by writing+reading a short payload then closing the client side,
// then asserts that the bridge-end slog lines on BOTH sides carry a
// non-empty cause field that classifies which side terminated the
// bridge. The client's close is the canonical local_close on the
// sender's side; the listener observes the resulting WebSocket
// teardown as a peer_close from its perspective.
//
// The scenario asserts the *shape* of the cause attribute rather
// than exact close-code values: per-pump scheduling and per-backend
// WebSocket close-code semantics can vary, but the cause field is
// always populated to one of bridgecause.Name's stable labels.
//
// Cross-backend: identical on mock and Azure. The bridge cancel-
// cause classifier sits inside relay.Bridge, so neither the relay
// service nor the protocol layer affect what cause the operator
// sees in the log.
func ScenarioBridgeCauseLogs(t *testing.T, b Backend) {
	t.Helper()
	AssertNoLeaks(t)

	echo := StartPlainEcho(t)
	tun := b.Setup(t, SetupOptions{
		NumListeners:   1,
		SenderMode:     ModePortForward,
		Target:         echo.Addr(),
		AllowedTargets: []string{echo.Addr()},
	})
	requireLogs(t, tun)

	conn := dialWithRetry(t, tun.SenderAddr, 5*time.Second)
	want := []byte("p10 bridge cause\n")
	writeAll(t, conn, want)
	got := readN(t, conn, len(want), 10*time.Second)
	if !bytes.Equal(got, want) {
		t.Fatalf("echo mismatch: got=%q want=%q", got, want)
	}
	// Close the local client side: the sender's tcpToWS pump
	// reads EOF and classifies as local_close. The sender then
	// tears down the WebSocket, which the listener observes as
	// peer_close (a peer-side WebSocket failure).
	_ = conn.Close()

	senderLine := waitForLogLineContaining(t, tun.Senders[0].Logs, 10*time.Second,
		`msg="bridge ended"`, "cause=")
	if !strings.Contains(senderLine, "cause=local_close") {
		t.Fatalf("sender bridge-end cause not local_close:\n%s", senderLine)
	}
	listenerLine := waitForLogLineContaining(t, tun.Listeners[0].Logs, 10*time.Second,
		`msg="bridge ended"`, "cause=")
	if !strings.Contains(listenerLine, "cause=peer_close") {
		t.Fatalf("listener bridge-end cause not peer_close:\n%s", listenerLine)
	}
}

// ScenarioBridgePerDirection_NormalClose drives one port-forward
// bridge to a local clean close, then asserts that the sender's
// bridge-end slog line does NOT carry the per-direction error
// attributes tcp_to_ws_err= / ws_to_tcp_err=. These attributes are
// conditional in the caller (emitted only when the respective
// direction's error is non-nil after the induced-cancellation
// filter), so their absence proves both sender pumps ended cleanly:
// tcpToWS returned nil on TCP EOF (after conn.Close), and wsToTCP
// was induced-cancelled by the bridge's own ctx-cancel (filtered to
// nil).
//
// The listener side is intentionally not asserted: the sender's
// post-bridge ws.CloseNow() does not exchange a normal-close frame,
// so the listener's wsToTCP pump observes an abrupt frame-header
// EOF — a real per-direction error that the new attribute correctly
// surfaces.
//
// Cross-backend: identical on mock and Azure. The conditional
// log-attr policy lives in the sender call site, and the per-
// direction nil/non-nil decision lives in relay.Bridge's
// isInducedCancellation filter — neither depends on the relay
// service.
func ScenarioBridgePerDirection_NormalClose(t *testing.T, b Backend) {
	t.Helper()
	AssertNoLeaks(t)

	echo := StartPlainEcho(t)
	tun := b.Setup(t, SetupOptions{
		NumListeners:   1,
		SenderMode:     ModePortForward,
		Target:         echo.Addr(),
		AllowedTargets: []string{echo.Addr()},
	})
	requireLogs(t, tun)

	conn := dialWithRetry(t, tun.SenderAddr, 5*time.Second)
	want := []byte("p11 per-direction\n")
	writeAll(t, conn, want)
	got := readN(t, conn, len(want), 10*time.Second)
	if !bytes.Equal(got, want) {
		t.Fatalf("echo mismatch: got=%q want=%q", got, want)
	}
	_ = conn.Close()

	senderLine := waitForLogLineContaining(t, tun.Senders[0].Logs, 10*time.Second,
		`msg="bridge ended"`, "cause=")
	if strings.Contains(senderLine, "tcp_to_ws_err=") || strings.Contains(senderLine, "ws_to_tcp_err=") {
		t.Fatalf("sender bridge-end carries per-direction error on normal close:\n%s", senderLine)
	}
}
