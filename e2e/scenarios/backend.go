package scenarios

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
	// ModeConnect prepares the topology for stdio-style senders: no
	// sender process is started at Setup time. Each call to
	// Tunnel.OpenConnect launches a fresh sender (subprocess for
	// Azure, in-process goroutine for the mock) bound to a target
	// chosen by the caller, with stdin/stdout reachable through the
	// returned ConnectClient.
	ModeConnect
)

func (m SenderMode) String() string {
	switch m {
	case ModePortForward:
		return "port-forward"
	case ModeSOCKS5:
		return "socks5"
	case ModeConnect:
		return "connect"
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
// implementation stays reachable from any future *testing.B callers.
// The only TB methods used are Helper / Fatalf / Logf / Cleanup, all
// on the shared interface.
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
	//
	// The Performance suite's ConnectLatency_Serial scenarios drive
	// this threshold against the steady-state path — they discard one
	// untimed warm-up dial before measuring. Cold-start cost is
	// regression-protected separately via ColdStartLatencyThreshold
	// and ConnectLatency_ColdStart_* scenarios.
	ConnectLatencyThreshold() time.Duration

	// ColdStartLatencyThreshold is the per-backend ceiling for the
	// very first connection through a freshly-started sender — the
	// connection that pays one-time costs the steady-state threshold
	// excludes (most notably the EntraTokenProvider's first OAuth2
	// token fetch). It is read on the cell-pinned backend so an Azure
	// implementation may use a wider value for Entra cells than for
	// SAS cells.
	//
	// The ConnectLatency_ColdStart_* scenarios assert exactly one
	// timed dial against this threshold. The budget is intentionally
	// looser than ConnectLatencyThreshold so it remains stable across
	// the credential paths operators legitimately use (workload
	// identity federation in CI, `az` CLI shell-out locally) while
	// still catching multi-second regressions.
	ColdStartLatencyThreshold() time.Duration

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
	//
	// NumListeners may be zero for scenarios that want to attach a
	// listener later via Tunnel.AddListener (sender-retries, no-
	// listener, etc.). With zero listeners the returned tunnel has
	// an empty Listeners slice but a fully-configured sender (when
	// SenderMode is not ModeConnect).
	//
	// For SenderMode==ModeConnect, no sender process is launched at
	// Setup time. The returned tunnel exposes Tunnel.OpenConnect to
	// spawn a fresh stdio-bridged sender per call. SenderAddr and
	// SenderAddrs are empty on the returned tunnel in that case.
	Setup(t testing.TB, opts SetupOptions) *Tunnel

	// SetupExpectingFailure brings up the topology described by opts
	// and EXPECTS something to fail per the trigger contract below.
	// Fatals the test if nothing fails. Returns a FailureHandle whose
	// log accessors expose the captured aztunnel log output for
	// assertion. Trigger semantics per failure type:
	//
	//   - Listener-side failure (OverrideListenerAuth.BadSASKey, or
	//     OverrideHycoName): SetupExpectingFailure starts the listener
	//     and waits up to 30 s for the listener to log a control-
	//     channel failure. Returns when the failure is observed. The
	//     sender is NOT started.
	//
	//   - Sender-side failure (OverrideSenderAuth.BadSASKey, etc.):
	//     SetupExpectingFailure starts the listener (if any) AND the
	//     sender, and either waits for the sender to log a dial
	//     failure OR (for port-forward sender) performs ONE client
	//     dial against the SenderAddr to trigger the failure. Returns
	//     when the failure is observed within 30 s.
	//
	//   - ModeConnect failure: SetupExpectingFailure starts the
	//     listener (if any). The sender is NOT started at Setup time
	//     — the test calls Tunnel.OpenConnect later, which invokes
	//     connect with the overridden config and observes the
	//     failure.
	//
	// All cases: SetupExpectingFailure ensures no orphan subprocesses
	// outlive the test; FailureHandle.Close is a no-op for
	// already-dead processes but is required by symmetry with
	// Setup's t.Cleanup pattern. Assertions must be made on the
	// aztunnel-side LOG SHAPE (e.g. "log line containing 'auth
	// failed' AND not containing the redacted secret"), NOT on
	// relay-side wire bytes — those can differ between Azure and the
	// mock.
	SetupExpectingFailure(t testing.TB, opts SetupOptions) FailureHandle
}

// SetupOptions configures the topology a backend should create.
type SetupOptions struct {
	// NumListeners is the count of listener processes/goroutines to
	// start against the same entity. Zero is allowed; scenarios that
	// attach a listener later via Tunnel.AddListener (no-listener,
	// sender-retries) start at zero. Negative values are rejected.
	NumListeners int

	// NumSenders is the count of sender processes/goroutines to start
	// against the same entity. 0 or 1 means a single sender (current
	// single-sender behaviour, for back-compat with the original four
	// scenarios). 2+ means N senders; each gets its own free bind and
	// is exposed via Tunnel.Senders / Tunnel.SenderAddrs. Ignored for
	// SenderMode==ModeConnect.
	NumSenders int

	// SenderMode picks port-forward, SOCKS5, or stdio-style connect.
	SenderMode SenderMode

	// Target is the dial target for port-forward mode. Ignored for
	// SOCKS5 (clients choose their own target via the SOCKS5
	// handshake) and for ModeConnect (each OpenConnect call passes
	// its own target).
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

	// OverrideListenerAuth, if non-nil, replaces the listener's auth
	// credentials before launch. Used by SetupExpectingFailure to
	// provoke listener-side auth failures.
	OverrideListenerAuth *AuthOverride

	// OverrideSenderAuth, if non-nil, replaces the sender's auth
	// credentials before launch. Used by SetupExpectingFailure to
	// provoke sender-side auth failures.
	OverrideSenderAuth *AuthOverride

	// OverrideHycoName, if non-empty, replaces the hyco name on both
	// listener and sender. Used to test "hyco not found" rejection
	// on Azure (mock's hyco model is dynamic and treats any name as
	// valid; scenarios that need this override should scope-gate
	// themselves on mock).
	OverrideHycoName string
}

// AuthOverride substitutes auth credentials when SetupExpectingFailure
// brings up a topology that should fail authentication. Exactly one
// field is set per use.
type AuthOverride struct {
	// BadSASKey, if non-empty, replaces the SAS key with a
	// syntactically-valid-but-wrong value (typically a base64
	// "this is a bad key" string). Both Azure and the mock reject
	// the resulting SAS token.
	BadSASKey string

	// BadEntraToken, if non-empty, replaces the Entra token with
	// a syntactically-valid-but-wrong value. Azure-only effect; the
	// mock backend ignores it because the mock has no Entra
	// validation path.
	BadEntraToken string

	// UseOppositeSASDirection, if true, replaces the SAS credentials
	// with the valid keys from the OPPOSITE direction — i.e. on
	// OverrideListenerAuth it supplies the sender-direction key as
	// the listener's credentials, and vice versa on
	// OverrideSenderAuth. Exercises the relay's per-key Listen vs
	// Send claim enforcement: the SAS material itself authenticates
	// successfully, but the claim does not authorize the action.
	//
	// Mutually exclusive with BadSASKey on the same override —
	// setting both is a test-author error and Backends MUST fatal.
	//
	// Azure-only contract: the mock relay does not model per-key
	// direction, so callers MUST scope the scenario to AzureOnly
	// (or otherwise gate on backend) and the Azure backend MUST
	// skip cells whose auth method is not SAS — the relevant key
	// material exists only on the SAS hyco.
	UseOppositeSASDirection bool
}

// FailureHandle is returned by Backend.SetupExpectingFailure. It
// exposes the captured aztunnel-side logs of the failing subprocess(es)
// so the scenario can assert on aztunnel's error-shape contract (and
// crucially, that the failed-auth secret never appears in the log).
type FailureHandle interface {
	// ListenerLogs returns the captured stderr of the listener
	// subprocess (Azure) or the slog output buffer of the in-process
	// listener (mock). Returns the empty string if no listener was
	// started (sender-only failure modes).
	ListenerLogs() string

	// SenderLogs returns the captured stderr of the sender
	// subprocess / the slog buffer of the in-process sender.
	// Returns the empty string if no sender was started (listener-
	// only failure modes or ModeConnect tests that have not yet
	// invoked OpenConnect).
	SenderLogs() string

	// Close releases any per-handle resources. The t.Cleanup hooks
	// registered by SetupExpectingFailure already perform teardown;
	// Close is a no-op for already-dead processes but is required
	// by symmetry with Setup's t.Cleanup pattern.
	Close()
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

	// DialDurationSamples returns the count of observations recorded
	// in the aztunnel_dial_duration_seconds histogram on this
	// sender's metrics surface. Used by ScenarioMetrics_DialDuration
	// to confirm the dial path actually observed the histogram.
	// Optional: backends that don't expose the histogram leave this
	// nil and the scenario calls t.Skip.
	DialDurationSamples func() uint64

	// TokenFetchOK returns one TokenFetchObservation per
	// (provider) label observed in the aztunnel_token_fetch_total
	// and aztunnel_token_fetch_seconds_count metrics on this
	// sender's /metrics surface, filtered to result="ok". Returns
	// nil when no observations have been recorded yet.
	//
	// Used by ScenarioTokenFetchMetric to assert that exactly one
	// token-provider was used and that the counter and histogram
	// counts agree. Optional: backends that do not wire token-fetch
	// metrics leave this nil and the scenario skips. Both the Azure
	// backend and the in-process mock (via its {sas, entra} auth axis)
	// populate it.
	TokenFetchOK func() []TokenFetchObservation

	// Stop drops this sender. Idempotent.
	Stop func()

	// Logs returns every log line this sender has emitted so far,
	// joined by newlines. See Listener.Logs for usage and the
	// optional-nil contract.
	Logs func() string
}

// TokenFetchObservation is one (provider, counter, histogram-count)
// triple observed on a sender's /metrics surface for a single
// `provider` label with result="ok". Used by ScenarioTokenFetchMetric
// to assert exactly one provider was exercised and that the counter
// and histogram count agree (the wrapper observes both per call).
type TokenFetchObservation struct {
	// Provider is the value of the `provider` Prometheus label
	// (e.g. "entra" or "sas") on the observed metric line.
	Provider string

	// CounterValue is the aztunnel_token_fetch_total{provider=…,
	// result="ok"} value at the moment TokenFetchOK was called.
	CounterValue uint64

	// HistogramCount is the aztunnel_token_fetch_seconds_count{
	// provider=…, result="ok"} value at the moment TokenFetchOK was
	// called. Must equal CounterValue (the observability wrapper
	// records both per token fetch).
	HistogramCount uint64
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

	// openConnect is set by Backend.Setup when SenderMode==ModeConnect.
	// Nil otherwise. Use Tunnel.OpenConnect to invoke; the closure
	// indirection lets backends populate it with subprocess (Azure)
	// or in-process goroutine (mock) semantics without exposing the
	// difference to scenarios.
	openConnect func(t testing.TB, target string) ConnectClient

	// sshProxyCommand is set by Backend.Setup for backends that can
	// spawn a sender subprocess reachable from outside the test
	// process (Azure). Nil otherwise. Use Tunnel.SSHProxyCommand to
	// retrieve.
	sshProxyCommand tunnelSSHProxyCommand
}

// OpenConnect launches a fresh stdio-bridged sender against target
// through this tunnel and returns a ConnectClient bound to its
// stdin/stdout/stderr. Each call produces a NEW sender (subprocess
// for Azure, goroutine for mock); closing the returned ConnectClient
// releases that sender. Fatals the test if the tunnel was not set up
// with SenderMode==ModeConnect.
func (tun *Tunnel) OpenConnect(t testing.TB, target string) ConnectClient {
	t.Helper()
	if tun.openConnect == nil {
		t.Fatalf("Tunnel.OpenConnect called but SenderMode was not ModeConnect")
	}
	return tun.openConnect(t, target)
}

// SetOpenConnect is the backend-side setter for the openConnect
// closure populated during Setup when SenderMode==ModeConnect. It is
// exported only for backend implementations in sibling packages
// (e2e/backends/{azure,mock}); scenarios should never call it.
func (tun *Tunnel) SetOpenConnect(fn func(t testing.TB, target string) ConnectClient) {
	tun.openConnect = fn
}

// SSHProxyCommand, when non-nil, returns the argv and env that
// ssh's ProxyCommand should run to launch
// `aztunnel relay-sender connect <target>` against this tunnel's
// listener. The caller passes "%h:%p" (or any literal target) as
// target; ssh substitutes %h/%p at invocation time.
//
// Populated by backends that support spawning a sender subprocess
// reachable from outside the test process (Azure). Nil for backends
// that don't (mock: in-process control channel is not reachable from
// a fresh subprocess). Scenarios that need it (ScenarioSSH_ProxyCommand)
// check for nil and skip with a clear reason.
//
// The closure shares the tunnel's already-acquired env so the
// listener and the ssh-spawned subprocess use the SAME hyco
// coordinates.
type tunnelSSHProxyCommand func(target string) (argv []string, env []string)

// sshProxyCommand stores the optional ssh-ProxyCommand builder.
// Use Tunnel.SetSSHProxyCommand to populate.
func (tun *Tunnel) SSHProxyCommand(target string) (argv []string, env []string, ok bool) {
	if tun.sshProxyCommand == nil {
		return nil, nil, false
	}
	argv, env = tun.sshProxyCommand(target)
	return argv, env, true
}

// SetSSHProxyCommand is the backend-side setter for the optional
// ssh-ProxyCommand builder. Backends populate it during Setup when
// they can spawn a reachable sender subprocess.
func (tun *Tunnel) SetSSHProxyCommand(fn func(target string) (argv []string, env []string)) {
	tun.sshProxyCommand = fn
}
