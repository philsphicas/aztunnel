// Package metrics provides Prometheus metrics for aztunnel.
package metrics

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
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
