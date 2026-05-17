// Package metrics provides Prometheus metrics for aztunnel.
package metrics

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"github.com/philsphicas/aztunnel/internal/bridgecause"
	"github.com/philsphicas/aztunnel/internal/relay"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
)

const namespace = "aztunnel"

// OverflowTarget is used as the target label when the number of unique
// targets exceeds MaxTargets.
const OverflowTarget = "__other__"

const (
	ReasonDialFailed        = "dial_failed"
	ReasonDialTimeout       = "dial_timeout"
	ReasonRelayFailed       = "relay_failed"
	ReasonAuthFailed        = "auth_failed"
	ReasonEnvelopeError     = "envelope_error"
	ReasonAllowlistRejected = "allowlist_rejected"
	// ReasonDNSNotFound is the reason label for dial failures caused by
	// name resolution returning no result (typically NXDOMAIN or NODATA).
	// Distinct from ReasonDialFailed so operators can see DNS-misconfig
	// failures separately from network-layer failures.
	ReasonDNSNotFound = "dns_not_found"
	// ReasonDNSTimeout is the reason label for dial failures caused by
	// DNS resolution exceeding the resolver's deadline. Distinct from
	// ReasonDialTimeout because the failure happened before any SYN was
	// sent; the underlying network may be fine.
	ReasonDNSTimeout = "dns_timeout"
	// ReasonListenerAtCapacity is recorded by the listener when an
	// incoming connection is rejected because the listener has hit
	// its own capacity cap (streamSem for active target streams, or
	// pendingSem for envelope-pending mux streams). Distinct from
	// ReasonRelayFailed (Azure Relay outage) so operators can tell
	// "we're at our configured limit" apart from "Azure had a
	// problem" in `aztunnel_connection_errors_total`.
	ReasonListenerAtCapacity = "listener_at_capacity"
	// ReasonMuxOpenFailed is the catch-all for non-dial, non-ctx
	// failures bubbling out of MuxPool.OpenStream (smux setup
	// failure, mux handshake parse error, listener rejection that
	// isn't the v1-fallback marker). Dial failures are already
	// recorded by MuxDialer.connectLocked (which calls DialWithRetry
	// directly and emits ConnectionError via DialReason itself), and
	// context cancellation is not a connection error, so neither
	// path records this reason — callers must filter those out
	// before recording.
	ReasonMuxOpenFailed = "mux_open_failed"
)

// Mux session lifecycle-exit reasons reported via RecordMuxRotation.
// Despite the "MuxRotation" prefix (kept for label backward
// compatibility), this group covers all paths that retire a pool
// session: scheduled / forced rotations, sticky v1-fallback evictions
// (`unsupported`), open-failure evictions (`open_failed`), and pool
// shutdown (`pool_closed`). Use these constants to keep label
// cardinality bounded.
const (
	MuxRotationScheduled   = "scheduled"
	MuxRotationForced      = "force_after_grace"
	MuxRotationUnsupported = "unsupported"
	MuxRotationPoolClosed  = "pool_closed"
	MuxRotationOpenFailed  = "open_failed"
)

// Metrics holds all Prometheus metrics for aztunnel.
type Metrics struct {
	Registry *prometheus.Registry

	// MaxTargets is the maximum number of unique target label values.
	// Once exceeded, new targets are recorded as OverflowTarget.
	// Zero means unlimited.
	MaxTargets int

	connectionsTotal   *prometheus.CounterVec
	connectionErrors   *prometheus.CounterVec
	bytesTotal         *prometheus.CounterVec
	activeConnections  *prometheus.GaugeVec
	controlChannelUp   prometheus.Gauge
	connectionDuration *prometheus.HistogramVec
	dialDuration       *prometheus.HistogramVec
	tokenFetchSeconds  *prometheus.HistogramVec
	tokenFetchTotal    *prometheus.CounterVec

	muxSessionsActive     *prometheus.GaugeVec
	muxStreamOpenDuration *prometheus.HistogramVec
	muxSessionAge         *prometheus.HistogramVec
	muxRotationsTotal     *prometheus.CounterVec
	muxPoolSaturatedTotal *prometheus.CounterVec

	targetCount atomic.Int64
	targets     sync.Map // map[string]struct{}
}

// New creates a new Metrics instance with a custom Prometheus registry.
func New() *Metrics {
	reg := prometheus.NewRegistry()
	reg.MustRegister(collectors.NewGoCollector())
	reg.MustRegister(collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))

	m := &Metrics{
		Registry: reg,

		connectionsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "connections_total",
			Help:      "Total connections that completed setup and entered bridging.",
		}, []string{"role", "target", "status"}),

		connectionErrors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "connection_errors_total",
			Help:      "Total number of connection errors, by reason.",
		}, []string{"role", "reason"}),

		bytesTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "bytes_total",
			Help:      "Total bytes transferred through the relay tunnel.",
		}, []string{"role", "target", "direction"}),

		activeConnections: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "active_connections",
			Help:      "Number of currently active bridged connections.",
		}, []string{"role", "target"}),

		controlChannelUp: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "control_channel_connected",
			Help:      "Whether the listener control channel is connected (1) or not (0).",
		}),

		connectionDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: namespace,
			Name:      "connection_duration_seconds",
			Help:      "Duration of completed connections in seconds.",
			Buckets:   []float64{1, 5, 15, 30, 60, 120, 300, 600, 1800, 3600},
		}, []string{"role", "target"}),

		dialDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: namespace,
			Name:      "dial_duration_seconds",
			Help:      "Time to establish outbound connections in seconds.",
			Buckets:   []float64{0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30},
		}, []string{"role"}),

		tokenFetchSeconds: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: namespace,
			Name:      "token_fetch_seconds",
			Help: "Latency of TokenProvider.GetToken calls. " +
				"Records end-to-end GetToken latency, including any " +
				"in-process caching the wrapped provider performs " +
				"(e.g. EntraTokenProvider's cache hits land as " +
				"sub-millisecond observations alongside underlying-" +
				"credential refreshes; SAS providers re-sign per call " +
				"and have no cache, so observations reflect the signing " +
				"cost). The tail extends to 60s to keep slow-path " +
				"signal (e.g. `az` shell-out cold start) bucketed.",
			Buckets: []float64{0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 15, 30, 60},
		}, []string{"provider", "result"}),

		tokenFetchTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "token_fetch_total",
			Help:      "Count of TokenProvider.GetToken calls by outcome.",
		}, []string{"provider", "result"}),

		muxSessionsActive: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "mux_sessions_active",
			Help:      "Number of currently active persistent mux sessions.",
		}, []string{"role"}),

		muxStreamOpenDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: namespace,
			Name:      "mux_stream_open_seconds",
			Help:      "Time from OpenStream call to smux stream open (pool admission + smux SYN; excludes the per-stream envelope handshake).",
			Buckets:   []float64{0.0005, 0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5},
		}, []string{"role"}),

		muxSessionAge: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: namespace,
			Name:      "mux_session_age_seconds",
			Help:      "Age of mux pool sessions at lifecycle exit (rotation or eviction). Measured from the pool slot's creation; the underlying relay WebSocket may have been transparently reconnected during the slot's lifetime, so this is slot age, not necessarily WS age.",
			// Buckets span up to 12h so the default 6h scheduled
			// rotation falls into a finite bucket (rather than +Inf)
			// and is observable in the distribution.
			Buckets: []float64{1, 30, 60, 300, 600, 1800, 3000, 3600, 7200, 10800, 21600, 32400, 43200},
		}, []string{"role"}),

		muxRotationsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "mux_rotations_total",
			Help:      "Total mux session lifecycle exits (rotations and evictions), by reason: scheduled, force_after_grace, unsupported, pool_closed, open_failed.",
		}, []string{"role", "reason"}),

		muxPoolSaturatedTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "mux_pool_saturated_total",
			Help:      "Total times a caller blocked on a saturated mux pool and gave up.",
		}, []string{"role"}),
	}

	reg.MustRegister(
		m.connectionsTotal,
		m.connectionErrors,
		m.bytesTotal,
		m.activeConnections,
		m.controlChannelUp,
		m.connectionDuration,
		m.dialDuration,
		m.tokenFetchSeconds,
		m.tokenFetchTotal,
		m.muxSessionsActive,
		m.muxStreamOpenDuration,
		m.muxSessionAge,
		m.muxRotationsTotal,
		m.muxPoolSaturatedTotal,
	)

	return m
}

// SanitizeTarget returns target if it is within the cardinality budget,
// or OverflowTarget if the cap has been reached. Targets that have been
// seen before are always returned as-is.
func (m *Metrics) SanitizeTarget(target string) string {
	if m == nil {
		return target
	}
	if m.MaxTargets <= 0 {
		return target
	}

	for {
		// Fast path: already-known target.
		if _, ok := m.targets.Load(target); ok {
			return target
		}

		cur := m.targetCount.Load()
		if cur >= int64(m.MaxTargets) {
			// Re-check: another goroutine may have stored this target
			// between our Load and this cap check.
			if _, ok := m.targets.Load(target); ok {
				return target
			}
			return OverflowTarget
		}

		// Try to reserve a slot atomically.
		if !m.targetCount.CompareAndSwap(cur, cur+1) {
			continue
		}

		// Slot reserved. Store the target, undoing the increment if
		// another goroutine stored it first.
		if _, loaded := m.targets.LoadOrStore(target, struct{}{}); loaded {
			m.targetCount.Add(-1)
		}

		return target
	}
}

// ConnectionOpened increments the active connection gauge and should be
// called when a bridge begins. Returns a ConnectionTracker to record the
// outcome when the connection ends. The target is sanitized through the
// cardinality guard.
func (m *Metrics) ConnectionOpened(role, target string) *ConnectionTracker {
	if m == nil {
		return nil
	}
	target = m.SanitizeTarget(target)
	m.activeConnections.WithLabelValues(role, target).Inc()
	return &ConnectionTracker{m: m, role: role, target: target}
}

// ConnectionError records a connection failure that did not reach the bridge.
func (m *Metrics) ConnectionError(role, reason string) {
	if m == nil {
		return
	}
	m.connectionErrors.WithLabelValues(role, reason).Inc()
}

// DialReason maps a dial error to a metric reason label. It returns
// ReasonDialTimeout for network timeouts, ReasonDNSTimeout for DNS
// resolver timeouts, ReasonDNSNotFound for non-timeout DNS failures, or
// fallback for any other error.
//
// The DNS-error classification is scoped to listener target dials by
// gating on `fallback == ReasonDialFailed`. Sender callers pass
// ReasonRelayFailed and skip the DNS branch entirely; their DNS errors
// fall through to the timeout / fallback paths below.
//
// Ordering mirrors classifyDialError in internal/listener: ctx-deadline
// is checked first so operator-cancelled dials keep the timeout
// classification; *net.DNSError is checked before the generic
// netErr.Timeout() branch because DNS timeouts also satisfy
// net.Error.Timeout() and would otherwise be misclassified as
// ReasonDialTimeout.
func DialReason(err error, fallback string) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return ReasonDialTimeout
	}
	if fallback == ReasonDialFailed {
		var dnsErr *net.DNSError
		if errors.As(err, &dnsErr) {
			if dnsErr.IsTimeout {
				return ReasonDNSTimeout
			}
			return ReasonDNSNotFound
		}
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return ReasonDialTimeout
	}
	return fallback
}

// ObserveDialDuration records how long an outbound dial took.
func (m *Metrics) ObserveDialDuration(role string, seconds float64) {
	if m == nil {
		return
	}
	m.dialDuration.WithLabelValues(role).Observe(seconds)
}

// ObserveTokenFetch records the latency and outcome of a single
// TokenProvider.GetToken call. Safe to call on a nil receiver — observation
// is dropped silently in that case so callers can wire the observer
// unconditionally and let the call site decide whether metrics are enabled.
func (m *Metrics) ObserveTokenFetch(provider, result string, durationSec float64) {
	if m == nil {
		return
	}
	m.tokenFetchSeconds.WithLabelValues(provider, result).Observe(durationSec)
	m.tokenFetchTotal.WithLabelValues(provider, result).Inc()
}

// SetControlChannelConnected sets the control channel gauge.
func (m *Metrics) SetControlChannelConnected(up bool) {
	if m == nil {
		return
	}
	if up {
		m.controlChannelUp.Set(1)
	} else {
		m.controlChannelUp.Set(0)
	}
}

// MuxSessionOpened increments the active mux sessions gauge. Safe to
// call on a nil receiver.
func (m *Metrics) MuxSessionOpened(role string) {
	if m == nil {
		return
	}
	m.muxSessionsActive.WithLabelValues(role).Inc()
}

// MuxSessionClosed decrements the active mux sessions gauge. Safe to
// call on a nil receiver. Use RecordMuxRotation in addition when the
// close is a pool-driven rotation (so the age histogram and rotation
// reason counter are updated).
func (m *Metrics) MuxSessionClosed(role string) {
	if m == nil {
		return
	}
	m.muxSessionsActive.WithLabelValues(role).Dec()
}

// RecordMuxRotation observes the age at which a mux pool session was
// retired and increments the per-reason lifecycle-exit counter. The
// "Rotation" in the name is historical; this covers all exit paths
// (scheduled / forced rotations, sticky v1-fallback evictions,
// open-failure evictions, and pool shutdown). Age measured from the
// pool slot's birth — note that MuxDialer can transparently reconnect
// the underlying WebSocket during a slot's lifetime, so the observed
// age describes the pool slot, not necessarily the currently active
// relay WS. Safe to call on a nil receiver.
func (m *Metrics) RecordMuxRotation(role string, age time.Duration, reason string) {
	if m == nil {
		return
	}
	m.muxSessionAge.WithLabelValues(role).Observe(age.Seconds())
	m.muxRotationsTotal.WithLabelValues(role, reason).Inc()
}

// ObserveMuxStreamOpen records the time from pool.OpenStream entry to
// smux stream open (pool admission + smux SYN). The per-stream envelope
// handshake runs after this point and is NOT included in this histogram.
// Safe to call on a nil receiver.
func (m *Metrics) ObserveMuxStreamOpen(role string, seconds float64) {
	if m == nil {
		return
	}
	m.muxStreamOpenDuration.WithLabelValues(role).Observe(seconds)
}

// MuxPoolSaturated increments the pool-saturation counter, indicating a
// caller blocked on a full pool and ultimately gave up (ctx expired).
// Safe to call on a nil receiver.
func (m *Metrics) MuxPoolSaturated(role string) {
	if m == nil {
		return
	}
	m.muxPoolSaturatedTotal.WithLabelValues(role).Inc()
}

// ConnectionTracker records the outcome of a single bridged connection.
type ConnectionTracker struct {
	m      *Metrics
	role   string
	target string
}

// Done records the completion of a connection. toRelayBytes is data sent
// into the relay (local endpoint → relay); fromRelayBytes is data received
// from the relay (relay → local endpoint).
func (t *ConnectionTracker) Done(durationSec float64, toRelayBytes, fromRelayBytes int64, err error) {
	if t == nil {
		return
	}
	status := "success"
	if err != nil {
		status = "error"
	}
	t.m.activeConnections.WithLabelValues(t.role, t.target).Dec()
	t.m.connectionsTotal.WithLabelValues(t.role, t.target, status).Inc()
	t.m.connectionDuration.WithLabelValues(t.role, t.target).Observe(durationSec)
	t.m.bytesTotal.WithLabelValues(t.role, t.target, "to_relay").Add(float64(toRelayBytes))
	t.m.bytesTotal.WithLabelValues(t.role, t.target, "from_relay").Add(float64(fromRelayBytes))
}

// TrackedBridge wraps relay.Bridge with connection lifecycle tracking.
// Safe to call on a nil receiver.
func (m *Metrics) TrackedBridge(ctx context.Context, ws *websocket.Conn, rwc net.Conn, role, target string) (relay.BridgeResult, error) {
	tracker := m.ConnectionOpened(role, target)
	start := time.Now()
	var result relay.BridgeResult
	var err error
	defer func() {
		tracker.Done(time.Since(start).Seconds(), result.Stats.TCPToWS, result.Stats.WSToTCP, err)
	}()
	result, err = relay.Bridge(ctx, ws, rwc)
	return result, err
}

// InstrumentedDial wraps relay.DialWithRetry with duration and error metrics.
// Safe to call on a nil receiver (falls through to raw DialWithRetry).
func (m *Metrics) InstrumentedDial(ctx context.Context, endpoint, entityPath string, tp relay.TokenProvider, opts relay.ClientOptions, role string, logger *slog.Logger) (*websocket.Conn, error) {
	start := time.Now()
	ws, err := relay.DialWithRetry(ctx, endpoint, entityPath, tp, opts, logger)
	m.ObserveDialDuration(role, time.Since(start).Seconds())
	if err != nil {
		m.ConnectionError(role, DialReason(err, ReasonRelayFailed))
		return nil, err
	}
	return ws, nil
}

// closeWriter is implemented by net.TCPConn and smux.Stream. It allows a
// bridge to signal end-of-stream in one direction without tearing down the
// other (half-close), which is required by protocols like HTTP/1.0
// Connection: close, SSH channel close, and SMTP QUIT.
type closeWriter interface {
	CloseWrite() error
}

// closeWriteOrClose calls CloseWrite if available and falls back to Close
// otherwise. Errors are intentionally swallowed: best-effort signalling
// after the source has already ended, and the deferred Close in the caller
// will surface anything that matters.
func closeWriteOrClose(c io.Closer) {
	if cw, ok := c.(closeWriter); ok {
		_ = cw.CloseWrite()
		return
	}
	_ = c.Close()
}

// streamBridgeBufSize is the per-direction io.Copy buffer used to shuttle
// bytes between an smux stream and the local target. 64 KiB is large
// enough that a single read can absorb a meaningful slice of the
// per-stream smux window (muxconfig.MaxStreamBuffer, currently 1 MiB)
// while keeping the per-bridge memory footprint tiny — each bridged
// connection allocates two of these. Don't grow this beyond
// MaxStreamBuffer; doing so wastes memory without reducing syscalls.
const streamBridgeBufSize = 64 * 1024

// TrackedStreamBridge bidirectionally copies data between an smux stream
// (or any net.Conn) and a local target net.Conn while recording connection
// lifecycle metrics.
//
// Unlike TrackedBridge (which is built for a websocket+TCP pair and forces
// teardown when either side ends), TrackedStreamBridge supports half-close:
// when one direction sees EOF, it calls CloseWrite() on the destination
// rather than closing both sides. This preserves protocols that depend on
// signalling end-of-input while still reading a response (HTTP/1.0, SSH,
// SMTP, etc.).
//
// The returned BridgeResult mirrors TrackedBridge: Stats.TCPToWS /
// Stats.WSToTCP carry per-direction byte counts (conn→stream / stream→conn
// respectively), TCPToWS / WSToTCP carry the terminating error of each
// direction's pump (nil on clean EOF), and the second-return error is the
// first non-nil pump error preserved for callers that just check err != nil.
//
// EndCause carries the bridgecause classification of why the bridge ended,
// matching v1's relay.Bridge: the first pump exit stamps a cause sentinel
// (CauseLocalClose for the conn→stream direction, CausePeerClose for the
// stream→conn direction, CauseTimeout for net.Error.Timeout()) on the
// bridge ctx via WithCancelCause, and EndCause is resolved from
// context.Cause(ctx) so a parent cancel-cause (e.g. CauseRenewFailure
// stamped by the listener's renewLoop) wins over a pump-driven exit.
//
// Safe to call on a nil receiver.
func (m *Metrics) TrackedStreamBridge(ctx context.Context, stream, conn net.Conn, role, target string) (relay.BridgeResult, error) {
	tracker := m.ConnectionOpened(role, target)
	start := time.Now()
	var toRelay, fromRelay atomic.Int64
	var bridgeErr error
	defer func() {
		tracker.Done(time.Since(start).Seconds(), toRelay.Load(), fromRelay.Load(), bridgeErr)
	}()

	// Capture the caller-supplied ctx so we can distinguish external
	// cancellation (caller shutting us down) from internal cancellation
	// (a real per-direction copy error triggering hard teardown). Both
	// look identical to the inner derived ctx, but only the former
	// should surface as bridgeErr — the latter is already represented
	// by the propagated direction error.
	outerCtx := ctx
	ctx, cancel := context.WithCancelCause(ctx)
	// Cancellation cause is first-writer-wins (context.WithCancelCause
	// semantics). Pumps stamp pump-driven causes via cancel(...) on
	// hard teardown; this deferred cancel(nil) is a no-op cause-wise
	// (cancel-with-nil maps to context.Canceled in stdlib but only
	// takes effect if no prior cancel ran) and exists to release the
	// cancelCtx resources. EndCause is resolved before this defer
	// runs and goroutines are joined.
	defer cancel(nil)

	// Hard teardown if ctx is cancelled externally (e.g. process shutdown).
	// Half-close cannot rescue a cancelled context; close both ends.
	//
	// teardownFired records whether the teardown goroutine actually
	// closed the endpoints because of ctx cancellation. The post-wait
	// outerCtx.Err() attribution check below consults it so we don't
	// flag a clean half-close on both sides as forcibly-cancelled
	// just because the caller's ctx happened to expire after the
	// copies completed cleanly.
	//
	// Race-resolution: teardownDecision is committed-to from TWO
	// places — the teardown goroutine on ctx.Done(), and main right
	// after wg.Wait() returns. First caller wins. If main wins, both
	// copies have already exited (wg.Wait has returned), so the
	// teardown's close cannot have caused the exit — teardownFired
	// stays false, bridge attributes nil. If teardown wins, at least
	// one copy was still in-flight (main hadn't yet reached the Do),
	// so the teardown's close DID affect the in-flight data — flag
	// fires, bridge attributes outerCtx.Err(). The residual race —
	// teardown winning the Do race in the microsecond window after
	// copies exhausted their data but before wg.Wait returned — is a
	// metrics-attribution near-miss only, not a correctness issue.
	var teardownDecision sync.Once
	var teardownFired bool // guarded by teardownDecision
	done := make(chan struct{})
	teardownDone := make(chan struct{})
	go func() {
		defer close(teardownDone)
		select {
		case <-ctx.Done():
			teardownDecision.Do(func() {
				teardownFired = true
				_ = stream.Close()
				_ = conn.Close()
			})
		case <-done:
		}
	}()

	errc := make(chan error, 2)
	var wg sync.WaitGroup
	wg.Add(2)

	var toRelayErr, fromRelayErr error

	// firstCause records the bridgecause sentinel for the first pump
	// to exit. Used on the happy half-close path where neither pump
	// triggers a hard teardown (so no cancel-with-cause runs and
	// context.Cause(ctx) returns nil): the recorded sentinel is then
	// surfaced via EndCause below.
	//
	// We cannot stamp via cancel-with-cause on clean EOF — the
	// teardown goroutine watches ctx.Done() and would fire
	// immediately, closing the destination and defeating the
	// CloseWrite-and-drain half-close semantics.
	var firstCausePtr atomic.Pointer[error]
	recordCause := func(c error) {
		if c == nil {
			return
		}
		firstCausePtr.CompareAndSwap(nil, &c)
	}

	// causeFromPumpExit picks the bridgecause sentinel a pump's exit
	// stamps. Mirrors relay.causeFromPumpExit's policy adapted to
	// the mux bridge's symmetric direction names: conn→stream is
	// local-side (CauseLocalClose), stream→conn is peer-side
	// (CausePeerClose), net.Error.Timeout() wins as CauseTimeout
	// regardless of side, context.Canceled / DeadlineExceeded pass
	// through so bridgecause.Name's stdlib aliasing classifies them.
	causeFromPumpExit := func(toRelaySide bool, err error) error {
		if err == nil {
			if toRelaySide {
				return bridgecause.CauseLocalClose
			}
			return bridgecause.CausePeerClose
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return err
		}
		var netErr net.Error
		if errors.As(err, &netErr) && netErr.Timeout() {
			return bridgecause.CauseTimeout
		}
		if toRelaySide {
			return bridgecause.CauseLocalClose
		}
		return bridgecause.CausePeerClose
	}

	// conn → stream (to_relay)
	go func() {
		defer wg.Done()
		buf := make([]byte, streamBridgeBufSize)
		n, err := io.CopyBuffer(countingWriter{w: stream, c: &toRelay}, conn, buf)
		_ = n
		nerr := normalizeBridgeErr(err)
		recordCause(causeFromPumpExit(true, nerr))
		if nerr != nil {
			// Hard teardown on a real (non-EOF) copy error: cancel
			// the bridge ctx so the shutdown goroutine closes BOTH
			// ends. Half-closing only one side can hang the bridge
			// indefinitely if the opposite direction is idle (e.g.
			// an interactive protocol with no reverse traffic) —
			// the mux stream and pool slot would be held until the
			// pool's ctx eventually expires. Stamp the bridgecause
			// sentinel via cancel-with-cause so EndCause surfaces
			// the right label.
			cancel(causeFromPumpExit(true, nerr))
		} else {
			// Source exhausted (clean EOF). Signal end-of-input to
			// the destination via CloseWrite so half-close-aware
			// protocols (HTTP/1.0, SSH, SMTP) can drain pending
			// responses on the reverse direction before tearing
			// down. Do NOT cancel the bridge ctx: the teardown
			// goroutine watches ctx.Done() and would close the
			// destination side, defeating the half-close. The
			// recordCause above captures the sentinel for EndCause.
			closeWriteOrClose(stream)
		}
		toRelayErr = nerr
		errc <- nerr
	}()

	// stream → conn (from_relay)
	go func() {
		defer wg.Done()
		buf := make([]byte, streamBridgeBufSize)
		n, err := io.CopyBuffer(countingWriter{w: conn, c: &fromRelay}, stream, buf)
		_ = n
		nerr := normalizeBridgeErr(err)
		recordCause(causeFromPumpExit(false, nerr))
		if nerr != nil {
			cancel(causeFromPumpExit(false, nerr))
		} else {
			closeWriteOrClose(conn)
		}
		fromRelayErr = nerr
		errc <- nerr
	}()

	wg.Wait()

	// Commit the teardown decision from main BEFORE close(done) so
	// that a ctx cancellation racing the post-completion path
	// resolves to "no teardown fired" — main's Do here, runs first,
	// no-ops the teardown goroutine's eventual Do. This is the half
	// of the race-resolution comment on teardownDecision.
	teardownDecision.Do(func() {
		// teardownFired stays false: both copies have exited
		// (wg.Wait returned), so any subsequent close from the
		// teardown goroutine cannot have caused the exit.
	})
	close(done)
	<-teardownDone

	// First non-nil error wins. EOF/closed are normalized to nil.
	for range 2 {
		if err := <-errc; err != nil && bridgeErr == nil {
			bridgeErr = err
		}
	}
	// Attribute external cancellation to bridgeErr ONLY when the
	// teardown goroutine actually closed the endpoints
	// (teardownFired is true) AND the outer ctx is what was
	// cancelled. Without the teardownFired gate, an outerCtx that's
	// cancelled AFTER both copy goroutines have completed cleanly
	// (clean half-close on both sides) but BEFORE this check runs
	// would falsely flag a successful bridge as forcibly-cancelled.
	if bridgeErr == nil && teardownFired && outerCtx.Err() != nil {
		bridgeErr = outerCtx.Err()
	}
	// Resolve EndCause with v1-parity "first pump exit wins" semantics:
	//   1. Parent cancel-cause wins when the parent actually cancelled
	//      (mirrors the bridgeErr attribution above).
	//   2. Otherwise the first pump's recorded cause wins (matches v1's
	//      relay.Bridge: the side that ends the bridge owns the cause).
	//   3. Fall back to context.Cause(ctx) — non-nil only when a pump
	//      hard-tearndown ran but firstCausePtr was somehow unset (a
	//      defensive belt-and-braces; firstCausePtr is recorded before
	//      cancel-with-cause so this branch should be unreachable).
	//   4. Else unknown.
	var endCause string
	switch {
	case teardownFired && outerCtx.Err() != nil:
		endCause = bridgecause.Name(context.Cause(outerCtx))
	default:
		if ptr := firstCausePtr.Load(); ptr != nil {
			endCause = bridgecause.Name(*ptr)
		} else if c := context.Cause(ctx); c != nil {
			endCause = bridgecause.Name(c)
		} else {
			endCause = bridgecause.Name(nil)
		}
	}
	result := relay.BridgeResult{
		Stats:    relay.BridgeStats{TCPToWS: toRelay.Load(), WSToTCP: fromRelay.Load()},
		TCPToWS:  toRelayErr,
		WSToTCP:  fromRelayErr,
		EndCause: endCause,
	}
	return result, bridgeErr
}

// normalizeBridgeErr maps the "clean EOF / closed network connection"
// cases to nil so the metrics tracker records success on a clean half-close
// shutdown. Real I/O errors propagate unchanged.
//
// io.ErrUnexpectedEOF is deliberately NOT normalized: it represents a
// truncated read, not an orderly half-close. Treating it as clean EOF
// would push the bridge onto the half-close path (CloseWrite on the
// opposite direction) when it should be on the hard-teardown path, and
// could hang the bridge if the opposite direction is idle.
func normalizeBridgeErr(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, io.EOF) {
		return nil
	}
	if errors.Is(err, io.ErrClosedPipe) {
		return nil
	}
	if errors.Is(err, net.ErrClosed) {
		return nil
	}
	return err
}

// countingWriter wraps an io.Writer and increments an atomic counter for
// every byte successfully written. Used by TrackedStreamBridge to record
// per-direction byte totals without an additional copy.
type countingWriter struct {
	w io.Writer
	c *atomic.Int64
}

func (cw countingWriter) Write(p []byte) (int, error) {
	n, err := cw.w.Write(p)
	if n > 0 {
		cw.c.Add(int64(n))
	}
	return n, err
}
