package scenarios

import (
	"testing"
	"time"
)

func TestMinProgressFloor(t *testing.T) {
	cases := []struct {
		name     string
		duration time.Duration
		interval time.Duration
		want     int
	}{
		// Floor is a small absolute number regardless of shape, because
		// the achievable ack rate is RTT-bounded and unknown a priori.
		{"zero interval", 5 * time.Second, 0, 5},
		{"zero duration", 0, 25 * time.Millisecond, 5},
		{"short coarse", 100 * time.Millisecond, 100 * time.Millisecond, 5},
		{"medium", 5 * time.Second, 25 * time.Millisecond, 5},
		{"large", 60 * time.Second, 10 * time.Millisecond, 5},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			got := minProgressFloor(DuplexShape{Duration: c.duration, Interval: c.interval})
			if got != c.want {
				t.Errorf("minProgressFloor(%v, %v) = %d, want %d", c.duration, c.interval, got, c.want)
			}
		})
	}
}

func TestDuplexShape_Validate(t *testing.T) {
	good := DuplexShape{
		Flows:          2,
		NumTargets:     2,
		Mode:           ModeSOCKS5,
		Interval:       10 * time.Millisecond,
		MaxOutstanding: 1,
		BodySize:       32,
		Duration:       1 * time.Second,
		SampleSize:     16,
	}
	if err := good.validate(); err != nil {
		t.Fatalf("good shape: %v", err)
	}

	type tc struct {
		name string
		fn   func(s *DuplexShape)
	}
	bad := []tc{
		{"Flows<1", func(s *DuplexShape) { s.Flows = 0 }},
		{"NumTargets<1", func(s *DuplexShape) { s.NumTargets = 0 }},
		{"BodySize<1", func(s *DuplexShape) { s.BodySize = 0 }},
		{"Duration<=0", func(s *DuplexShape) { s.Duration = 0 }},
		{"MaxOutstanding<1", func(s *DuplexShape) { s.MaxOutstanding = 0 }},
		{"BadMode", func(s *DuplexShape) { s.Mode = ModeConnect }},
		{"PortForwardWithFanOut", func(s *DuplexShape) { s.Mode = ModePortForward; s.NumTargets = 2 }},
	}
	for _, c := range bad {
		c := c
		t.Run(c.name, func(t *testing.T) {
			s := good
			c.fn(&s)
			if err := s.validate(); err == nil {
				t.Errorf("expected validation error")
			}
		})
	}
}

func TestAggregateDuplex_PerLegPercentilesAndThroughput(t *testing.T) {
	// Construct a fixed flow result set with deterministic per-leg timings
	// so the aggregator's percentile and throughput math is verifiable.
	mkSamples := func(rtts []time.Duration) []probeExchange {
		out := make([]probeExchange, len(rtts))
		for i, r := range rtts {
			out[i] = probeExchange{
				seq:         uint32(i),
				rtt:         r,
				requestLeg:  r / 2,
				responseLeg: r/2 - time.Microsecond,
				serverThink: time.Microsecond,
			}
		}
		return out
	}
	results := []flowResult{
		{samples: mkSamples([]time.Duration{10 * time.Millisecond, 20 * time.Millisecond, 30 * time.Millisecond}), acked: 9},  // 10 acks
		{samples: mkSamples([]time.Duration{15 * time.Millisecond, 25 * time.Millisecond, 35 * time.Millisecond}), acked: 19}, // 20 acks
		{err: errTestFlow},
	}
	s := DuplexShape{Flows: 3, BodySize: 100, Duration: 1 * time.Second}
	m := aggregateDuplex(results, s, 2*time.Second)

	if m.flowN != 3 || m.successN != 2 {
		t.Errorf("flowN/successN = %d/%d, want 3/2", m.flowN, m.successN)
	}
	if m.sampleN != 6 {
		t.Errorf("sampleN=%d, want 6", m.sampleN)
	}
	// Pooled rtt = [10,20,30,15,25,35] ms; sorted -> 10,15,20,25,30,35.
	// p50 with nearest-rank floor on (n-1)*p ≈ position 2 = 20ms.
	if m.rttP50 != 20*time.Millisecond {
		t.Errorf("rttP50=%v, want 20ms", m.rttP50)
	}
	// p95 ≈ position 4 or 5 (depending on percentile def) -> 30 or 35ms.
	if m.rttP95 < 30*time.Millisecond || m.rttP95 > 35*time.Millisecond {
		t.Errorf("rttP95=%v, want 30..35ms", m.rttP95)
	}
	// Throughput: 10 + 20 = 30 acks across Duration (1s) = 30 acks/s.
	if m.acksPerSec != 30 {
		t.Errorf("acksPerSec=%d, want 30", m.acksPerSec)
	}
	// Bytes/sec per direction = acks * BodySize = 30 * 100 = 3000.
	if m.bytesPerSecPerDir != 3000 {
		t.Errorf("bytesPerSecPerDir=%d, want 3000", m.bytesPerSecPerDir)
	}
	// ack spread = 20 - 10 = 10.
	if m.ackSpread != 10 {
		t.Errorf("ackSpread=%d, want 10", m.ackSpread)
	}
}

func TestAggregateDuplex_AllFailed_ZeroSafe(t *testing.T) {
	results := []flowResult{{err: errTestFlow}, {err: errTestFlow}}
	s := DuplexShape{Flows: 2, BodySize: 100, Duration: 1 * time.Second}
	m := aggregateDuplex(results, s, 2*time.Second)
	if m.successN != 0 {
		t.Errorf("successN=%d, want 0", m.successN)
	}
	if m.sampleN != 0 || m.acksPerSec != 0 || m.ackSpread != 0 {
		t.Errorf("expected all metrics zero on all-failed, got %+v", m)
	}
}

func TestRecordDuplexMatrixRow_RoundTrips(t *testing.T) {
	perfMatrixSink.drain() // start clean
	t.Cleanup(func() { perfMatrixSink.drain() })

	m := duplexMetrics{
		rttP50: 5 * time.Millisecond, rttP95: 9 * time.Millisecond,
		reqLegP50: 2 * time.Millisecond, reqLegP95: 4 * time.Millisecond,
		respLegP50: 2 * time.Millisecond, respLegP95: 4 * time.Millisecond,
		thinkP50: 1 * time.Microsecond, thinkP95: 3 * time.Microsecond,
		acksPerSec: 100, bytesPerSecPerDir: 5000,
		ackSpread: 2, sampleN: 50, flowN: 4, successN: 4,
		wall: 2 * time.Second,
	}
	recordDuplexMatrixRow("TestE2E_Mock/sas/Duplex_Probe_SOCKS5_FanOut", m)
	rows := perfMatrixSink.drain()
	if len(rows) != 1 {
		t.Fatalf("rows=%d, want 1", len(rows))
	}
	r := rows[0]
	if r.family != duplexFamily {
		t.Errorf("family=%q, want %q", r.family, duplexFamily)
	}
	if r.rttP50 != 5*time.Millisecond || r.rttP95 != 9*time.Millisecond {
		t.Errorf("rtt p50/p95 mismatch: %v/%v", r.rttP50, r.rttP95)
	}
	if r.acksPerSec != 100 || r.sampleN != 50 {
		t.Errorf("acks/sample mismatch: %d/%d", r.acksPerSec, r.sampleN)
	}

	rec := r.record()
	if rec.MetricFamily != duplexFamily {
		t.Errorf("MetricFamily=%q want %q", rec.MetricFamily, duplexFamily)
	}
	if rec.RTTP50Ns == nil || *rec.RTTP50Ns != (5*time.Millisecond).Nanoseconds() {
		t.Errorf("RTTP50Ns=%v want 5ms", rec.RTTP50Ns)
	}
	if rec.AcksPerSec == nil || *rec.AcksPerSec != 100 {
		t.Errorf("AcksPerSec=%v want 100", rec.AcksPerSec)
	}
	if rec.SampleN != 50 || rec.FlowN != 4 {
		t.Errorf("SampleN/FlowN=%d/%d, want 50/4", rec.SampleN, rec.FlowN)
	}
}

func TestRenderDuplexTable_RendersDuplexOnly(t *testing.T) {
	rows := []perfMatrixRow{
		{family: "", axis: "sas", scenario: "Echo", mode: "SOCKS5", coldN: 1, warmN: 1, successN: 1, attemptN: 1, wall: time.Second},
		{family: duplexFamily, axis: "sas", scenario: "Duplex_Probe_FanOut", mode: "SOCKS5", rttP50: 5 * time.Millisecond, rttP95: 9 * time.Millisecond, sampleN: 50, acksPerSec: 100, successN: 4, attemptN: 4, wall: 2 * time.Second},
	}
	out := renderDuplexTable(rows)
	if out == "" {
		t.Fatalf("renderDuplexTable returned empty for input with one duplex row")
	}
	for _, want := range []string{"PERF MATRIX (duplex", "Duplex_Probe_FanOut", "5ms", "100"} {
		if !contains(out, want) {
			t.Errorf("rendered table missing %q\n%s", want, out)
		}
	}
	// Must NOT include the rtt-only row.
	if contains(out, "Echo") {
		t.Errorf("duplex table erroneously included rtt-only row 'Echo':\n%s", out)
	}
}

// errTestFlow is a sentinel for tests that synthesize failed flowResults.
var errTestFlow = &testFlowErr{}

type testFlowErr struct{}

func (*testFlowErr) Error() string { return "test: synthetic flow error" }

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
