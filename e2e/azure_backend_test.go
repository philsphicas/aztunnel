//go:build e2e

package e2e

import (
	"strconv"
	"testing"
	"time"

	"github.com/philsphicas/aztunnel/internal/testharness/relayparity"
)

// azureBackend implements relayparity.Backend against a real Azure
// Relay namespace. It is the source-of-truth side of the parity
// matrix: any scenario divergence between this backend and the
// in-process MockBackend (mockrelay/testharness/parity) is a behavioural gap in
// the mock that we have to either fix or document.
//
// Each instance is bound to a single auth method ("entra" or "sas")
// so the caller can run the same scenario suite once per available
// auth. All listeners and senders are real aztunnel subprocesses
// driven by the existing helpers (startListener, startPortForwardSender,
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
//     TestParity_Azure so each scenario gets isolation between
//     successive Setup calls — and so scenarios that call Setup
//     twice (e.g. ScenarioErrorPropagation_*) hold disjoint hycos.
//   - leaseSharedHyco: returns a process-shared, lazily-leased pair
//     drained at TestMain exit. Used by BenchmarkParity_Azure so
//     benchstat runs do not pay per-sub-bench provisioning.
type azureBackend struct {
	authName   string
	acquireEnv func(testing.TB) *relayEnv
}

// Name returns the backend identifier used in test sub-paths, e.g.
// "azure-entra" or "azure-sas".
func (b *azureBackend) Name() string { return "azure-" + b.authName }

// Setup brings up the requested topology (NumListeners listeners and
// max(NumSenders,1) senders), waits until every listener has logged
// "control channel connected" and every sender has logged its bind
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
func (b *azureBackend) Setup(t testing.TB, opts relayparity.SetupOptions) *relayparity.Tunnel {
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

	listenerArgs := []string{"--metrics-addr", "127.0.0.1:0"}
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

	spawnListener := func(t testing.TB) *relayparity.Listener {
		t.Helper()
		lst := startListener(t, env, auth, listenerArgs...)
		waitForLog(t, lst, "control channel connected", 30*time.Second)
		metricsAddr := lst.MetricsAddr(t, 15*time.Second)
		return &relayparity.Listener{
			Addr:      metricsAddr,
			Completed: scrapeCounter(metricsAddr, "aztunnel_connections_total"),
			Active:    scrapeGauge(metricsAddr, "aztunnel_active_connections"),
			Stop:      func() { lst.Stop(t) },
			Logs:      func() string { return lst.logs.String() },
		}
	}

	senderArgs := []string{"--metrics-addr", "127.0.0.1:0"}

	listeners := make([]*relayparity.Listener, 0, opts.NumListeners)
	for i := 0; i < opts.NumListeners; i++ {
		listeners = append(listeners, spawnListener(t))
	}

	senderAddrs := make([]string, 0, numSenders)
	senders := make([]*relayparity.Sender, 0, numSenders)
	for i := 0; i < numSenders; i++ {
		var proc *aztunnelProcess
		var logMsg string
		switch opts.SenderMode {
		case relayparity.ModePortForward:
			proc = startPortForwardSender(t, env, auth, opts.Target, senderArgs...)
			logMsg = "port-forward listening"
		case relayparity.ModeSOCKS5:
			proc = startSOCKS5Sender(t, env, auth, senderArgs...)
			logMsg = "socks5-proxy listening"
		default:
			t.Fatalf("unknown SenderMode %v", opts.SenderMode)
		}
		bindAddr := waitForLogAddr(t, proc, logMsg, 15*time.Second)
		metricsAddr := proc.MetricsAddr(t, 15*time.Second)
		senderAddrs = append(senderAddrs, bindAddr)
		senders = append(senders, &relayparity.Sender{
			Addr:      bindAddr,
			Completed: scrapeCounter(metricsAddr, "aztunnel_connections_total"),
			Active:    scrapeGauge(metricsAddr, "aztunnel_active_connections"),
			Stop:      func() { proc.Stop(t) },
			Logs:      func() string { return proc.logs.String() },
		})
	}

	tun := &relayparity.Tunnel{
		SenderAddr:  senderAddrs[0],
		SenderAddrs: senderAddrs,
		Senders:     senders,
		Listeners:   listeners,
	}
	tun.AddListener = func(t *testing.T) *relayparity.Listener {
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
