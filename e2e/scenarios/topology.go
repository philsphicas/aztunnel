package scenarios

import (
	"context"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// RunTopologyScenarios runs the multi-listener / multi-sender /
// single-target e2e scenarios against b. Each scenario fires at a
// curated set of
// (N, M) matrix cells; cells that don't meet a scenario's minimum
// requirements are simply not emitted (no t.Skip noise in output).
//
// Cells are conservative — we cover the four corners of the {1,2}×{1,2}
// matrix only where they add information. Scaling K up at high cells
// burns mock-CI budget without adding new failure modes.
func RunTopologyScenarios(t *testing.T, b Backend) {
	t.Helper()
	runScenarioCases(t, b, topologyCases())
}

// topologyCases is the metadata-only registry of topology scenarios.
//
// Topology scenarios are parameterised by an inner N×M cell matrix
// (number of senders × number of listeners). Each registry entry's
// run closure iterates that scenario's cells and emits per-cell
// sub-tests named `N{n}M{m}`. The cell matrices are conservative —
// we cover only the four corners of the {1,2}×{1,2} matrix where
// they add information.
//
// Capability gates (e.g. AddListener nil) are NOT scope decisions —
// they're handled inside the scenario body so backends missing a
// capability are simply not exercised (no t.Skip noise).
func topologyCases() []scenarioCase {
	type cell struct{ n, m int }
	all := []cell{{1, 1}, {1, 2}, {2, 1}, {2, 2}}
	withMultiListener := []cell{{1, 2}, {2, 2}}
	withMultiSender := []cell{{2, 1}, {2, 2}}

	matrixRunner := func(cells []cell, fn func(*testing.T, Backend, int, int)) func(*testing.T, Backend) {
		return func(t *testing.T, b Backend) {
			t.Helper()
			for _, c := range cells {
				c := c
				name := fmt.Sprintf("N%dM%d", c.n, c.m)
				t.Run(name, func(t *testing.T) {
					fn(t, b, c.n, c.m)
				})
			}
		}
	}

	return []scenarioCase{
		{name: "NSenderMListener_Echo", scope: AnyBackend, run: matrixRunner(all, scenarioNMEcho)},
		{name: "Distribution_PerListener", scope: AnyBackend, run: matrixRunner(withMultiListener, scenarioDistributionPerListener)},
		{name: "Distribution_PerSender", scope: AnyBackend, run: matrixRunner(withMultiSender, scenarioDistributionPerSender)},
		{name: "HotDropListener", scope: AnyBackend, run: matrixRunner(withMultiListener, scenarioHotDropListener)},
		// Hot-add starts at M=1 and grows; running it across multiple
		// (N) cells doesn't add a new failure mode, so it gets just
		// one representative cell.
		{name: "HotAddListener", scope: AnyBackend, run: matrixRunner([]cell{{1, 1}}, scenarioHotAddListener)},
		// Back-pressure only needs M=2 with MaxConnections=2 to
		// exercise the slot logic; a second sender doesn't change
		// the semaphore behaviour.
		{name: "MaxConn_BackPressure", scope: AnyBackend, run: matrixRunner([]cell{{1, 2}}, scenarioMaxConnBackPressure)},
		// Single-cell scenarios run directly with no N{n}M{m}
		// sub-test wrapping.
		{name: "ListenerRestart_Recovers", scope: AnyBackend, run: ScenarioListenerRestart_Recovers},
	}
}

// scenarioNMEcho drives K parallel framed probe flows distributed
// round-robin across SenderAddrs, all targeting a single ServerProbe
// WorkloadServer. Each probeOnce validates request+response framing,
// nonce, response size, and payload pattern; the byte-transparency
// property is exercised separately by BulkTransfer. This is the
// workhorse functional check for the topology slice; everything
// downstream assumes it holds.
func scenarioNMEcho(t *testing.T, b Backend, n, m int) {
	t.Helper()
	AssertNoLeaks(t)
	srv := StartWorkloadServer(t, ServerBehavior{Mode: ServerProbe, RespSize: defaultProbeBody})
	tun := b.Setup(t, SetupOptions{
		NumListeners:   m,
		NumSenders:     n,
		SenderMode:     ModePortForward,
		Target:         srv.Addr(),
		AllowedTargets: []string{srv.Addr()},
	})

	const k = 8
	var wg sync.WaitGroup
	errs := make(chan error, k)
	for i := 0; i < k; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			addr := tun.SenderAddrs[i%len(tun.SenderAddrs)]
			if err := probeOnce(srv, addr, defaultProbeBody, defaultProbeBody, 15*time.Second); err != nil {
				errs <- fmt.Errorf("flow %d via %s: %w", i, addr, err)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	if firstErr := drainErrors(errs); firstErr != nil {
		t.Fatalf("%v", firstErr)
	}
}

// scenarioDistributionPerListener drives short framed probes round-
// robin across senders until every listener has handled at least one
// bridge to completion, or a bounded budget is exhausted. Azure
// Relay's listener selection is explicitly non-uniform at small
// sample sizes (e2e/multi_test.go documents observed ratios up to
// 1:7 at K=8), so a fixed-K "then assert" approach would have a
// non-trivial spurious-failure rate. The convergence loop here gives
// the relay as many sequential rendezvous as it needs to cover every
// listener while still bounding total wall-clock and total dials —
// failing only when the distribution genuinely never converged.
func scenarioDistributionPerListener(t *testing.T, b Backend, n, m int) {
	t.Helper()
	AssertNoLeaks(t)
	srv := StartWorkloadServer(t, ServerBehavior{Mode: ServerProbe, RespSize: defaultProbeBody})
	tun := b.Setup(t, SetupOptions{
		NumListeners:   m,
		NumSenders:     n,
		SenderMode:     ModePortForward,
		Target:         srv.Addr(),
		AllowedTargets: []string{srv.Addr()},
	})

	const (
		kInit  = 20
		kBatch = 10
		kMax   = 100
	)
	overallDeadline := time.Now().Add(90 * time.Second)
	sent := 0

	issue := func(count int) {
		for i := 0; i < count && sent < kMax; i++ {
			if time.Now().After(overallDeadline) {
				t.Fatalf("convergence deadline exceeded after %d probes", sent)
			}
			addr := tun.SenderAddrs[sent%len(tun.SenderAddrs)]
			if err := probeOnce(srv, addr, defaultProbeBody, defaultProbeBody, 10*time.Second); err != nil {
				t.Fatalf("probe %d via %s: %v", sent, addr, err)
			}
			sent++
		}
	}

	converged := func() bool {
		for _, l := range tun.Listeners {
			if l.Completed() <= 0 {
				return false
			}
		}
		return true
	}

	issue(kInit)
	waitForListenerSum(t, tun, int64(sent), 5*time.Second)
	for !converged() && sent < kMax && time.Now().Before(overallDeadline) {
		issue(kBatch)
		waitForListenerSum(t, tun, int64(sent), 5*time.Second)
	}

	for i, l := range tun.Listeners {
		c := l.Completed()
		t.Logf("listener[%d] completed=%d (after %d probes)", i, c, sent)
		if c <= 0 {
			t.Errorf("listener[%d] completed=%d after %d probes, want > 0", i, c, sent)
		}
	}
}

// scenarioDistributionPerSender mirrors scenarioDistributionPerListener
// for senders. With probes dialed round-robin across N senders, every
// sender is exercised by construction for the dial count; the metric
// assertion proves each sender actually bridged the dial to a relay
// rendezvous (vs. accepted the TCP and failed before forwarding).
//
// The convergence loop is here primarily for symmetry with the
// listener variant. Per-sender distribution converges much faster
// because round-robin is enforced by this test, but the same shape
// keeps both scenarios easy to reason about.
func scenarioDistributionPerSender(t *testing.T, b Backend, n, m int) {
	t.Helper()
	AssertNoLeaks(t)
	srv := StartWorkloadServer(t, ServerBehavior{Mode: ServerProbe, RespSize: defaultProbeBody})
	tun := b.Setup(t, SetupOptions{
		NumListeners:   m,
		NumSenders:     n,
		SenderMode:     ModePortForward,
		Target:         srv.Addr(),
		AllowedTargets: []string{srv.Addr()},
	})

	const (
		kInit  = 20
		kBatch = 10
		kMax   = 100
	)
	overallDeadline := time.Now().Add(90 * time.Second)
	sent := 0

	issue := func(count int) {
		for i := 0; i < count && sent < kMax; i++ {
			if time.Now().After(overallDeadline) {
				t.Fatalf("convergence deadline exceeded after %d probes", sent)
			}
			addr := tun.SenderAddrs[sent%len(tun.SenderAddrs)]
			if err := probeOnce(srv, addr, defaultProbeBody, defaultProbeBody, 10*time.Second); err != nil {
				t.Fatalf("probe %d via %s: %v", sent, addr, err)
			}
			sent++
		}
	}

	converged := func() bool {
		for _, s := range tun.Senders {
			if s.Completed() <= 0 {
				return false
			}
		}
		return true
	}

	issue(kInit)
	waitForSenderSum(t, tun, int64(sent), 5*time.Second)
	for !converged() && sent < kMax && time.Now().Before(overallDeadline) {
		issue(kBatch)
		waitForSenderSum(t, tun, int64(sent), 5*time.Second)
	}

	for i, s := range tun.Senders {
		c := s.Completed()
		t.Logf("sender[%d] completed=%d (after %d probes)", i, c, sent)
		if c <= 0 {
			t.Errorf("sender[%d] completed=%d after %d probes, want > 0", i, c, sent)
		}
	}
}

// scenarioHotDropListener verifies the surviving-listener path. It
// opens enough held-open probe flows for at least one to land on
// listener[0], snapshots A0 = listener[0].Active(), stops
// listener[0], and asserts:
//
//   - at least A0 flows fail/EOF within bounded time;
//   - the surviving K-A0 flows keep making forward progress;
//   - a fresh probeOnce dial after drop succeeds.
//
// Azure Relay distributes connections nonuniformly across listeners
// (observed ratios up to 1:7 with K=8), so we open in batches of
// kBatch=8 up to a hard cap of kMax=40 flows until A0 > 0. If we
// can't get any flow onto listener[0] after kMax attempts, the
// backend distribution is degenerate and the test fails — silently
// skipping would let a real hot-drop regression slip through.
func scenarioHotDropListener(t *testing.T, b Backend, n, m int) {
	t.Helper()
	AssertNoLeaks(t)
	srv := StartWorkloadServer(t, ServerBehavior{Mode: ServerProbe, RespSize: defaultProbeBody})
	tun := b.Setup(t, SetupOptions{
		NumListeners:   m,
		NumSenders:     n,
		SenderMode:     ModePortForward,
		Target:         srv.Addr(),
		AllowedTargets: []string{srv.Addr()},
	})

	const (
		kBatch = 8
		kMax   = 40
	)
	var flows []*probeFlow
	var a0 int64
	for len(flows) < kMax {
		// Open one batch.
		batchStart := len(flows)
		for i := 0; i < kBatch && len(flows) < kMax; i++ {
			addr := tun.SenderAddrs[len(flows)%len(tun.SenderAddrs)]
			f, err := startProbeFlow(addr, 15*time.Second, ProbeConfig{})
			if err != nil {
				t.Fatalf("start probe flow %d: %v", len(flows), err)
			}
			flows = append(flows, f)
			t.Cleanup(f.Stop)
		}
		// Wait until every probe flow opened so far is bridged.
		if !waitForSumActive(tun, int64(len(flows)), 10*time.Second) {
			t.Fatalf("sum of listener Active() never reached %d after batch starting at %d",
				len(flows), batchStart)
		}
		a0 = tun.Listeners[0].Active()
		if a0 > 0 {
			break
		}
	}
	if a0 == 0 {
		t.Fatalf("after %d long flows, listener[0] Active()=0; backend distribution is degenerate", len(flows))
	}
	k := len(flows)
	for i, l := range tun.Listeners {
		t.Logf("pre-drop: listener[%d] Active()=%d", i, l.Active())
	}
	// Re-read a0 immediately before Stop() so the snapshot is as
	// fresh as possible — any per-listener scrape skew from the
	// logging loop above is bounded by the time between this read
	// and Stop() (microseconds).
	a0 = tun.Listeners[0].Active()
	t.Logf("pre-drop: %d flows open, listener[0] Active()=%d", k, a0)

	// Drop listener[0]. New rendezvous attempts will only land on
	// remaining listeners thereafter.
	tun.Listeners[0].Stop()

	// Count flows that have gone dead (write or read fails) within the
	// bounded window. The scenario contract is exactly A0 broken: the
	// flows that were on listener[0]. Fewer would mean the drop did
	// not propagate; more would mean a surviving listener silently
	// tore down in-flight bridges, which is the regression we want to
	// catch. Wait up to 15 s for the lower bound, then settle for 5 s
	// before re-counting so any async surviving-listener-side
	// breakages (e.g. timer-driven cleanup) have time to manifest.
	deadline := time.Now().Add(15 * time.Second)
	var dead int
	for time.Now().Before(deadline) {
		dead = 0
		for _, f := range flows {
			if f.broken() {
				dead++
			}
		}
		if int64(dead) >= a0 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	time.Sleep(5 * time.Second)
	dead = 0
	for _, f := range flows {
		if f.broken() {
			dead++
		}
	}
	if int64(dead) < a0 {
		t.Errorf("after dropping listener[0]: %d/%d flows broken, want >= %d (A0)", dead, k, a0)
	}
	if int64(dead) > a0 {
		t.Errorf("after dropping listener[0]: %d/%d flows broken, want <= %d (A0); surviving listener(s) silently tore down %d in-flight bridge(s)", dead, k, a0, int64(dead)-a0)
	}

	// Verify every surviving flow keeps making forward progress. If A0
	// happened to equal K (all flows on the dropped listener), all flows
	// are broken and we skip the survivor check; otherwise we exercise
	// every non-broken flow — a regression where the surviving listener
	// silently dropped some bridges but left others healthy would slip
	// through if we only probed one.
	if a0 < int64(k) {
		survivors := 0
		for i, f := range flows {
			if f.broken() {
				continue
			}
			base := f.acked()
			if !waitProgress(f, base, 5*time.Second) {
				t.Errorf("survivor flow %d made no progress after drop (acked stuck at %d)", i, base)
				f.Dump(t, srv, fmt.Sprintf("hotdrop-flow-%d", i))
				continue
			}
			survivors++
		}
		if survivors == 0 {
			t.Fatalf("expected at least one surviving flow but all are broken")
		}
		t.Logf("post-drop: %d/%d surviving flows still progressing", survivors, k)
	}

	// A fresh dial after drop must succeed via the surviving listener.
	if err := probeOnce(srv, tun.SenderAddr, defaultProbeBody, defaultProbeBody, 15*time.Second); err != nil {
		t.Errorf("post-drop fresh dial: %v", err)
	}
}

// scenarioHotAddListener verifies that adding a listener mid-flight
// converges: existing flows keep making forward progress, and new flows
// reach the new listener within a bounded window. Because Azure Relay's
// listener selection is non-uniform at small sample sizes
// (e2e/multi_test.go documents observed ratios up to 1:7 at K=8), a
// fixed-K probe count would have a non-trivial spurious-failure rate
// when no flow lands on the freshly-added listener. Instead we drive
// short probes in batches until newListener.Completed() > 0, capped at
// a hard probe budget — that bounds the test runtime while making a
// true "no traffic ever reaches the new listener" regression observable.
func scenarioHotAddListener(t *testing.T, b Backend, n, m int) {
	t.Helper()
	AssertNoLeaks(t)
	srv := StartWorkloadServer(t, ServerBehavior{Mode: ServerProbe, RespSize: defaultProbeBody})
	tun := b.Setup(t, SetupOptions{
		NumListeners:   m,
		NumSenders:     n,
		SenderMode:     ModePortForward,
		Target:         srv.Addr(),
		AllowedTargets: []string{srv.Addr()},
	})
	if tun.AddListener == nil {
		t.Skip("backend does not implement hot-add (Tunnel.AddListener is nil)")
	}

	const kActive = 4
	flows := make([]*probeFlow, kActive)
	for i := 0; i < kActive; i++ {
		addr := tun.SenderAddrs[i%len(tun.SenderAddrs)]
		f, err := startProbeFlow(addr, 15*time.Second, ProbeConfig{})
		if err != nil {
			t.Fatalf("start probe flow %d: %v", i, err)
		}
		flows[i] = f
		t.Cleanup(f.Stop)
	}

	newListener := tun.AddListener(t)
	if newListener == nil {
		t.Fatalf("AddListener returned nil")
	}

	// Drive short probes in batches until the new listener completes
	// at least one bridge, or we exhaust the probe budget. We need
	// the listener to be the chosen destination at least once;
	// Azure's selection is non-uniform but biased toward freshly-
	// attached listeners in practice, so this typically converges
	// within the first batch.
	const (
		kBatch      = 8
		kMax        = 60
		probeBudget = 60 * time.Second
	)
	probeDeadline := time.Now().Add(probeBudget)
	sent := 0
	for newListener.Completed() == 0 && sent < kMax && time.Now().Before(probeDeadline) {
		for i := 0; i < kBatch && sent < kMax && time.Now().Before(probeDeadline); i++ {
			addr := tun.SenderAddrs[sent%len(tun.SenderAddrs)]
			if err := probeOnce(srv, addr, defaultProbeBody, defaultProbeBody, 10*time.Second); err != nil {
				t.Fatalf("probe %d via %s: %v", sent, addr, err)
			}
			sent++
		}
		// Brief settle for Completed() to update.
		settle := time.Now().Add(2 * time.Second)
		for newListener.Completed() == 0 && time.Now().Before(settle) {
			time.Sleep(50 * time.Millisecond)
		}
	}
	if c := newListener.Completed(); c <= 0 {
		t.Errorf("new listener completed=%d after %d probes, want > 0", c, sent)
	} else {
		t.Logf("hot-add converged: new listener completed=%d after %d probes", c, sent)
	}

	// Existing flows must still be alive and making forward progress.
	for i, f := range flows {
		if f.broken() {
			t.Errorf("existing flow %d broken after hot-add", i)
			f.Dump(t, srv, fmt.Sprintf("hotadd-flow-%d", i))
			continue
		}
		base := f.acked()
		if !waitProgress(f, base, 5*time.Second) {
			t.Errorf("existing flow %d made no progress after hot-add (acked stuck at %d)", i, base)
			f.Dump(t, srv, fmt.Sprintf("hotadd-flow-%d", i))
		}
	}
}

// scenarioMaxConnBackPressure verifies the per-listener semaphore
// holds: with MaxConnections=2 on each of M=2 listeners, no more than
// 4 bridges may be active at any sampled moment, and all K=8 dial
// attempts ultimately succeed via client-side retry. The current
// listener implementation drops overflow accepts (non-blocking
// tryAcquire) — so clients see EOF / closed connection and must
// retry. This scenario asserts both invariants.
//
// Each successful client holds its bridge open for holdDur after the
// initial probe so the metric sampler (20 ms ticker) reliably catches
// the steady-state concurrent count. Without the hold, short probes
// complete in single-digit milliseconds and the sampler races them.
func scenarioMaxConnBackPressure(t *testing.T, b Backend, n, m int) {
	t.Helper()
	AssertNoLeaks(t)
	srv := StartWorkloadServer(t, ServerBehavior{Mode: ServerProbe, RespSize: defaultProbeBody})
	const maxConn = 2
	tun := b.Setup(t, SetupOptions{
		NumListeners:   m,
		NumSenders:     n,
		SenderMode:     ModePortForward,
		Target:         srv.Addr(),
		AllowedTargets: []string{srv.Addr()},
		MaxConnections: maxConn,
	})

	const k = 8
	const holdDur = 300 * time.Millisecond
	const overall = 90 * time.Second
	const limit = int64(maxConn) * 2 // M=2 listeners

	var maxObserved int64
	samplerCtx, samplerCancel := context.WithCancel(context.Background())
	defer samplerCancel()
	samplerDone := make(chan struct{})
	go func() {
		defer close(samplerDone)
		ticker := time.NewTicker(20 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-samplerCtx.Done():
				return
			case <-ticker.C:
				var sum int64
				for _, l := range tun.Listeners {
					sum += l.Active()
				}
				if sum > atomic.LoadInt64(&maxObserved) {
					atomic.StoreInt64(&maxObserved, sum)
				}
			}
		}
	}()

	var wg sync.WaitGroup
	errs := make(chan error, k)
	for i := 0; i < k; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			addr := tun.SenderAddrs[i%len(tun.SenderAddrs)]
			deadline := time.Now().Add(overall)
			var lastErr error
			for time.Now().Before(deadline) {
				err := probeWithHold(srv, addr, holdDur, 5*time.Second)
				if err == nil {
					return
				}
				lastErr = err
				// Brief jitter so the retries don't all crash the
				// gate at once. 50–150 ms keeps the test responsive
				// while letting saturated listeners free a slot.
				time.Sleep(50*time.Millisecond + time.Duration(i%3)*30*time.Millisecond)
			}
			errs <- fmt.Errorf("flow %d via %s: %w", i, addr, lastErr)
		}(i)
	}
	wg.Wait()
	close(errs)

	samplerCancel()
	<-samplerDone

	if firstErr := drainErrors(errs); firstErr != nil {
		t.Errorf("%v", firstErr)
	}
	if observed := atomic.LoadInt64(&maxObserved); observed > limit {
		t.Errorf("max concurrent Active() observed=%d, want <= %d (M=%d * MaxConn=%d)",
			observed, limit, m, maxConn)
	} else {
		t.Logf("max concurrent Active() observed=%d (limit %d)", observed, limit)
	}
}

// probeWithHold dials addr, runs one probe exchange against srv with
// the same validation probeOnce performs (header, nonce, response
// size, pattern), holds the bridge open (idle) for hold, then closes.
// Used by scenarioMaxConnBackPressure to give the 20 ms metric sampler
// a chance to observe the steady-state Active count across all
// listeners; short exchanges that complete in single-digit milliseconds
// race the sampler and produce observed=0 spuriously.
//
// On failure the returned error carries the same per-leg server-side
// attribution probeOnce provides (outbound break vs. server stall vs.
// return-leg failure), and the server-side record is always consumed
// so a partial exchange does not leak a probeStats entry.
func probeWithHold(srv *WorkloadServer, addr string, hold, timeout time.Duration) error {
	conn, err := net.DialTimeout("tcp", addr, timeout)
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	defer conn.Close() //nolint:errcheck // best-effort
	_ = conn.SetDeadline(time.Now().Add(timeout + hold))

	nonce := randNonce()
	if err := probeExchangeOnConn(conn, nonce, defaultProbeBody, defaultProbeBody); err != nil {
		return localizeProbeError(srv, nonce, err)
	}
	// Hold the conn open so the sampler observes the elevated Active
	// count; both server and client retain their bridge bookkeeping
	// until the deferred Close fires after the sleep.
	time.Sleep(hold)
	// Successful exchange — drop the record so it doesn't linger.
	_, _ = srv.ConsumeProbeRecord(nonce)
	return nil
}

// --- helpers --------------------------------------------------------

// drainErrors returns the first error from a closed channel, with a
// summary mention of how many additional errors followed.
func drainErrors(errs <-chan error) error {
	first, ok := <-errs
	if !ok {
		return nil
	}
	count := 1
	for range errs {
		count++
	}
	if count == 1 {
		return first
	}
	return fmt.Errorf("%w (+%d more errors)", first, count-1)
}

// waitForListenerSum polls Tunnel.Listeners for sum-of-Completed to
// reach want. Used after a burst of short probes where the last few
// Done() callbacks may still be in flight when the sender goroutine
// returns. Fails the test on deadline.
func waitForListenerSum(t *testing.T, tun *Tunnel, want int64, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		var sum int64
		for _, l := range tun.Listeners {
			sum += l.Completed()
		}
		if sum >= want {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	var sum int64
	for _, l := range tun.Listeners {
		sum += l.Completed()
	}
	t.Fatalf("listener-completed sum reached %d after %v, want >= %d", sum, timeout, want)
}

func waitForSenderSum(t *testing.T, tun *Tunnel, want int64, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		var sum int64
		for _, s := range tun.Senders {
			sum += s.Completed()
		}
		if sum >= want {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	var sum int64
	for _, s := range tun.Senders {
		sum += s.Completed()
	}
	t.Fatalf("sender-completed sum reached %d after %v, want >= %d", sum, timeout, want)
}

// waitForSumActive polls until the sum of Active() across all
// listeners reaches want. Returns true on success, false on deadline.
func waitForSumActive(tun *Tunnel, want int64, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		var sum int64
		for _, l := range tun.Listeners {
			sum += l.Active()
		}
		if sum >= want {
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return false
}

// ScenarioListenerRestart_Recovers: bring up one listener and one
// port-forward sender, drive a round-trip through them, stop the
// listener, attach a fresh listener on the same hyco, drive another
// round-trip through the same sender bind. Asserts the sender
// reattaches and bridges to the new listener within a bounded
// budget. Subsumes the legacy TestPortForwardRecoveryAfterListenerRestart.
func ScenarioListenerRestart_Recovers(t *testing.T, b Backend) {
	t.Helper()
	AssertNoLeaks(t)

	srv := StartWorkloadServer(t, ServerBehavior{Mode: ServerProbe, RespSize: defaultProbeBody})
	tun := b.Setup(t, SetupOptions{
		NumListeners:   1,
		SenderMode:     ModePortForward,
		Target:         srv.Addr(),
		AllowedTargets: []string{srv.Addr()},
	})

	// Pre-restart round-trip.
	if err := probeOnce(srv, tun.SenderAddr, defaultProbeBody, defaultProbeBody, 15*time.Second); err != nil {
		t.Fatalf("pre-restart probe: %v", err)
	}

	// Stop the listener and attach a fresh one.
	tun.Listeners[0].Stop()
	if tun.AddListener == nil {
		t.Fatalf("backend does not support hot-attach (Tunnel.AddListener is nil)")
	}
	_ = tun.AddListener(t)

	// Post-restart round-trip. Bounded retry: Azure Relay may
	// briefly retain stale routing state for the killed listener;
	// the sender's next dial may fail before the new listener's
	// control channel is fully registered. Retry with backoff.
	deadline := time.Now().Add(20 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		if err := probeOnce(srv, tun.SenderAddr, defaultProbeBody, defaultProbeBody, 5*time.Second); err == nil {
			return
		} else {
			lastErr = err
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("post-restart probe never succeeded within 20s: %v", lastErr)
}
