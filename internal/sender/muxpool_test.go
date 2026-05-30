package sender

import (
	"context"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/philsphicas/aztunnel/internal/metrics"
)

// --- fake opener for pool tests ---

// fakeOpener is a deterministic muxOpener for MuxPool tests. It does not
// actually open WebSockets; OpenStream returns net.Pipe() halves. The
// fake exposes counters so tests can assert how many sessions the pool
// created and how it distributed streams across them.
type fakeOpener struct {
	id          int
	created     time.Time
	mu          sync.Mutex
	streamsOpen int
	totalOpens  int
	closed      atomic.Bool

	// ctx is the per-session ctx passed to the factory by the pool.
	// Tests assert that evictSession cancels it.
	ctx context.Context

	// behaviour knobs
	openDelay    time.Duration
	failOnce     atomic.Pointer[error] // single-shot OpenStream error
	unavailable  atomic.Bool           // sticky v1-fallback
	holdOnOpenCh chan struct{}         // nil = no hold; non-nil = OpenStream blocks until receive
	// honorSessCtx makes OpenStream return ErrMuxDialerClosed when the
	// per-session ctx (f.ctx, set by the pool factory) is done. This
	// mirrors the real MuxDialer behaviour and is opt-in so existing
	// tests that don't care about parent-ctx propagation stay simple.
	honorSessCtx atomic.Bool
}

func (f *fakeOpener) OpenStream(ctx context.Context) (net.Conn, error) {
	if f.honorSessCtx.Load() && f.ctx != nil && f.ctx.Err() != nil {
		return nil, ErrMuxDialerClosed
	}
	if f.openDelay > 0 {
		select {
		case <-time.After(f.openDelay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if f.holdOnOpenCh != nil {
		select {
		case <-f.holdOnOpenCh:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if errPtr := f.failOnce.Swap(nil); errPtr != nil {
		return nil, *errPtr
	}
	f.mu.Lock()
	f.streamsOpen++
	f.totalOpens++
	f.mu.Unlock()
	a, b := net.Pipe()
	return &countingFakeStream{Conn: a, parent: f, remote: b}, nil
}

func (f *fakeOpener) Close() {
	f.closed.Store(true)
}

func (f *fakeOpener) Unavailable() bool {
	return f.unavailable.Load()
}

type countingFakeStream struct {
	net.Conn
	parent *fakeOpener
	remote net.Conn
	once   sync.Once
}

func (s *countingFakeStream) Close() error {
	s.once.Do(func() {
		s.parent.mu.Lock()
		s.parent.streamsOpen--
		s.parent.mu.Unlock()
		_ = s.remote.Close()
	})
	return s.Conn.Close()
}

// fakeFactory builds a dialerFactory backed by fakeOpener. Each factory
// invocation returns a fresh opener and stores it in `created` so tests
// can inspect/reach into the openers post-hoc. The optional `customize`
// hook lets the test set per-opener knobs (e.g. mark some unavailable).
func fakeFactory(t *testing.T, customize func(int, *fakeOpener)) (dialerFactory, *[]*fakeOpener) {
	t.Helper()
	var (
		mu      sync.Mutex
		created []*fakeOpener
	)
	factory := func(ctx context.Context) muxOpener {
		mu.Lock()
		id := len(created)
		f := &fakeOpener{id: id, created: time.Now(), ctx: ctx}
		if customize != nil {
			customize(id, f)
		}
		created = append(created, f)
		mu.Unlock()
		return f
	}
	return factory, &created
}

// --- tests ---

func newTestPool(t *testing.T, opts MuxPoolOptions, factory dialerFactory) *MuxPool {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	p := newMuxPoolWithFactory(ctx, opts, factory)
	t.Cleanup(p.Close)
	return p
}

// TestMuxPool_SerialReuseOneSession is the "idle session" optimization:
// when traffic is strictly serial (open, use, close, repeat), the pool
// must stick to session 0 forever and NOT create extra sessions just
// because MaxSessions > 1. This avoids burning N rendezvous on light
// workloads.
func TestMuxPool_SerialReuseOneSession(t *testing.T) {
	factory, created := fakeFactory(t, nil)
	pool := newTestPool(t, MuxPoolOptions{MaxSessions: 4, MaxStreamsPerSession: 8}, factory)

	for i := range 5 {
		stream, err := pool.OpenStream(context.Background())
		if err != nil {
			t.Fatalf("OpenStream #%d: %v", i, err)
		}
		_ = stream.Close()
	}

	if got := len(*created); got != 1 {
		t.Errorf("len(created) = %d, want 1 (serial workload should reuse one session)", got)
	}
}

// TestMuxPool_GrowsUnderConcurrency verifies the HA-spread design goal:
// when several streams are in flight at once, the pool grows up to
// MaxSessions so the streams are spread across multiple rendezvous (and
// thus, in practice, may land on different HA listeners).
func TestMuxPool_GrowsUnderConcurrency(t *testing.T) {
	factory, created := fakeFactory(t, nil)
	pool := newTestPool(t, MuxPoolOptions{MaxSessions: 3, MaxStreamsPerSession: 100}, factory)

	streams := make([]net.Conn, 3)
	for i := range 3 {
		s, err := pool.OpenStream(context.Background())
		if err != nil {
			t.Fatalf("OpenStream #%d: %v", i, err)
		}
		streams[i] = s
	}
	for _, s := range streams {
		defer s.Close() //nolint:errcheck // test cleanup
	}

	if got := len(*created); got != 3 {
		t.Errorf("len(created) = %d, want 3 (concurrent workload should grow to MaxSessions)", got)
	}
}

// TestMuxPool_GrowthCappedAtMaxSessions verifies that once the pool
// reaches MaxSessions, additional concurrent streams reuse existing
// sessions rather than creating more.
func TestMuxPool_GrowthCappedAtMaxSessions(t *testing.T) {
	factory, created := fakeFactory(t, nil)
	pool := newTestPool(t, MuxPoolOptions{MaxSessions: 2, MaxStreamsPerSession: 100}, factory)

	const N = 5
	streams := make([]net.Conn, 0, N)
	for range N {
		s, err := pool.OpenStream(context.Background())
		if err != nil {
			t.Fatalf("OpenStream: %v", err)
		}
		streams = append(streams, s)
	}
	defer func() {
		for _, s := range streams {
			_ = s.Close()
		}
	}()

	if got := len(*created); got != 2 {
		t.Errorf("len(created) = %d, want 2 (capped at MaxSessions)", got)
	}
}

// TestMuxPool_BlocksAtCap_AndWakesOnRelease covers admission control:
// with MaxSessions=1 and MaxStreamsPerSession=1, the first OpenStream
// succeeds, the second blocks, and closing the first wakes the second.
func TestMuxPool_BlocksAtCap_AndWakesOnRelease(t *testing.T) {
	factory, _ := fakeFactory(t, nil)
	pool := newTestPool(t, MuxPoolOptions{MaxSessions: 1, MaxStreamsPerSession: 1}, factory)

	first, err := pool.OpenStream(context.Background())
	if err != nil {
		t.Fatalf("OpenStream #1: %v", err)
	}

	type result struct {
		s   net.Conn
		err error
	}
	secondCh := make(chan result, 1)
	go func() {
		s, err := pool.OpenStream(context.Background())
		secondCh <- result{s, err}
	}()

	select {
	case r := <-secondCh:
		t.Fatalf("second OpenStream should have blocked, got s=%v err=%v", r.s, r.err)
	case <-time.After(100 * time.Millisecond):
		// ok — still blocked
	}

	if err := first.Close(); err != nil {
		t.Fatalf("close first: %v", err)
	}

	select {
	case r := <-secondCh:
		if r.err != nil {
			t.Fatalf("second OpenStream returned error after release: %v", r.err)
		}
		_ = r.s.Close()
	case <-time.After(2 * time.Second):
		t.Fatal("second OpenStream did not unblock after first was closed")
	}
}

// TestMuxPool_BlocksAtCap_RespectsCtx ensures a waiting OpenStream
// returns ctx.Err() promptly when its caller ctx is cancelled.
func TestMuxPool_BlocksAtCap_RespectsCtx(t *testing.T) {
	factory, _ := fakeFactory(t, nil)
	pool := newTestPool(t, MuxPoolOptions{MaxSessions: 1, MaxStreamsPerSession: 1}, factory)

	first, err := pool.OpenStream(context.Background())
	if err != nil {
		t.Fatalf("OpenStream #1: %v", err)
	}
	defer first.Close() //nolint:errcheck // test cleanup

	ctx, cancel := context.WithCancel(context.Background())
	type result struct{ err error }
	ch := make(chan result, 1)
	go func() {
		_, err := pool.OpenStream(ctx)
		ch <- result{err}
	}()

	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case r := <-ch:
		if !errors.Is(r.err, context.Canceled) {
			t.Fatalf("expected context.Canceled, got %v", r.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("OpenStream did not return after ctx cancel")
	}
}

// TestMuxPool_EvictsAndRetriesOnUnsupported covers mixed-version HA
// rollouts: when a session reports ErrMuxUnsupported, the pool evicts it
// (it does NOT remain in the pool as a sticky-unavailable slot — that was
// the old behaviour before the bounded retry budget) and retries with a
// fresh rendezvous, which may land on a different v2-capable listener.
// After the bounded retry budget is exhausted the pool sets its sticky
// fallback cache and returns ErrMuxUnsupported.
func TestMuxPool_EvictsAndRetriesOnUnsupported(t *testing.T) {
	mu := &sync.Mutex{}
	v1OpenerCount := 0

	factory, created := fakeFactory(t, func(id int, f *fakeOpener) {
		mu.Lock()
		isV1 := v1OpenerCount < 2
		if isV1 {
			v1OpenerCount++
		}
		mu.Unlock()
		if isV1 {
			err := ErrMuxUnsupported
			f.failOnce.Store(&err)
			f.unavailable.Store(true)
		}
	})
	pool := newTestPool(t, MuxPoolOptions{MaxSessions: 4, MaxStreamsPerSession: 1}, factory)

	stream, err := pool.OpenStream(context.Background())
	if err != nil {
		t.Fatalf("OpenStream: %v (created %d openers)", err, len(*created))
	}
	defer stream.Close() //nolint:errcheck // test cleanup

	if got := len(*created); got != 3 {
		t.Errorf("len(created) = %d, want 3 (2 v1-evict, then 1 v2-success)", got)
	}
}

// TestMuxPool_AllUnavailableReturnsErrMuxUnsupported verifies the
// terminal v1-fallback path: when every dial within the bounded
// eviction budget returns ErrMuxUnsupported, the pool evicts each
// session as it fails, stops after MaxSessions attempts, returns
// ErrMuxUnsupported, AND populates the pool-level sticky
// `unsupportedUntil` cache so subsequent calls within
// muxUnavailableTTL short-circuit without re-dialing.
func TestMuxPool_AllUnavailableReturnsErrMuxUnsupported(t *testing.T) {
	factory, created := fakeFactory(t, func(_ int, f *fakeOpener) {
		err := ErrMuxUnsupported
		f.failOnce.Store(&err)
		f.unavailable.Store(true)
	})
	pool := newTestPool(t, MuxPoolOptions{MaxSessions: 2, MaxStreamsPerSession: 1}, factory)

	_, err := pool.OpenStream(context.Background())
	if !errors.Is(err, ErrMuxUnsupported) {
		t.Fatalf("expected ErrMuxUnsupported when all sessions unavailable, got %v", err)
	}
	// With MaxSessions=2 we should attempt exactly 2 mux rendezvous
	// before declaring the fleet v1-only — not 3. Each extra failed
	// rendezvous is a full WebSocket dial + envelope exchange paid on
	// every cold-start v1 fallback.
	if got := len(*created); got != 2 {
		t.Errorf("first OpenStream against all-v1 fleet dialed %d openers; want 2 (MaxSessions)", got)
	}

	// Pool-level sticky cache: a follow-up OpenStream call within
	// muxUnavailableTTL must short-circuit without dialing a fresh
	// opener (which would re-pay the rendezvous cost against the same
	// known-v1 fleet).
	dialsBefore := len(*created)
	_, err = pool.OpenStream(context.Background())
	if !errors.Is(err, ErrMuxUnsupported) {
		t.Fatalf("expected ErrMuxUnsupported on second call within sticky window, got %v", err)
	}
	if got := len(*created); got != dialsBefore {
		t.Errorf("second OpenStream call dialed %d additional opener(s); want 0 (sticky cache leak)", got-dialsBefore)
	}
}

// TestMuxPool_UnsupportedScopedToNoV2Left is the regression guard for
// the "setting pool-wide unsupported cache after maxEvictions can
// disable mux even when a working v2 session still exists in another
// slot" finding. The sticky cache must only be committed when there is
// no working v2 session left in the pool — otherwise traffic that
// could reuse the established v2 session is forced to v1 for the next
// muxUnavailableTTL.
func TestMuxPool_UnsupportedScopedToNoV2Left(t *testing.T) {
	// Session 0 is v2 and works. Sessions 1+ are v1-only: each first
	// OpenStream returns ErrMuxUnsupported and the opener is sticky-
	// unavailable so selectOrGrowLocked skips it in the future.
	factory, _ := fakeFactory(t, func(id int, f *fakeOpener) {
		if id >= 1 {
			err := ErrMuxUnsupported
			f.failOnce.Store(&err)
			f.unavailable.Store(true)
		}
	})
	pool := newTestPool(t, MuxPoolOptions{MaxSessions: 2, MaxStreamsPerSession: 1}, factory)

	// Warm up session 0. Hold its slot so subsequent OpenStream
	// forces growth into session 1+ (the v1-only fleet).
	s0, err := pool.OpenStream(context.Background())
	if err != nil {
		t.Fatalf("warmup OpenStream: %v", err)
	}

	// Trigger growth. Each new session lands on the v1-only fake
	// fleet and returns ErrMuxUnsupported. After maxEvictions, the
	// pool's per-call budget is exhausted and OpenStream returns
	// ErrMuxUnsupported — but session 0 is still alive and v2, so
	// the pool-wide sticky cache MUST NOT be set.
	_, err = pool.OpenStream(context.Background())
	if !errors.Is(err, ErrMuxUnsupported) {
		t.Fatalf("growth-failure OpenStream: got %v, want ErrMuxUnsupported", err)
	}

	// Release session 0's slot and reopen. With the fix, the pool's
	// unsupportedUntil is unset (session 0 is still v2), so the next
	// OpenStream picks session 0 and succeeds. Without the fix, the
	// sticky cache short-circuits this call to ErrMuxUnsupported and
	// mux is wrongly disabled for muxUnavailableTTL.
	_ = s0.Close()
	s2, err := pool.OpenStream(context.Background())
	if err != nil {
		t.Fatalf("post-growth-failure OpenStream (session 0 should still be usable): got %v", err)
	}
	_ = s2.Close()
}

// TestMuxPool_CloseWakesWaiter verifies pool teardown wakes blocked
// OpenStream goroutines with ErrMuxPoolClosed.
func TestMuxPool_CloseWakesWaiter(t *testing.T) {
	factory, _ := fakeFactory(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pool := newMuxPoolWithFactory(ctx, MuxPoolOptions{MaxSessions: 1, MaxStreamsPerSession: 1}, factory)

	first, err := pool.OpenStream(context.Background())
	if err != nil {
		t.Fatalf("OpenStream #1: %v", err)
	}
	defer first.Close() //nolint:errcheck // test cleanup

	type result struct {
		s   net.Conn
		err error
	}
	ch := make(chan result, 1)
	go func() {
		s, err := pool.OpenStream(context.Background())
		ch <- result{s, err}
	}()

	time.Sleep(50 * time.Millisecond)
	pool.Close()

	select {
	case r := <-ch:
		if !errors.Is(r.err, ErrMuxPoolClosed) {
			t.Fatalf("expected ErrMuxPoolClosed after Close, got %v", r.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("OpenStream did not return after Close")
	}
}

// TestMuxPool_CloseIsPermanent ensures OpenStream calls after Close
// return ErrMuxPoolClosed and do NOT silently create new sessions. This
// is the regression guard for the "MuxPool.Close must be permanent"
// finding from review.
func TestMuxPool_CloseIsPermanent(t *testing.T) {
	factory, created := fakeFactory(t, nil)
	pool := newTestPool(t, MuxPoolOptions{MaxSessions: 4, MaxStreamsPerSession: 4}, factory)

	// Warm up so the pool has at least one session, then close.
	s, err := pool.OpenStream(context.Background())
	if err != nil {
		t.Fatalf("warmup OpenStream: %v", err)
	}
	_ = s.Close()
	priorCount := len(*created)
	pool.Close()

	// Idempotent: second Close is a no-op.
	pool.Close()

	for range 3 {
		_, err := pool.OpenStream(context.Background())
		if !errors.Is(err, ErrMuxPoolClosed) {
			t.Fatalf("OpenStream after Close returned %v, want ErrMuxPoolClosed", err)
		}
	}
	if got := len(*created); got != priorCount {
		t.Errorf("Close should not allow new session creation; created went from %d to %d", priorCount, got)
	}
}

// TestMuxPool_ParentCtxCancelReturnsClosed is the regression guard for
// the "OpenStream spins when parent ctx is cancelled but pool.Close()
// is never called" finding. Because each pooled session's sessCtx is
// derived from p.poolCtx, cancelling the parent ctx causes every
// dialer to return ErrMuxDialerClosed; without a poolCtx.Err() check
// at the top of the retry loop, OpenStream would spin forever because
// p.closed is still false. The fix surfaces ErrMuxPoolClosed instead.
//
// Note: this test opts the fakeOpener into honorSessCtx so it faithfully
// mirrors MuxDialer's parent-ctx propagation (returns ErrMuxDialerClosed
// when its session ctx is done) — this is what makes the loop spin
// without the fix.
func TestMuxPool_ParentCtxCancelReturnsClosed(t *testing.T) {
	factory, created := fakeFactory(t, func(_ int, f *fakeOpener) {
		f.honorSessCtx.Store(true)
	})
	parentCtx, cancelParent := context.WithCancel(context.Background())
	pool := newMuxPoolWithFactory(parentCtx, MuxPoolOptions{MaxSessions: 2, MaxStreamsPerSession: 4}, factory)
	t.Cleanup(pool.Close)

	// Warm up so a session exists.
	s, err := pool.OpenStream(context.Background())
	if err != nil {
		t.Fatalf("warmup OpenStream: %v", err)
	}
	_ = s.Close()
	priorCreated := len(*created)

	// Cancel the parent ctx without calling pool.Close(). The pool's
	// internal poolCtx is now done but p.closed is still false. Every
	// existing session's f.ctx is done, so its fake opener will return
	// ErrMuxDialerClosed (mirroring MuxDialer); without the fix, the
	// retry loop would also keep growing fresh sessions whose newly
	// minted f.ctx is born-cancelled.
	cancelParent()

	// Subsequent OpenStream calls must surface ErrMuxPoolClosed in
	// bounded time — never spin.
	done := make(chan error, 1)
	go func() {
		s, err := pool.OpenStream(context.Background())
		if s != nil {
			_ = s.Close()
		}
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, ErrMuxPoolClosed) {
			t.Fatalf("OpenStream after parent-ctx cancel returned %v, want ErrMuxPoolClosed", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("OpenStream did not return after parent-ctx cancel (likely spinning on ErrMuxDialerClosed)")
	}

	// Sanity: OpenStream returning ErrMuxPoolClosed must not have
	// spun the loop growing dozens of doomed sessions.
	if got := len(*created); got > priorCreated+1 {
		t.Errorf("OpenStream grew %d new sessions after parent-ctx cancel; want <= 1", got-priorCreated)
	}
}

// TestMuxPool_MultipleWaitersWakeOnReleases is the regression guard for
// the "1-buffered notify channel coalesces wakeups and strands waiters"
// finding from review. With broadcast wakeups, all waiters re-check on
// every release.
func TestMuxPool_MultipleWaitersWakeOnReleases(t *testing.T) {
	factory, _ := fakeFactory(t, nil)
	pool := newTestPool(t, MuxPoolOptions{MaxSessions: 1, MaxStreamsPerSession: 1}, factory)

	first, err := pool.OpenStream(context.Background())
	if err != nil {
		t.Fatalf("OpenStream #1: %v", err)
	}

	const waiters = 5
	results := make(chan error, waiters)
	streams := make(chan net.Conn, waiters)
	for range waiters {
		go func() {
			s, err := pool.OpenStream(context.Background())
			if err != nil {
				results <- err
				return
			}
			streams <- s
			results <- nil
		}()
	}

	time.Sleep(100 * time.Millisecond)

	// Release slots one at a time; each release should wake exactly one
	// waiter (since slots is sized 1 and we only release 1 at a time).
	_ = first.Close()
	collected := []net.Conn{}
	for range waiters {
		select {
		case err := <-results:
			if err != nil {
				t.Fatalf("waiter returned err: %v", err)
			}
			s := <-streams
			_ = s.Close() // this release wakes the next waiter
			collected = append(collected, s)
		case <-time.After(3 * time.Second):
			t.Fatalf("timed out after %d waiters completed", len(collected))
		}
	}
}

// TestMuxPool_LeastLoadedSelection verifies that when the pool is at
// MaxSessions and a new stream arrives, the least-loaded session is
// preferred. This keeps load balanced across HA listeners over time.
func TestMuxPool_LeastLoadedSelection(t *testing.T) {
	factory, created := fakeFactory(t, nil)
	pool := newTestPool(t, MuxPoolOptions{MaxSessions: 2, MaxStreamsPerSession: 10}, factory)

	// Open 4 streams. The pool grows to 2 sessions during the first 2,
	// then picks the least-loaded for #3 and #4.
	openStreams := make([]net.Conn, 0, 4)
	for range 4 {
		s, err := pool.OpenStream(context.Background())
		if err != nil {
			t.Fatalf("OpenStream: %v", err)
		}
		openStreams = append(openStreams, s)
	}
	defer func() {
		for _, s := range openStreams {
			_ = s.Close()
		}
	}()

	if got := len(*created); got != 2 {
		t.Fatalf("len(created) = %d, want 2", got)
	}

	// Both sessions should have 2 streams each — least-loaded balanced.
	s0InFlight := (*created)[0].streamsOpen
	s1InFlight := (*created)[1].streamsOpen
	if s0InFlight != 2 || s1InFlight != 2 {
		t.Errorf("streamsOpen per session = (%d, %d), want (2, 2) — least-loaded should balance",
			s0InFlight, s1InFlight)
	}
}

// TestMuxPool_StreamCloseIdempotent guards against double-close
// decrementing the slot counter twice (which would let the next caller
// over-subscribe the cap).
func TestMuxPool_StreamCloseIdempotent(t *testing.T) {
	factory, created := fakeFactory(t, nil)
	pool := newTestPool(t, MuxPoolOptions{MaxSessions: 1, MaxStreamsPerSession: 1}, factory)

	s, err := pool.OpenStream(context.Background())
	if err != nil {
		t.Fatalf("OpenStream: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("first close: %v", err)
	}
	// Second close must be safe.
	_ = s.Close()

	sess := (*created)[0]
	sess.mu.Lock()
	open := sess.streamsOpen
	sess.mu.Unlock()
	if open != 0 {
		t.Errorf("streamsOpen = %d, want 0 after close", open)
	}
	// A subsequent OpenStream must succeed (cap was 1, slot freed once).
	if _, err := pool.OpenStream(context.Background()); err != nil {
		t.Errorf("OpenStream after close: %v", err)
	}
}

// TestMuxPool_Defaults_OptsAreNormalized covers the NewMuxPool ctor's
// option-defaulting branches.
func TestMuxPool_Defaults_OptsAreNormalized(t *testing.T) {
	// Pin the published defaults so a silent change to DefaultMuxSessions /
	// DefaultMaxStreamsPerSession trips this test — the CLI flag defaults
	// in cmd/aztunnel/cli.go, help.go, README.md, and docs/mux.md are kept
	// in sync with these constants by hand. Update all of them together.
	if DefaultMuxSessions != 2 {
		t.Errorf("DefaultMuxSessions = %d, want 2 (update CLI/help/docs together if changing)", DefaultMuxSessions)
	}
	if DefaultMaxStreamsPerSession != 256 {
		t.Errorf("DefaultMaxStreamsPerSession = %d, want 256 (update CLI/help/docs together if changing)", DefaultMaxStreamsPerSession)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p := NewMuxPool(ctx, MuxPoolOptions{
		TokenProvider: stubTokenProvider{},
	})
	defer p.Close()
	if p.opts.MaxSessions != DefaultMuxSessions {
		t.Errorf("default MaxSessions = %d, want %d", p.opts.MaxSessions, DefaultMuxSessions)
	}
	if p.opts.MaxStreamsPerSession != DefaultMaxStreamsPerSession {
		t.Errorf("default MaxStreamsPerSession = %d, want %d", p.opts.MaxStreamsPerSession, DefaultMaxStreamsPerSession)
	}
	if p.opts.RotateAfter != 6*time.Hour {
		t.Errorf("default RotateAfter = %v, want 6h", p.opts.RotateAfter)
	}
	if p.opts.RotateGrace != 5*time.Minute {
		t.Errorf("default RotateGrace = %v, want 5m", p.opts.RotateGrace)
	}
	if p.opts.Logger == nil {
		t.Error("default Logger should be slog.Default()")
	}
}

// --- Rotation tests ---

// rotationOpts returns options tuned for fast deterministic rotation
// behaviour in tests.
func rotationOpts(maxSessions int, rotateAfter, rotateGrace time.Duration) MuxPoolOptions {
	return MuxPoolOptions{
		MaxSessions:          maxSessions,
		MaxStreamsPerSession: 8,
		RotateAfter:          rotateAfter,
		RotateGrace:          rotateGrace,
	}
}

// waitFor polls the predicate until it returns true or the timeout
// expires. Polling avoids relying on a single fragile sleep.
func waitFor(t *testing.T, timeout time.Duration, pred func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if pred() {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return pred()
}

// TestMuxPool_RotationDrainGraceful: when rotateAt fires and inFlight
// is already 0 (or drops to 0 within grace), the session is evicted and
// its opener is closed. This is the primary "happy path" for rotation.
func TestMuxPool_RotationDrainGraceful(t *testing.T) {
	factory, created := fakeFactory(t, nil)
	pool := newTestPool(t, rotationOpts(2, 30*time.Millisecond, 1*time.Second), factory)

	s, err := pool.OpenStream(context.Background())
	if err != nil {
		t.Fatalf("OpenStream: %v", err)
	}
	_ = s.Close() // inFlight back to 0; rotation drains immediately when it fires

	if !waitFor(t, 2*time.Second, func() bool {
		return (*created)[0].closed.Load() && pool.sessionCount() == 0
	}) {
		t.Fatalf("rotation did not evict session: closed=%v sessionCount=%d",
			(*created)[0].closed.Load(), pool.sessionCount())
	}
}

// TestMuxPool_RotationForceCloseAfterGrace: when streams are still
// in-flight at rotateAt + RotateGrace, the watcher force-closes the
// session anyway. This is a destructive operation, but bounded by the
// grace window as a rotation-policy decision: we'd rather kill the
// session and let the pool dial a fresh one than let stuck streams
// pin a session past its intended rotation point. The grace window
// is NOT correctness-driven (Azure Relay tolerates sender WebSockets
// past SAS `se=`; see the relay probe memory) — it's a defensive
// bound on per-session lifetime.
func TestMuxPool_RotationForceCloseAfterGrace(t *testing.T) {
	factory, created := fakeFactory(t, nil)
	pool := newTestPool(t, rotationOpts(2, 20*time.Millisecond, 50*time.Millisecond), factory)

	s, err := pool.OpenStream(context.Background())
	if err != nil {
		t.Fatalf("OpenStream: %v", err)
	}
	defer s.Close() //nolint:errcheck // intentionally NOT closing before grace expires

	// Wait long enough for rotateAt (20ms) + grace (50ms) + slack.
	if !waitFor(t, 2*time.Second, func() bool {
		return (*created)[0].closed.Load()
	}) {
		t.Fatalf("opener was not force-closed after RotateGrace expired (closed=%v)",
			(*created)[0].closed.Load())
	}
	if pool.sessionCount() != 0 {
		t.Errorf("sessionCount = %d after force-rotation, want 0", pool.sessionCount())
	}
}

// TestMuxPool_RotationCreatesReplacementDuringDrain: with MaxSessions=1
// and a long-lived stream on session 0, when session 0 enters draining
// the pool must grow a NEW session for incoming streams rather than
// blocking or returning errors. Draining sessions don't count toward
// MaxSessions — this prevents rotation from stalling traffic.
func TestMuxPool_RotationCreatesReplacementDuringDrain(t *testing.T) {
	factory, created := fakeFactory(t, nil)
	// Long grace so the original session is still in 'draining' (not yet
	// evicted) when we try to open the second stream.
	pool := newTestPool(t, rotationOpts(1, 20*time.Millisecond, 5*time.Second), factory)

	first, err := pool.OpenStream(context.Background())
	if err != nil {
		t.Fatalf("first OpenStream: %v", err)
	}
	defer first.Close() //nolint:errcheck // hold open to keep session 0 in drain

	// Wait until session 0 is marked draining.
	if !waitFor(t, 2*time.Second, func() bool {
		pool.mu.Lock()
		defer pool.mu.Unlock()
		if len(pool.sessions) == 0 {
			return false
		}
		return pool.sessions[0].draining.Load()
	}) {
		t.Fatal("session 0 did not enter draining state")
	}

	// New stream during drain must grow a new session, NOT block on cap.
	second, err := pool.OpenStream(context.Background())
	if err != nil {
		t.Fatalf("second OpenStream during drain: %v", err)
	}
	defer second.Close() //nolint:errcheck // test cleanup

	if got := len(*created); got != 2 {
		t.Errorf("len(created) = %d, want 2 (drain should spawn replacement)", got)
	}
}

// TestMuxPool_RotationCancelsSessCtxOnEvict: the "no zombie reconnect"
// invariant. evictSession must cancel the per-session ctx BEFORE
// closing the opener so that any in-flight MuxDialer.OpenStream sees
// parentCtx cancellation and bails out instead of re-dialing.
func TestMuxPool_RotationCancelsSessCtxOnEvict(t *testing.T) {
	factory, created := fakeFactory(t, nil)
	pool := newTestPool(t, rotationOpts(2, 20*time.Millisecond, 1*time.Second), factory)

	s, err := pool.OpenStream(context.Background())
	if err != nil {
		t.Fatalf("OpenStream: %v", err)
	}
	_ = s.Close()

	if !waitFor(t, 2*time.Second, func() bool {
		return (*created)[0].closed.Load()
	}) {
		t.Fatal("rotation did not close opener")
	}

	if err := (*created)[0].ctx.Err(); err == nil {
		t.Error("expected per-session ctx to be cancelled after eviction, but ctx.Err() is nil")
	}
}

// TestMuxPool_RotationWatcherExitsOnPoolClose: regression guard against
// watcher-goroutine leaks. When the pool is closed, every rotation
// watcher must terminate. In practice Close closes each session's
// `evicted` channel before cancelling poolCtx, so watchers parked on
// the rotation timer normally exit via the s.evicted select arm; the
// poolCtx.Done() arm is the defensive fallback. This test asserts the
// invariant the user cares about: no leaked watcher goroutine after
// Close returns.
func TestMuxPool_RotationWatcherExitsOnPoolClose(t *testing.T) {
	factory, _ := fakeFactory(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Long rotateAfter so the watcher is parked on the rotation timer
	// (not the grace timer) when we close.
	pool := newMuxPoolWithFactory(ctx, rotationOpts(1, 1*time.Hour, 1*time.Hour), factory)

	s, err := pool.OpenStream(context.Background())
	if err != nil {
		t.Fatalf("OpenStream: %v", err)
	}
	_ = s.Close()
	pool.Close()

	// goleak isn't pulled in; use a small bounded sleep to give any leaked
	// goroutine a chance to misbehave, then verify pool ctx is dead.
	time.Sleep(20 * time.Millisecond)
	if err := pool.poolCtx.Err(); err == nil {
		t.Error("pool ctx should be cancelled after Close()")
	}
}

// closeWriteCountingConn is a minimal net.Conn that records whether
// CloseWrite() (vs Close()) was called. Used to verify *pooledStream's
// CloseWrite delegation.
type closeWriteCountingConn struct {
	net.Conn
	closeWriteCalls atomic.Int32
	closeCalls      atomic.Int32
}

func (c *closeWriteCountingConn) CloseWrite() error {
	c.closeWriteCalls.Add(1)
	return nil
}

func (c *closeWriteCountingConn) Close() error {
	c.closeCalls.Add(1)
	return c.Conn.Close()
}

// closeOnlyConn is a minimal net.Conn that does NOT implement
// CloseWrite. *pooledStream.CloseWrite must fall back to Close() so the
// peer still gets a definite signal.
type closeOnlyConn struct {
	net.Conn
	closeCalls atomic.Int32
}

func (c *closeOnlyConn) Close() error {
	c.closeCalls.Add(1)
	return c.Conn.Close()
}

// TestPooledStream_CloseWriteDelegates is the regression test for the
// half-close bug where pooledStream embedded net.Conn (the interface,
// not the concrete *smux.Stream) and therefore did NOT promote
// CloseWrite — so the metrics bridge fell back to Close() on EOF,
// tearing down the whole stream before the peer could drain remaining
// bytes for half-close-aware protocols (HTTP/1.0, SSH, SMTP).
func TestPooledStream_CloseWriteDelegates(t *testing.T) {
	factory, _ := fakeFactory(t, nil)
	pool := newTestPool(t, MuxPoolOptions{MaxSessions: 1, MaxStreamsPerSession: 1}, factory)
	// We don't actually open through the pool — we just need a real
	// pooledSession so newPooledStream's bookkeeping is consistent.
	stream, err := pool.OpenStream(context.Background())
	if err != nil {
		t.Fatalf("OpenStream (to obtain a real pooledSession): %v", err)
	}
	ps := stream.(*pooledStream)
	sess := ps.sess

	t.Run("delegates to CloseWrite when underlying supports it", func(t *testing.T) {
		a, _ := net.Pipe()
		t.Cleanup(func() { _ = a.Close() })
		cw := &closeWriteCountingConn{Conn: a}
		s := &pooledStream{Conn: cw, sess: sess, pool: pool}
		if err := s.CloseWrite(); err != nil {
			t.Fatalf("CloseWrite: %v", err)
		}
		if got := cw.closeWriteCalls.Load(); got != 1 {
			t.Errorf("CloseWrite calls = %d, want 1", got)
		}
		if got := cw.closeCalls.Load(); got != 0 {
			t.Errorf("Close calls = %d, want 0 (CloseWrite must not tear down both directions)", got)
		}
	})

	t.Run("falls back to Close when underlying lacks CloseWrite", func(t *testing.T) {
		a, _ := net.Pipe()
		t.Cleanup(func() { _ = a.Close() })
		co := &closeOnlyConn{Conn: a}
		s := &pooledStream{Conn: co, sess: sess, pool: pool}
		if err := s.CloseWrite(); err != nil {
			t.Fatalf("CloseWrite fallback: %v", err)
		}
		if got := co.closeCalls.Load(); got != 1 {
			t.Errorf("Close fallback calls = %d, want 1", got)
		}
	})
}

// TestMuxPool_SaturationCounterOnlyOnDeadline is the regression guard
// for the false-positive saturation counter the iter-8 Copilot review
// caught. The counter must increment ONLY when:
//   - the caller's ctx is DeadlineExceeded (a real saturation event), and
//   - the pool itself is NOT shutting down (poolCtx is alive).
//
// In particular, plain caller-side cancellation (ctx.Canceled) is NOT
// a saturation event — it's the application giving up. Counting those
// would inflate the metric and trigger ops alerts during normal
// shutdown.
func TestMuxPool_SaturationCounterOnlyOnDeadline(t *testing.T) {
	const role = "sender"

	t.Run("DeadlineExceeded increments", func(t *testing.T) {
		m := metrics.New()
		factory, _ := fakeFactory(t, nil)
		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(cancel)
		pool := newMuxPoolWithFactory(ctx,
			MuxPoolOptions{MaxSessions: 1, MaxStreamsPerSession: 1, Metrics: m},
			factory)
		t.Cleanup(pool.Close)

		first, err := pool.OpenStream(context.Background())
		if err != nil {
			t.Fatalf("first OpenStream: %v", err)
		}
		defer first.Close() //nolint:errcheck // test cleanup

		// Saturate: pool is full, this caller times out waiting.
		waitCtx, waitCancel := context.WithTimeout(context.Background(), 75*time.Millisecond)
		defer waitCancel()
		_, err = pool.OpenStream(waitCtx)
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("expected DeadlineExceeded, got %v", err)
		}
		if got := counterValue(t, m.Registry, "aztunnel_mux_pool_saturated_total", "role", role); got != 1 {
			t.Errorf("mux_pool_saturated_total{sender} = %v, want 1 after DeadlineExceeded", got)
		}
	})

	t.Run("plain Canceled does not increment", func(t *testing.T) {
		m := metrics.New()
		factory, _ := fakeFactory(t, nil)
		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(cancel)
		pool := newMuxPoolWithFactory(ctx,
			MuxPoolOptions{MaxSessions: 1, MaxStreamsPerSession: 1, Metrics: m},
			factory)
		t.Cleanup(pool.Close)

		first, err := pool.OpenStream(context.Background())
		if err != nil {
			t.Fatalf("first OpenStream: %v", err)
		}
		defer first.Close() //nolint:errcheck // test cleanup

		waitCtx, waitCancel := context.WithCancel(context.Background())
		errCh := make(chan error, 1)
		go func() {
			_, err := pool.OpenStream(waitCtx)
			errCh <- err
		}()
		// Give the caller a moment to enter the wait branch.
		time.Sleep(50 * time.Millisecond)
		waitCancel()

		select {
		case err := <-errCh:
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("expected Canceled, got %v", err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("OpenStream did not return after ctx cancel")
		}
		if got := counterValue(t, m.Registry, "aztunnel_mux_pool_saturated_total", "role", role); got != 0 {
			t.Errorf("mux_pool_saturated_total{sender} = %v after plain ctx.Cancel, want 0", got)
		}
	})

	t.Run("pool shutdown race does not increment", func(t *testing.T) {
		m := metrics.New()
		factory, _ := fakeFactory(t, nil)
		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(cancel)
		pool := newMuxPoolWithFactory(ctx,
			MuxPoolOptions{MaxSessions: 1, MaxStreamsPerSession: 1, Metrics: m},
			factory)

		first, err := pool.OpenStream(context.Background())
		if err != nil {
			t.Fatalf("first OpenStream: %v", err)
		}
		defer first.Close() //nolint:errcheck // test cleanup

		// Use a caller ctx that WILL hit DeadlineExceeded, but also
		// shut the pool down so poolCtx is Done. The race is real
		// but on shutdown we explicitly do NOT count it.
		waitCtx, waitCancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer waitCancel()
		errCh := make(chan error, 1)
		go func() {
			_, err := pool.OpenStream(waitCtx)
			errCh <- err
		}()
		// Shut down the pool while OpenStream is waiting.
		time.Sleep(30 * time.Millisecond)
		pool.Close()

		select {
		case <-errCh:
		// We don't assert which error wins the race between
		// DeadlineExceeded, ErrMuxPoolClosed, and Canceled —
		// only that the saturation counter is not incremented
		// when the pool itself is being torn down.
		case <-time.After(2 * time.Second):
			t.Fatal("OpenStream did not return after pool shutdown")
		}
		if got := counterValue(t, m.Registry, "aztunnel_mux_pool_saturated_total", "role", role); got != 0 {
			t.Errorf("mux_pool_saturated_total{sender} = %v while pool shutting down, want 0", got)
		}
	})
}

// TestMuxPool_OpenFailureEvictsAndRoutesToHealthyBusySession is the
// regression guard for the "non-Unsupported OpenStream failures leave a
// broken-but-idle candidate in the pool, which the selector keeps
// picking over a healthy busy session" finding. With the fix:
//
//  1. The broken session is evicted BEFORE its slot is released so a
//     concurrent waiter cannot reserve it through the release-broadcast
//     window.
//  2. The retry loop sets preferExisting so selectOrGrowLocked routes
//     to the healthy busy session's spare capacity (path 3) instead of
//     growing yet another fresh session that would hit the same outage.
//  3. The eviction is reported via RecordMuxRotation with
//     reason=MuxRotationOpenFailed for operator visibility.
//
// Without the fix, the call would either spin growing failed fresh
// sessions until maxEvictions and return the underlying error, or (on
// a later call) pick the broken session as an idle candidate and fail
// again.
func TestMuxPool_OpenFailureEvictsAndRoutesToHealthyBusySession(t *testing.T) {
	failErr := errors.New("simulated relay dial failure")
	factory, created := fakeFactory(t, func(id int, f *fakeOpener) {
		// Session id 1 fails its first OpenStream with a non-
		// Unsupported, non-DialerClosed error. With the fix the
		// pool evicts session 1 and routes the retry to session 0's
		// spare slot capacity. Session 0 (the healthy warmup) and
		// any later sessions succeed normally.
		if id == 1 {
			err := failErr
			f.failOnce.Store(&err)
		}
	})

	m := metrics.New()
	pool := newTestPool(t,
		MuxPoolOptions{MaxSessions: 2, MaxStreamsPerSession: 4, Metrics: m},
		factory)

	// Warm up session 0 and hold a stream so its slot stays busy. This
	// is what forces the selector to grow (path 2 → session 1) on the
	// next OpenStream call.
	warmup, err := pool.OpenStream(context.Background())
	if err != nil {
		t.Fatalf("warmup OpenStream: %v", err)
	}
	defer warmup.Close() //nolint:errcheck // test cleanup

	// Trigger the failure-and-route path. Without the fix this either
	// returns an error (growth-only retry burns the eviction budget on
	// fresh fail-on-first dials) or — on a follow-up call — picks the
	// broken-but-idle session 1 and fails again. With the fix, the
	// retry routes to session 0's spare capacity and succeeds.
	stream, err := pool.OpenStream(context.Background())
	if err != nil {
		t.Fatalf("OpenStream after failed-grow: got %v, want success via healthy busy session", err)
	}
	defer stream.Close() //nolint:errcheck // test cleanup

	// Only sessions 0 and 1 should have been created: session 0 (warmup,
	// healthy) and session 1 (grown then evicted). The retry must NOT
	// have grown a fresh session 2 — that would re-hit the outage.
	if got := len(*created); got != 2 {
		t.Errorf("len(created) = %d, want 2 (session 0 healthy + session 1 evicted; no fresh grow)", got)
	}

	// Session 1 must have been evicted (its opener.Close() called).
	if len(*created) > 1 && !(*created)[1].closed.Load() {
		t.Errorf("session 1 opener not closed; eviction did not happen")
	}

	// The returned stream must be on session 0 — that's the route-to-
	// healthy-busy guarantee. streamsOpen counts net.Pipe halves
	// currently held by the test (2 = warmup + this stream).
	if len(*created) > 0 {
		s0 := (*created)[0]
		s0.mu.Lock()
		open0 := s0.streamsOpen
		s0.mu.Unlock()
		if open0 != 2 {
			t.Errorf("session 0 streamsOpen = %d, want 2 (warmup + retry-routed stream)", open0)
		}
	}

	// Eviction must be recorded with reason=open_failed so operators
	// can distinguish session unhealth from scheduled rotation or
	// unsupported v1 rejection.
	if got := counterValue(t, m.Registry, "aztunnel_mux_rotations_total",
		"role", "sender", "reason", metrics.MuxRotationOpenFailed); got != 1 {
		t.Errorf("mux_rotations_total{role=sender, reason=%s} = %v, want 1",
			metrics.MuxRotationOpenFailed, got)
	}
}

// TestMuxPool_OpenFailurePersistentOutageReturnsError verifies that
// when every dial in the eviction budget fails (no healthy session in
// the pool to route to), the pool exhausts maxEvictions and surfaces
// the last underlying error rather than looping forever.
func TestMuxPool_OpenFailurePersistentOutageReturnsError(t *testing.T) {
	failErr := errors.New("simulated persistent relay outage")
	factory, created := fakeFactory(t, func(_ int, f *fakeOpener) {
		err := failErr
		f.failOnce.Store(&err)
	})
	pool := newTestPool(t,
		MuxPoolOptions{MaxSessions: 3, MaxStreamsPerSession: 4},
		factory)

	_, err := pool.OpenStream(context.Background())
	if err == nil {
		t.Fatalf("OpenStream succeeded against persistent outage; want failure")
	}
	if !errors.Is(err, failErr) {
		t.Errorf("OpenStream error = %v, want chain containing %v", err, failErr)
	}
	// maxEvictions = MaxSessions = 3 → at most 3 dial attempts.
	if got := len(*created); got > 3 {
		t.Errorf("len(created) = %d after persistent outage; want <= maxEvictions (3) — runaway loop?",
			got)
	}
}

// TestMuxPool_OpenFailureCallerCancelDoesNotEvict verifies that a
// caller-ctx cancellation does NOT tear down the selected session.
// The dialer surfaces ctx-caused errors as ErrMuxDialFailed wrapping
// context.Canceled / DeadlineExceeded; treating those as session
// unhealth would wrongly evict a perfectly good session and force
// the next caller to pay another relay rendezvous.
func TestMuxPool_OpenFailureCallerCancelDoesNotEvict(t *testing.T) {
	factory, created := fakeFactory(t, func(_ int, f *fakeOpener) {
		// holdOnOpenCh blocks OpenStream until the test sends; the
		// caller's ctx will fire first.
		f.holdOnOpenCh = make(chan struct{})
	})
	pool := newTestPool(t,
		MuxPoolOptions{MaxSessions: 2, MaxStreamsPerSession: 4},
		factory)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	_, err := pool.OpenStream(ctx)
	if err == nil {
		t.Fatalf("OpenStream succeeded; want caller-cancel error")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("OpenStream error = %v, want chain containing context.Canceled", err)
	}

	// Session 0 must still be in the pool — not evicted. We probe this
	// by asserting that its opener.Close() was NOT called.
	if got := len(*created); got != 1 {
		t.Fatalf("len(created) = %d, want 1", got)
	}
	if (*created)[0].closed.Load() {
		t.Errorf("session 0 was evicted on caller-cancel; want it preserved")
	}
}

// TestMuxPool_OpenFailurePreferExistingFallsBackToGrow guards against
// the "preferExisting deadlocks when every viable existing session is
// at MaxStreamsPerSession but growth is still allowed" scenario. With
// preferExisting set, selectOrGrowLocked tries existing least-loaded
// first, but MUST fall back to growth when no existing slot is
// reservable — otherwise the caller hangs on waitCh while pool budget
// remains.
func TestMuxPool_OpenFailurePreferExistingFallsBackToGrow(t *testing.T) {
	failErr := errors.New("simulated relay dial failure")
	factory, created := fakeFactory(t, func(id int, f *fakeOpener) {
		// Session id 1 (the first grow inside the test OpenStream)
		// fails. Session id 2 (the fallback grow after preferExisting
		// finds no reservable slot on sess 0) succeeds.
		if id == 1 {
			err := failErr
			f.failOnce.Store(&err)
		}
	})

	// MaxStreamsPerSession=1 forces sess 0 to be at cap once the
	// warmup stream is held. After the failure-and-evict, preferExisting
	// is set but sess 0 has no free slot — the retry must grow sess 2.
	pool := newTestPool(t,
		MuxPoolOptions{MaxSessions: 3, MaxStreamsPerSession: 1},
		factory)

	warmup, err := pool.OpenStream(context.Background())
	if err != nil {
		t.Fatalf("warmup OpenStream: %v", err)
	}
	defer warmup.Close() //nolint:errcheck // test cleanup

	// Trigger the failed grow + fallback grow path.
	streamCtx, streamCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer streamCancel()
	stream, err := pool.OpenStream(streamCtx)
	if err != nil {
		t.Fatalf("OpenStream with preferExisting fallback: got %v, want success via fresh grow", err)
	}
	defer stream.Close() //nolint:errcheck // test cleanup

	// Sessions 0 (warmup, healthy), 1 (failed, evicted), 2 (fresh grow,
	// healthy). preferExisting must not have suppressed the grow.
	if got := len(*created); got != 3 {
		t.Errorf("len(created) = %d, want 3 (warmup + failed-evict + fallback-grow)", got)
	}
}

// TestMuxPool_BudgetExhaustedWithV2_UsesExistingCapacity guards against
// the "hasV2 short-circuits to ErrMuxUnsupported even when an existing
// v2 session has spare stream capacity" finding. After the per-call
// eviction budget is exhausted (growth keeps landing on v1-only
// listeners), the pool must give an existing v2 session with spare
// capacity one last chance before falling back to v1 — otherwise
// mixed-fleet rollouts route traffic to v1 unnecessarily.
func TestMuxPool_BudgetExhaustedWithV2_UsesExistingCapacity(t *testing.T) {
	// Session 0 is v2 with spare capacity (MaxStreamsPerSession=4).
	// Sessions 1+ are v1-only: each first OpenStream returns
	// ErrMuxUnsupported and the opener becomes sticky-unavailable.
	factory, created := fakeFactory(t, func(id int, f *fakeOpener) {
		if id >= 1 {
			err := ErrMuxUnsupported
			f.failOnce.Store(&err)
			f.unavailable.Store(true)
		}
	})
	pool := newTestPool(t,
		MuxPoolOptions{MaxSessions: 2, MaxStreamsPerSession: 4},
		factory)

	// Warm up session 0 to inFlight=1 (still has 3 free slots).
	s0, err := pool.OpenStream(context.Background())
	if err != nil {
		t.Fatalf("warmup OpenStream: %v", err)
	}
	defer s0.Close() //nolint:errcheck // test cleanup

	// Drive the OpenStream retry loop into the budget-exhausted branch.
	// Selector path: session 0 is non-idle (inFlight=1) so step 1 skips
	// it; step 2 grows session 1 (v1-only, ErrMuxUnsupported), evict +
	// retry; selector hits MaxSessions=2 non-draining, falls to
	// pickLeastLoaded which reserves session 0 — and OpenStream on
	// session 0 should succeed.
	//
	// Note: This test also covers the case where the "preferExisting"
	// retry fails because the grown session is itself v1; the inline
	// one-shot in the budget-exhausted branch is the safety net.
	stream, err := pool.OpenStream(context.Background())
	if err != nil {
		t.Fatalf("OpenStream after budget exhaustion: got %v, want success via existing v2", err)
	}
	defer stream.Close() //nolint:errcheck // test cleanup

	// Session 0 must have served the second stream too — assert its
	// totalOpens is 2 (warmup + the budget-exhausted recovery).
	if len(*created) < 1 {
		t.Fatalf("len(created) = %d, want >=1", len(*created))
	}
	s0Opener := (*created)[0]
	s0Opener.mu.Lock()
	totalOpens := s0Opener.totalOpens
	s0Opener.mu.Unlock()
	if totalOpens != 2 {
		t.Errorf("session 0 totalOpens = %d, want 2 (warmup + budget-exhausted one-shot)", totalOpens)
	}

	// The pool-wide sticky cache MUST NOT be set: session 0 is still
	// v2 and usable. A follow-up OpenStream (after closing stream)
	// should succeed without re-dialing.
	_ = stream.Close()
	s2, err := pool.OpenStream(context.Background())
	if err != nil {
		t.Fatalf("follow-up OpenStream: got %v, want success (sticky cache must be unset)", err)
	}
	_ = s2.Close()
}

// TestMuxPool_BudgetExhaustedV2_LastChanceFailureEvictsAndRecords guards
// against the "broken existing v2 session lingers in the pool with no
// metric signal after a failed lastChance open" finding. When the
// one-shot pickLeastLoaded path hits a genuine session error (smux
// setup, handshake parse, or a stale ErrMuxDialerClosed), the pool
// must evict the session and record the rotation under
// MuxRotationOpenFailed before returning ErrMuxUnsupported — otherwise
// the broken session stays selectable for the next caller AND
// operators have no signal in aztunnel_mux_rotations_total that v2
// sessions are dying.
func TestMuxPool_BudgetExhaustedV2_LastChanceFailureEvictsAndRecords(t *testing.T) {
	failErr := errors.New("simulated mid-life v2 session failure")
	m := metrics.New()
	// Session 0 is v2 and survives the warmup. Sessions 1+ are v1-only
	// (ErrMuxUnsupported + sticky-unavailable) so growth in the retry
	// loop burns through the eviction budget and forces the hasV2
	// branch.
	factory, created := fakeFactory(t, func(id int, f *fakeOpener) {
		if id >= 1 {
			err := ErrMuxUnsupported
			f.failOnce.Store(&err)
			f.unavailable.Store(true)
		}
	})
	pool := newTestPool(t,
		MuxPoolOptions{MaxSessions: 2, MaxStreamsPerSession: 4, Metrics: m},
		factory)

	warmup, err := pool.OpenStream(context.Background())
	if err != nil {
		t.Fatalf("warmup OpenStream: %v", err)
	}
	defer warmup.Close() //nolint:errcheck // test cleanup

	// Arm session 0 to fail on its next OpenStream — this is the
	// lastChance call inside the budget-exhausted branch.
	(*created)[0].failOnce.Store(&failErr)

	// Drive the retry loop into budget exhaustion + hasV2 + lastChance
	// failure. Expected outcome: caller gets ErrMuxUnsupported (v1
	// fallback) AND session 0 is evicted AND mux_rotations_total{
	// reason=open_failed} = 1.
	_, err = pool.OpenStream(context.Background())
	if !errors.Is(err, ErrMuxUnsupported) {
		t.Fatalf("expected ErrMuxUnsupported (v1 fallback), got %v", err)
	}

	if !(*created)[0].closed.Load() {
		t.Errorf("session 0 opener not closed; lastChance failure must evict the broken session")
	}

	if got := counterValue(t, m.Registry, "aztunnel_mux_rotations_total",
		"role", "sender", "reason", metrics.MuxRotationOpenFailed); got != 1 {
		t.Errorf("mux_rotations_total{role=sender, reason=%s} = %v, want 1 (lastChance failure must record rotation)",
			metrics.MuxRotationOpenFailed, got)
	}
}

// TestMuxPool_OpenStreamHonorsPrecancelledCtx guards the contract that
// a caller whose ctx is already done at OpenStream entry receives
// ctx.Err() and does NOT get a stream. Without the upfront ctx check,
// the established-session fast path inside MuxDialer.OpenStream
// (which only checks parentCtx) would silently hand the caller a
// stream — weakening the timeout/cancellation guarantees the
// port-forward and SOCKS5 admission paths rely on.
func TestMuxPool_OpenStreamHonorsPrecancelledCtx(t *testing.T) {
	factory, created := fakeFactory(t, nil)
	m := metrics.New()
	pool := newTestPool(t,
		MuxPoolOptions{MaxSessions: 1, MaxStreamsPerSession: 4, Metrics: m},
		factory)

	// Warm session 0 so the fast path (established session, spare
	// capacity) is what a precancelled caller would otherwise reach.
	warmup, err := pool.OpenStream(context.Background())
	if err != nil {
		t.Fatalf("warmup OpenStream: %v", err)
	}
	defer warmup.Close() //nolint:errcheck // test cleanup

	// Precancel the caller's ctx, then call OpenStream. Must return
	// ctx.Canceled — not a stream.
	cancelledCtx, cancel := context.WithCancel(context.Background())
	cancel()
	stream, err := pool.OpenStream(cancelledCtx)
	if err == nil {
		_ = stream.Close()
		t.Fatalf("OpenStream returned a stream for precancelled ctx; want ctx.Canceled")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("OpenStream error = %v, want context.Canceled", err)
	}

	// Precancel handling must NOT increment the saturation counter —
	// that's for callers that ctx-expired waiting on a full pool, not
	// for callers who gave up before entering the wait.
	if got := counterValue(t, m.Registry, "aztunnel_mux_pool_saturated_total", "role", "sender"); got != 0 {
		t.Errorf("mux_pool_saturated_total{sender} = %v, want 0 for precancelled ctx", got)
	}

	// And the warmup-only session count must be unchanged — no extra
	// session was dialed for the precancelled caller.
	if got := len(*created); got != 1 {
		t.Errorf("len(created) = %d, want 1 (precancelled caller must not dial)", got)
	}
}
