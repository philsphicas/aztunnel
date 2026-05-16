//go:build e2e

package e2e

import (
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestMultiListenerPortForwardSmoke verifies that two listeners on the same
// hyco can serve a single port-forward sender. This is the smallest answer
// to the user's open question: "if I have multiple listeners, does it work?"
//
// Assertions (kept conservative because Azure's listener-selection behavior
// across multiple listeners on the same hyco is NOT specified as round-robin):
//
//   - all N connections through the sender round-trip data correctly;
//   - neither listener subprocess emits a log line matching `level=error`.
//
// Logged but NOT asserted:
//
//   - per-listener aztunnel_connections_total via scrapeMetrics. A strong skew
//     (e.g. one listener handles 0 of N) is a meaningful signal for humans
//     reading the test log, but is not a hard assertion because Azure's
//     selection is not guaranteed to be balanced — especially at small N.
//
// Strict distribution assertions belong in a mock-backed compatibility matrix
// (Phase 2 of the e2e enhancements plan), where listener selection is
// controllable. See plan.md in the session workspace for context.
func TestMultiListenerPortForwardSmoke(t *testing.T) {
	env := requireRelayEnv(t)

	const numListeners = 2
	const numFlows = 8

	for _, auth := range availableAuths(t, env) {
		t.Run(auth.name, func(t *testing.T) {
			echo := startEchoServer(t)

			listeners := make([]*aztunnelProcess, numListeners)
			listenerMetricsAddrs := make([]string, numListeners)
			for i := 0; i < numListeners; i++ {
				listeners[i] = startListener(t, env, auth,
					"--allow", echo.Addr(),
					"--metrics-addr", "127.0.0.1:0",
					"--log-level", "debug",
				)
				waitForLog(t, listeners[i], "control channel connected", 30*time.Second)
				listenerMetricsAddrs[i] = listeners[i].MetricsAddr(t, 15*time.Second)
			}

			sender := startPortForwardSender(t, env, auth, echo.Addr(),
				"--log-level", "debug",
			)
			senderAddr := waitForLogAddr(t, sender, "port-forward listening", 15*time.Second)

			// Run numFlows concurrent round-trips. Each flow opens a fresh
			// TCP connection, writes a unique payload, reads it back, and
			// closes — exactly the shape a user would experience.
			var wg sync.WaitGroup
			errs := make(chan error, numFlows)
			for i := 0; i < numFlows; i++ {
				wg.Add(1)
				go func(id int) {
					defer wg.Done()
					conn, err := net.DialTimeout("tcp", senderAddr, 15*time.Second)
					if err != nil {
						errs <- fmt.Errorf("flow %d dial: %w", id, err)
						return
					}
					defer conn.Close()
					msg := fmt.Sprintf("flow-%d-%d\n", id, time.Now().UnixNano())
					if _, err := conn.Write([]byte(msg)); err != nil {
						errs <- fmt.Errorf("flow %d write: %w", id, err)
						return
					}
					buf := make([]byte, len(msg))
					if _, err := io.ReadFull(conn, buf); err != nil {
						errs <- fmt.Errorf("flow %d read: %w", id, err)
						return
					}
					if string(buf) != msg {
						errs <- fmt.Errorf("flow %d echo mismatch: got %q want %q", id, buf, msg)
					}
				}(i)
			}
			wg.Wait()
			close(errs)
			var flowErrs []string
			for err := range errs {
				flowErrs = append(flowErrs, err.Error())
			}
			if len(flowErrs) > 0 {
				t.Fatalf("%d/%d flows failed:\n  %s", len(flowErrs), numFlows, strings.Join(flowErrs, "\n  "))
			}

			// Wait for per-listener connection counters to stop changing. We
			// can't wait on "sum == numFlows" because flows complete and the
			// counter may already be at numFlows on one side. Instead, poll
			// until the sum across listeners is >= numFlows.
			waitUntilSumGE(t, listenerMetricsAddrs, "aztunnel_connections_total",
				float64(numFlows), 15*time.Second)

			// Log per-listener counts so a human reading the test output can
			// see distribution. Not asserted — see function comment.
			for i, addr := range listenerMetricsAddrs {
				count := sumMetric(scrapeMetricsBest(addr), "aztunnel_connections_total")
				t.Logf("listener %d (%s) handled %v connections", i, addr, count)
			}

			// Assertion: neither listener subprocess emitted an error-level log
			// line. We accept slog's `level=ERROR` and Kong/Cobra-style
			// `Error:` prefixes, case-insensitively, but exclude the
			// well-known retry-on-disconnect Warn lines.
			for i, l := range listeners {
				if hits := findErrorLines(l.logs.String()); len(hits) > 0 {
					t.Errorf("listener %d emitted %d unexpected error log line(s):\n  %s",
						i, len(hits), strings.Join(hits, "\n  "))
				}
			}
		})
	}
}

// waitUntilSumGE polls /metrics on each addr at 100ms and succeeds when the
// sum of sumMetric(name) across all addrs is >= want. Calls t.Fatalf on timeout.
func waitUntilSumGE(t *testing.T, addrs []string, name string, want float64, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var total float64
	for time.Now().Before(deadline) {
		total = 0
		for _, a := range addrs {
			total += sumMetric(scrapeMetricsBest(a), name)
		}
		if total >= want {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("waitUntilSumGE: %s across %v did not reach %v within %v (last sum %v)",
		name, addrs, want, timeout, total)
}

// findErrorLines returns log lines that look like error-level emissions from
// either slog ("level=ERROR") or CLI front-ends ("Error:"). It excludes
// known-OK warn lines (relay dial retries, control channel reconnects).
func findErrorLines(logs string) []string {
	var hits []string
	for _, line := range strings.Split(logs, "\n") {
		lower := strings.ToLower(line)
		if !strings.Contains(lower, "level=error") && !strings.Contains(lower, "error:") {
			continue
		}
		// Suppress lines we know are benign on this code path. The smoke
		// test does not provoke any of these intentionally.
		if strings.Contains(lower, "retrying") {
			continue
		}
		hits = append(hits, line)
	}
	return hits
}
