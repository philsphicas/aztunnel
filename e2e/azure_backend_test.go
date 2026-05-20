//go:build e2e

package e2e

import (
	"strconv"
	"testing"
	"time"

	"github.com/philsphicas/aztunnel/internal/testharness/e2escenarios"
)

// azureBackend implements e2escenarios.Backend against a real Azure
// Relay namespace. It is the source-of-truth side of the mock-vs-Azure
// conformance matrix: any scenario divergence between this backend and
// the in-process MockBackend (mockrelay/testharness/mockbackend) is a
// behavioural gap in the mock that we have to either fix or document.
//
// An azureBackend exists in one of two modes:
//
//   - Factory: axis is non-nil, authName is empty. Returned from
//     newAzureBackendFactory. Axes() reports the auth dimension; the
//     harness enumerates it via Cell() to produce pinned backends.
//   - Pinned: axis is nil, authName is "entra" or "sas". Returned
//     from Cell(). Setup reads authName to pick the auth method.
//     Pinned backends have no further axes.
//
// All listeners and senders are real aztunnel subprocesses driven by
// the existing helpers (startListener, startPortForwardSender,
// startSOCKS5Sender), so they exercise the same code paths that
// production users hit. Each subprocess exposes its own Prometheus
// /metrics endpoint via --metrics-addr 127.0.0.1:0, which the
// Listener / Sender accessor closures scrape on demand to satisfy
// Completed() / Active() reads.
//
// Each Setup call acquires a relayEnv via acquireEnv. The two
// strategies in tree:
//
//   - requireDedicatedHyco: provisions a fresh (entra, sas) hyco pair
//     and registers a t.Cleanup that tears it down. Used by
//     TestE2E_Azure so each scenario gets isolation between
//     successive Setup calls — and so scenarios that call Setup
//     twice (e.g. ScenarioErrorPropagation_*) hold disjoint hycos.
//   - leaseSharedHyco: returns a process-shared, lazily-leased pair
//     drained at TestMain exit. Used by BenchmarkE2E_Azure so
//     benchstat runs do not pay per-sub-bench provisioning.
type azureBackend struct {
	axis       *authAxis
	authName   string
	acquireEnv func(testing.TB) *relayEnv
}

// authAxis is the e2escenarios.Axis the Azure backend varies over.
// Values is the set of auth methods discovered at factory-construction
// time via availableAuthNames; the harness enumerates them in order
// and emits one t.Run per value.
type authAxis struct {
	values []string
}

func (*authAxis) Name() string       { return "auth" }
func (a *authAxis) Values() []string { return a.values }

// newAzureBackendFactory returns an *azureBackend whose Axes() lists
// the auth methods available in this process — discovered once via
// availableAuthNames(t) — and whose Cell(values) returns a fresh
// *azureBackend pinned to values["auth"]. Cell-pinned backends have
// no further axes (Axes() returns nil), so the harness only loops
// over the auth axis once.
//
// acquireEnv is fixed by the caller (requireDedicatedHyco for
// scenario runs, leaseSharedHyco for benchmarks).
func newAzureBackendFactory(t testing.TB, acquireEnv func(testing.TB) *relayEnv) *azureBackend {
	return &azureBackend{
		axis:       &authAxis{values: availableAuthNames(t)},
		acquireEnv: acquireEnv,
	}
}

// Name returns the backend identifier (always "azure"). The harness
// does not embed it in sub-test paths — the auth dimension appears
// via the axis t.Run wrapping — but scenarios and external callers
// may surface it in debug output.
func (*azureBackend) Name() string { return "azure" }

// Axes returns the matrix dimensions this backend varies over.
// Factory backends (constructed via newAzureBackendFactory) return
// the auth axis; pinned backends (returned from Cell) return nil.
func (b *azureBackend) Axes() []e2escenarios.Axis {
	if b.axis == nil {
		return nil
	}
	return []e2escenarios.Axis{b.axis}
}

// Cell returns a fresh *azureBackend pinned to the cell described by
// values. Factory backends require values["auth"]; pinned backends
// (axis == nil) accept only an empty values map and return a clone
// of the receiver.
func (b *azureBackend) Cell(values map[string]string) e2escenarios.Backend {
	if b.axis == nil {
		if len(values) != 0 {
			panic("azureBackend.Cell: pinned backend accepts no axis values")
		}
		return &azureBackend{authName: b.authName, acquireEnv: b.acquireEnv}
	}
	auth, ok := values["auth"]
	if !ok {
		panic("azureBackend.Cell: missing required axis key \"auth\"")
	}
	if len(values) != 1 {
		panic("azureBackend.Cell: expected exactly one axis value (auth)")
	}
	return &azureBackend{authName: auth, acquireEnv: b.acquireEnv}
}

// Setup brings up the requested topology (NumListeners listeners and
// max(NumSenders,1) senders), waits until every listener has logged
// "control_started" and every sender has logged its bind
// address, then attaches metrics-scrape closures and returns the
// Tunnel handle. All subprocesses are torn down via the existing
// t.Cleanup wiring inside startAztunnelWithSAS.
//
// acquireEnv MUST be the first side-effect: when it provisions a
// dedicated pair (requireDedicatedHyco), the t.Cleanup registered
// for PairToken.Teardown then sits BENEATH the listener/sender
// Stop cleanups registered later. LIFO order ensures every
// subprocess in this Setup is killed before its hyco pair is
// deleted, which prevents ARM Delete from racing a still-attached
// listener's keep-alives.
func (b *azureBackend) Setup(t testing.TB, opts e2escenarios.SetupOptions) *e2escenarios.Tunnel {
	t.Helper()
	if opts.NumListeners < 1 {
		t.Fatalf("NumListeners must be >= 1, got %d", opts.NumListeners)
	}
	numSenders := opts.NumSenders
	if numSenders < 1 {
		numSenders = 1
	}

	env := b.acquireEnv(t)
	auth := authFromEnv(t, env, b.authName)

	// Azure backend runs at debug log level so observability
	// scenarios can assert on Debug-level lifecycle lines
	// (e.g. "bridge ended") without per-scenario log-level overrides.
	listenerArgs := []string{"--metrics-addr", "127.0.0.1:0", "--log-level", "debug"}
	for _, target := range opts.AllowedTargets {
		listenerArgs = append(listenerArgs, "--allow", target)
	}
	if opts.MaxConnections > 0 {
		listenerArgs = append(listenerArgs, "--max-connections",
			strconv.Itoa(opts.MaxConnections))
	}
	if opts.ConnectTimeout > 0 {
		listenerArgs = append(listenerArgs, "--connect-timeout",
			opts.ConnectTimeout.String())
	}

	spawnListener := func(t testing.TB) *e2escenarios.Listener {
		t.Helper()
		lst := startListener(t, env, auth, listenerArgs...)
		waitForLog(t, lst, "control_started", 30*time.Second)
		metricsAddr := lst.MetricsAddr(t, 15*time.Second)
		return &e2escenarios.Listener{
			Addr:             metricsAddr,
			Completed:        scrapeCounter(metricsAddr, "aztunnel_connections_total"),
			Active:           scrapeGauge(metricsAddr, "aztunnel_active_connections"),
			ConnectionErrors: scrapeConnectionErrors(metricsAddr),
			Stop:             func() { lst.Stop(t) },
			Logs:             func() string { return lst.logs.String() },
		}
	}

	senderArgs := []string{"--metrics-addr", "127.0.0.1:0", "--log-level", "debug"}

	listeners := make([]*e2escenarios.Listener, 0, opts.NumListeners)
	for i := 0; i < opts.NumListeners; i++ {
		listeners = append(listeners, spawnListener(t))
	}

	senderAddrs := make([]string, 0, numSenders)
	senders := make([]*e2escenarios.Sender, 0, numSenders)
	for i := 0; i < numSenders; i++ {
		var proc *aztunnelProcess
		var logMsg string
		switch opts.SenderMode {
		case e2escenarios.ModePortForward:
			proc = startPortForwardSender(t, env, auth, opts.Target, senderArgs...)
			logMsg = "port-forward listening"
		case e2escenarios.ModeSOCKS5:
			proc = startSOCKS5Sender(t, env, auth, senderArgs...)
			logMsg = "socks5-proxy listening"
		default:
			t.Fatalf("unknown SenderMode %v", opts.SenderMode)
		}
		bindAddr := waitForLogAddr(t, proc, logMsg, 15*time.Second)
		metricsAddr := proc.MetricsAddr(t, 15*time.Second)
		senderAddrs = append(senderAddrs, bindAddr)
		senders = append(senders, &e2escenarios.Sender{
			Addr:      bindAddr,
			Completed: scrapeCounter(metricsAddr, "aztunnel_connections_total"),
			Active:    scrapeGauge(metricsAddr, "aztunnel_active_connections"),
			Stop:      func() { proc.Stop(t) },
			Logs:      func() string { return proc.logs.String() },
		})
	}

	tun := &e2escenarios.Tunnel{
		SenderAddr:  senderAddrs[0],
		SenderAddrs: senderAddrs,
		Senders:     senders,
		Listeners:   listeners,
	}
	tun.AddListener = func(t *testing.T) *e2escenarios.Listener {
		t.Helper()
		l := spawnListener(t)
		tun.Listeners = append(tun.Listeners, l)
		return l
	}
	return tun
}

// scrapeCounter returns a closure that scrapes /metrics on addr and
// returns the sum of metric `name` (as int64) across all label
// combinations. Used for `aztunnel_connections_total` — bridges that
// have completed (Done()).
func scrapeCounter(addr, name string) func() int64 {
	return func() int64 {
		text := scrapeMetricsBest(addr)
		return int64(sumMetric(text, name))
	}
}

// scrapeGauge returns a closure that scrapes /metrics on addr and
// returns the sum of metric `name` (as int64) across all label
// combinations. Used for `aztunnel_active_connections` — in-flight
// bridges.
func scrapeGauge(addr, name string) func() int64 {
	return func() int64 {
		text := scrapeMetricsBest(addr)
		return int64(sumMetric(text, name))
	}
}

// scrapeConnectionErrors returns a closure that scrapes /metrics on
// addr and returns the sum of aztunnel_connection_errors_total samples
// whose `reason` label equals the requested reason. Used by negative-
// path e2e scenarios that assert the listener classified a dial
// failure into a specific reason bucket.
func scrapeConnectionErrors(addr string) func(reason string) int64 {
	return func(reason string) int64 {
		text := scrapeMetricsBest(addr)
		return int64(sumMetricByLabel(text,
			"aztunnel_connection_errors_total", "reason", reason))
	}
}
