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
// against b as sub-tests under the caller's t. They express
// connection-setup and short-session work as testing.T so CI fails
// when wall time exceeds the per-backend threshold.
//
// Two scenarios assert connect time against the backend's
// ConnectLatencyPolicy (ConnectLatency_Serial) / ColdStartLatency-
// Threshold (ConnectLatency_ColdStart); ShortSession_Serial is
// observation-only.
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
// applied inside each scenario via b.ConnectLatencyPolicy(),
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

// ScenarioConnectLatency_Serial_PortForward opens a backend-defined
// number of serial port-forward connections (ConnectLatencyPolicy.
// Iterations), each performing one 1-byte echo round-trip, and asserts
// the batch against the backend's quantile gate (upper-median <
// NormalP50 AND soft-tail < SoftTail; see ConnectLatencyPolicy). One
// additional untimed warm-up dial precedes the measured loop so
// per-process cold-start costs (notably the EntraTokenProvider's first
// credential fetch — ~2-3 s observed against Entra ID) don't bleed into
// the measured samples. The scenario logs the warm-up elapsed and a
// per-iteration elapsed plus the distribution so a failure is
// diagnosable from CI output alone.
//
// Walltime is bounded by Iterations × (SpikeCeiling + connectSlack) in
// the worst case (every dial stalls to its deadline), well inside the
// e2e job's 60 m envelope; the happy path is Iterations × ~steady-state.
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
// 1 byte echoed back → close. It collects every iteration's elapsed
// time and asserts the batch against the backend's
// ConnectLatencyPolicy quantile gate (see evaluateConnectLatency and
// the ConnectLatencyPolicy doc) rather than checking each sample
// against a single ceiling.
//
// The quantile gate exists because the dominant failure mode against
// real Azure Relay is an isolated multi-second rendezvous spike that
// originates in the relay control plane, not in aztunnel (proven from
// full sender+listener logs: zero client-side retries, the listener
// sits idle in ws.Read for the whole stall, then accepts normally).
// A per-sample ceiling turns that unfixable tail into a flake; a
// per-sample ceiling with a tolerated-spike count reintroduces the
// same cliff at a higher count. The upper-median + soft-tail gate
// tolerates the sparse tail while still tripping on a broad regression
// (which an old "max < 3 s" check missed entirely below 3 s).
//
// A single warm-up dial precedes the measured loop. Its elapsed is
// logged but neither asserted nor counted. The warm-up absorbs
// per-process cold-start costs the steady-state scenario isn't trying
// to characterize — most prominently the EntraTokenProvider's first
// credential fetch, which observably adds ~2-3 s to the first
// connection. Cold-start cost is regression-protected separately via
// ConnectLatency_ColdStart_*.
//
// Operational errors (dial/write/read/echo-mismatch/close) → t.Fatalf:
// they mean the harness is fundamentally broken and every subsequent
// iteration would burn the same deadline. The per-dial deadline is
// anchored to policy.SpikeCeiling + connectSlack (not the assertion
// thresholds) so a tolerated spike is measured rather than killed by
// an i/o timeout.
//
// dialWithRetry / dialSOCKS5WithRetry are intentionally not used —
// the retry helpers exist to swallow first-dial transient races on
// brand-new tunnels, but by the first measured iteration the relay is
// fully warm (one untimed warm-up dial precedes the loop); failing on
// a real dial error here is what we want to measure.
//
// When a tolerated spike occurs (a sample >= NormalP50) but the batch
// still passes, the captured sender/listener rendezvous traces are
// dumped so the resp_wait phase split is visible for that run — the
// failure dump alone would discard them on a green run.
func runSerialConnectLatency(t *testing.T, b Backend, mode SenderMode) {
	t.Helper()
	policy := b.ConnectLatencyPolicy()
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
	dumpConnectLatencyLogsOnFail(t, tun)

	payload := []byte{0x42}
	buf := make([]byte, 1)

	// The per-dial deadline is anchored to SpikeCeiling, not to the
	// assertion thresholds, so a tolerated rendezvous spike is measured
	// and folded into the quantiles rather than killed by an i/o
	// timeout (which would be an unrecoverable t.Fatalf below).
	deadlineBudget := policy.SpikeCeiling

	warmupElapsed, err := timeOneConnect(tun.SenderAddr, echo.Addr(), mode, payload, buf, deadlineBudget)
	if err != nil {
		t.Fatalf("warmup: %v (elapsed=%v)", err, warmupElapsed)
	}
	t.Logf("ConnectLatency_Serial_%v warmup: %v (untimed)", mode, warmupElapsed)

	samples := make([]time.Duration, 0, policy.Iterations)
	spikes := 0
	for i := 0; i < policy.Iterations; i++ {
		elapsed, err := timeOneConnect(tun.SenderAddr, echo.Addr(), mode, payload, buf, deadlineBudget)
		if err != nil {
			t.Fatalf("iter %d: %v (elapsed=%v)", i, err, elapsed)
		}
		samples = append(samples, elapsed)
		if elapsed >= policy.NormalP50 {
			spikes++
			t.Logf("ConnectLatency_Serial_%v iter %d: %v [tolerated spike >= NormalP50 %v; see rendezvous trace dump below for the resp_wait phase split]",
				mode, i, elapsed, policy.NormalP50)
			continue
		}
		t.Logf("ConnectLatency_Serial_%v iter %d: %v", mode, i, elapsed)
	}

	t.Logf("ConnectLatency_Serial_%v distribution: %s (policy normalP50=%v softTail=%v over %d iters, %d tolerated spike(s))",
		mode, fmtDist("connect", samples), policy.NormalP50, policy.SoftTail, policy.Iterations, spikes)

	if ok, reason := evaluateConnectLatency(samples, policy); !ok {
		t.Errorf("ConnectLatency_Serial_%v: %s", mode, reason)
	}

	// Surface the per-dial rendezvous traces (which carry resp_wait, the
	// relay-side hold of the HTTP 101) whenever a tolerated spike
	// occurred but the batch still passed. The DEBUG trace is captured
	// into the sender/listener log buffers on every dial, but the
	// failure dump only fires on t.Failed() — so on a green-but-spiky
	// run the very trace that diagnoses Phase 2 (sender cold dial) vs
	// Phase 3 (relay hold) would otherwise be discarded. We dump only
	// the trace lines (not the full DEBUG buffers) to keep green-run CI
	// output lean. Skip when the batch failed: dumpConnectLatencyLogsOnFail
	// already dumps the full logs then.
	if spikes > 0 && !t.Failed() {
		t.Logf("ConnectLatency_Serial_%v: %d tolerated spike(s) >= NormalP50 %v on a passing run; dumping rendezvous traces for phase analysis",
			mode, spikes, policy.NormalP50)
		dumpTunnelRendezvousTraces(t, tun)
	}
}

// evaluateConnectLatency judges a batch of connect-latency samples
// against a policy's quantile gate. It returns ok=false with a
// human-readable reason naming the first failing arm. See
// ConnectLatencyPolicy for the rationale behind the two-arm gate and
// the deliberate absence of a per-sample / spike-count cap.
//
// The gate is intentionally tolerant of a sparse tail of independent
// spikes (the soft-tail statistic discards the top ceil(10%) of
// samples) but strict about a broad shift (the upper-median moves the
// moment the bulk of samples regress).
func evaluateConnectLatency(samples []time.Duration, p ConnectLatencyPolicy) (ok bool, reason string) {
	if len(samples) == 0 {
		return false, "no connect-latency samples collected"
	}
	sorted := make([]time.Duration, len(samples))
	copy(sorted, samples)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })

	p50 := upperMedian(sorted)
	if p50 >= p.NormalP50 {
		return false, fmt.Sprintf("median connect latency %v >= normal threshold %v (broad regression across %d samples; %s)",
			p50, p.NormalP50, len(sorted), fmtDist("connect", sorted))
	}

	tolerate := tolerableSpikes(len(sorted))
	softTail := softTailSample(sorted, tolerate)
	if softTail >= p.SoftTail {
		return false, fmt.Sprintf("soft-tail connect latency %v >= soft-tail threshold %v (tail degradation beyond the %d tolerated spike(s) across %d samples; %s)",
			softTail, p.SoftTail, tolerate, len(sorted), fmtDist("connect", sorted))
	}
	return true, ""
}

// upperMedian returns the upper of the two central samples for an
// even-length sorted slice, or the central sample for an odd length:
// sorted[len/2]. Choosing the upper central value makes the median
// assertion conservative — a regression that lifts half the samples
// still trips it.
func upperMedian(sorted []time.Duration) time.Duration {
	return sorted[len(sorted)/2]
}

// tolerableSpikes is the number of top samples the soft-tail statistic
// discards: ceil(10% of n). This scales the spike tolerance with the
// iteration count (2 spikes over 20, 1 over 10) so the gate degrades
// gracefully instead of at a fixed cliff.
func tolerableSpikes(n int) int {
	return (n + 9) / 10
}

// softTailSample returns the largest sample after discarding the top
// `tolerate` outliers: sorted[len-1-tolerate], clamped to the slice.
func softTailSample(sorted []time.Duration, tolerate int) time.Duration {
	idx := len(sorted) - 1 - tolerate
	if idx < 0 {
		idx = 0
	}
	return sorted[idx]
}

// dumpConnectLatencyLogsOnFail registers a t.Cleanup that prints the
// captured sender and listener logs for every process in the tunnel,
// but only when the scenario has failed. The ConnectLatency_Serial
// scenarios' dominant failure mode against real Azure Relay is a
// single multi-second connect stall (one iteration spiking from
// ~750 ms steady-state to >3 s, or past the 8 s i/o-timeout deadline
// on the SOCKS5 variant). Without the binary logs that stall is
// undiagnosable from CI output — it could be a client-side dial
// retry / control-channel reconnect or pure Azure tail latency, and
// the test output alone cannot distinguish them. Dumping on failure
// makes the next occurrence actionable.
//
// Registering a single failure-gated cleanup (rather than dumping at
// each failing assertion) keeps successful runs silent and emits at
// most one dump even when multiple iterations breach the threshold;
// Logs() returns the cumulative buffer, so one dump captures the
// whole run. Backends that do not wire log capture (Logs == nil)
// contribute nothing.
func dumpConnectLatencyLogsOnFail(t *testing.T, tun *Tunnel) {
	t.Helper()
	t.Cleanup(func() {
		if t.Failed() {
			dumpTunnelLogs(t, tun)
		}
	})
}

// dumpTunnelLogs prints the captured sender and listener logs for
// every process in the tunnel, skipping nil handles and backends that
// did not wire log capture (Logs == nil). It is unconditional; the
// failure gate lives in dumpConnectLatencyLogsOnFail's cleanup. It
// dumps the FULL buffers and so is reserved for failing runs — passing
// runs use the lean dumpTunnelRendezvousTraces instead.
func dumpTunnelLogs(t *testing.T, tun *Tunnel) {
	t.Helper()
	for i, s := range tun.Senders {
		if s == nil || s.Logs == nil {
			continue
		}
		t.Logf("--- sender[%d] logs ---\n%s", i, s.Logs())
	}
	for i, l := range tun.Listeners {
		if l == nil || l.Logs == nil {
			continue
		}
		t.Logf("--- listener[%d] logs ---\n%s", i, l.Logs())
	}
}

// dumpTunnelRendezvousTraces prints ONLY the rendezvous-trace log lines
// (the dns/tcp/tls/req_written/first_byte/resp_wait phase split emitted
// by internal/relay's dial tracer) from each process in the tunnel. It
// is the lean counterpart to dumpTunnelLogs, used on PASSING-but-spiky
// runs: the Azure sender/listener subprocesses log at DEBUG, so dumping
// their entire buffers on every green run that happened to tolerate a
// spike would bloat CI output (and risk hitting log truncation limits),
// while phase analysis only needs the trace lines. Full-buffer dumps
// stay reserved for failing runs (dumpConnectLatencyLogsOnFail).
func dumpTunnelRendezvousTraces(t *testing.T, tun *Tunnel) {
	t.Helper()
	emitted := false
	dump := func(label string, logs func() string) {
		if traces := filterTraceLines(logs()); traces != "" {
			t.Logf("--- %s rendezvous traces ---\n%s", label, traces)
			emitted = true
		}
	}
	for i, s := range tun.Senders {
		if s == nil || s.Logs == nil {
			continue
		}
		dump(fmt.Sprintf("sender[%d]", i), s.Logs)
	}
	for i, l := range tun.Listeners {
		if l == nil || l.Logs == nil {
			continue
		}
		dump(fmt.Sprintf("listener[%d]", i), l.Logs)
	}
	if !emitted {
		// Distinguish "tracer fired, here are the spans" from "nothing
		// captured" (tracer disabled, buffer rolled, or no log capture
		// wired) so the preceding "dumping…" announcement is never
		// followed by confusing silence.
		t.Logf("(no rendezvous trace lines captured)")
	}
}

// filterTraceLines returns only the lines of logs that carry a
// rendezvous trace (both the "relay rendezvous trace" sender lines and
// the "accept rendezvous trace" listener lines, including their
// "(dial failed)" variants), joined by newlines. Returns "" when none
// match.
func filterTraceLines(logs string) string {
	var b strings.Builder
	for _, line := range strings.Split(logs, "\n") {
		if !strings.Contains(line, "rendezvous trace") {
			continue
		}
		if b.Len() > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(line)
	}
	return b.String()
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
// two ConnectLatency_ColdStart variants. It builds a fresh topology
// and times a cold-sender dial, asserting the elapsed time is below
// b.ColdStartLatencyThreshold(). To absorb isolated Azure Relay
// rendezvous spikes (independent of aztunnel) it uses a best-of-2
// attempt loop: each attempt spawns a brand-new sender process (still
// a cold token) and the scenario passes if either attempt is under
// threshold. On a passing run where an earlier attempt spiked, the
// spiked attempt's rendezvous trace is dumped for phase analysis.
//
// Unlike runSerialConnectLatency, no warm-up dial precedes the
// measurement — measuring the cold-start cost is the whole point.
// The per-dial deadline budget is decoupled from the assertion
// threshold (deadlineBudget = threshold + SpikeCeiling, plus
// connectSlack inside timeOneConnect) so a tolerable spike returns a
// measured over-threshold sample for the retry instead of a fatal i/o
// timeout, while a genuine regression still surfaces as the explicit
// elapsed >= threshold assertion.
func runColdStartConnectLatency(t *testing.T, b Backend, mode SenderMode) {
	t.Helper()
	threshold := b.ColdStartLatencyThreshold()
	// Decouple the per-dial deadline from the assertion threshold, the
	// same way the serial scenario does. A cold-start dial that pays a
	// tolerable rendezvous spike on top of the genuine cold-token cost
	// must return a measured (over-threshold) sample so the best-of-2
	// retry below can run; if the deadline fired at threshold+slack the
	// spike would surface as a fatal i/o timeout and defeat the retry.
	// SpikeCeiling is the backend's model of the largest tolerable
	// rendezvous spike; threshold+SpikeCeiling comfortably exceeds any
	// cold-start+spike combination while still bounding a genuine hang.
	deadlineBudget := threshold + b.ConnectLatencyPolicy().SpikeCeiling
	echo := StartPlainEcho(t)
	opts := SetupOptions{
		NumListeners:   1,
		SenderMode:     mode,
		AllowedTargets: []string{echo.Addr()},
	}
	if mode == ModePortForward {
		opts.Target = echo.Addr()
	}

	payload := []byte{0x42}
	buf := make([]byte, 1)

	// Best-of-2-on-spike: a cold-start dial occasionally pays an
	// isolated Azure Relay rendezvous spike on top of the genuine
	// one-time cold-token cost. Because the spike originates in the
	// relay control plane and is independent of aztunnel, a second
	// attempt with a freshly-started sender (a new sender process =>
	// still a cold token, because the token cache lives in that
	// process) almost never spikes again. Passing if either attempt is
	// under budget turns a per-attempt spike probability p into ~p^2
	// without widening the threshold (which would mask real cold-start
	// regressions). The mock backend is deterministic and never spikes,
	// so attempt 1 always passes there and the retry is dead code in CI
	// for it.
	const attempts = 2
	var elapsed time.Duration
	var spikedTun *Tunnel
	for attempt := 1; attempt <= attempts; attempt++ {
		// A fresh Setup spawns a brand-new sender process, which is
		// what makes the token cold again. Each Setup registers its own
		// t.Cleanup, so the prior attempt's resources are released at
		// test end.
		tun := b.Setup(t, opts)
		dumpConnectLatencyLogsOnFail(t, tun)

		var err error
		elapsed, err = timeOneConnect(tun.SenderAddr, echo.Addr(), mode, payload, buf, deadlineBudget)
		if err != nil {
			t.Fatalf("cold-start dial (attempt %d/%d): %v (elapsed=%v)", attempt, attempts, err, elapsed)
		}
		if elapsed < threshold {
			t.Logf("ConnectLatency_ColdStart_%v: %v (< %v) [attempt %d/%d]", mode, elapsed, threshold, attempt, attempts)
			// If an earlier attempt spiked but this one recovered, the
			// scenario passes and the failure dump never fires — so
			// surface the spiked attempt's rendezvous trace (resp_wait)
			// for phase analysis, mirroring the serial scenario. Dump
			// only the trace lines, not the full DEBUG buffers, to keep
			// passing-run CI output lean.
			if spikedTun != nil {
				t.Logf("ConnectLatency_ColdStart_%v: recovered after a tolerated spike; dumping the spiked attempt's rendezvous trace for phase analysis", mode)
				dumpTunnelRendezvousTraces(t, spikedTun)
			}
			return
		}
		spikedTun = tun
		t.Logf("ConnectLatency_ColdStart_%v: attempt %d/%d spiked at %v (>= %v)", mode, attempt, attempts, elapsed, threshold)
	}
	t.Errorf("cold-start connect latency %v >= threshold %v on all %d attempts", elapsed, threshold, attempts)
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
// deadlineBudget + connectSlack. deadlineBudget is the caller's
// generous per-dial ceiling (the serial scenario passes its
// SpikeCeiling; the cold-start scenario passes its threshold +
// SpikeCeiling); the slack only widens the timeout headroom so a tolerated slow dial is
// measured and returned to the caller rather than surfacing as an i/o
// timeout from the deadline firing.
//
// The deadline is anchored to start (the pre-dial timestamp),
// not to time.Now() after the dial returns. Anchoring to start
// bounds the entire dial+write+read+close iteration to a single
// deadlineBudget + connectSlack window; if dial itself burns most of
// that window the remaining read/write inherit whatever is left.
// Anchoring to post-dial time.Now() would let the whole
// iteration take up to 2 × (deadlineBudget + connectSlack) in the
// worst case (dial uses one window, deadline allows another),
// violating the bound documented on the calling scenario.
//
// Returns the elapsed time even on error so the caller can log
// timing context alongside the failure reason.
func timeOneConnect(senderAddr, target string, mode SenderMode, payload, buf []byte, deadlineBudget time.Duration) (time.Duration, error) {
	const connectSlack = 5 * time.Second
	start := time.Now()
	conn, err := dialSender(senderAddr, target, mode, deadlineBudget+connectSlack)
	if err != nil {
		return time.Since(start), err
	}
	_ = conn.SetDeadline(start.Add(deadlineBudget + connectSlack))
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

// dialSender opens a connection to the sender bind for the given mode.
// For SOCKS5 it performs the CONNECT handshake to the supplied target;
// for port-forward it returns the raw TCP connection.
func dialSender(senderAddr, target string, mode SenderMode, timeout time.Duration) (net.Conn, error) {
	switch mode {
	case ModePortForward:
		return net.DialTimeout("tcp", senderAddr, timeout)
	case ModeSOCKS5:
		return DialSOCKS5(senderAddr, target, timeout)
	default:
		return nil, fmt.Errorf("unknown SenderMode %v", mode)
	}
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
//	  WarmupRequests = 0 — the first request's round-trip is reported
//	  as cold_rtt (includes connection establishment)
//	steady-state scenarios (ConnPrewarmed):
//	  WarmupRequests = 1 — first request is discarded; the rest are
//	  reported as warm_rtt_min/mean/p50/p95/p99/max
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
	// ColdRTT is the cold-path round-trip: open → first response
	// received on a fresh connection. It spans the full
	// request → target → response round-trip and, because the
	// connection is new, includes connection establishment. With a
	// tiny payload against an instant echo target, "first response"
	// coincides with "round-trip complete", so this is an honest RTT.
	ColdRTT time.Duration
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
				t.Logf("conn[%d] warm_n=%d warm_rtt_min=%v warm_rtt_mean=%v warm_rtt_max=%v",
					i, len(r.Warm), wmin, wmean, wmax)
			case len(r.Warm) > 0:
				wmin, wmax, wmean := minMaxMean(r.Warm)
				t.Logf("conn[%d] cold_rtt=%v warm_n=%d warm_rtt_min=%v warm_rtt_mean=%v warm_rtt_max=%v",
					i, r.FirstReq.ColdRTT, len(r.Warm), wmin, wmean, wmax)
			default:
				t.Logf("conn[%d] cold_rtt=%v",
					i, r.FirstReq.ColdRTT)
			}
		}(i)
	}
	wg.Wait()
	wall := time.Since(start)

	var failures int
	var coldRTT, warmAll []time.Duration
	for _, r := range results {
		if r.Err != nil {
			failures++
			continue
		}
		if w.WarmupRequests == 0 {
			coldRTT = append(coldRTT, r.FirstReq.ColdRTT)
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

	if len(coldRTT) > 0 {
		parts = append(parts, fmtDist("cold_rtt", coldRTT))
	}
	if len(warmAll) > 0 {
		parts = append(parts, fmtDist("warm_rtt", warmAll))
	}
	parts = append(parts,
		fmt.Sprintf("wall=%v", wall),
		fmt.Sprintf("budget=%v", budget),
		fmt.Sprintf("cold_n=%d", len(coldRTT)),
		fmt.Sprintf("warm_n=%d", len(warmAll)),
		fmt.Sprintf("num_targets=%d", len(addrs)),
		fmt.Sprintf("success=%d/%d", w.TotalConns-failures, w.TotalConns),
	)
	t.Logf("%s", strings.Join(parts, " "))

	recordPerfMatrixRow(t.Name(), coldRTT, warmAll,
		w.TotalConns-failures, w.TotalConns, wall)

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
			res.FirstReq.ColdRTT = time.Since(dialStart)
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
	writeErr := make(chan error, 1)
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
				// Close conn so the reader unblocks immediately
				// instead of waiting for the read deadline.
				_ = conn.Close()
				writeErr <- fmt.Errorf("rand.Read: %w", err)
				return
			}
			h.Write(chunk[:n])
			if err := writeFull(conn, chunk[:n]); err != nil {
				_ = conn.Close()
				writeErr <- fmt.Errorf("conn.Write: %w", err)
				return
			}
			remaining -= n
		}
		var sum [sha256.Size]byte
		copy(sum[:], h.Sum(nil))
		writeErr <- nil
		sentHash <- sum
	}()

	gotHash := sha256.New()
	n, copyErr := io.CopyN(gotHash, conn, BulkTransferBytes)
	// Always drain writeErr — the writer reports failure here
	// rather than silently returning, so a write-side failure
	// surfaces with its actual error instead of as a misleading
	// reader timeout.
	if err := <-writeErr; err != nil {
		t.Fatalf("write side: %v", err)
	}
	if copyErr != nil {
		t.Fatalf("read after %d bytes: %v", n, copyErr)
	}
	want := <-sentHash
	if !bytes.Equal(want[:], gotHash.Sum(nil)) {
		t.Fatal("SHA-256 mismatch: bulk transfer data corrupted")
	}
	t.Logf("transferred %d MiB intact", BulkTransferBytes>>20)
}
