package sender

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/philsphicas/aztunnel/internal/metrics"
	"github.com/philsphicas/aztunnel/internal/relay"
)

// ErrMuxPoolClosed is returned by MuxPool.OpenStream after the pool has
// been closed. It is distinct from net.ErrClosed so callers can
// distinguish "the pool went away" from "a particular stream's connection
// was closed".
var ErrMuxPoolClosed = errors.New("mux pool: closed")

// Pool sizing and rotation timing defaults. Exported so callers can
// log effective values before NewMuxPool is invoked (see PortForward
// and SOCKS5Proxy). See MuxPoolOptions.RotateAfter / RotateGrace for
// the rotation rationale.
const (
	// DefaultMuxSessions is the default cap on non-draining mux
	// sessions kept open by the pool (MuxPoolOptions.MaxSessions). 2
	// lets the pool grow lazily for bursty multi-stream workloads (and
	// spreads them across two HA listeners when available) while
	// staying at a single rendezvous for typical single-stream
	// workloads — `selectOrGrowLocked` keeps session 0 pinned until
	// concurrent traffic actually forces a grow.
	DefaultMuxSessions = 2

	// DefaultMaxStreamsPerSession is the default per-session stream
	// cap (MuxPoolOptions.MaxStreamsPerSession). Sized to comfortably
	// cover typical port-forward / SOCKS5 fan-out without saturating
	// smux's internal frame buffers.
	DefaultMaxStreamsPerSession = 256

	// defaultMuxRotateAfter is the default age at which a session is
	// marked for graceful rotation. Empirical probing against Azure
	// Relay shows that sender rendezvous WebSockets survive past their
	// SAS token's `se=` timestamp indefinitely as long as keepalives
	// flow — token validation happens at handshake only. So rotation
	// is primarily defensive (fleet rebalancing across HA listeners,
	// guarding against unknown long-term relay limits), not a
	// correctness requirement. 6h gives 4 rotations/day for HA spread
	// without disrupting typical long-lived streams.
	defaultMuxRotateAfter = 6 * time.Hour

	// defaultMuxRotateGrace bounds how long the rotation watcher waits
	// for in-flight streams to drain before force-closing the session.
	// Force-close is destructive: streams that overlap the rotation
	// boundary by more than the grace window are killed. At a 6h
	// cadence it fires rarely, but when it does, workloads that need
	// a single stream to live across the boundary uninterrupted
	// should pin the sender to v1 (--max-protocol-version=1) instead.
	defaultMuxRotateGrace = 5 * time.Minute
)

// muxOpener is the abstraction MuxPool needs from each session it
// manages. *MuxDialer implements this directly; tests inject fakes.
type muxOpener interface {
	// OpenStream opens a new logical stream over this session's underlying
	// mux. Returns ErrMuxUnsupported if the peer rejected the v2 handshake.
	OpenStream(ctx context.Context) (net.Conn, error)

	// Close tears down the underlying session and frees its resources.
	Close()

	// Unavailable reports whether this opener is in its sticky v1-fallback
	// window. The pool uses this to skip the session for new streams and
	// to decide when to propagate ErrMuxUnsupported to the caller.
	Unavailable() bool
}

// dialerFactory builds a fresh muxOpener bound to the supplied lifetime
// context. The pool calls this to grow lazily, passing a per-session
// cancelable ctx (NOT the pool ctx) so that eviction can permanently kill
// a single session without affecting other pool members. Production
// wires the factory to NewMuxDialer; tests inject fakes.
type dialerFactory func(sessCtx context.Context) muxOpener

// MuxPoolOptions configures a MuxPool. All fields are required unless
// noted otherwise. See NewMuxPool for defaults.
type MuxPoolOptions struct {
	Endpoint      string
	EntityPath    string
	TokenProvider relay.TokenProvider
	ClientOptions relay.ClientOptions
	Logger        *slog.Logger
	Metrics       *metrics.Metrics

	// MaxSessions caps the number of non-draining mux sessions the pool
	// will keep open at once. Draining sessions (in their rotation grace
	// window) do NOT count toward this limit, so a rotation never
	// stalls new streams. Larger values may spread load across multiple
	// HA listeners. Defaults to 1.
	MaxSessions int

	// MaxStreamsPerSession bounds the number of concurrent logical
	// streams per session. Stream openers block (with ctx) until a slot
	// frees up when all sessions are at this cap and the pool is at
	// MaxSessions. Defaults to 256.
	MaxStreamsPerSession int

	// RotateAfter is how long after a session is established it gets
	// flagged for graceful rotation. Rotation is primarily defensive
	// (HA fleet rebalancing and defence-in-depth against unknown
	// long-term relay limits); empirical probing shows Azure Relay does
	// not actively tear down rendezvous WebSockets at SAS token
	// expiry. Defaults to 6 hours when set to 0; set to a large value
	// (e.g. 24h) to effectively disable rotation in tests.
	RotateAfter time.Duration

	// RotateGrace bounds how long the rotation watcher waits for active
	// streams to finish naturally after the session begins draining.
	// On expiry the watcher force-closes the underlying mux, killing
	// any remaining streams. Defaults to 5 minutes.
	RotateGrace time.Duration
}

// MuxPool holds a set of persistent mux sessions and dispatches each new
// logical stream to one of them. The pool grows lazily up to
// MaxSessions whenever every existing session is currently servicing one
// or more in-flight streams; this gives bursty traffic a chance to spread
// across multiple Azure Relay rendezvous (and thus, in practice, multiple
// listeners in an HA fleet) without paying the cost of N rendezvous up
// front. Single-stream-at-a-time workloads stay on session 0 forever.
//
// Sessions are rotated periodically (see RotateAfter). Rotation is
// defensive — Azure Relay validates SAS tokens at handshake time only,
// and rendezvous WebSockets survive past the token's `se=` indefinitely
// as long as keepalives flow — so rotation primarily exists for HA fleet
// rebalancing and defence-in-depth against unknown long-term relay
// limits. Rotation drains gracefully when possible and force-closes
// after RotateGrace.
type MuxPool struct {
	opts    MuxPoolOptions
	factory dialerFactory

	// poolCtx is derived from the caller's ctx at construction; cancelled
	// by Close. Per-session ctxs are derived from poolCtx so that pool
	// teardown cancels every session.
	poolCtx context.Context
	cancel  context.CancelFunc

	mu       sync.Mutex
	sessions []*pooledSession
	notifyCh chan struct{} // close-and-replace on every slot release/close (broadcast wakeup)
	closed   bool

	// unsupportedUntil is a pool-level sticky cache for the v1-fallback
	// signal. When any session evicts due to ErrMuxUnsupported (the
	// listener is v1-only), we record the rejection here so subsequent
	// OpenStream calls fast-path to ErrMuxUnsupported without dialing.
	// Without this, MaxSessions > 1 against an all-v1 fleet would re-
	// dial and evict fresh v2 attempts on every local connection until
	// enough unavailable sessions accumulated to hit allUnavail — adding
	// repeated failed rendezvous latency. Mirrors the per-dialer
	// muxUnavailUntil TTL.
	unsupportedUntil time.Time
}

// pooledSession wraps a single muxOpener with bookkeeping. inFlight and
// slots together implement per-session admission control:
//   - slots is a buffered channel sized to MaxStreamsPerSession;
//     reserving a slot is a non-blocking send;
//     releasing is a single receive.
//   - inFlight is atomically updated alongside slot reservation so the
//     pool can pick the least-loaded session without taking p.mu.
//
// Rotation fields (set when the session is created):
//   - sessCtx / sessCancel: cancelled by evictSession to make the
//     underlying MuxDialer terminal — prevents zombie reconnects.
//   - birth / rotateAt: timestamps of this *pool slot*, not of the
//     underlying relay WebSocket. MuxDialer can transparently
//     reconnect after its smux session dies (see MuxDialer.OpenStream's
//     slow path), which gives the slot a fresher WebSocket without
//     updating birth/rotateAt — so the slot's rotation cadence is
//     decoupled from any individual WS's lifetime.
//   - draining: set true under p.mu when rotateAt is reached; thereafter
//     selectOrGrowLocked skips this session for new streams.
//   - drained: closed by releaseSlot exactly once when draining AND
//     inFlight reaches 0. The watcher selects on it to know when graceful
//     drain has completed.
//   - evicted: closed by evictSession exactly once. The watcher selects
//     on it to bail out if some other path (e.g. ErrMuxUnsupported)
//     evicted this session first.
type pooledSession struct {
	opener   muxOpener
	slots    chan struct{}
	inFlight atomic.Int64

	sessCtx    context.Context
	sessCancel context.CancelFunc

	birth    time.Time
	rotateAt time.Time

	draining    atomic.Bool
	drained     chan struct{}
	drainedOnce sync.Once
	evicted     chan struct{}
	evictedOnce sync.Once
	// recordOnce ensures the per-session lifecycle-exit metric is
	// emitted at most once per session. Without it, a watchRotation
	// that has passed the grace select and a Close() taking the same
	// session snapshot could both call RecordMuxRotation for the same
	// session, double-counting exits and ages.
	recordOnce sync.Once
}

// recordRotation emits the per-session lifecycle-exit metric for s
// exactly once. Despite the name, this covers all exit reasons
// (scheduled rotation, forced rotation, ErrMuxUnsupported, open-failed
// eviction, pool close) — not just scheduled rotations. Subsequent
// calls (from any path: watchRotation, Close, the ErrMuxUnsupported
// eviction path in OpenStream, the open-failed eviction path) are
// no-ops.
func (s *pooledSession) recordRotation(m *metrics.Metrics, reason string) {
	s.recordOnce.Do(func() {
		m.RecordMuxRotation("sender", time.Since(s.birth), reason)
	})
}

// NewMuxPool builds a pool bound to ctx. The returned pool dials lazily;
// the first OpenStream call creates session 0. Close is safe to call
// multiple times.
func NewMuxPool(ctx context.Context, opts MuxPoolOptions) *MuxPool {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.MaxSessions <= 0 {
		opts.MaxSessions = DefaultMuxSessions
	}
	if opts.MaxStreamsPerSession <= 0 {
		opts.MaxStreamsPerSession = DefaultMaxStreamsPerSession
	}
	if opts.RotateAfter <= 0 {
		opts.RotateAfter = defaultMuxRotateAfter
	}
	if opts.RotateGrace <= 0 {
		opts.RotateGrace = defaultMuxRotateGrace
	}
	poolCtx, cancel := context.WithCancel(ctx)
	p := &MuxPool{
		opts:     opts,
		poolCtx:  poolCtx,
		cancel:   cancel,
		notifyCh: make(chan struct{}),
	}
	p.factory = func(sessCtx context.Context) muxOpener {
		return NewMuxDialer(sessCtx, opts.Endpoint, opts.EntityPath, opts.TokenProvider, opts.ClientOptions, opts.Logger, opts.Metrics)
	}
	return p
}

// newMuxPoolWithFactory is the test-only constructor that lets a test
// inject a fake muxOpener factory. Behaviour is identical to NewMuxPool
// in production.
func newMuxPoolWithFactory(ctx context.Context, opts MuxPoolOptions, factory dialerFactory) *MuxPool {
	p := NewMuxPool(ctx, opts)
	p.factory = factory
	return p
}

// OpenStream returns a new net.Conn-wrapped logical stream. The supplied
// ctx scopes the underlying handshake; it does NOT govern the lifetime
// of the returned stream (the caller's bridge owns that).
//
// Behaviour:
//   - If a session is idle (inFlight == 0) and available, it is reused.
//   - Otherwise, if the pool has room to grow (< MaxSessions), a fresh
//     session is opened. This is what gives bursty workloads HA spread.
//   - Otherwise, the least-loaded existing session is used (subject to
//     its MaxStreamsPerSession cap).
//   - If every session is at the per-session cap AND the pool is at
//     MaxSessions, the call blocks until a slot frees (or ctx expires).
//   - If every session is in the sticky v1-fallback window AND the pool
//     is at MaxSessions, ErrMuxUnsupported is returned so the caller can
//     fall through to v1.
func (p *MuxPool) OpenStream(ctx context.Context) (net.Conn, error) {
	start := time.Now()
	// Bound the number of evictions per OpenStream call. Without this,
	// growing-then-evicting an unsupported session loops forever when
	// the relay only hosts v1 listeners. The bound is MaxSessions
	// because that's a reasonable retry budget — but Azure Relay's
	// listener selection is opaque (load balancing is non-uniform per
	// the multi-listener e2e probe), so this is not a *guarantee* of
	// reaching a v2-capable listener within MaxSessions tries. After
	// the budget is exhausted, the pool-level sticky cache short-
	// circuits subsequent calls to ErrMuxUnsupported for
	// muxUnavailableTTL, so retry cost stays bounded across the whole
	// run.
	maxEvictions := p.opts.MaxSessions
	if maxEvictions <= 0 {
		maxEvictions = 1
	}
	evictions := 0
	// preferExisting flips true after we evict a session for a non-
	// Unsupported open failure. It instructs selectOrGrowLocked to skip
	// the "grow a fresh session" path when an existing healthy non-
	// draining session is available, so we route to that session's
	// spare stream capacity instead of churning another fresh dial
	// against the same outage. Reset semantics: this is a per-call
	// flag; a fresh OpenStream invocation starts with it false.
	preferExisting := false

	for {
		// Honor caller-ctx cancellation/expiry BEFORE reserving a
		// slot. Without this, a caller whose ctx was already done at
		// entry (e.g. an admission timeout that fired just before the
		// goroutine got CPU time) would still receive a stream — the
		// established-session fast path inside MuxDialer.OpenStream
		// only checks parentCtx, not the caller's ctx. Checking here
		// also closes the corresponding race for callers that cancel
		// before entry. Bonus: this is the only way the saturation
		// metric stays clean — a precancelled caller must not race the
		// `select` below.
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		p.mu.Lock()
		if p.closed {
			p.mu.Unlock()
			return nil, ErrMuxPoolClosed
		}
		// If the parent ctx (passed to NewMuxPool) was cancelled
		// WITHOUT pool.Close() being called, poolCtx is done but
		// p.closed is still false. Every session's dialer ctx is
		// derived from poolCtx, so each opener.OpenStream returns
		// ErrMuxDialerClosed and the retry loop below would spin
		// indefinitely. Treat a done poolCtx as closed so shutdown
		// returns deterministically.
		if p.poolCtx.Err() != nil {
			p.mu.Unlock()
			return nil, ErrMuxPoolClosed
		}
		// Pool-level sticky v1-fallback: if a recent eviction confirmed
		// the listener fleet is v1-only, fast-path to ErrMuxUnsupported
		// for muxUnavailableTTL so we don't re-pay the rendezvous cost
		// on every new local connection.
		if !p.unsupportedUntil.IsZero() && time.Now().Before(p.unsupportedUntil) {
			p.mu.Unlock()
			return nil, ErrMuxUnsupported
		}
		sess, allUnavail := p.selectOrGrowLocked(preferExisting)
		waitCh := p.notifyCh
		p.mu.Unlock()

		if allUnavail {
			p.mu.Lock()
			p.unsupportedUntil = time.Now().Add(muxUnavailableTTL)
			p.mu.Unlock()
			return nil, ErrMuxUnsupported
		}

		if sess != nil {
			stream, err := sess.opener.OpenStream(ctx)
			if err != nil {
				if errors.Is(err, ErrMuxUnsupported) {
					evictions++
					// Evict the now-known-v1 session so the slot
					// frees and a fresh rendezvous can be attempted.
					// Evict BEFORE releasing the slot so a concurrent
					// waiter cannot reserve this session in the window
					// between releaseSlot's broadcast and eviction.
					sess.recordRotation(p.opts.Metrics, metrics.MuxRotationUnsupported)
					p.evictSession(sess)
					p.releaseSlot(sess)
					if evictions >= maxEvictions {
						// We've exhausted the retry budget against an
						// all-v1 listener fleet. Set the pool-level
						// sticky cache so subsequent OpenStream calls
						// within muxUnavailableTTL fast-path to
						// ErrMuxUnsupported without re-dialing — but
						// ONLY if we have no working v2 session left.
						// With MaxSessions > 1, a single busy v2
						// session can coexist in another slot with
						// repeated growth failures here; setting the
						// pool-wide cache then would incorrectly
						// disable mux for traffic that could still
						// reuse the existing v2 session.
						p.mu.Lock()
						hasV2 := false
						for _, s := range p.sessions {
							if !s.draining.Load() && !s.opener.Unavailable() {
								hasV2 = true
								break
							}
						}
						if !hasV2 {
							p.unsupportedUntil = time.Now().Add(muxUnavailableTTL)
							p.mu.Unlock()
							return nil, ErrMuxUnsupported
						}
						// hasV2 = true: an existing v2 session may have
						// spare stream capacity that the selector's
						// grow-first ordering bypassed (the session was
						// non-idle, so path 2 grew instead of trying
						// pickLeastLoaded). Make one last attempt to
						// reserve a slot on the least-loaded existing
						// session before falling back to v1. If no slot
						// is reservable OR the open fails, the caller
						// gets ErrMuxUnsupported and uses v1 — a
						// bounded operation — rather than blocking on
						// the existing slot to free (potentially
						// unbounded if the session has long-lived
						// streams).
						lastChance := p.pickLeastLoadedLocked()
						p.mu.Unlock()
						if lastChance == nil {
							return nil, ErrMuxUnsupported
						}
						stream, err := lastChance.opener.OpenStream(ctx)
						if err != nil {
							// Caller-cancellation: surface the real
							// reason, do not disturb the session's
							// pool state. The session itself may
							// still be healthy and useful to other
							// callers.
							if ctx.Err() != nil &&
								(errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)) {
								p.releaseSlot(lastChance)
								return nil, fmt.Errorf("mux pool: open stream: %w", err)
							}
							// Genuine session-side failure (smux
							// setup, handshake parse, ErrMuxDialerClosed
							// from a concurrent eviction, or a rare
							// mid-life ErrMuxUnsupported). Evict the
							// session and record the rotation so it
							// can't be reselected and operators see
							// the signal in
							// aztunnel_mux_rotations_total{reason=
							// "open_failed"|"unsupported"}, mirroring
							// the main retry loop's default failure
							// handling. Then return ErrMuxUnsupported
							// so the caller falls back to v1 — the
							// one-shot is best-effort: a failure here
							// should not turn into a hard connection
							// error when v1 is available.
							reason := metrics.MuxRotationOpenFailed
							if errors.Is(err, ErrMuxUnsupported) {
								reason = metrics.MuxRotationUnsupported
							}
							lastChance.recordRotation(p.opts.Metrics, reason)
							p.evictSession(lastChance)
							p.releaseSlot(lastChance)
							// If the last-chance attempt itself
							// confirmed v1 (ErrMuxUnsupported), the
							// pool may now hold no v2-capable session.
							// Mirror the maxEvictions-exhaustion arm
							// above: re-scan and arm the pool-wide
							// sticky cache so subsequent OpenStream
							// calls fast-path to ErrMuxUnsupported
							// without re-dialing for muxUnavailableTTL.
							// Only ErrMuxUnsupported justifies the
							// cache; an open_failed result is a
							// session-side error and doesn't prove the
							// listener fleet is v1-only.
							if errors.Is(err, ErrMuxUnsupported) {
								p.mu.Lock()
								hasV2 := false
								for _, s := range p.sessions {
									if !s.draining.Load() && !s.opener.Unavailable() {
										hasV2 = true
										break
									}
								}
								if !hasV2 {
									p.unsupportedUntil = time.Now().Add(muxUnavailableTTL)
								}
								p.mu.Unlock()
							}
							return nil, ErrMuxUnsupported
						}
						p.opts.Metrics.ObserveMuxStreamOpen("sender", time.Since(start).Seconds())
						return newPooledStream(stream, lastChance, p), nil
					}
					// A fresh rendezvous may land on a v2 listener.
					continue
				}
				if errors.Is(err, ErrMuxDialerClosed) {
					// This session was evicted (by rotation or some
					// other path) between selector and Open. Retry on
					// a different session.
					p.releaseSlot(sess)
					continue
				}
				// Caller cancellation surfaces as a context-typed
				// error wrapped by the dialer (e.g. ErrMuxDialFailed
				// wrapping context.Canceled). Don't disturb the pool's
				// session state — the session itself may still be
				// healthy and useful to other callers. The narrow
				// guard (err must itself be context-caused) prevents a
				// real session failure from being preserved just
				// because the caller's ctx happened to expire after
				// the failure but before we got here.
				if ctx.Err() != nil &&
					(errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)) {
					p.releaseSlot(sess)
					return nil, fmt.Errorf("mux pool: open stream: %w", err)
				}
				// The session's dialer is in an unhealthy state: the
				// relay dial, handshake, or smux setup failed and its
				// underlying session is now nil. Evict so the
				// selector doesn't keep picking this broken-but-idle
				// candidate over healthy busy sessions in other
				// slots, and set preferExisting so the retry routes
				// to those healthy sessions (sharing their spare
				// stream capacity) instead of growing another fresh
				// session that would hit the same outage. Bounded by
				// the same eviction budget as the ErrMuxUnsupported
				// path so a persistent outage cannot loop
				// indefinitely; the caller's next OpenStream starts
				// with a fresh budget.
				evictions++
				sess.recordRotation(p.opts.Metrics, metrics.MuxRotationOpenFailed)
				p.evictSession(sess)
				p.releaseSlot(sess)
				if evictions >= maxEvictions {
					return nil, fmt.Errorf("mux pool: open stream: %w", err)
				}
				preferExisting = true
				continue
			}
			p.opts.Metrics.ObserveMuxStreamOpen("sender", time.Since(start).Seconds())
			return newPooledStream(stream, sess, p), nil
		}

		// All sessions full and pool at MaxSessions. Wait.
		select {
		case <-waitCh:
		case <-ctx.Done():
			// Only count genuine pool saturation: caller's
			// deadline expired while waiting AND we aren't in
			// pool shutdown. Plain ctx.Canceled (caller-initiated
			// shutdown) or a poolCtx race with ctx is not a
			// saturation event.
			if errors.Is(ctx.Err(), context.DeadlineExceeded) && p.poolCtx.Err() == nil {
				p.opts.Metrics.MuxPoolSaturated("sender")
			}
			return nil, ctx.Err()
		case <-p.poolCtx.Done():
			return nil, ErrMuxPoolClosed
		}
	}
}

// selectOrGrowLocked is called with p.mu held. It either reserves a slot
// on an existing/new session and returns it, returns (nil, true) if all
// pool slots are exhausted by sticky-unavailable sessions (signalling v1
// fallback), or returns (nil, false) if the caller should wait.
//
// Draining sessions (those in their rotation grace window) are skipped
// in every path and are NOT counted toward MaxSessions — so a rotation
// in progress never stalls new streams.
//
// preferExisting=true reorders the selector so existing-session
// least-loaded selection is tried BEFORE growing a fresh session. The
// OpenStream retry loop sets this after evicting a session that failed
// to open a stream (dial/handshake/smux setup error), so the retry
// routes to spare stream capacity on a healthy busy session rather
// than churning another fresh dial that would likely hit the same
// outage. Growth is still allowed as a fallback when no existing
// session has a reservable slot, so the caller doesn't hang on
// waitCh while pool budget remains.
func (p *MuxPool) selectOrGrowLocked(preferExisting bool) (*pooledSession, bool) {
	// 1. Look for an idle, available, non-draining session — single-
	//    stream-at-a-time workloads stay on session 0 forever, avoiding
	//    wasted relay dials.
	for _, s := range p.sessions {
		if s.draining.Load() {
			continue
		}
		if !s.opener.Unavailable() && s.inFlight.Load() == 0 {
			if p.tryReserveLocked(s) {
				return s, false
			}
		}
	}

	// preferExisting routes the retry to spare capacity on an
	// existing healthy session before attempting growth. If no
	// existing session has a reservable slot, falls through to the
	// growth path below.
	if preferExisting {
		if best := p.pickLeastLoadedLocked(); best != nil {
			return best, false
		}
	}

	// 2. If no idle session exists and the non-draining session count is
	//    below MaxSessions, grow. Bursty traffic ends up spread across
	//    new sessions, each of which may land on a different listener.
	//    Draining sessions don't count toward the cap so a rotation
	//    cannot starve new streams.
	nonDraining := 0
	for _, s := range p.sessions {
		if !s.draining.Load() {
			nonDraining++
		}
	}
	if nonDraining < p.opts.MaxSessions {
		ns := p.newSessionLocked()
		p.tryReserveLocked(ns) // always succeeds — fresh channel
		return ns, false
	}

	// 3. Pool at MaxSessions non-draining sessions. Pick the least-
	//    loaded available, non-draining session that still has a free
	//    slot. (Already attempted above if preferExisting was set, but
	//    the pool composition can change under p.mu between the two
	//    calls only via paths that hold p.mu — and we've held it the
	//    whole time. The second call is harmless: it just returns nil
	//    again. Avoiding it would require carrying the result through
	//    the growth branch, which doesn't improve clarity.)
	if best := p.pickLeastLoadedLocked(); best != nil {
		return best, false
	}

	// 4. No slot free among non-draining sessions. Are ALL non-draining
	//    sessions sticky-unavailable? (Draining sessions are excluded —
	//    they're on their way out and will be replaced shortly.)
	allUnavail := nonDraining > 0
	for _, s := range p.sessions {
		if s.draining.Load() {
			continue
		}
		if !s.opener.Unavailable() {
			allUnavail = false
			break
		}
	}
	return nil, allUnavail
}

// pickLeastLoadedLocked returns a reserved slot on the least-loaded
// non-draining, non-unavailable session that still has free capacity.
// Returns nil if no such session exists. Caller must hold p.mu.
//
// Reserves slots speculatively on each candidate to avoid TOCTOU
// between inFlight read and reservation, then unreserves losers.
func (p *MuxPool) pickLeastLoadedLocked() *pooledSession {
	var best *pooledSession
	for _, s := range p.sessions {
		if s.draining.Load() || s.opener.Unavailable() {
			continue
		}
		if p.tryReserveLocked(s) {
			if best == nil || s.inFlight.Load() < best.inFlight.Load() {
				if best != nil {
					p.unreserveLocked(best)
				}
				best = s
			} else {
				p.unreserveLocked(s)
			}
		}
	}
	return best
}

// newSessionLocked constructs a fresh pooledSession with its own
// cancelable ctx (rooted at poolCtx), appends it to p.sessions, and
// starts the rotation watcher goroutine. Caller must hold p.mu.
func (p *MuxPool) newSessionLocked() *pooledSession {
	now := time.Now()
	sessCtx, sessCancel := context.WithCancel(p.poolCtx)
	ns := &pooledSession{
		opener:     p.factory(sessCtx),
		slots:      make(chan struct{}, p.opts.MaxStreamsPerSession),
		sessCtx:    sessCtx,
		sessCancel: sessCancel,
		birth:      now,
		rotateAt:   now.Add(p.opts.RotateAfter),
		drained:    make(chan struct{}),
		evicted:    make(chan struct{}),
	}
	p.sessions = append(p.sessions, ns)
	go p.watchRotation(ns)
	return ns
}

// watchRotation runs for the lifetime of one pooledSession. It sleeps
// until rotateAt, marks the session draining (under p.mu, so it
// serialises with selectOrGrowLocked), waits for active streams to
// finish (or RotateGrace to elapse), then evicts the session. The
// watcher exits early if the session is evicted by some other path
// (ErrMuxUnsupported eviction in OpenStream, or pool Close).
func (p *MuxPool) watchRotation(s *pooledSession) {
	wait := time.Until(s.rotateAt)
	if wait < 0 {
		wait = 0
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()

	select {
	case <-timer.C:
	case <-p.poolCtx.Done():
		return
	case <-s.evicted:
		return
	}

	// Mark draining under p.mu so that:
	//   - any concurrent selectOrGrowLocked either reserves BEFORE this
	//     point (and thus is reflected in inFlight when we read it
	//     below), or sees draining=true and skips this session.
	//   - releaseSlot's "drain complete" close-of-drained race is
	//     resolved with our own check below.
	p.mu.Lock()
	s.draining.Store(true)
	alreadyDrained := s.inFlight.Load() == 0
	p.mu.Unlock()
	p.broadcast() // wake any waiters; they'll re-select past this session

	if alreadyDrained {
		s.drainedOnce.Do(func() { close(s.drained) })
	}

	graceTimer := time.NewTimer(p.opts.RotateGrace)
	defer graceTimer.Stop()
	reason := metrics.MuxRotationScheduled
	select {
	case <-s.drained:
		// Graceful: all streams finished within the grace window.
	case <-graceTimer.C:
		// Grace window expired with streams still in flight. Force-
		// close: this is a rotation-policy decision (we'd rather
		// drop a few in-flight streams now than let sessions live
		// indefinitely past our defensive 6h cap), not a correctness
		// requirement — Azure validates SAS at handshake time only,
		// so the WS itself would survive past rotateAfter as long
		// as keepalives flow.
		reason = metrics.MuxRotationForced
	case <-p.poolCtx.Done():
		return
	case <-s.evicted:
		return
	}

	age := time.Since(s.birth)
	p.opts.Logger.Info("mux session rotated",
		"age", age,
		"reason", reason,
		"in_flight_at_close", s.inFlight.Load())
	s.recordRotation(p.opts.Metrics, reason)
	p.evictSession(s)
}

// tryReserveLocked atomically reserves a slot on s and bumps its
// inFlight counter. Returns false if s is already at the per-session cap.
// Caller must hold p.mu.
func (p *MuxPool) tryReserveLocked(s *pooledSession) bool {
	select {
	case s.slots <- struct{}{}:
		s.inFlight.Add(1)
		return true
	default:
		return false
	}
}

// unreserveLocked is the inverse of tryReserveLocked. Caller must hold
// p.mu.
func (p *MuxPool) unreserveLocked(s *pooledSession) {
	<-s.slots
	s.inFlight.Add(-1)
}

// releaseSlot is called from the wrapped stream's Close, and from
// OpenStream's error paths. It is the lock-free release counterpart of
// tryReserveLocked + the broadcast wakeup.
//
// If this release brings inFlight to zero on a draining session, it
// signals the rotation watcher that graceful drain is complete.
func (p *MuxPool) releaseSlot(s *pooledSession) {
	<-s.slots
	rem := s.inFlight.Add(-1)
	if rem == 0 && s.draining.Load() {
		s.drainedOnce.Do(func() { close(s.drained) })
	}
	p.broadcast()
}

// broadcast wakes every waiter currently blocked in OpenStream. The
// close-and-replace pattern is used because a 1-buffered notify channel
// would coalesce wakeups and strand waiters when several slots free at
// once.
func (p *MuxPool) broadcast() {
	p.mu.Lock()
	old := p.notifyCh
	p.notifyCh = make(chan struct{})
	p.mu.Unlock()
	close(old)
}

// evictSession removes s from the pool's slice and closes its opener.
// Idempotent: a second call (e.g. from the rotation watcher after
// OpenStream already evicted on ErrMuxUnsupported) is a no-op.
//
// The session's ctx is cancelled BEFORE the session is removed from
// selector visibility (i.e. inside the p.mu critical section). This
// is what makes MuxDialer terminal: any in-flight
// MuxDialer.OpenStream that already selected this session and is
// about to call session.OpenStream observes parentCtx.Err() != nil
// on its post-OpenStream re-check and returns ErrMuxDialerClosed
// instead of returning a stream from a doomed session. The pool's
// retry loop then picks a fresh session.
//
// The actual opener.Close() (which tears down smux + websocket) is
// deferred outside the lock — it's expensive and does not re-enter
// MuxPool, so the lock can be released first.
func (p *MuxPool) evictSession(s *pooledSession) {
	p.mu.Lock()
	found := false
	for i, x := range p.sessions {
		if x == s {
			p.sessions = append(p.sessions[:i], p.sessions[i+1:]...)
			found = true
			break
		}
	}
	if found {
		s.evictedOnce.Do(func() { close(s.evicted) })
		s.sessCancel()
	}
	p.mu.Unlock()
	if !found {
		return
	}
	s.opener.Close()
	p.broadcast()
}

// Close tears down every session and makes future OpenStream calls fail
// with ErrMuxPoolClosed. Idempotent.
//
// Like evictSession, the per-session contexts are cancelled INSIDE the
// p.mu critical section — so concurrent OpenStream calls that already
// selected a session observe parentCtx.Err() != nil on their post-
// OpenStream re-check and return ErrMuxDialerClosed instead of a doomed
// stream. The pool's poolCtx is then cancelled (outside the lock) so
// new OpenStream callers fast-path to ErrMuxPoolClosed.
func (p *MuxPool) Close() {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.closed = true
	sessions := p.sessions
	p.sessions = nil
	for _, s := range sessions {
		s.evictedOnce.Do(func() { close(s.evicted) })
		s.sessCancel()
	}
	p.mu.Unlock()

	for _, s := range sessions {
		s.recordRotation(p.opts.Metrics, metrics.MuxRotationPoolClosed)
		s.opener.Close()
	}
	p.cancel()
	p.broadcast()
}

// sessionCount returns the number of live sessions currently tracked by
// the pool. Intended for tests that need to assert rotation eviction
// happened.
func (p *MuxPool) sessionCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.sessions)
}

// pooledStream wraps the smux.Stream net.Conn so Close() decrements the
// pool's slot bookkeeping. Idempotent: double-close is safe.
//
// CloseWrite is implemented to delegate to the underlying conn so that
// the half-close-aware bridge in metrics.TrackedStreamBridge actually
// works for mux streams. Embedding net.Conn alone does not promote
// CloseWrite from the underlying *smux.Stream (the embedded type is the
// interface, not the concrete) — without this method, the bridge falls
// back to Close() on EOF and tears down the whole stream before the
// peer can drain remaining bytes for half-close protocols (HTTP/1.0,
// SSH, SMTP).
type pooledStream struct {
	net.Conn
	sess      *pooledSession
	pool      *MuxPool
	closeOnce sync.Once
}

func newPooledStream(c net.Conn, sess *pooledSession, pool *MuxPool) *pooledStream {
	return &pooledStream{Conn: c, sess: sess, pool: pool}
}

func (s *pooledStream) Close() error {
	closeErr := s.Conn.Close()
	s.closeOnce.Do(func() {
		s.pool.releaseSlot(s.sess)
	})
	return closeErr
}

// CloseWrite signals end-of-input to the peer without tearing down the
// read direction. *smux.Stream implements CloseWrite; if the underlying
// conn does not, fall back to Close() so callers still get a definite
// signal (preserves prior behaviour for non-smux test fakes).
func (s *pooledStream) CloseWrite() error {
	if cw, ok := s.Conn.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return s.Conn.Close()
}
