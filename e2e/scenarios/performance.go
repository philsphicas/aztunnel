package scenarios

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"testing"
	"time"
)

// RunPerformanceScenarios runs the timing-assertion scenarios
// against b as sub-tests under the caller's t. These complement
// the testing.B benchmarks in bench.go: same shape of work, but
// expressed as testing.T so CI fails when wall time exceeds the
// per-backend threshold.
//
// Two scenarios assert against ConnectLatencyThreshold; the third
// (ShortSession_Serial) is observation-only — it logs per-
// iteration timings but does not assert a threshold.
//
// Scenarios run sequentially. Each builds its own topology via
// b.Setup so the timing measurement is dominated by the per-
// connection cost, not by amortised setup.
func RunPerformanceScenarios(t *testing.T, b Backend) {
	t.Helper()
	scenarios := []struct {
		name string
		run  func(*testing.T, Backend)
	}{
		{"ConnectLatency_Serial_PortForward", ScenarioConnectLatency_Serial_PortForward},
		{"ConnectLatency_Serial_SOCKS5", ScenarioConnectLatency_Serial_SOCKS5},
		{"ShortSession_Serial", ScenarioShortSession_Serial},
	}
	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			sc.run(t, b)
		})
	}
}

// ScenarioConnectLatency_Serial_PortForward opens 10 serial
// port-forward connections, each performing one 1-byte echo
// round-trip, and asserts every iteration completes inside
// b.ConnectLatencyThreshold(). The scenario logs per-iteration
// elapsed time so a failure makes the offending iteration
// obvious.
//
// Iteration count is fixed at 10. Walltime bounds at the current
// 3 s thresholds:
//   - Happy path: 10 × ~1 s rendezvous ≈ 10 s per scenario.
//   - Threshold-only violation (operational success, elapsed too
//     high): bounded by 10 × (threshold + connectSlack) ≈ 80 s
//     per scenario, since runSerialConnectLatency uses
//     t.Errorf + continue on threshold violations and a
//     successful iteration can use up to threshold+connectSlack
//     of budget before the deadline fires.
//   - Operational error (dial/echo/close failure): fails fast at
//     ≈ threshold + connectSlack ≈ 8 s via t.Fatalf.
//
// All three bounds keep the scenario well inside both the
// per-test default timeout and the e2e job's 20 m envelope.
func ScenarioConnectLatency_Serial_PortForward(t *testing.T, b Backend) {
	t.Helper()
	AssertNoLeaks(t)
	runSerialConnectLatency(t, b, ModePortForward)
}

// ScenarioConnectLatency_Serial_SOCKS5 mirrors the port-forward
// scenario but drives traffic through a SOCKS5 proxy sender. The
// SOCKS5 handshake adds two short round-trips per connection on
// top of the relay rendezvous; the same threshold applies to both
// variants because the SOCKS5 cost is in the tens of milliseconds
// — well inside the per-backend headroom.
func ScenarioConnectLatency_Serial_SOCKS5(t *testing.T, b Backend) {
	t.Helper()
	AssertNoLeaks(t)
	runSerialConnectLatency(t, b, ModeSOCKS5)
}

// ScenarioShortSession_Serial opens 10 serial port-forward
// connections, each carrying a 1 KB round-trip (write 1 KB → read
// 1 KB back), and logs the per-iteration wall time. The scenario
// publishes a CI-visible baseline; it does not assert a threshold.
//
// The 1 KB payload is small enough to fit in a single TCP frame
// on every reasonable MTU and large enough to exercise the
// bridge's read-path beyond the 1-byte echo of the connect-latency
// scenarios.
func ScenarioShortSession_Serial(t *testing.T, b Backend) {
	t.Helper()
	AssertNoLeaks(t)
	echo := StartPlainEcho(t)
	tun := b.Setup(t, SetupOptions{
		NumListeners:   1,
		SenderMode:     ModePortForward,
		Target:         echo.Addr(),
		AllowedTargets: []string{echo.Addr()},
	})

	const iterations = 10
	const payloadSize = 1024
	payload := make([]byte, payloadSize)
	for i := range payload {
		// Deterministic filler so a corruption shows up as a clean
		// diff in t.Errorf output rather than as crypto/rand noise.
		payload[i] = byte(i)
	}
	buf := make([]byte, payloadSize)

	for i := 0; i < iterations; i++ {
		start := time.Now()
		conn, err := net.DialTimeout("tcp", tun.SenderAddr, 30*time.Second)
		if err != nil {
			t.Fatalf("iter %d: dial: %v", i, err)
		}
		_ = conn.SetDeadline(time.Now().Add(30 * time.Second))
		if err := writeFull(conn, payload); err != nil {
			_ = conn.Close()
			t.Fatalf("iter %d: write: %v", i, err)
		}
		if _, err := io.ReadFull(conn, buf); err != nil {
			_ = conn.Close()
			t.Fatalf("iter %d: read: %v", i, err)
		}
		if !bytes.Equal(buf, payload) {
			_ = conn.Close()
			t.Fatalf("iter %d: echo mismatch", i)
		}
		if err := conn.Close(); err != nil {
			t.Fatalf("iter %d: close: %v", i, err)
		}
		t.Logf("ShortSession_Serial iter %d: %v", i, time.Since(start))
	}
}

// runSerialConnectLatency is the shared implementation of the two
// ConnectLatency_Serial variants. Each iteration: dial (port-
// forward TCP or SOCKS5 CONNECT) → write 1 byte → read 1 byte
// echoed back → close. Asserts elapsed < threshold per iteration.
//
// Failure-mode handling is deliberately split:
//
//   - Operational errors (dial/write/read/echo-mismatch/close) →
//     t.Fatalf. These mean the harness is fundamentally broken
//     (no rendezvous, target unreachable, socket draining
//     timeout, etc.). Continuing past them wastes CI walltime
//     because every subsequent iteration is likely to hit the
//     same (threshold + connectSlack) ~ 8 s timeout — at 10
//     iterations × 2 scenarios × 2 Azure auth cells that's a
//     320 s failure-path burn against the 20 m e2e workflow
//     envelope. Fail fast.
//   - Threshold violations (operational success, elapsed >=
//     threshold) → t.Errorf + continue. These produce a
//     measured elapsed time on every iteration, so continuing is
//     cheap (each iteration completed normally inside the budget
//     window) and lets a flaky CI run surface every offending
//     iteration in one report. CI still fails the suite either
//     way.
//
// dialWithRetry / dialSOCKS5WithRetry are intentionally not used —
// the retry helpers exist to swallow first-dial transient races on
// brand-new tunnels, but the Performance scenarios open exactly
// one topology and then dial it 10 times in sequence. By
// iteration 2 the relay is fully warm; failing on a real dial
// error here is what we want to measure.
func runSerialConnectLatency(t *testing.T, b Backend, mode SenderMode) {
	t.Helper()
	threshold := b.ConnectLatencyThreshold()
	echo := StartPlainEcho(t)
	opts := SetupOptions{
		NumListeners:   1,
		SenderMode:     mode,
		AllowedTargets: []string{echo.Addr()},
	}
	if mode == ModePortForward {
		opts.Target = echo.Addr()
	}
	tun := b.Setup(t, opts)

	const iterations = 10
	payload := []byte{0x42}
	buf := make([]byte, 1)

	for i := 0; i < iterations; i++ {
		elapsed, err := timeOneConnect(tun.SenderAddr, echo.Addr(), mode, payload, buf, threshold)
		if err != nil {
			t.Fatalf("iter %d: %v (elapsed=%v)", i, err, elapsed)
		}
		if elapsed >= threshold {
			t.Errorf("iter %d: connect latency %v >= threshold %v", i, elapsed, threshold)
			continue
		}
		t.Logf("ConnectLatency_Serial_%v iter %d: %v (< %v)", mode, i, elapsed, threshold)
	}
}

// timeOneConnect opens one connection in the given mode, writes
// payload, reads it back into buf, verifies the echo matches the
// payload byte-for-byte, closes the connection, and returns the
// wall time from pre-dial to post-close.
//
// The contract for the measured budget is Dial → write → read →
// verify → Close, so the elapsed time is sampled AFTER a successful
// Close. A failing Close (e.g. timeout draining the socket) counts
// against the budget by design.
//
// The dial timeout and connection deadline both use
// threshold + connectSlack rather than threshold so a timing
// regression surfaces as the explicit elapsed >= threshold
// assertion in the caller rather than as an i/o timeout from
// the deadline firing exactly at threshold. The slack only widens
// the timeout headroom; it does not change the threshold being
// asserted.
//
// The deadline is anchored to start (the pre-dial timestamp),
// not to time.Now() after the dial returns. Anchoring to start
// bounds the entire dial+write+read+close iteration to a single
// threshold + connectSlack window; if dial itself burns most of
// that window the remaining read/write inherit whatever is left.
// Anchoring to post-dial time.Now() would let the whole
// iteration take up to 2 × (threshold + connectSlack) in the
// worst case (dial uses one window, deadline allows another),
// violating the bound documented on the calling scenario.
//
// Returns the elapsed time even on error so the caller can log
// timing context alongside the failure reason.
func timeOneConnect(senderAddr, target string, mode SenderMode, payload, buf []byte, threshold time.Duration) (time.Duration, error) {
	const connectSlack = 5 * time.Second
	start := time.Now()
	conn, err := benchDial(senderAddr, target, mode, threshold+connectSlack)
	if err != nil {
		return time.Since(start), err
	}
	_ = conn.SetDeadline(start.Add(threshold + connectSlack))
	if err := writeFull(conn, payload); err != nil {
		_ = conn.Close()
		return time.Since(start), err
	}
	if _, err := io.ReadFull(conn, buf); err != nil {
		_ = conn.Close()
		return time.Since(start), err
	}
	if !bytes.Equal(buf, payload) {
		_ = conn.Close()
		return time.Since(start), fmt.Errorf("echo mismatch: got %x want %x", buf, payload)
	}
	if err := conn.Close(); err != nil {
		return time.Since(start), err
	}
	return time.Since(start), nil
}
