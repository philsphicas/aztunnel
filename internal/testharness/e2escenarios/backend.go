package e2escenarios

import (
	"testing"
	"time"
)

// SenderMode selects which aztunnel sender to bring up.
type SenderMode int

const (
	// ModePortForward starts a relay-sender port-forward bound to
	// SetupOptions.Target. Dialing the resulting SenderAddr is
	// equivalent to dialing Target directly through the tunnel.
	ModePortForward SenderMode = iota
	// ModeSOCKS5 starts a relay-sender socks5-proxy. The resulting
	// SenderAddr speaks SOCKS5; each client connection chooses its own
	// target via the SOCKS5 handshake.
	ModeSOCKS5
)

func (m SenderMode) String() string {
	switch m {
	case ModePortForward:
		return "port-forward"
	case ModeSOCKS5:
		return "socks5"
	default:
		return "unknown"
	}
}

// Axis is one matrix dimension a Backend can vary over. The harness
// enumerates Values in order, wrapping each in t.Run(value, ...) so
// the rendered sub-test path reads as `<entry>/<value>/<scenario>`
// (or with deeper axis nesting, `<entry>/<value1>/<value2>/<scenario>`).
//
// Axis Names are purely informational (used by error messages and
// the Cell() contract — see Backend.Cell). They do not appear in
// sub-test paths; only Values do.
type Axis interface {
	Name() string
	Values() []string
}

// Backend is the abstraction e2e scenarios run against. Each
// implementation knows how to bring up a topology that satisfies
// SetupOptions and to tear it down cleanly via t.Cleanup.
//
// Setup takes testing.TB rather than *testing.T so the same backend
// implementation is reachable from both tests (RunCoreScenarios,
// RunTopologyScenarios) and benchmarks (RunBenchmarks). The only TB
// methods used are Helper / Fatalf / Logf / Cleanup, all on the
// shared interface.
type Backend interface {
	// Name identifies the backend (e.g. "mock", "azure"). The harness
	// does not embed it in sub-test paths — axis values fill that
	// role via t.Run wrapping — but scenarios and external callers
	// may surface it in debug output.
	Name() string

	// Axes returns the matrix dimensions this backend varies over.
	// Order is significant: the harness nests t.Run calls in the
	// returned order, so Axes()[0] becomes the outermost sub-test
	// layer. Returning nil (or an empty slice) means the backend has
	// no axes and the harness jumps straight to scenarios.
	Axes() []Axis

	// Cell returns a Backend pinned to the cell described by values.
	// values is keyed by Axis.Name and contains exactly one entry per
	// axis returned by Axes(); the harness builds it as it descends
	// the t.Run nesting. When the backend has axes, Cell must return
	// a fresh Backend instance — never the receiver — because
	// subsequent cells would otherwise overwrite the pinned state on
	// the same receiver. A no-axis backend has no per-cell state to
	// clobber, so Cell({}) on such a backend may return the receiver
	// unchanged.
	//
	// Cell implementations should validate that values contains every
	// axis key they expect; a missing or misspelled key is a harness
	// contract bug and should panic with a precise message (Cell has
	// no testing.TB to fail on cleanly).
	Cell(values map[string]string) Backend

	// ConnectLatencyThreshold is the per-backend ceiling for a single
	// fresh-connection round-trip (Dial → 1-byte write → 1-byte echo
	// read → Close). The Performance suite's serial-connect scenarios
	// assert each iteration completes inside this budget.
	//
	// The threshold is read on the cell-pinned backend (after Cell()),
	// so implementations may vary it per axis value (e.g. tighter for
	// SAS than for Entra) when data justifies the differentiation.
	// Both backends currently return a single value regardless of cell.
	//
	// "Connect latency" is interpreted broadly: the budget covers
	// every wall-clock cost the harness pays from `net.Dial` (or the
	// SOCKS5 CONNECT handshake) through a successful `conn.Close()` of
	// the verified 1-byte round-trip. That includes the SOCKS5
	// handshake on the SOCKS5 variant, the listener-side rendezvous,
	// the target dial, the echo round-trip, and the socket close. A
	// backend that wanted to split those costs into separate budgets
	// would need a richer interface.
	ConnectLatencyThreshold() time.Duration

	// Setup brings up a relay topology described by opts and returns a
	// Tunnel handle the scenario can drive. All resources (goroutines,
	// subprocesses, listeners, sockets) are registered for cleanup on
	// t via t.Cleanup; the scenario does not have to release them.
	//
	// Setup must block until the topology is ready: every sender's
	// bind address is accepting connections and every requested
	// listener has its control channel attached to the relay (for
	// subprocess backends, the control_started log; for the
	// in-process backend, the aztunnel_control_channel_connected
	// gauge). Scenarios assume the tunnel is fully connected on
	// return.
	Setup(t testing.TB, opts SetupOptions) *Tunnel
}

// SetupOptions configures the topology a backend should create.
type SetupOptions struct {
	// NumListeners is the count of listener processes/goroutines to
	// start against the same entity. Must be >= 1.
	NumListeners int

	// NumSenders is the count of sender processes/goroutines to start
	// against the same entity. 0 or 1 means a single sender (current
	// single-sender behaviour, for back-compat with the original four
	// scenarios). 2+ means N senders; each gets its own free bind and
	// is exposed via Tunnel.Senders / Tunnel.SenderAddrs.
	NumSenders int

	// SenderMode picks port-forward vs SOCKS5.
	SenderMode SenderMode

	// Target is the dial target for port-forward mode. Ignored for
	// SOCKS5 (clients choose their own target via the SOCKS5
	// handshake).
	Target string

	// AllowedTargets is the listener --allow value(s). Empty means
	// allow all; tests usually pass the addresses of the target
	// servers they started. Slice order is not significant.
	AllowedTargets []string

	// MaxConnections caps concurrent target dials on each listener
	// (`aztunnel relay-listener --max-connections=N`). 0 means
	// unlimited.
	MaxConnections int

	// ConnectTimeout overrides the listener's dial timeout for target
	// connections (`aztunnel relay-listener --connect-timeout=DUR`).
	// 0 means leave at the listener default (30 s). Scenarios that
	// provoke dial failures use a shorter value so the test isn't
	// dominated by the default 30 s wait.
	ConnectTimeout time.Duration
}

// Listener is a handle to a single listener in a Tunnel. Backends
// populate Tunnel.Listeners with one entry per running listener.
//
// All accessor closures (Completed, Active, ConnectionErrors) are safe
// to call concurrently and return monotonically-updating values without
// blocking on the listener.
type Listener struct {
	// Addr is the listener's metrics-scrape address for subprocess
	// backends; it is left empty for in-process backends that expose
	// counters directly via the Completed/Active closures.
	Addr string

	// Completed returns the number of bridged connections this
	// listener has handled to completion (aztunnel_connections_total
	// summed across all label combinations on this listener's
	// metrics surface). Increments only after the bridge ends, so it
	// is suitable for distribution-after-the-fact assertions but not
	// for "is a connection currently in flight" checks — use Active
	// for that.
	Completed func() int64

	// Active returns the number of bridges currently in flight on
	// this listener (aztunnel_active_connections summed across all
	// label combinations).
	Active func() int64

	// ConnectionErrors returns the value of
	// aztunnel_connection_errors_total filtered to samples whose
	// reason label equals the given reason, summed across any other
	// label combinations (e.g. role) on this listener's metrics
	// surface. Used by negative-path scenarios to assert the listener
	// classified a dial failure into the expected reason bucket.
	//
	// Returns 0 when the metric has no samples for that reason yet
	// (counters are not initialized until the first observation).
	ConnectionErrors func(reason string) int64

	// Stop drops this listener: in-process backends cancel the
	// listener's context and wait for the goroutine to exit;
	// subprocess backends kill and reap the listener process.
	// Idempotent; safe to call from a scenario goroutine.
	Stop func()

	// Logs returns every log line this listener has emitted so far,
	// joined by newlines. Observability e2e scenarios grep this
	// string for cross-process correlation IDs (e.g. bridge_id).
	// Optional: backends that do not capture logs may leave this nil
	// and scenarios that need it call t.Skip.
	Logs func() string
}

// Sender is a handle to a single sender in a Tunnel. Backends populate
// Tunnel.Senders with one entry per running sender. Tunnel.SenderAddrs
// holds the same Addr values in the same order for convenience.
type Sender struct {
	// Addr is the local bind clients dial. For ModePortForward it is
	// a plain TCP target that forwards to SetupOptions.Target. For
	// ModeSOCKS5 it is a SOCKS5 proxy.
	Addr string

	// Completed returns the number of bridged connections this
	// sender has handled to completion. See Listener.Completed for
	// caveats; the same metric semantics apply with role="sender".
	Completed func() int64

	// Active returns the number of bridges currently in flight on
	// this sender.
	Active func() int64

	// Stop drops this sender. Idempotent.
	Stop func()

	// Logs returns every log line this sender has emitted so far,
	// joined by newlines. See Listener.Logs for usage and the
	// optional-nil contract.
	Logs func() string
}

// Tunnel is a running listener/sender/relay topology returned by
// Backend.Setup. Scenarios drive it by dialing into SenderAddrs from
// client goroutines.
//
// Back-compat with the four original scenarios is preserved: SenderAddr
// remains the field they read and always equals SenderAddrs[0]. New
// multi-sender scenarios index SenderAddrs directly.
type Tunnel struct {
	// SenderAddr is the host:port clients dial for the first (or
	// only) sender. Always equal to SenderAddrs[0]. Kept as a top-
	// level field so the original single-sender scenarios compile
	// unchanged.
	SenderAddr string

	// SenderAddrs holds every sender's bind address in the order
	// they were spawned. len(SenderAddrs) == len(Senders) ==
	// max(NumSenders, 1).
	SenderAddrs []string

	// Senders is the per-sender handle slice, in the same order as
	// SenderAddrs. Topology scenarios reach for Senders[i].Completed
	// to verify per-sender distribution.
	Senders []*Sender

	// Listeners is the per-listener handle slice. len(Listeners) ==
	// initial NumListeners; AddListener appends.
	Listeners []*Listener

	// AddListener spawns an additional listener against the same
	// entity with the same SetupOptions used at Setup time, blocks
	// until its control channel is attached, appends the handle to
	// Listeners, and returns the new handle. The caller's t.Cleanup
	// is already wired up; scenarios do not need to call Stop unless
	// they want to drop the listener mid-scenario.
	//
	// May be nil for backends that haven't wired hot-add yet; the
	// hot-add scenario calls t.Skip when it is.
	AddListener func(t *testing.T) *Listener
}
