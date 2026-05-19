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
