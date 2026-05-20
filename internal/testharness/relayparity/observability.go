package relayparity

import (
	"bytes"
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"
)

// RunObservabilitySuite runs the cross-backend observability parity
// scenarios against b. Each scenario asserts a log-shape contract
// that operators rely on for cross-process correlation (e.g. a
// bridge_id present on both ends).
//
// Backends that do not capture per-handle logs (Listener.Logs /
// Sender.Logs are nil) will trip the t.Fatal in the scenario itself;
// adding a new backend therefore forces the implementer to wire
// log capture so the parity claim stays honest.
func RunObservabilitySuite(t *testing.T, b Backend) {
	t.Helper()
	scenarios := []struct {
		name string
		run  func(*testing.T, Backend)
	}{
		{"BridgeID_Correlation", ScenarioBridgeID_Correlation},
		{"ControlSessionID_OnConnectedLine", ScenarioControlSessionID_OnConnectedLine},
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
// listener's "control channel connected" log line carries a
// non-empty control_session_id field, and that the field is stable
// across the per-session lifecycle (the "control loop started" line
// minted at runControlLoop entry must share the same id). Both lines
// are produced inside relay.runControlLoop, so this scenario is the
// cross-backend gate proving that the per-session logger binding
// propagates to the canonical lifecycle lines — the same lines
// every other parity scenario waits for during topology setup.
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

	// Backend.Setup blocks until "control channel connected" has
	// been logged, so a short poll on lst.Logs is sufficient — keep
	// a small grace window for pipe-flushing on subprocess backends.
	connectedLine := waitForLogLineContaining(t, lst.Logs, 5*time.Second, "control channel connected")
	m := controlSessionIDRe.FindStringSubmatch(connectedLine)
	if m == nil {
		t.Fatalf("`control channel connected` line missing control_session_id field:\n%s", connectedLine)
	}
	sessionID := m[1]

	// Anchor the "control loop started" lookup on the same
	// session id we just observed on the connected line. A
	// listener that retried after an earlier failure (e.g.
	// transient dial error) will have multiple started/ended
	// pairs in the buffer; matching on the id pins this assertion
	// to the lifecycle of the connected session rather than to
	// whatever attempt happened to be logged first.
	startedNeedle := "control_session_id=" + sessionID
	startedLine := waitForLogLineContaining(t, lst.Logs, 2*time.Second, "control loop started", startedNeedle)
	if !controlSessionIDRe.MatchString(startedLine) {
		t.Fatalf("`control loop started` line missing control_session_id field:\n%s", startedLine)
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
