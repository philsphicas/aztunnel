//go:build e2e

package azure

import (
	"context"
	"io"
	"net"
	"os/exec"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/philsphicas/aztunnel/e2e/scenarios"
)

// azureBackend implements scenarios.Backend against a real Azure
// Relay namespace. It is the source-of-truth side of the mock-vs-Azure
// conformance matrix: any scenario divergence between this backend and
// the in-process MockBackend (e2e/backends/mock) is a
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

// authAxis is the scenarios.Axis the Azure backend varies over.
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
func (b *azureBackend) Axes() []scenarios.Axis {
	if b.axis == nil {
		return nil
	}
	return []scenarios.Axis{b.axis}
}

// Cell returns a fresh *azureBackend pinned to the cell described by
// values. Factory backends require values["auth"]; pinned backends
// (axis == nil) accept only an empty values map and return a clone
// of the receiver.
func (b *azureBackend) Cell(values map[string]string) scenarios.Backend {
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

// ConnectLatencyThreshold returns the per-backend connect-latency
// ceiling for the Performance suite. Azure pays the real Azure
// Relay control-plane rendezvous round-trip (~950 ms typical),
// listener-side target dial, plus the bridged echo round-trip;
// 3 s is generous to absorb cloud variance without masking the
// Azure-class regressions (which we expect to be on the order of
// seconds, not hundreds of ms).
//
// Returns 3 s regardless of authName: both Entra and SAS hit the
// same control-plane rendezvous path.
func (*azureBackend) ConnectLatencyThreshold() time.Duration {
	return 3 * time.Second
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
func (b *azureBackend) Setup(t testing.TB, opts scenarios.SetupOptions) *scenarios.Tunnel {
	t.Helper()
	if opts.NumListeners < 0 {
		t.Fatalf("NumListeners must be >= 0, got %d", opts.NumListeners)
	}
	switch opts.SenderMode {
	case scenarios.ModePortForward, scenarios.ModeSOCKS5, scenarios.ModeConnect:
	default:
		t.Fatalf("unknown SenderMode %v", opts.SenderMode)
	}
	numSenders := opts.NumSenders
	if numSenders < 1 {
		numSenders = 1
	}
	// ModeConnect spawns senders on demand via Tunnel.OpenConnect.
	// No persistent sender subprocess at Setup time.
	if opts.SenderMode == scenarios.ModeConnect {
		numSenders = 0
	}

	env := b.acquireEnv(t)
	auth := authFromEnv(t, env, b.authName)
	if opts.OverrideHycoName != "" {
		auth.hyco = opts.OverrideHycoName
	}

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

	spawnListener := func(t testing.TB) *scenarios.Listener {
		t.Helper()
		lst := startListener(t, env, auth, listenerArgs...)
		waitForLog(t, lst, "control_started", 30*time.Second)
		metricsAddr := lst.MetricsAddr(t, 15*time.Second)
		return &scenarios.Listener{
			Addr:             metricsAddr,
			Completed:        scrapeCounter(metricsAddr, "aztunnel_connections_total"),
			Active:           scrapeGauge(metricsAddr, "aztunnel_active_connections"),
			ConnectionErrors: scrapeConnectionErrors(metricsAddr),
			Stop:             func() { lst.Stop(t) },
			Logs:             func() string { return lst.logs.String() },
		}
	}

	senderArgs := []string{"--metrics-addr", "127.0.0.1:0", "--log-level", "debug"}

	listeners := make([]*scenarios.Listener, 0, opts.NumListeners)
	for i := 0; i < opts.NumListeners; i++ {
		listeners = append(listeners, spawnListener(t))
	}

	senderAddrs := make([]string, 0, numSenders)
	senders := make([]*scenarios.Sender, 0, numSenders)
	for i := 0; i < numSenders; i++ {
		var proc *aztunnelProcess
		var logMsg string
		switch opts.SenderMode {
		case scenarios.ModePortForward:
			proc = startPortForwardSender(t, env, auth, opts.Target, senderArgs...)
			logMsg = "port-forward listening"
		case scenarios.ModeSOCKS5:
			proc = startSOCKS5Sender(t, env, auth, senderArgs...)
			logMsg = "socks5-proxy listening"
		}
		bindAddr := waitForLogAddr(t, proc, logMsg, 15*time.Second)
		metricsAddr := proc.MetricsAddr(t, 15*time.Second)
		senderAddrs = append(senderAddrs, bindAddr)
		senders = append(senders, &scenarios.Sender{
			Addr:                bindAddr,
			Completed:           scrapeCounter(metricsAddr, "aztunnel_connections_total"),
			Active:              scrapeGauge(metricsAddr, "aztunnel_active_connections"),
			DialDurationSamples: scrapeHistogramCount(metricsAddr, "aztunnel_dial_duration_seconds"),
			Stop:                func() { proc.Stop(t) },
			Logs:                func() string { return proc.logs.String() },
		})
	}

	tun := &scenarios.Tunnel{
		SenderAddrs: senderAddrs,
		Senders:     senders,
		Listeners:   listeners,
	}
	if len(senderAddrs) > 0 {
		tun.SenderAddr = senderAddrs[0]
	}
	tun.AddListener = func(t *testing.T) *scenarios.Listener {
		t.Helper()
		l := spawnListener(t)
		tun.Listeners = append(tun.Listeners, l)
		return l
	}
	if opts.SenderMode == scenarios.ModeConnect {
		tun.SetOpenConnect(makeAzureOpenConnect(env, auth))
		tun.SetSSHProxyCommand(makeAzureSSHProxyCommand(t, env, auth))
	}
	return tun
}

// makeAzureSSHProxyCommand returns a closure that constructs the
// ssh ProxyCommand argv + env entries for ssh-driven connect
// scenarios. The closure captures the env and auth resolved during
// Setup so the ssh-spawned subprocess uses the SAME hyco
// coordinates as the listener.
func makeAzureSSHProxyCommand(t testing.TB, env *relayEnv, auth authConfig) func(target string) ([]string, []string) {
	t.Helper()
	binary := aztunnelBinary(t)
	return func(target string) ([]string, []string) {
		argv := []string{
			binary,
			"relay-sender", "connect", target,
			"--relay", env.relayName,
			"--hyco", auth.hyco,
			"--log-level", "debug",
		}
		extraEnv := []string{"AZTUNNEL_RELAY_NAME=" + env.relayName}
		if auth.senderSAS != nil {
			extraEnv = append(extraEnv,
				"AZTUNNEL_KEY_NAME="+auth.senderSAS.keyName,
				"AZTUNNEL_KEY="+auth.senderSAS.key,
			)
		}
		return argv, extraEnv
	}
}

// makeAzureOpenConnect returns the Tunnel.OpenConnect closure for the
// Azure backend. Each call launches a fresh `aztunnel relay-sender
// connect <target>` subprocess, piping stdin/stdout/stderr. Closing
// the returned ConnectClient kills the subprocess.
func makeAzureOpenConnect(env *relayEnv, auth authConfig) func(t testing.TB, target string) scenarios.ConnectClient {
	return func(t testing.TB, target string) scenarios.ConnectClient {
		t.Helper()
		binary := aztunnelBinary(t)
		ctx, cancel := context.WithCancel(context.Background())
		cmd := exec.CommandContext(ctx, binary,
			"relay-sender", "connect", target,
			"--relay", env.relayName,
			"--hyco", auth.hyco,
			"--log-level", "debug",
		)
		setAztunnelEnv(cmd, env, auth.senderSAS)
		stdin, err := cmd.StdinPipe()
		if err != nil {
			cancel()
			t.Fatalf("stdin pipe: %v", err)
		}
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			cancel()
			t.Fatalf("stdout pipe: %v", err)
		}
		logs := &logBuffer{}
		cmd.Stderr = logs
		if err := cmd.Start(); err != nil {
			cancel()
			t.Fatalf("start connect: %v", err)
		}
		cc := &azureConnectClient{cmd: cmd, stdin: stdin, stdout: stdout, logs: logs, cancel: cancel}
		t.Cleanup(func() { _ = cc.Close() })
		return cc
	}
}

// azureConnectClient is the Azure backend's scenarios.ConnectClient
// implementation. Bridges stdio of the relay-sender connect
// subprocess; Logs is the captured stderr; Wait blocks on cmd.Wait;
// Close kills the subprocess (idempotent).
type azureConnectClient struct {
	cmd      *exec.Cmd
	stdin    io.WriteCloser
	stdout   io.ReadCloser
	logs     *logBuffer
	cancel   context.CancelFunc
	closeOne sync.Once
	waitErr  error
	waitDone chan struct{}
	waitOnce sync.Once
}

func (c *azureConnectClient) Read(p []byte) (int, error)  { return c.stdout.Read(p) }
func (c *azureConnectClient) Write(p []byte) (int, error) { return c.stdin.Write(p) }
func (c *azureConnectClient) Logs() string                { return c.logs.String() }

func (c *azureConnectClient) Wait(ctx context.Context) error {
	c.waitOnce.Do(func() {
		c.waitDone = make(chan struct{})
		go func() {
			c.waitErr = c.cmd.Wait()
			close(c.waitDone)
		}()
	})
	select {
	case <-c.waitDone:
		return c.waitErr
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *azureConnectClient) Close() error {
	c.closeOne.Do(func() {
		// Close the stdio pipes first so any caller blocked in
		// Read/Write returns immediately, and so the subprocess
		// sees EOF on its stdin.
		_ = c.stdin.Close()
		_ = c.stdout.Close()
		c.cancel()
		if c.cmd.Process != nil {
			_ = c.cmd.Process.Kill()
		}
		// Kick off Wait if not already running, then block on it
		// so the subprocess is reaped and its file descriptors
		// released before Close returns. AssertNoLeaks (goroutine
		// + FD deltas) runs in scenario t.Cleanup and would
		// false-positive if the Wait goroutine outlives Close.
		c.waitOnce.Do(func() {
			c.waitDone = make(chan struct{})
			go func() {
				c.waitErr = c.cmd.Wait()
				close(c.waitDone)
			}()
		})
		// Bounded wait for reap. cmd.Process.Kill is SIGKILL on
		// Unix; the process should exit within seconds. The 3 s
		// budget keeps Close from blocking test cleanup forever
		// on a stuck child.
		select {
		case <-c.waitDone:
		case <-time.After(3 * time.Second):
		}
	})
	return nil
}

// SetupExpectingFailure brings up the Azure topology with the auth
// overrides applied. Listener-side overrides wait for the listener's
// "control channel disconnected" log; sender-side overrides start
// the sender and either wait for "relay dial failed" or trigger it
// with one local TCP connect; ModeConnect overrides leave the
// sender for the caller to spawn via Tunnel.OpenConnect.
func (b *azureBackend) SetupExpectingFailure(t testing.TB, opts scenarios.SetupOptions) scenarios.FailureHandle {
	t.Helper()
	switch opts.SenderMode {
	case scenarios.ModePortForward, scenarios.ModeSOCKS5, scenarios.ModeConnect:
	default:
		t.Fatalf("unknown SenderMode %v", opts.SenderMode)
	}
	if opts.OverrideListenerAuth == nil && opts.OverrideSenderAuth == nil && opts.OverrideHycoName == "" {
		t.Fatalf("SetupExpectingFailure requires at least one override (ListenerAuth, SenderAuth, or HycoName)")
	}

	env := b.acquireEnv(t)
	auth := authFromEnv(t, env, b.authName)
	hyco := auth.hyco
	if opts.OverrideHycoName != "" {
		hyco = opts.OverrideHycoName
	}

	// Resolve listener / sender SAS credentials with overrides
	// applied. Entra-token overrides are not yet wired; the
	// scope-gated CrossClaim scenario uses ListenerAuth/SenderAuth
	// directly.
	listenerSAS := auth.listenerSAS
	senderSAS := auth.senderSAS
	if opts.OverrideListenerAuth != nil && opts.OverrideListenerAuth.BadSASKey != "" {
		listenerSAS = &sasCredentials{
			keyName: keyNameOr(auth.listenerSAS, env.sasListenerKeyName),
			key:     opts.OverrideListenerAuth.BadSASKey,
		}
	}
	if opts.OverrideSenderAuth != nil && opts.OverrideSenderAuth.BadSASKey != "" {
		senderSAS = &sasCredentials{
			keyName: keyNameOr(auth.senderSAS, env.sasSenderKeyName),
			key:     opts.OverrideSenderAuth.BadSASKey,
		}
	}

	listenerArgs := []string{
		"relay-listener",
		"--hyco", hyco,
		"--relay", env.relayName,
		"--log-level", "debug",
	}
	for _, target := range opts.AllowedTargets {
		listenerArgs = append(listenerArgs, "--allow", target)
	}

	listenerLogs := func() string { return "" }
	senderLogs := func() string { return "" }

	// Listener-side failure: spin up listener, wait for the
	// control-channel disconnect log line.
	if opts.OverrideListenerAuth != nil || opts.OverrideHycoName != "" {
		proc := startAztunnelWithSAS(t, env, listenerSAS, listenerArgs...)
		listenerLogs = func() string { return proc.logs.String() }
		waitForLog(t, proc, "control channel disconnected", 30*time.Second)
		return &azureFailureHandle{listenerLogs: listenerLogs, senderLogs: senderLogs}
	}

	// Sender-side failure: bring up a healthy listener (if asked)
	// then start the sender with bad creds and observe.
	if opts.NumListeners > 0 {
		lp := startListener(t, env, auth, "--metrics-addr", "127.0.0.1:0", "--log-level", "debug")
		listenerLogs = func() string { return lp.logs.String() }
		waitForLog(t, lp, "control_started", 30*time.Second)
	}

	if opts.SenderMode == scenarios.ModeConnect {
		// Caller drives the failure via Tunnel.OpenConnect. Build
		// a fake auth config that carries the overridden sender SAS.
		failAuth := auth
		failAuth.senderSAS = senderSAS
		failAuth.hyco = hyco
		return &azureFailureHandle{
			listenerLogs: listenerLogs,
			senderLogs:   senderLogs,
			openConnect:  makeAzureOpenConnect(env, failAuth),
		}
	}

	// Port-forward or SOCKS5: start sender with bad creds, dial
	// locally to trigger the relay dial, wait for the failure log.
	var proc *aztunnelProcess
	switch opts.SenderMode {
	case scenarios.ModePortForward:
		target := opts.Target
		if target == "" {
			target = "127.0.0.1:9999"
		}
		proc = startAztunnelWithSAS(t, env, senderSAS,
			"relay-sender", "port-forward", target,
			"--relay", env.relayName,
			"--hyco", hyco,
			"--bind", "127.0.0.1:0",
			"--log-level", "debug",
		)
	case scenarios.ModeSOCKS5:
		proc = startAztunnelWithSAS(t, env, senderSAS,
			"relay-sender", "socks5-proxy",
			"--relay", env.relayName,
			"--hyco", hyco,
			"--bind", "127.0.0.1:0",
			"--log-level", "debug",
		)
	}
	senderLogs = func() string { return proc.logs.String() }

	bindAddr := waitForLogAddr(t, proc, senderBindLogPrefix(opts.SenderMode), 15*time.Second)

	conn, err := net.DialTimeout("tcp", bindAddr, 5*time.Second)
	if err != nil {
		t.Fatalf("dial sender during SetupExpectingFailure: %v", err)
	}
	azureTriggerSenderRelayDial(conn, opts.SenderMode)
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, _ = io.ReadAll(conn)
	_ = conn.Close()

	waitForLog(t, proc, "relay dial failed", 30*time.Second)
	return &azureFailureHandle{listenerLogs: listenerLogs, senderLogs: senderLogs}
}

// azureTriggerSenderRelayDial sends the minimal byte sequence each
// sender mode requires to provoke its relay dial. Mirrors the
// mock backend helper of the same shape; kept per-backend to
// avoid pulling a helper into the scenarios package that only
// failure-mode tests need.
func azureTriggerSenderRelayDial(conn net.Conn, mode scenarios.SenderMode) {
	switch mode {
	case scenarios.ModeSOCKS5:
		// SOCKS5 greeting: VER=5, NMETHODS=1, METHOD=0 (no auth).
		_, _ = conn.Write([]byte{0x05, 0x01, 0x00})
		_ = conn.SetReadDeadline(time.Now().Add(1 * time.Second))
		drain := make([]byte, 2)
		_, _ = io.ReadFull(conn, drain)
		// CONNECT request: VER=5, CMD=1 (CONNECT), RSV=0,
		// ATYP=1 (IPv4), DST=127.0.0.1, PORT=9999.
		_, _ = conn.Write([]byte{0x05, 0x01, 0x00, 0x01, 127, 0, 0, 1, 0x27, 0x0F})
	default:
		_, _ = conn.Write([]byte("trigger\n"))
	}
}

// senderBindLogPrefix returns the "x listening" log substring that
// each sender mode emits when its bind is ready.
func senderBindLogPrefix(mode scenarios.SenderMode) string {
	switch mode {
	case scenarios.ModeSOCKS5:
		return "socks5-proxy listening"
	default:
		return "port-forward listening"
	}
}

// keyNameOr returns sas.keyName if sas is non-nil and has a
// keyName, else fallback. Used when an override carries a bad
// key value but not a key name.
func keyNameOr(sas *sasCredentials, fallback string) string {
	if sas != nil && sas.keyName != "" {
		return sas.keyName
	}
	return fallback
}

// azureFailureHandle is the Azure backend's scenarios.FailureHandle
// implementation. Logs accessors snapshot from logBuffer; Close is a
// no-op because the parent t.Cleanup already kills + reaps every
// subprocess.
type azureFailureHandle struct {
	listenerLogs func() string
	senderLogs   func() string
	openConnect  func(t testing.TB, target string) scenarios.ConnectClient
}

func (h *azureFailureHandle) ListenerLogs() string { return h.listenerLogs() }
func (h *azureFailureHandle) SenderLogs() string   { return h.senderLogs() }
func (h *azureFailureHandle) Close()               {}

// OpenConnect lets ModeConnect failure scenarios drive a connect
// invocation against this handle's overridden auth credentials.
// Fatals if the handle was not built for a ModeConnect scenario.
func (h *azureFailureHandle) OpenConnect(t testing.TB, target string) scenarios.ConnectClient {
	t.Helper()
	if h.openConnect == nil {
		t.Fatalf("FailureHandle.OpenConnect called on a non-ModeConnect handle")
	}
	return h.openConnect(t, target)
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

// scrapeHistogramCount returns a closure that scrapes /metrics on
// addr and returns the sum of <name>_count across every label
// combination — i.e. the total number of observations recorded in
// the histogram named `name`. Used by ScenarioMetrics_DialDuration
// to confirm the dial path actually observed the histogram.
func scrapeHistogramCount(addr, name string) func() uint64 {
	return func() uint64 {
		text := scrapeMetricsBest(addr)
		return uint64(sumMetric(text, name+"_count"))
	}
}
