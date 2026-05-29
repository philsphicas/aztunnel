package scenarios

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"io"
	"net"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// RunPerformanceScenarios runs the timing-assertion scenarios
// against b as sub-tests under the caller's t. These complement
// the testing.B benchmarks in bench.go: same shape of work, but
// expressed as testing.T so CI fails when wall time exceeds the
// per-backend threshold.
//
// Two scenarios assert per-iteration time against
// ConnectLatencyThreshold; ShortSession_Serial is observation-only.
// The 12 parameterized echo-workload scenarios that follow
// (Serial|Parallel × ConnPerRequest|ConnReused|ConnPrewarmed ×
// _PortForward|_SOCKS5), plus the single _MultiTarget variant that
// exercises the NumTargets axis, emit a per-round
// workload-summary log line for trend analysis; each round's wall
// time is strictly bounded by roundBudget(threshold, w) because
// per-conn dial+I/O deadlines are clamped to the remaining round
// budget. See the WorkloadShape doc block below for details.
//
// The summary line is tagged with `scenario=<t.Name()>` so each
// line is self-identifying when copied out of test output for
// off-line aggregation. The subtest path is the slash-joined chain
// of Backend.Axes() values followed by the scenario subtest name
// (the string passed to t.Run, not the Go function name — e.g.
// `TestE2E_Azure/entra/Parallel_ConnPrewarmedEcho_SOCKS5`); axis
// NAMES are not encoded — only the per-cell VALUES.
//
// Scenarios run sequentially. Each builds its own topology via
// b.Setup so the timing measurement is dominated by the per-
// connection cost, not by amortised setup.
func RunPerformanceScenarios(t *testing.T, b Backend) {
	t.Helper()
	runScenarioCases(t, b, performanceCases())
}

// performanceCases is the metadata-only registry of performance
// scenarios. All entries are AnyBackend: per-backend thresholds are
// applied inside each scenario via b.ConnectLatencyThreshold(),
// b.RoundBudget(), etc., so the same scenario can run against both
// without false alarms.
//
// BulkTransfer (10 MiB SHA-256) is registered here as a throughput-
// sized parity gate, alongside the latency-sized scenarios.
func performanceCases() []scenarioCase {
	return []scenarioCase{
		{name: "ConnectLatency_Serial_PortForward", scope: AnyBackend, run: ScenarioConnectLatency_Serial_PortForward},
		{name: "ConnectLatency_Serial_SOCKS5", scope: AnyBackend, run: ScenarioConnectLatency_Serial_SOCKS5},
		{name: "ConnectLatency_ColdStart_PortForward", scope: AnyBackend, run: ScenarioConnectLatency_ColdStart_PortForward},
		{name: "ConnectLatency_ColdStart_SOCKS5", scope: AnyBackend, run: ScenarioConnectLatency_ColdStart_SOCKS5},
		{name: "ShortSession_Serial", scope: AnyBackend, run: ScenarioShortSession_Serial},
		{name: "BulkTransfer", scope: AnyBackend, run: ScenarioBulkTransfer},
		// Parameterized echo-workload scenarios — see WorkloadShape doc below.
		{name: "Serial_ConnPerRequestEcho_PortForward", scope: AnyBackend, run: ScenarioSerial_ConnPerRequestEcho_PortForward},
		{name: "Serial_ConnReusedEcho_PortForward", scope: AnyBackend, run: ScenarioSerial_ConnReusedEcho_PortForward},
		{name: "Serial_ConnPrewarmedEcho_PortForward", scope: AnyBackend, run: ScenarioSerial_ConnPrewarmedEcho_PortForward},
		{name: "Parallel_ConnPerRequestEcho_PortForward", scope: AnyBackend, run: ScenarioParallel_ConnPerRequestEcho_PortForward},
		{name: "Parallel_ConnReusedEcho_PortForward", scope: AnyBackend, run: ScenarioParallel_ConnReusedEcho_PortForward},
		{name: "Parallel_ConnPrewarmedEcho_PortForward", scope: AnyBackend, run: ScenarioParallel_ConnPrewarmedEcho_PortForward},
		{name: "Serial_ConnPerRequestEcho_SOCKS5", scope: AnyBackend, run: ScenarioSerial_ConnPerRequestEcho_SOCKS5},
		{name: "Serial_ConnReusedEcho_SOCKS5", scope: AnyBackend, run: ScenarioSerial_ConnReusedEcho_SOCKS5},
		{name: "Serial_ConnPrewarmedEcho_SOCKS5", scope: AnyBackend, run: ScenarioSerial_ConnPrewarmedEcho_SOCKS5},
		{name: "Parallel_ConnPerRequestEcho_SOCKS5", scope: AnyBackend, run: ScenarioParallel_ConnPerRequestEcho_SOCKS5},
		{name: "Parallel_ConnReusedEcho_SOCKS5", scope: AnyBackend, run: ScenarioParallel_ConnReusedEcho_SOCKS5},
		{name: "Parallel_ConnPrewarmedEcho_SOCKS5", scope: AnyBackend, run: ScenarioParallel_ConnPrewarmedEcho_SOCKS5},
		{name: "Parallel_ConnPrewarmedEcho_SOCKS5_MultiTarget", scope: AnyBackend, run: ScenarioParallel_ConnPrewarmedEcho_SOCKS5_MultiTarget},
	}
}

// ScenarioConnectLatency_Serial_PortForward opens 10 serial
// port-forward connections, each performing one 1-byte echo
// round-trip, and asserts every iteration completes inside
// b.ConnectLatencyThreshold(). One additional untimed warm-up
// dial precedes the measured loop so per-process cold-start
// costs (notably the EntraTokenProvider's first credential
// fetch — ~2-3 s observed against Entra ID) don't bleed into
// the first measured iteration. The scenario logs the warm-up
// elapsed for visibility and a per-iteration elapsed so a
// failure makes the offending iteration obvious.
//
// Measured iteration count is fixed at 10 (plus 1 warm-up).
// Walltime bounds at the current 3 s thresholds:
//   - Happy path: 11 × ~1 s rendezvous ≈ 11 s per scenario.
//   - Threshold-only violation (operational success, elapsed too
//     high): bounded by 11 × (threshold + connectSlack) ≈ 88 s
//     per scenario, since runSerialConnectLatency uses
//     t.Errorf + continue on threshold violations and a
//     successful iteration can use up to threshold+connectSlack
//     of budget before the deadline fires.
//   - Operational error (dial/echo/close failure): fails fast at
//     ≈ threshold + connectSlack ≈ 8 s via t.Fatalf.
//
// All three bounds keep the scenario well inside both the
// per-test default timeout and the e2e job's 40 m envelope.
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
// ConnectLatency_Serial variants. Each measured iteration: dial
// (port-forward TCP or SOCKS5 CONNECT) → write 1 byte → read
// 1 byte echoed back → close. Asserts elapsed < threshold per
// measured iteration.
//
// A single warm-up dial precedes the measured loop. Its elapsed
// is logged but neither threshold-asserted nor counted in the
// measured iteration count. The warm-up absorbs per-process
// cold-start costs that the steady-state scenario isn't trying
// to characterize — most prominently the EntraTokenProvider's
// first credential fetch, which observably adds ~2-3 s to the
// first connection and exceeds the 3 s threshold on most runs
// against Entra ID. Without the warm-up, ConnectLatency_Serial
// flakes on Entra cells while reporting healthy steady-state
// latency on every subsequent iteration.
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
//     320 s failure-path burn against the 40 m e2e workflow
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
// one topology, perform one untimed warm-up dial, and then dial
// it 10 times in sequence. By the first measured iteration the
// relay is fully warm; failing on a real dial error here is what
// we want to measure.
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

	warmupElapsed, err := timeOneConnect(tun.SenderAddr, echo.Addr(), mode, payload, buf, threshold)
	if err != nil {
		t.Fatalf("warmup: %v (elapsed=%v)", err, warmupElapsed)
	}
	t.Logf("ConnectLatency_Serial_%v warmup: %v (untimed, threshold %v)", mode, warmupElapsed, threshold)

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

// ScenarioConnectLatency_ColdStart_PortForward opens exactly one
// port-forward connection on a freshly-started sender, times the
// full Dial → 1-byte write → 1-byte echo read → Close round-trip,
// and asserts the elapsed wall time is less than
// b.ColdStartLatencyThreshold(). It deliberately performs no warm-
// up: the measured iteration is the first connection through the
// sender, so per-process cold-start costs the steady-state scenario
// excludes (most prominently the EntraTokenProvider's first OAuth2
// token fetch) are inside the budget.
//
// This scenario is a regression alarm on first-connection latency.
// The steady-state ConnectLatency_Serial_* scenarios discard one
// untimed warm-up dial before measuring; if that warm-up dial
// silently drifts from ~1 s to >20 s (e.g. a token-cache regression
// or an Entra ID outage) the steady-state scenarios would keep
// passing while operators would see catastrophic first-connection
// latency. ColdStart guards exactly that path with a separate,
// wider budget.
//
// Backends configure the budget via ColdStartLatencyThreshold; see
// the Backend.ColdStartLatencyThreshold doc for the rationale on
// the Azure value (covers both workload-identity-federation in CI
// at ~1.3 s and `az` CLI shell-out locally at ~3.3 s).
//
// Failure-mode handling mirrors runSerialConnectLatency: operational
// errors are t.Fatalf, threshold violations are t.Errorf. With only
// one measured dial the distinction matters less than in the serial
// case but the symmetry keeps log lines uniform between the two
// scenario families.
func ScenarioConnectLatency_ColdStart_PortForward(t *testing.T, b Backend) {
	t.Helper()
	AssertNoLeaks(t)
	runColdStartConnectLatency(t, b, ModePortForward)
}

// ScenarioConnectLatency_ColdStart_SOCKS5 mirrors the port-forward
// variant against a SOCKS5 proxy sender. The SOCKS5 handshake adds
// tens of milliseconds on top of the relay rendezvous and OAuth2
// token fetch; the same ColdStartLatencyThreshold applies because
// the handshake cost is negligible relative to the cold-start
// budget.
func ScenarioConnectLatency_ColdStart_SOCKS5(t *testing.T, b Backend) {
	t.Helper()
	AssertNoLeaks(t)
	runColdStartConnectLatency(t, b, ModeSOCKS5)
}

// runColdStartConnectLatency is the shared implementation of the
// two ConnectLatency_ColdStart variants. It builds a fresh topology,
// performs exactly one timed dial through the cold sender, and
// asserts the elapsed time is below b.ColdStartLatencyThreshold().
//
// Unlike runSerialConnectLatency, no warm-up dial precedes the
// measurement — measuring the cold-start cost is the whole point.
// The dial timeout and connection deadline both use
// threshold + connectSlack so a regression surfaces as the explicit
// elapsed >= threshold assertion rather than as an i/o timeout from
// the deadline firing exactly at the threshold.
func runColdStartConnectLatency(t *testing.T, b Backend, mode SenderMode) {
	t.Helper()
	threshold := b.ColdStartLatencyThreshold()
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

	payload := []byte{0x42}
	buf := make([]byte, 1)

	elapsed, err := timeOneConnect(tun.SenderAddr, echo.Addr(), mode, payload, buf, threshold)
	if err != nil {
		t.Fatalf("cold-start dial: %v (elapsed=%v)", err, elapsed)
	}
	if elapsed >= threshold {
		t.Errorf("cold-start connect latency %v >= threshold %v", elapsed, threshold)
		return
	}
	t.Logf("ConnectLatency_ColdStart_%v: %v (< %v)", mode, elapsed, threshold)
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

// ============================================================================
// Parameterized echo-workload scenarios.
//
// These complement the threshold-asserting smoke scenarios above by
// capturing how representative application workload shapes (e.g. "15
// serial REST-style calls, fresh conn each time", "5 concurrent conns
// each doing 6 sequential round-trips", "5 pre-warmed conns sustained
// throughput") behave on a backend. Each scenario emits a single
// `workload-summary` log line so the same code can be diffed across
// backends (mock / westus2 / AUE / India / …) to characterize the
// cost of the Azure Relay rendezvous and bridged data path.
//
// Three connection lifecycles, two concurrency modes, two sender modes:
//
//   ConnPerRequest   each measured request gets a fresh conn (cold path)
//   ConnReused       one conn handles many requests; cold path
//                    AND warm path are measured in the same run
//   ConnPrewarmed    one warm-up request per conn is discarded; only
//                    the steady-state distribution is reported
//
//   Serial           one conn in flight at a time
//   Parallel         N conns in flight simultaneously
//
//   _PortForward     sender = `aztunnel relay-sender port-forward`
//   _SOCKS5          sender = `aztunnel relay-sender socks5-proxy`
//
// Scenarios do not assert per-request latency thresholds; the
// workload-summary line carries the per-shape distribution for
// trend / regression detection. Each round's wall time is strictly
// bounded by roundBudget(threshold, w) — per-conn dial+I/O deadlines
// are clamped to the remaining round budget, so a hung or slow
// connection cannot extend the round beyond its budget. CI failure
// surfaces when the workload itself fails (connection refused,
// mismatched echo, listener-side rejection, etc.) or when a conn
// hits the clamped deadline before completing.
// ============================================================================

// WorkloadShape parameterizes a single echo-workload run.
//
// The (TotalConns, Concurrency, RequestsPerConn) triple defines the
// shape; WarmupRequests determines how many of each conn's leading
// requests are discarded from the reported stats.
//
//	cold-path scenarios (ConnPerRequest, ConnReused):
//	  WarmupRequests = 0 — first request times count as ttfw/ttc
//	steady-state scenarios (ConnPrewarmed):
//	  WarmupRequests = 1 — first request is discarded; the rest are
//	  reported as warm_min/mean/p50/p95/p99/max
type WorkloadShape struct {
	TotalConns      int
	Concurrency     int
	RequestsPerConn int // total requests per conn, INCLUDING warmup
	WarmupRequests  int // first N requests per conn discarded from stats
	ReqSize         int
	RespSize        int
	// NumTargets is the number of downstream echo servers the
	// workload fans out to. Connections are assigned to targets
	// round-robin by conn index. Defaults to 1.
	//
	// Only meaningful in ModeSOCKS5 — the SOCKS5 client picks the
	// target per connection, and the listener allows any address
	// in the configured list. In ModePortForward the listener has a
	// single fixed Target on SetupOptions, so NumTargets > 1 is
	// rejected by validate(); fan-out under PortForward needs a
	// separate axis (NumListeners) and is out of scope here.
	NumTargets   int
	Mode         SenderMode
	RepeatRounds int
}

func runWorkload(t *testing.T, b Backend, w WorkloadShape) {
	t.Helper()
	AssertNoLeaks(t)
	if w.Concurrency <= 0 {
		w.Concurrency = 1
	}
	if w.RepeatRounds <= 0 {
		w.RepeatRounds = 1
	}
	if w.NumTargets <= 0 {
		w.NumTargets = 1
	}
	if err := w.validate(); err != nil {
		t.Fatalf("invalid WorkloadShape: %v", err)
	}
	addrs := make([]string, w.NumTargets)
	for i := range addrs {
		addrs[i] = StartPlainEcho(t).Addr()
	}
	opts := SetupOptions{NumListeners: 1, SenderMode: w.Mode, AllowedTargets: addrs}
	if w.Mode == ModePortForward {
		// validate() ensures NumTargets == 1 in this mode.
		opts.Target = addrs[0]
	}
	tun := b.Setup(t, opts)
	threshold := b.ConnectLatencyThreshold()

	for r := 0; r < w.RepeatRounds; r++ {
		if w.RepeatRounds > 1 {
			t.Logf("--- round %d/%d ---", r+1, w.RepeatRounds)
		}
		runRound(t, tun, addrs, threshold, w)
	}
}

// validate checks the workload-shape invariants the runner relies on.
// Called from runWorkload AFTER the zero-value defaults for
// Concurrency, RepeatRounds, and NumTargets have been filled in.
func (w WorkloadShape) validate() error {
	if w.TotalConns < 1 {
		return fmt.Errorf("TotalConns must be >= 1, got %d", w.TotalConns)
	}
	if w.Concurrency > w.TotalConns {
		return fmt.Errorf("WorkloadShape.Concurrency (%d) must be <= TotalConns (%d): more parallel slots than connections is a misconfiguration",
			w.Concurrency, w.TotalConns)
	}
	if w.RequestsPerConn < 1 {
		return fmt.Errorf("RequestsPerConn must be >= 1, got %d", w.RequestsPerConn)
	}
	if w.WarmupRequests < 0 {
		return fmt.Errorf("WarmupRequests must be >= 0, got %d", w.WarmupRequests)
	}
	if w.WarmupRequests >= w.RequestsPerConn {
		return fmt.Errorf("WarmupRequests (%d) must be < RequestsPerConn (%d) to leave at least one measured request",
			w.WarmupRequests, w.RequestsPerConn)
	}
	if w.ReqSize < 1 || w.RespSize < 1 {
		return fmt.Errorf("ReqSize (%d) and RespSize (%d) must both be >= 1", w.ReqSize, w.RespSize)
	}
	if w.ReqSize != w.RespSize {
		return fmt.Errorf("ReqSize (%d) and RespSize (%d) must match: the workload is an echo, so the response is the request",
			w.ReqSize, w.RespSize)
	}
	if w.NumTargets < 1 {
		return fmt.Errorf("NumTargets must be >= 1, got %d", w.NumTargets)
	}
	if w.NumTargets > 1 && w.Mode == ModePortForward {
		return fmt.Errorf("NumTargets > 1 is only supported with ModeSOCKS5; ModePortForward has a single fixed Target (got NumTargets=%d)", w.NumTargets)
	}
	switch w.Mode {
	case ModePortForward, ModeSOCKS5:
	default:
		return fmt.Errorf("mode must be ModePortForward or ModeSOCKS5, got %v", w.Mode)
	}
	return nil
}

// connResult holds the per-conn timings runOneConn returns.
//
// If WarmupRequests == 0, FirstReq is the cold-path measurement
// (dial → write[0] → read[0]) and Warm holds the elapsed times of
// requests 1..N-1 (each measured from write to read of that request).
//
// If WarmupRequests > 0, FirstReq is zero-valued and Warm holds the
// elapsed times of requests WarmupRequests..N-1.
type connResult struct {
	FirstReq firstReqTiming
	Warm     []time.Duration
	Err      error
}

type firstReqTiming struct {
	TTFW time.Duration // dial → first write complete (request transmitted to local sender; NOT the conventional "time to first byte received")
	TTC  time.Duration // dial → first read complete (full request → response round-trip including dial)
}

func runRound(t *testing.T, tun *Tunnel, addrs []string, threshold time.Duration, w WorkloadShape) {
	results := make([]connResult, w.TotalConns)

	sem := make(chan struct{}, w.Concurrency)
	var wg sync.WaitGroup
	budget := roundBudget(threshold, w)
	start := time.Now()
	roundDeadline := start.Add(budget)

	for i := 0; i < w.TotalConns; i++ {
		sem <- struct{}{}
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			defer func() { <-sem }()
			target := addrs[i%len(addrs)]
			results[i] = runOneConn(tun.SenderAddr, target, w, roundDeadline)
			r := results[i]
			if r.Err != nil {
				t.Logf("conn[%d] FAILED: %v", i, r.Err)
				return
			}
			switch {
			case w.WarmupRequests > 0 && len(r.Warm) > 0:
				wmin, wmax, wmean := minMaxMean(r.Warm)
				t.Logf("conn[%d] warm_n=%d warm_min=%v warm_mean=%v warm_max=%v",
					i, len(r.Warm), wmin, wmean, wmax)
			case len(r.Warm) > 0:
				wmin, wmax, wmean := minMaxMean(r.Warm)
				t.Logf("conn[%d] first_req_ttfw=%v first_req_ttc=%v warm_n=%d warm_min=%v warm_mean=%v warm_max=%v",
					i, r.FirstReq.TTFW, r.FirstReq.TTC, len(r.Warm), wmin, wmean, wmax)
			default:
				t.Logf("conn[%d] ttfw=%v ttc=%v",
					i, r.FirstReq.TTFW, r.FirstReq.TTC)
			}
		}(i)
	}
	wg.Wait()
	wall := time.Since(start)

	var failures int
	var coldTTFW, coldTTC, warmAll []time.Duration
	for _, r := range results {
		if r.Err != nil {
			failures++
			continue
		}
		if w.WarmupRequests == 0 {
			coldTTFW = append(coldTTFW, r.FirstReq.TTFW)
			coldTTC = append(coldTTC, r.FirstReq.TTC)
		}
		warmAll = append(warmAll, r.Warm...)
	}
	if failures > 0 {
		t.Errorf("%d/%d connections failed", failures, w.TotalConns)
	}

	var parts []string
	// Tag the summary line with the full subtest path so the line is
	// self-identifying when copied out of test output for off-line
	// aggregation. The subtest path is the slash-joined chain of
	// Backend.Axes() values followed by the scenario subtest name
	// (the string passed to t.Run, not the Go function name — e.g.
	// `TestE2E_Azure/entra/Parallel_ConnPrewarmedEcho_SOCKS5`).
	parts = append(parts, "workload-summary", fmt.Sprintf("scenario=%s", t.Name()))

	if len(coldTTFW) > 0 {
		parts = append(parts,
			fmtDist("ttfw", coldTTFW),
			fmtDist("ttc", coldTTC),
		)
	}
	if len(warmAll) > 0 {
		parts = append(parts, fmtDist("warm", warmAll))
	}
	parts = append(parts,
		fmt.Sprintf("wall=%v", wall),
		fmt.Sprintf("budget=%v", budget),
		fmt.Sprintf("cold_n=%d", len(coldTTFW)),
		fmt.Sprintf("warm_n=%d", len(warmAll)),
		fmt.Sprintf("num_targets=%d", len(addrs)),
		fmt.Sprintf("success=%d/%d", w.TotalConns-failures, w.TotalConns),
	)
	t.Logf("%s", strings.Join(parts, " "))

	// Go's net deadlines are best-effort: an I/O op can return a few
	// hundred milliseconds after its deadline under scheduler / OS
	// poll jitter, so allow a small epsilon over the budget before
	// failing the sanity assertion.
	const sanityEpsilon = 2 * time.Second
	if wall > budget+sanityEpsilon {
		t.Fatalf("sanity: round wall=%v exceeded budget=%v + epsilon=%v despite per-conn deadline clamping (shape: %+v)", wall, budget, sanityEpsilon, w)
	}
}

// roundBudget returns the per-round wall-time ceiling for shape w on
// a backend with the given per-conn connect-latency threshold. The
// formula models per-conn cost as `threshold` (cold path: dial +
// rendezvous) plus `RequestsPerConn × 500 ms` (warm path: pessimistic
// upper bound on RTT for cross-region links), multiplied by the
// serial depth (TotalConns / Concurrency rounded up), with a 2×
// safety factor and a 60 s floor for trivial shapes. Round wall time
// is bounded by this budget because per-conn dial+I/O deadlines in
// runOneConn are clamped to the remaining round budget; the post-hoc
// check in runRound is a sanity assertion.
func roundBudget(threshold time.Duration, w WorkloadShape) time.Duration {
	serialDepth := (w.TotalConns + w.Concurrency - 1) / w.Concurrency
	perConn := threshold + time.Duration(w.RequestsPerConn)*500*time.Millisecond
	budget := time.Duration(serialDepth) * perConn * 2
	if budget < 60*time.Second {
		budget = 60 * time.Second
	}
	return budget
}

// fmtDist returns a space-joined "<label>_<stat>=<v>" sequence for a
// distribution. Includes p50/p95/p99 only when the sample is large
// enough for each to be meaningful (10/20/100 samples respectively).
func fmtDist(label string, ds []time.Duration) string {
	if len(ds) == 0 {
		return ""
	}
	sorted := make([]time.Duration, len(ds))
	copy(sorted, ds)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	min, max, mean := minMaxMean(sorted)
	out := []string{
		fmt.Sprintf("%s_min=%v", label, min),
		fmt.Sprintf("%s_mean=%v", label, mean),
	}
	if len(sorted) >= 10 {
		out = append(out, fmt.Sprintf("%s_p50=%v", label, pct(sorted, 0.50)))
	}
	if len(sorted) >= 20 {
		out = append(out, fmt.Sprintf("%s_p95=%v", label, pct(sorted, 0.95)))
	}
	if len(sorted) >= 100 {
		out = append(out, fmt.Sprintf("%s_p99=%v", label, pct(sorted, 0.99)))
	}
	out = append(out, fmt.Sprintf("%s_max=%v", label, max))
	return strings.Join(out, " ")
}

// pct returns the value at the given percentile (0.0..1.0) from a
// pre-sorted slice. Uses linear-rank selection (no interpolation);
// callers should require enough samples for the percentile to be
// meaningful before calling.
func pct(sorted []time.Duration, p float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	idx := int(float64(len(sorted)-1) * p)
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}

func minMaxMean(ds []time.Duration) (min, max, mean time.Duration) {
	min, max = ds[0], ds[0]
	var sum time.Duration
	for _, d := range ds {
		if d < min {
			min = d
		}
		if d > max {
			max = d
		}
		sum += d
	}
	return min, max, sum / time.Duration(len(ds))
}

// runOneConn dials the sender, runs the workload on the resulting
// connection, and returns the per-conn timing breakdown. The dial
// timeout is the smaller of 60 s and the time remaining until
// roundDeadline; the post-dial connection deadline is the earlier of
// now+120 s and roundDeadline. If less than minDeadline remains
// before dialing, the dial is skipped and an error is returned (the
// floor is only enforced at this pre-dial gate; the clamped
// deadlines themselves are not floored). This keeps each round's
// wall time strictly bounded by runRound's roundBudget.
func runOneConn(senderAddr, target string, w WorkloadShape, roundDeadline time.Time) connResult {
	const minDeadline = 100 * time.Millisecond
	if time.Until(roundDeadline) <= minDeadline {
		return connResult{Err: fmt.Errorf("round budget exhausted before dial")}
	}
	dialTimeout := 60 * time.Second
	if remaining := time.Until(roundDeadline); remaining < dialTimeout {
		dialTimeout = remaining
	}
	dialStart := time.Now()
	var conn net.Conn
	var err error
	switch w.Mode {
	case ModePortForward:
		conn, err = net.DialTimeout("tcp", senderAddr, dialTimeout)
	case ModeSOCKS5:
		conn, err = DialSOCKS5(senderAddr, target, dialTimeout)
	default:
		return connResult{Err: fmt.Errorf("unsupported mode %v", w.Mode)}
	}
	if err != nil {
		return connResult{Err: fmt.Errorf("dial: %w", err)}
	}
	defer conn.Close() //nolint:errcheck // best-effort cleanup
	connDeadline := time.Now().Add(120 * time.Second)
	if connDeadline.After(roundDeadline) {
		connDeadline = roundDeadline
	}
	if err := conn.SetDeadline(connDeadline); err != nil {
		return connResult{Err: fmt.Errorf("set deadline: %w", err)}
	}

	req := make([]byte, w.ReqSize)
	for i := range req {
		req[i] = byte(i)
	}
	buf := make([]byte, w.RespSize)

	res := connResult{}
	for i := 0; i < w.RequestsPerConn; i++ {
		reqStart := time.Now()
		if err = writeFull(conn, req); err != nil {
			res.Err = fmt.Errorf("write[%d]: %w", i, err)
			return res
		}
		if i == 0 && w.WarmupRequests == 0 {
			res.FirstReq.TTFW = time.Since(dialStart)
		}
		if _, err = io.ReadFull(conn, buf); err != nil {
			res.Err = fmt.Errorf("read[%d]: %w", i, err)
			return res
		}
		if !bytes.Equal(buf, req) {
			res.Err = fmt.Errorf("echo[%d]: payload mismatch", i)
			return res
		}
		elapsed := time.Since(reqStart)
		switch {
		case i == 0 && w.WarmupRequests == 0:
			res.FirstReq.TTC = time.Since(dialStart)
		case i >= w.WarmupRequests:
			res.Warm = append(res.Warm, elapsed)
		}
	}
	return res
}

// ----------------------------------------------------------------------------
// PortForward scenarios
// ----------------------------------------------------------------------------

// ScenarioSerial_ConnPerRequestEcho_PortForward dials 15 connections one
// at a time; each does a single 1 KB → 1 KB echo round-trip and
// closes. Measures cold-path latency (the dial cost dominates).
func ScenarioSerial_ConnPerRequestEcho_PortForward(t *testing.T, b Backend) {
	runWorkload(t, b, WorkloadShape{
		TotalConns: 15, Concurrency: 1, RequestsPerConn: 1,
		ReqSize: 1024, RespSize: 1024, Mode: ModePortForward,
	})
}

// ScenarioSerial_ConnReusedEcho_PortForward dials one connection and
// does 30 sequential 1 KB → 1 KB echo round-trips on it. First-request
// stats include the dial cost (cold path); the remaining 29 are
// reported as warm distribution.
func ScenarioSerial_ConnReusedEcho_PortForward(t *testing.T, b Backend) {
	runWorkload(t, b, WorkloadShape{
		TotalConns: 1, Concurrency: 1, RequestsPerConn: 30,
		ReqSize: 1024, RespSize: 1024, Mode: ModePortForward,
	})
}

// ScenarioSerial_ConnPrewarmedEcho_PortForward dials one connection,
// does one warm-up round-trip (discarded), then 50 measured 1 KB → 1 KB
// echo round-trips. Reports steady-state distribution only.
func ScenarioSerial_ConnPrewarmedEcho_PortForward(t *testing.T, b Backend) {
	runWorkload(t, b, WorkloadShape{
		TotalConns: 1, Concurrency: 1, RequestsPerConn: 51, WarmupRequests: 1,
		ReqSize: 1024, RespSize: 1024, Mode: ModePortForward,
	})
}

// ScenarioParallel_ConnPerRequestEcho_PortForward dials 30 connections
// concurrently, each doing a single 1 KB → 1 KB echo round-trip.
// Measures cold-path latency under contention.
func ScenarioParallel_ConnPerRequestEcho_PortForward(t *testing.T, b Backend) {
	runWorkload(t, b, WorkloadShape{
		TotalConns: 30, Concurrency: 30, RequestsPerConn: 1,
		ReqSize: 1024, RespSize: 1024, Mode: ModePortForward,
	})
}

// ScenarioParallel_ConnReusedEcho_PortForward dials 5 connections
// concurrently; each does 6 sequential 1 KB → 1 KB echo round-trips.
// Reports cold-path (per-conn first request) and warm path together.
func ScenarioParallel_ConnReusedEcho_PortForward(t *testing.T, b Backend) {
	runWorkload(t, b, WorkloadShape{
		TotalConns: 5, Concurrency: 5, RequestsPerConn: 6,
		ReqSize: 1024, RespSize: 1024, Mode: ModePortForward,
	})
}

// ScenarioParallel_ConnPrewarmedEcho_PortForward dials 5 connections
// concurrently; each does one warm-up round-trip (discarded) and 20
// measured 1 KB → 1 KB echo round-trips. 100 total warm samples
// reported as the steady-state distribution. Closest of the family
// to a sustained-throughput test.
func ScenarioParallel_ConnPrewarmedEcho_PortForward(t *testing.T, b Backend) {
	runWorkload(t, b, WorkloadShape{
		TotalConns: 5, Concurrency: 5, RequestsPerConn: 21, WarmupRequests: 1,
		ReqSize: 1024, RespSize: 1024, Mode: ModePortForward,
	})
}

// ----------------------------------------------------------------------------
// SOCKS5 scenarios (identical workload shapes, SOCKS5 sender)
// ----------------------------------------------------------------------------

func ScenarioSerial_ConnPerRequestEcho_SOCKS5(t *testing.T, b Backend) {
	runWorkload(t, b, WorkloadShape{
		TotalConns: 15, Concurrency: 1, RequestsPerConn: 1,
		ReqSize: 1024, RespSize: 1024, Mode: ModeSOCKS5,
	})
}

func ScenarioSerial_ConnReusedEcho_SOCKS5(t *testing.T, b Backend) {
	runWorkload(t, b, WorkloadShape{
		TotalConns: 1, Concurrency: 1, RequestsPerConn: 30,
		ReqSize: 1024, RespSize: 1024, Mode: ModeSOCKS5,
	})
}

func ScenarioSerial_ConnPrewarmedEcho_SOCKS5(t *testing.T, b Backend) {
	runWorkload(t, b, WorkloadShape{
		TotalConns: 1, Concurrency: 1, RequestsPerConn: 51, WarmupRequests: 1,
		ReqSize: 1024, RespSize: 1024, Mode: ModeSOCKS5,
	})
}

func ScenarioParallel_ConnPerRequestEcho_SOCKS5(t *testing.T, b Backend) {
	runWorkload(t, b, WorkloadShape{
		TotalConns: 30, Concurrency: 30, RequestsPerConn: 1,
		ReqSize: 1024, RespSize: 1024, Mode: ModeSOCKS5,
	})
}

func ScenarioParallel_ConnReusedEcho_SOCKS5(t *testing.T, b Backend) {
	runWorkload(t, b, WorkloadShape{
		TotalConns: 5, Concurrency: 5, RequestsPerConn: 6,
		ReqSize: 1024, RespSize: 1024, Mode: ModeSOCKS5,
	})
}

func ScenarioParallel_ConnPrewarmedEcho_SOCKS5(t *testing.T, b Backend) {
	runWorkload(t, b, WorkloadShape{
		TotalConns: 5, Concurrency: 5, RequestsPerConn: 21, WarmupRequests: 1,
		ReqSize: 1024, RespSize: 1024, Mode: ModeSOCKS5,
	})
}

// ----------------------------------------------------------------------------
// Multi-target scenarios (NumTargets > 1; SOCKS5-only by validate())
// ----------------------------------------------------------------------------

// ScenarioParallel_ConnPrewarmedEcho_SOCKS5_MultiTarget mirrors
// ScenarioParallel_ConnPrewarmedEcho_SOCKS5 (5 parallel conns × 21
// requests, 1 warm-up discarded) but fans out across 4 distinct
// downstream echo servers — conns are assigned to targets
// round-robin by index. Exercises the SOCKS5 listener's allow-list
// fan-out path and the per-target rendezvous behavior under
// steady-state load. Single observation point for the NumTargets
// axis; broader sweep is out of scope here.
func ScenarioParallel_ConnPrewarmedEcho_SOCKS5_MultiTarget(t *testing.T, b Backend) {
	runWorkload(t, b, WorkloadShape{
		TotalConns: 5, Concurrency: 5, RequestsPerConn: 21, WarmupRequests: 1,
		ReqSize: 1024, RespSize: 1024, Mode: ModeSOCKS5,
		NumTargets: 4,
	})
}

// BulkTransferBytes is the payload size ScenarioBulkTransfer streams
// through the tunnel in each direction. 10 MiB is large enough to
// exercise multiple TCP windows and the bridge's read/write loop
// beyond the single-segment regime of the latency scenarios, yet
// small enough to keep wall time bounded on Azure (where the per-
// connection envelope is bandwidth-bound, currently ~14 Mbps).
//
// SHA-256 of the random payload is verified end-to-end; corruption
// surfaces as a hash mismatch regardless of payload size, so the
// signal does not require a multi-hundred-MB transfer.
const BulkTransferBytes = 10 << 20

// ScenarioBulkTransfer streams BulkTransferBytes of crypto/rand bytes
// from the sender into a plain TCP echo target, reads them back, and
// asserts SHA-256 parity. This catches in-flight corruption,
// truncation, and reorder bugs that the small-payload echo scenarios
// would miss, and exercises the per-connection throughput envelope of
// each backend.
//
// scope=AnyBackend: mock completes in <1 s; Azure completes in
// ~10-15 s per cell (one cell per auth axis value).
func ScenarioBulkTransfer(t *testing.T, b Backend) {
	t.Helper()
	AssertNoLeaks(t)

	echo := StartPlainEcho(t)
	tun := b.Setup(t, SetupOptions{
		NumListeners:   1,
		SenderMode:     ModePortForward,
		Target:         echo.Addr(),
		AllowedTargets: []string{echo.Addr()},
	})

	conn, err := net.DialTimeout("tcp", tun.SenderAddr, 10*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close() //nolint:errcheck // best-effort cleanup

	// 2 minutes covers the worst-observed Azure single-stream
	// throughput (~14 Mbps × 10 MiB ≈ 6 s) with margin for jitter.
	_ = conn.SetDeadline(time.Now().Add(2 * time.Minute))

	sentHash := make(chan [sha256.Size]byte, 1)
	go func() {
		h := sha256.New()
		chunk := make([]byte, 64*1024)
		remaining := BulkTransferBytes
		for remaining > 0 {
			n := len(chunk)
			if n > remaining {
				n = remaining
			}
			if _, err := rand.Read(chunk[:n]); err != nil {
				return
			}
			h.Write(chunk[:n])
			if _, err := conn.Write(chunk[:n]); err != nil {
				return
			}
			remaining -= n
		}
		var sum [sha256.Size]byte
		copy(sum[:], h.Sum(nil))
		sentHash <- sum
	}()

	gotHash := sha256.New()
	if n, err := io.CopyN(gotHash, conn, BulkTransferBytes); err != nil {
		t.Fatalf("read after %d bytes: %v", n, err)
	}
	want := <-sentHash
	if !bytes.Equal(want[:], gotHash.Sum(nil)) {
		t.Fatal("SHA-256 mismatch: bulk transfer data corrupted")
	}
	t.Logf("transferred %d MiB intact", BulkTransferBytes>>20)
}
