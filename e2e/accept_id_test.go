//go:build e2e

package e2e

import (
	"net"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestAcceptID_Saturation drives a listener with --max-connections=5
// past its semaphore cap with concurrent idle clients, then asserts
// that the listener's debug log lines for the accept lifecycle
// correlate via a stable accept_id and that every dropped accept
// also carries one (with reason=semaphore_full).
//
// This gives operators a stable correlation key: a specific
// dropped accept can be filtered out of a noisy log by its
// accept_id, separately from the accepts that succeeded.
//
// Why hold idle clients instead of round-tripping data: idle clients
// keep the listener-side semaphore slots occupied, so every accept
// that arrives while N clients are connected gets a deterministic
// path — either acquired (slots free) or dropped (slots full).
// Round-trip echo would race with drop because connections complete
// fast enough to free slots before the next accept arrives.
func TestAcceptID_Saturation(t *testing.T) {
	t.Parallel()
	env := requireDedicatedHyco(t)

	const (
		maxConns  = 5
		numClient = 20
	)

	for _, auth := range availableAuths(t, env) {
		t.Run(auth.name, func(t *testing.T) {
			echo := startEchoServer(t)
			listener := startListener(t, env, auth,
				"--allow", echo.Addr(),
				"--max-connections", strconv.Itoa(maxConns),
				"--metrics-addr", "127.0.0.1:0",
				"--log-level", "debug",
			)
			sender := startPortForwardSender(t, env, auth, echo.Addr(),
				"--log-level", "debug",
			)

			waitForLog(t, listener, "control_started", 30*time.Second)
			listenerMetrics := listener.MetricsAddr(t, 15*time.Second)
			senderAddr := waitForLogAddr(t, sender, "port-forward listening", 15*time.Second)

			// Open numClient connections concurrently. Each holds the
			// socket idle (no read/write) until close, so the
			// listener-side semaphore stays saturated and excess
			// accepts deterministically hit the drop path.
			var (
				mu        sync.Mutex
				openConns []net.Conn
				wg        sync.WaitGroup
			)
			for i := 0; i < numClient; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					conn, err := net.DialTimeout("tcp", senderAddr, 15*time.Second)
					if err != nil {
						// Sender accepts local TCP unconditionally; a
						// dial failure here would indicate the sender
						// is down, not a back-pressure signal.
						t.Errorf("client dial: %v", err)
						return
					}
					mu.Lock()
					openConns = append(openConns, conn)
					mu.Unlock()
				}()
			}
			wg.Wait()
			t.Cleanup(func() {
				mu.Lock()
				defer mu.Unlock()
				for _, c := range openConns {
					_ = c.Close()
				}
			})

			// Saturate first (5 listener-side slots full), then wait
			// for at least one drop to be logged. Both waits use the
			// authoritative sources: the active gauge for capacity,
			// the log line for the drop event we will then parse.
			waitForMetric(t, listenerMetrics, "aztunnel_active_connections",
				func(v float64) bool { return v >= maxConns }, 30*time.Second)
			if _, ok := listener.logs.waitFor("accept_dropped", 30*time.Second); !ok {
				t.Fatalf("listener never logged 'accept dropped' (waited 30s; have %d clients open)", len(openConns))
			}

			// Parse listener stderr: group log lines by accept_id and
			// inspect each group's contents. slog renders attrs as
			// key=value, so a fixed-format regex over the 16-char
			// base32 ID is precise enough to ignore unrelated lines.
			groups := groupByAcceptID(listener.logs.String())
			if len(groups) == 0 {
				t.Fatalf("no log lines carried accept_id (listener stderr did not include any 16-char accept_id values)\n--- listener log ---\n%s",
					listener.logs.String())
			}

			var (
				droppedIDs    []string
				acquiredIDs   []string
				lifecycleSeen int
			)
			for id, lines := range groups {
				kinds := classifyAcceptLines(lines)
				if kinds["dropped"] {
					droppedIDs = append(droppedIDs, id)
					if !kinds["semaphore_full"] {
						t.Errorf("accept_id %s: 'accept dropped' line missing reason=semaphore_full\n  lines: %v", id, lines)
					}
					if kinds["acquired"] || kinds["dial_started"] || kinds["dial_complete"] || kinds["released"] {
						t.Errorf("accept_id %s: dropped accept also has lifecycle events — must be one or the other, never both\n  lines: %v", id, lines)
					}
					continue
				}
				if kinds["acquired"] {
					acquiredIDs = append(acquiredIDs, id)
					if kinds["dial_started"] && kinds["dial_complete"] {
						lifecycleSeen++
					}
				}
			}

			if len(acquiredIDs) < maxConns {
				t.Errorf("only %d distinct acquired accept_ids observed; want >= %d (maxConns)\n  groups=%d",
					len(acquiredIDs), maxConns, len(groups))
			}
			if len(droppedIDs) == 0 {
				t.Errorf("no dropped accept_ids observed despite %d clients vs maxConns=%d", numClient, maxConns)
			}
			if lifecycleSeen == 0 {
				t.Errorf("no acquired accept_id had a full lifecycle (acquired + dial started + dial complete); want >= 1\n  acquired=%d, dropped=%d",
					len(acquiredIDs), len(droppedIDs))
			}
		})
	}
}

// acceptIDLineRe captures the 16-base32-char accept_id slog attribute
// as rendered by NewTextHandler — `accept_id=<base32-16>`, terminated
// by a word boundary. The charset is [A-Z2-7] per the idgen contract.
var acceptIDLineRe = regexp.MustCompile(`accept_id=([A-Z2-7]{16})\b`)

// groupByAcceptID scans every log line in raw, extracts accept_id from
// any line that has one, and returns a map keyed by accept_id with the
// matching lines as values. Lines without an accept_id are skipped.
func groupByAcceptID(raw string) map[string][]string {
	out := make(map[string][]string)
	for _, line := range strings.Split(raw, "\n") {
		m := acceptIDLineRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		out[m[1]] = append(out[m[1]], line)
	}
	return out
}

// classifyAcceptLines flags which lifecycle events (and the structured
// drop reason) appear across the lines for a single accept_id. The
// returned map is keyed by short tags the test uses for its
// assertions (no booleans are negated in the caller; missing keys
// already read as false). Match the message text directly — slog's
// TextHandler renders multi-word values quoted but its JSON form (and
// future renderers) may not, and the message strings here are
// distinctive enough that a bare substring match is unambiguous.
func classifyAcceptLines(lines []string) map[string]bool {
	kinds := make(map[string]bool)
	for _, ln := range lines {
		switch {
		case strings.Contains(ln, "accept acquired"):
			kinds["acquired"] = true
		case strings.Contains(ln, "accept_dropped"):
			kinds["dropped"] = true
		case strings.Contains(ln, "accept released"):
			kinds["released"] = true
		case strings.Contains(ln, "accept dial started"):
			kinds["dial_started"] = true
		case strings.Contains(ln, "accept dial complete"):
			kinds["dial_complete"] = true
		}
		if strings.Contains(ln, "reason=semaphore_full") {
			kinds["semaphore_full"] = true
		}
	}
	return kinds
}
