package scenarios

import (
	"testing"
	"time"
)

// ms is a brevity helper for building duration sample sets.
func ms(v int) time.Duration { return time.Duration(v) * time.Millisecond }

// repeat returns a slice of n samples all equal to d.
func repeat(d time.Duration, n int) []time.Duration {
	out := make([]time.Duration, n)
	for i := range out {
		out[i] = d
	}
	return out
}

// azurePolicy mirrors the tolerant production gate so the unit tests
// exercise the same thresholds CI uses. Kept local to avoid importing
// the azure backend (which carries a build tag and Azure deps).
var azurePolicy = ConnectLatencyPolicy{
	Iterations:   20,
	NormalP50:    1500 * time.Millisecond,
	SoftTail:     3 * time.Second,
	SpikeCeiling: 6 * time.Second,
}

func TestEvaluateConnectLatency(t *testing.T) {
	// A 20-sample steady-state batch (~400 ms) used as the base for
	// spike-injection cases.
	steady20 := repeat(ms(400), 20)

	withSpikes := func(base []time.Duration, spikes ...time.Duration) []time.Duration {
		out := append([]time.Duration(nil), base...)
		copy(out, spikes) // overwrite the first len(spikes) samples
		return out
	}

	tests := []struct {
		name    string
		samples []time.Duration
		policy  ConnectLatencyPolicy
		wantOK  bool
	}{
		{
			name:    "empty input fails",
			samples: nil,
			policy:  azurePolicy,
			wantOK:  false,
		},
		{
			name:    "all fast passes",
			samples: steady20,
			policy:  azurePolicy,
			wantOK:  true,
		},
		{
			name:    "one spike tolerated",
			samples: withSpikes(steady20, ms(5000)),
			policy:  azurePolicy,
			wantOK:  true,
		},
		{
			name:    "two spikes tolerated (ceil 10% of 20)",
			samples: withSpikes(steady20, ms(5000), ms(6000)),
			policy:  azurePolicy,
			wantOK:  true,
		},
		{
			name:    "three spikes trip the soft-tail",
			samples: withSpikes(steady20, ms(5000), ms(5000), ms(5000)),
			policy:  azurePolicy,
			wantOK:  false,
		},
		{
			name:    "uniform 2s regression trips the median",
			samples: repeat(ms(2000), 20),
			policy:  azurePolicy,
			wantOK:  false,
		},
		{
			name:    "ten-ten split trips the median (upper-median is slow)",
			samples: append(repeat(ms(400), 10), repeat(ms(2000), 10)...),
			policy:  azurePolicy,
			wantOK:  false,
		},
		{
			name:    "broad mid-tail rise below 3s old ceiling still trips median",
			samples: repeat(ms(1800), 20),
			policy:  azurePolicy,
			wantOK:  false,
		},
		{
			name:    "median exactly at threshold fails (>= is failure)",
			samples: repeat(ms(1500), 20),
			policy:  azurePolicy,
			wantOK:  false,
		},
		{
			name:    "soft-tail exactly at threshold fails",
			samples: withSpikes(steady20, ms(3000), ms(3000), ms(3000)),
			policy:  azurePolicy,
			wantOK:  false,
		},
		{
			name:    "single sample below median threshold passes",
			samples: repeat(ms(400), 1),
			policy:  azurePolicy,
			wantOK:  true,
		},
		{
			name:    "single slow sample fails (upper-median is that sample)",
			samples: repeat(ms(5000), 1),
			policy:  azurePolicy,
			wantOK:  false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ok, reason := evaluateConnectLatency(tc.samples, tc.policy)
			if ok != tc.wantOK {
				t.Fatalf("evaluateConnectLatency ok=%v want %v (reason=%q)", ok, tc.wantOK, reason)
			}
			if !ok && reason == "" {
				t.Errorf("failing evaluation returned empty reason")
			}
			if ok && reason != "" {
				t.Errorf("passing evaluation returned non-empty reason %q", reason)
			}
		})
	}
}

func TestUpperMedian(t *testing.T) {
	// Even length: upper of the two central samples.
	if got := upperMedian([]time.Duration{ms(1), ms(2), ms(3), ms(4)}); got != ms(3) {
		t.Errorf("even-length upperMedian = %v, want %v", got, ms(3))
	}
	// Odd length: the central sample.
	if got := upperMedian([]time.Duration{ms(1), ms(2), ms(3)}); got != ms(2) {
		t.Errorf("odd-length upperMedian = %v, want %v", got, ms(2))
	}
}

func TestTolerableSpikes(t *testing.T) {
	cases := map[int]int{1: 1, 9: 1, 10: 1, 11: 2, 20: 2, 21: 3, 30: 3}
	for n, want := range cases {
		if got := tolerableSpikes(n); got != want {
			t.Errorf("tolerableSpikes(%d) = %d, want %d", n, got, want)
		}
	}
}

func TestSoftTailSample(t *testing.T) {
	sorted := []time.Duration{ms(1), ms(2), ms(3), ms(4), ms(5)}
	// tolerate 0 => largest.
	if got := softTailSample(sorted, 0); got != ms(5) {
		t.Errorf("softTailSample tolerate=0 = %v, want %v", got, ms(5))
	}
	// tolerate 2 => discard top 2, largest remaining is sorted[2].
	if got := softTailSample(sorted, 2); got != ms(3) {
		t.Errorf("softTailSample tolerate=2 = %v, want %v", got, ms(3))
	}
	// tolerate >= len => clamps to the smallest sample, no panic.
	if got := softTailSample(sorted, 10); got != ms(1) {
		t.Errorf("softTailSample tolerate=10 = %v, want %v", got, ms(1))
	}
}
