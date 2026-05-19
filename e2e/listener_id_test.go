//go:build e2e

package e2e

import (
	"fmt"
	"io"
	"net"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

// listenerIDRe matches the listener_id attribute as emitted by slog's
// default text handler: `listener_id=<value>` where value is a token
// terminated by whitespace. The listener emits a fixed-length base32
// id (see internal/idgen.NewListenerID); the regex is intentionally
// looser so a future format change does not silently desync the test
// from production.
var listenerIDRe = regexp.MustCompile(`listener_id=([^\s]+)`)

// TestListenerID_PropagatesAndChangesOnRestart asserts the contract
// behind the listener_id observability feature:
//
//   - the listener mints exactly one ID per process and stamps every
//     ConnectResponse with it;
//   - the sender logs `listener_id=<id>` on receipt of every
//     successful response;
//   - two requests against the same listener carry the same
//     listener_id in the sender's log;
//   - restarting the listener (kill the subprocess, start a fresh
//     one against the same hyco) produces a different ID.
//
// The whole point of the feature is to make restart-driven failure
// modes observable from the sender side, so this is the only e2e
// shape that actually exercises the value.
func TestListenerID_PropagatesAndChangesOnRestart(t *testing.T) {
	t.Parallel()
	env := requireDedicatedHyco(t)

	for _, auth := range availableAuths(t, env) {
		auth := auth
		t.Run(auth.name, func(t *testing.T) {
			echo := startEchoServer(t)

			// --- phase 1: listener A serves two flows -----------------

			listenerA := startListener(t, env, auth,
				"--allow", echo.Addr(),
				"--metrics-addr", "127.0.0.1:0",
				"--log-level", "debug",
			)
			lineA := waitForLog(t, listenerA, "control channel connected", 30*time.Second)
			idA := extractListenerID(t, lineA)

			sender := startPortForwardSender(t, env, auth, echo.Addr(),
				"--log-level", "debug",
			)
			senderAddr := waitForLogAddr(t, sender, "port-forward listening", 15*time.Second)

			runFlow(t, senderAddr, "flow-1\n")
			runFlow(t, senderAddr, "flow-2\n")

			// Read every accepted-connection log emitted so far.
			// At this point both flows must have observed listener A.
			obs := waitForNAcceptLogs(t, sender, 2, 15*time.Second)
			for i, id := range obs {
				if id != idA {
					t.Errorf("flow %d sender-side listener_id=%q, want %q (listener A)", i+1, id, idA)
				}
			}

			// --- phase 2: kill listener A, start listener B -----------

			listenerA.Stop(t)

			listenerB := startListener(t, env, auth,
				"--allow", echo.Addr(),
				"--metrics-addr", "127.0.0.1:0",
				"--log-level", "debug",
			)
			lineB := waitForLog(t, listenerB, "control channel connected", 30*time.Second)
			idB := extractListenerID(t, lineB)
			if idB == idA {
				t.Fatalf("listener B minted the same ID %q as listener A; mint-per-instance broken", idB)
			}

			// --- phase 3: drive flows until the sender observes idB ---
			//
			// Azure Relay may briefly retain stale routing state for
			// listener A after kill (control-channel keepalive timeout).
			// Instead of sleeping a fixed amount, we drive short echo
			// flows until a new "listener accepted connection" log line
			// carries idB, bounded by a budget. This makes the test
			// resilient to varying relay teardown latency while still
			// catching a real regression (idB never appearing).
			seen := observedBefore(t, sender)
			deadline := time.Now().Add(60 * time.Second)
			var observedB bool
			var lastObserved string
			var lastFlowErr error
			for time.Now().Before(deadline) {
				// Transient probe failures are expected here: Relay may
				// route the flow to listener A's stale registration
				// before the keepalive timeout retires it. Record the
				// error and keep trying — only the absence of an idB
				// accept log within the budget is a real regression.
				if err := runFlowE(senderAddr, "probe\n", 10*time.Second); err != nil {
					lastFlowErr = err
				}
				ids := observedSince(t, sender, seen)
				if len(ids) > 0 {
					lastObserved = ids[len(ids)-1]
				}
				for _, id := range ids {
					if id == idB {
						observedB = true
						break
					}
				}
				if observedB {
					break
				}
				time.Sleep(500 * time.Millisecond)
			}
			if !observedB {
				t.Fatalf("after restart: sender never observed listener_id=%q within budget (last observed %q, A=%q, last flow error: %v)", idB, lastObserved, idA, lastFlowErr)
			}
		})
	}
}

// extractListenerID pulls the listener_id=… token out of a slog text
// line. Fails the test if the line does not contain one — the
// listener is supposed to emit listener_id with every log line once
// applyDefaults has wrapped its logger.
func extractListenerID(t testing.TB, line string) string {
	t.Helper()
	m := listenerIDRe.FindStringSubmatch(line)
	if m == nil {
		t.Fatalf("no listener_id in log line: %s", line)
	}
	return m[1]
}

// waitForNAcceptLogs blocks until the sender process has emitted at
// least n "listener accepted connection" log lines, then returns the
// listener_id values from the first n in order. Fails the test on
// timeout — that means the sender did not log on receipt, which is
// the regression we want to catch.
func waitForNAcceptLogs(t testing.TB, proc *aztunnelProcess, n int, timeout time.Duration) []string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		ids := acceptListenerIDs(proc.logs.String())
		if len(ids) >= n {
			return ids[:n]
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d \"listener accepted connection\" lines on sender", n)
	return nil
}

// observedBefore returns the count of "listener accepted connection"
// lines currently in the sender's log buffer. observedSince returns
// the listener_id values that have appeared *after* this baseline.
func observedBefore(t testing.TB, proc *aztunnelProcess) int {
	t.Helper()
	return len(acceptListenerIDs(proc.logs.String()))
}

func observedSince(t testing.TB, proc *aztunnelProcess, baseline int) []string {
	t.Helper()
	ids := acceptListenerIDs(proc.logs.String())
	if baseline >= len(ids) {
		return nil
	}
	return ids[baseline:]
}

// acceptListenerIDs scans log text for "listener accepted connection"
// lines and returns the listener_id token from each, in order.
func acceptListenerIDs(logs string) []string {
	var ids []string
	for _, line := range strings.Split(logs, "\n") {
		if !strings.Contains(line, "listener accepted connection") {
			continue
		}
		m := listenerIDRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		ids = append(ids, m[1])
	}
	return ids
}

// runFlow dials the sender's bind address, echoes a payload, and
// asserts a clean round-trip. The new flow gets its own ws→listener
// handshake (port-forward opens a fresh tunnel per accepted TCP
// connection), so each invocation produces one new "listener
// accepted connection" line on the sender side.
func runFlow(t *testing.T, senderAddr, payload string) {
	t.Helper()
	if err := runFlowE(senderAddr, payload, 15*time.Second); err != nil {
		t.Fatalf("%v", err)
	}
}

func runFlowE(senderAddr, payload string, timeout time.Duration) error {
	conn, err := net.DialTimeout("tcp", senderAddr, timeout)
	if err != nil {
		return fmt.Errorf("dial sender: %w", err)
	}
	defer conn.Close() //nolint:errcheck // best-effort
	_ = conn.SetDeadline(time.Now().Add(timeout))
	var wg sync.WaitGroup
	wg.Add(1)
	var got string
	var readErr error
	go func() {
		defer wg.Done()
		buf := make([]byte, len(payload))
		if _, err := io.ReadFull(conn, buf); err != nil {
			readErr = fmt.Errorf("read: %w", err)
			return
		}
		got = string(buf)
	}()
	if _, err := conn.Write([]byte(payload)); err != nil {
		return fmt.Errorf("write: %w", err)
	}
	wg.Wait()
	if readErr != nil {
		return readErr
	}
	if got != payload {
		return fmt.Errorf("echo mismatch: got %q want %q", got, payload)
	}
	return nil
}
