package scenarios

import (
	"fmt"
	"net"
	"sync"
	"testing"
	"time"
)

// DuplexShape configures a continuous, bidirectional request/response
// workload driven by probeFlow against ServerProbe targets. It is the
// perf-family peer of WorkloadShape (short-conn rtt) and StreamShape
// (server-paced trickle). Each flow holds one TCP connection open for
// Duration and runs paced request/response exchanges, recording per-leg
// timings in a bounded sample ring.
//
// The duplex shape exists so a test can answer "what round-trip and
// per-leg latency should I expect under sustained bidirectional load",
// across the same backend/auth/delay axes as the other families.
type DuplexShape struct {
	// Flows is the number of concurrent probeFlows held open after the
	// release barrier. Each flow runs on its own TCP connection.
	Flows int

	// NumTargets is the count of distinct ServerProbe targets the flows
	// fan out across (round-robin). SOCKS5 fan-out requires >1; the
	// PortForward path is single-target by design.
	NumTargets int

	// Mode is the sender mode. ModeSOCKS5 supports NumTargets > 1;
	// ModePortForward implies NumTargets == 1 (the single sender bind
	// terminates at one target).
	Mode SenderMode

	// Interval is the per-flow probe interval; passed to ProbeConfig.
	Interval time.Duration

	// MaxOutstanding bounds in-flight requests per flow. 1 gives clean
	// per-leg latency (no self-induced queueing in requestLeg). Values
	// > 1 measure pipelined throughput at the cost of per-leg purity.
	MaxOutstanding int

	// BodySize is the request and response body pattern bytes per
	// exchange, excluding the 28-byte probe header.
	BodySize int

	// ProcessingDelay is the server-side think time, mirrored from the
	// other shapes for parity.
	ProcessingDelay time.Duration

	// Duration is the steady-state window after the release barrier.
	// Flows are stopped at Duration's end and metrics aggregated from
	// the samples retained in each flow's ring buffer.
	Duration time.Duration

	// SampleSize is the per-flow ring-buffer size, passed to ProbeConfig.
	// Larger values give percentiles a longer steady-state tail at the
	// cost of memory. Set such that SampleSize * Flows is the maximum
	// resident sample population.
	SampleSize int

	// RepeatRounds runs the whole barrier-and-run cycle this many times
	// (default 1).
	RepeatRounds int
}

func (s DuplexShape) probeConfig() ProbeConfig {
	return ProbeConfig{
		Interval:       s.Interval,
		ReqSize:        s.BodySize,
		RespSize:       s.BodySize,
		MaxOutstanding: s.MaxOutstanding,
		SampleSize:     s.SampleSize,
	}
}

func (s DuplexShape) serverBehavior() ServerBehavior {
	return ServerBehavior{
		Mode:            ServerProbe,
		RespSize:        s.BodySize,
		ProcessingDelay: s.ProcessingDelay,
	}
}

// validate checks DuplexShape invariants after defaults are filled.
func (s DuplexShape) validate() error {
	if s.Flows < 1 {
		return fmt.Errorf("DuplexShape.Flows must be >= 1, got %d", s.Flows)
	}
	if s.NumTargets < 1 {
		return fmt.Errorf("DuplexShape.NumTargets must be >= 1, got %d", s.NumTargets)
	}
	if s.BodySize < 1 {
		return fmt.Errorf("DuplexShape.BodySize must be >= 1, got %d", s.BodySize)
	}
	if s.Duration <= 0 {
		return fmt.Errorf("DuplexShape.Duration must be > 0, got %v", s.Duration)
	}
	if s.MaxOutstanding < 1 {
		return fmt.Errorf("DuplexShape.MaxOutstanding must be >= 1, got %d", s.MaxOutstanding)
	}
	if s.Mode != ModeSOCKS5 && s.Mode != ModePortForward {
		return fmt.Errorf("DuplexShape.Mode must be ModeSOCKS5 or ModePortForward, got %v", s.Mode)
	}
	if s.Mode == ModePortForward && s.NumTargets != 1 {
		return fmt.Errorf("DuplexShape.Mode=PortForward requires NumTargets==1, got %d", s.NumTargets)
	}
	return nil
}

func runDuplexWorkload(t *testing.T, b Backend, s DuplexShape) {
	t.Helper()
	AssertNoLeaks(t)
	if s.NumTargets <= 0 {
		s.NumTargets = 1
	}
	if s.MaxOutstanding <= 0 {
		s.MaxOutstanding = 1
	}
	if s.RepeatRounds <= 0 {
		s.RepeatRounds = 1
	}
	if err := s.validate(); err != nil {
		t.Fatalf("invalid DuplexShape: %v", err)
	}

	// One ServerProbe target per fan-out endpoint. SOCKS5 fans out
	// across them; PortForward terminates at NumTargets==1.
	addrs := make([]string, s.NumTargets)
	behavior := s.serverBehavior()
	srvs := make([]*WorkloadServer, s.NumTargets)
	for i := range addrs {
		srvs[i] = StartWorkloadServer(t, behavior)
		addrs[i] = srvs[i].Addr()
	}

	setup := SetupOptions{
		NumListeners:   1,
		SenderMode:     s.Mode,
		AllowedTargets: addrs,
	}
	if s.Mode == ModePortForward {
		setup.Target = addrs[0]
	}
	tun := b.Setup(t, setup)

	for r := 0; r < s.RepeatRounds; r++ {
		if s.RepeatRounds > 1 {
			t.Logf("--- duplex round %d/%d ---", r+1, s.RepeatRounds)
		}
		runDuplexRound(t, tun, addrs, srvs, b.ConnectLatencyThreshold(), s)
	}
}

// flowResult is one probeFlow's outcome at the end of a duplex round.
type flowResult struct {
	samples []probeExchange
	summary probeSummary
	acked   int64
	err     error
}

func runDuplexRound(t *testing.T, tun *Tunnel, addrs []string, srvs []*WorkloadServer, threshold time.Duration, s DuplexShape) {
	budget := duplexBudget(threshold, s)
	start := time.Now()
	roundDeadline := start.Add(budget)

	flows := make([]*probeFlow, s.Flows)
	conns := make([]net.Conn, s.Flows)
	results := make([]flowResult, s.Flows)
	cfg := s.probeConfig()

	// Phase 1: each goroutine dials and constructs (but does not start)
	// a probeFlow. We don't use startProbeFlow because that would begin
	// traffic before the barrier — the point of the barrier is that all
	// flows run under simultaneous load for fairness comparisons.
	var ready sync.WaitGroup
	var done sync.WaitGroup
	release := make(chan struct{})
	var releaseTime time.Time
	ready.Add(s.Flows)
	done.Add(s.Flows)

	for i := 0; i < s.Flows; i++ {
		go func(i int) {
			defer done.Done()
			target := addrs[i%len(addrs)]

			dialTimeout := time.Until(roundDeadline)
			if dialTimeout > 60*time.Second {
				dialTimeout = 60 * time.Second
			}
			if dialTimeout <= 0 {
				results[i] = flowResult{err: fmt.Errorf("flow[%d]: round budget exhausted before dial", i)}
				ready.Done()
				return
			}

			var (
				c   net.Conn
				err error
			)
			switch s.Mode {
			case ModeSOCKS5:
				c, err = DialSOCKS5(tun.SenderAddr, target, dialTimeout)
			case ModePortForward:
				c, err = net.DialTimeout("tcp", tun.SenderAddr, dialTimeout)
			}
			if err != nil {
				results[i] = flowResult{err: fmt.Errorf("flow[%d] dial: %w", i, err)}
				ready.Done()
				return
			}
			conns[i] = c

			f := newProbeFlow(c, cfg)
			flows[i] = f

			ready.Done()
			<-release

			f.Start()
			// Run for Duration. A read-side break causes the flow's
			// reader to call signalDone (close stopCh + conn), which
			// wakes the select below so a broken flow reports
			// promptly instead of sitting until the full Duration.
			// Stop afterwards is a no-op for an already-broken flow.
			deadline := releaseTime.Add(s.Duration)
			select {
			case <-time.After(time.Until(deadline)):
			case <-f.stopCh:
			}
			f.Stop()

			samples := f.Samples()
			summary := f.Summary()
			res := flowResult{
				samples: samples,
				summary: summary,
				acked:   summary.acked,
			}
			minAcks := minProgressFloor(s)
			switch {
			case summary.broken:
				res.err = fmt.Errorf("flow[%d] broken: %v", i, summary.firstErr)
			case summary.writeErr != nil:
				res.err = fmt.Errorf("flow[%d] write error: %v", i, summary.writeErr)
			case summary.acked+1 < int64(minAcks):
				res.err = fmt.Errorf("flow[%d] insufficient progress: acked %d, want >= %d", i, summary.acked+1, minAcks)
			}
			results[i] = res
		}(i)
	}

	ready.Wait()
	releaseTime = time.Now()
	close(release)
	done.Wait()
	wall := time.Since(start)

	// Best-effort conn cleanup (Stop already closed them, but if a flow
	// failed before construction the conn may still be live).
	for i := range conns {
		if conns[i] != nil {
			_ = conns[i].Close() //nolint:errcheck
		}
	}
	// Free server-side records held for any nonces the flows allocated.
	for i, f := range flows {
		if f == nil {
			continue
		}
		// The flow's target was addrs[i%len(addrs)], which is srvs[i%len(addrs)].
		_, _ = srvs[i%len(srvs)].ConsumeProbeRecord(f.nonce)
	}

	m := aggregateDuplex(results, s, wall)
	logDuplexSummary(t, s, addrs, m)
	if m.successN < s.Flows {
		for i, r := range results {
			if r.err != nil {
				t.Logf("duplex flow[%d] failed: %v (samples=%d acked=%d)", i, r.err, len(r.samples), r.acked)
			}
		}
		t.Errorf("%d/%d duplex flows failed", s.Flows-m.successN, s.Flows)
	}
	recordDuplexMatrixRow(t.Name(), m)

	const sanityEpsilon = 5 * time.Second
	if wall > budget+sanityEpsilon {
		t.Fatalf("sanity: duplex round wall=%v exceeded budget=%v + epsilon=%v (shape: %+v)", wall, budget, sanityEpsilon, s)
	}
}

// duplexMetrics is the aggregate of one duplex round.
type duplexMetrics struct {
	rttP50, rttP95         time.Duration
	reqLegP50, reqLegP95   time.Duration
	respLegP50, respLegP95 time.Duration
	thinkP50, thinkP95     time.Duration
	acksPerSec             int64
	// bytesPerSecPerDir is the per-direction application throughput.
	// DuplexShape uses a symmetric BodySize, so each leg carries the
	// same number of bytes; the wire load on the bridge is therefore
	// 2 * bytesPerSecPerDir.
	bytesPerSecPerDir int64
	// ackSpread is max-min acks across successful flows. Broken flows
	// are excluded (their ack count is meaningless after the break),
	// so this measures fairness among the surviving population.
	ackSpread int64
	sampleN   int // total samples pooled across flows
	flowN     int
	successN  int
	wall      time.Duration
}

func aggregateDuplex(results []flowResult, s DuplexShape, wall time.Duration) duplexMetrics {
	m := duplexMetrics{flowN: s.Flows, wall: wall}
	var (
		rtt, req, resp, think []time.Duration
		totalAcks             int64
		minAcks               int64 = -1
		maxAcks               int64
	)
	for _, r := range results {
		if r.err != nil {
			continue
		}
		m.successN++
		for _, ex := range r.samples {
			rtt = append(rtt, ex.rtt)
			req = append(req, ex.requestLeg)
			resp = append(resp, ex.responseLeg)
			think = append(think, ex.serverThink)
		}
		// acked is the highest seq acked (-1 before first); count is +1.
		count := r.acked + 1
		if count < 0 {
			count = 0
		}
		totalAcks += count
		if minAcks < 0 || count < minAcks {
			minAcks = count
		}
		if count > maxAcks {
			maxAcks = count
		}
	}
	m.sampleN = len(rtt)
	m.rttP50 = repr(rtt, 0.50)
	m.rttP95 = repr(rtt, 0.95)
	m.reqLegP50 = repr(req, 0.50)
	m.reqLegP95 = repr(req, 0.95)
	m.respLegP50 = repr(resp, 0.50)
	m.respLegP95 = repr(resp, 0.95)
	m.thinkP50 = repr(think, 0.50)
	m.thinkP95 = repr(think, 0.95)

	// Throughput uses the steady-state Duration rather than wall to
	// exclude the dial/barrier phase. Each ack carries BodySize request
	// + BodySize response application bytes (probe header excluded).
	if s.Duration > 0 && m.successN > 0 {
		secs := s.Duration.Seconds()
		m.acksPerSec = int64(float64(totalAcks) / secs)
		m.bytesPerSecPerDir = int64(float64(totalAcks*int64(s.BodySize)) / secs)
	}
	if minAcks >= 0 && maxAcks >= minAcks {
		m.ackSpread = maxAcks - minAcks
	}
	return m
}

// minProgressFloor returns the minimum ack count a flow must reach to
// be counted as successful. Without a floor, a flow that stalls after
// its very first ack reports as a success — masking a partial break in
// the duplex matrix.
//
// The floor is a small absolute number (5) rather than scaled to
// Duration/Interval, because the effective ack rate is bounded by RTT
// (one outstanding probe at a time waits for the round-trip), which
// the shape cannot know a priori. A floor that scales with the
// theoretical max would unfairly fail RTT-bound healthy flows on
// slower backends; 5 acks is well above a stalled flow (1 ack or
// fewer) but well below any healthy short-duration run.
func minProgressFloor(s DuplexShape) int {
	_ = s
	return 5
}

func logDuplexSummary(t *testing.T, s DuplexShape, addrs []string, m duplexMetrics) {
	t.Logf("duplex-summary scenario=%s rtt_p50=%v rtt_p95=%v req_leg_p50=%v req_leg_p95=%v resp_leg_p50=%v resp_leg_p95=%v think_p50=%v think_p95=%v acks_per_sec=%d bytes_per_sec_per_dir=%d ack_spread=%d sample_n=%d flows=%d num_targets=%d success=%d/%d wall=%v",
		t.Name(), m.rttP50, m.rttP95, m.reqLegP50, m.reqLegP95, m.respLegP50, m.respLegP95, m.thinkP50, m.thinkP95, m.acksPerSec, m.bytesPerSecPerDir, m.ackSpread, m.sampleN, s.Flows, len(addrs), m.successN, s.Flows, m.wall)
}

// duplexBudget bounds a duplex round's wall time. It covers concurrent
// cold connect (two connect thresholds — all flows dial at once, not
// multiplied by Flows), the active Duration with a generous safety
// factor for tunnel and scheduler jitter, plus fixed slack and a 60s
// floor.
func duplexBudget(threshold time.Duration, s DuplexShape) time.Duration {
	connect := 2 * threshold
	active := s.Duration + s.Duration/2 // 1.5x for jitter
	budget := connect + active + 10*time.Second
	if budget < 60*time.Second {
		budget = 60 * time.Second
	}
	return budget
}
