package e2escenarios

import (
	"os"
	"runtime"
	"testing"
	"time"
)

// leakTolerance is the slack we allow between the before/after samples
// of goroutines and open file descriptors. A single scenario brings
// many transient resources to life (mock-relay http test server,
// metrics scrape pools, subprocess pipe copy goroutines on the e2e
// backend, ephemeral connection-close goroutines from net/http) that
// take a beat to settle once cleanup completes. Anything above this
// tolerance is treated as a leak. Holding ±10 across both backends
// has been validated empirically; tightening it later is fine.
const leakTolerance = 10

// leakSettleDeadline bounds how long AssertNoLeaks polls for the
// goroutine count to come back inside leakTolerance before declaring
// a leak. Cleanups (cancel + WaitGroup drains + subprocess Wait()) are
// synchronous, but Go-runtime bookkeeping for closed connections /
// timer goroutines can take a few ms to reflect in NumGoroutine.
const leakSettleDeadline = 2 * time.Second

// AssertNoLeaks samples the test process's goroutine count and (on
// Linux) open FD count before the scenario runs, then registers a
// t.Cleanup that re-samples after every other cleanup has finished
// and fails the test if either delta exceeds leakTolerance.
//
// Call AssertNoLeaks at the very top of a scenario, BEFORE invoking
// Backend.Setup. Because t.Cleanup runs in LIFO order, the leak
// cleanup we register here runs LAST, after the backend's cleanup
// chain has cancelled contexts, drained waitgroups, and reaped
// subprocesses — so by the time we re-sample, all the scenario's
// resources should have been released.
//
// FD checks rely on /proc/self/fd, which only exists on Linux. On
// other operating systems we skip the FD check and only assert on
// goroutines; the mock backend runs on every platform but the
// goroutine half is still a useful signal.
func AssertNoLeaks(t *testing.T) {
	t.Helper()

	// Two GC cycles before sampling let the runtime sweep any
	// "almost dead" goroutines that finished in the moments leading
	// up to the scenario starting — the first GC marks them, the
	// second collects. Without this, the baseline can be inflated
	// by a finalizer goroutine that exits inside the scenario,
	// producing a *negative* delta that we'd nonetheless treat as
	// noise. Two cycles is the standard cure.
	runtime.GC()
	runtime.GC()

	beforeGo := runtime.NumGoroutine()
	beforeFD, fdSupported := readFDCount()

	t.Cleanup(func() {
		// Poll for the goroutine count to drop back inside tolerance.
		// We sample every 50 ms; the loop exits as soon as the deltas
		// are within bounds, or after the settle deadline, whichever
		// is sooner. Don't sleep before the first sample — fast
		// backends (mock) frequently settle within microseconds.
		deadline := time.Now().Add(leakSettleDeadline)
		var afterGo int
		var afterFD int
		for {
			runtime.GC()
			afterGo = runtime.NumGoroutine()
			afterFD, _ = readFDCount()
			goLeak := afterGo-beforeGo > leakTolerance
			fdLeak := fdSupported && afterFD-beforeFD > leakTolerance
			if !goLeak && !fdLeak {
				return
			}
			if time.Now().After(deadline) {
				break
			}
			time.Sleep(50 * time.Millisecond)
		}

		if afterGo-beforeGo > leakTolerance {
			t.Errorf("goroutine leak: before=%d after=%d delta=%d (tolerance %d)",
				beforeGo, afterGo, afterGo-beforeGo, leakTolerance)
		}
		if fdSupported && afterFD-beforeFD > leakTolerance {
			t.Errorf("file-descriptor leak: before=%d after=%d delta=%d (tolerance %d)",
				beforeFD, afterFD, afterFD-beforeFD, leakTolerance)
		}
	})
}

// readFDCount returns the count of entries in /proc/self/fd. The
// second return is false if the file is unavailable (non-Linux); in
// that case the FD check is skipped without failing the test.
func readFDCount() (int, bool) {
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		return 0, false
	}
	return len(entries), true
}
