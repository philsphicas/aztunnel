//go:build e2e

package e2e

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/philsphicas/aztunnel/e2e/azrelay"
)

// relayEnv holds the Azure Relay configuration for e2e tests.
type relayEnv struct {
	relayName          string // Azure Relay namespace name
	hyco               string // hybrid connection name (Entra ID)
	sasHyco            string // hybrid connection name for SAS tests (optional)
	sasListenerKeyName string // SAS listener key name (optional)
	sasListenerKey     string // SAS listener key (optional)
	sasSenderKeyName   string // SAS sender key name (optional)
	sasSenderKey       string // SAS sender key (optional)
}

// authConfig describes a single auth method to test against.
type authConfig struct {
	name        string          // "entra" or "sas"
	hyco        string          // which hybrid connection to use
	listenerSAS *sasCredentials // nil → Entra ID auth
	senderSAS   *sasCredentials // nil → Entra ID auth
}

// requireRelayEnv is a thin compatibility forwarder over
// requireDedicatedHyco. Every caller of this function now receives a
// freshly-provisioned, dedicated hyco pair; the legacy shared
// E2E_*_HYCO_NAME / E2E_SAS_*_KEY env vars are no longer consulted.
//
// Kept (rather than mechanically renaming all callers) for two
// reasons during the per-test isolation migration (#49):
//
//  1. parity_test.go and parity_bench_test.go (slice-2 territory)
//     intentionally do NOT call t.Parallel() and own their own
//     migration path; they continue to call requireRelayEnv so the
//     parallel-test rules in requireDedicatedHyco's godoc do not
//     apply to them transitively. They still get a dedicated pair,
//     just on the serial path.
//
//  2. Avoids a "silent t.Skip" failure mode during the migration
//     window: if TestMain stopped pre-provisioning the legacy shared
//     pair AND an unmigrated test still read E2E_ENTRA_HYCO_NAME,
//     that test would skip without provisioning anything, leaving
//     CI green but coverage gone.
//
// Phase 6 will delete this once parity tests have their own
// per-scenario Backend wiring and there are no callers left.
func requireRelayEnv(t testing.TB) *relayEnv {
	t.Helper()
	return requireDedicatedHyco(t)
}

// hycoProvisionTimeout bounds a single Provider.Provision call from
// requireDedicatedHyco. The provisioner already retries 429s and
// transient 5xx through azcore (MaxRetries=6, MaxRetryDelay=60s) and
// 40901 conflicts through retryOnAuthRuleConflict; this ceiling
// stops a genuinely stuck control plane from hanging the test until
// the suite-wide go-test -timeout fires.
const hycoProvisionTimeout = 3 * time.Minute

// hycoTeardownTimeout bounds the t.Cleanup-registered Teardown call.
// Teardown also gates on the Provider semaphore so a swarm of test
// cleanups cannot stampede the namespace 429 envelope; the budget
// here must be larger than the worst-case sem wait + 2 × ARM Delete.
const hycoTeardownTimeout = 90 * time.Second

// requireProvider returns the process-scoped Provider. Skips the
// test when E2E_RELAY_NAME is unset (i.e. when TestMain did not
// construct a Provider).
func requireProvider(t testing.TB) *azrelay.Provider {
	t.Helper()
	if relayProvider == nil {
		t.Skip("E2E_RELAY_NAME must be set for e2e tests")
	}
	return relayProvider
}

// requireDedicatedHyco provisions a fresh (entra, sas) hyco pair for
// the calling test, registers a t.Cleanup that tears the pair down,
// and returns its connection metadata in the legacy *relayEnv shape.
// The pair is independent of every other test's pair: there is no
// way for a stray listener or sender from another test to route
// through it.
//
// # Ordering requirement
//
// Callers MUST invoke t.Parallel() BEFORE calling this function:
//
//	func TestSomething(t *testing.T) {
//	    t.Parallel()                            // FIRST
//	    env := requireDedicatedHyco(t)          // THEN provision
//	    auth := availableAuths(t, env)[0]
//	    // ...
//	}
//
// Go's testing package only releases a test to run in parallel with
// its peers once it calls t.Parallel(). If a test calls
// requireDedicatedHyco BEFORE t.Parallel(), the Provision will
// happen on the serial path and the Provider's concurrency semaphore
// cannot overlap it with peer provisions — the suite-wide wall-clock
// win collapses. Reviewing this ordering is a code-review checklist
// item; the function cannot enforce it because the testing package
// exposes no "am I parallel yet?" signal.
//
// Skips the test when E2E_RELAY_NAME is unset.
//
// Backend contract (for callers wiring this into a parity Backend
// implementation): one call → one fresh hyco pair. Scenarios that
// need multiple hyco pairs (e.g. cross-version listeners on
// different hycos) call requireDedicatedHyco multiple times and pay
// N× provisioning. Cross-call sharing within one scenario is out of
// scope for the Backend.Setup contract.
func requireDedicatedHyco(t testing.TB) *relayEnv {
	t.Helper()
	p := requireProvider(t)

	ctx, cancel := context.WithTimeout(context.Background(), hycoProvisionTimeout)
	defer cancel()
	tok, err := p.Provision(ctx)
	if err != nil {
		t.Fatalf("provision dedicated hyco pair: %v", err)
	}

	entra, sas := tok.HycoNames()
	t.Logf("provisioned dedicated hyco pair: %s, %s", entra, sas)

	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), hycoTeardownTimeout)
		defer cancel()
		if err := tok.Teardown(ctx); err != nil {
			// Log only — the janitor will reap anything we miss,
			// and failing the test on cleanup errors would mask
			// the actual test outcome.
			t.Logf("teardown dedicated hyco pair %s/%s: %v", entra, sas, err)
		}
	})

	r := tok.Result()
	return &relayEnv{
		relayName:          r.RelayName,
		hyco:               r.EntraHycoName,
		sasHyco:            r.SASHycoName,
		sasListenerKeyName: r.ListenerKeyName,
		sasListenerKey:     r.ListenerKey,
		sasSenderKeyName:   r.SenderKeyName,
		sasSenderKey:       r.SenderKey,
	}
}

// availableAuths returns auth configurations for each available method.
// Tests can iterate over these to run against both Entra and SAS.
// Set E2E_AUTH=entra or E2E_AUTH=sas to restrict to a single method.
func availableAuths(t testing.TB, env *relayEnv) []authConfig {
	t.Helper()
	filter := os.Getenv("E2E_AUTH") // "", "entra", "sas"
	switch filter {
	case "", "entra", "sas":
	default:
		t.Fatalf("unsupported E2E_AUTH value %q; expected \"entra\", \"sas\", or \"\" (both)", filter)
	}

	var configs []authConfig
	if env.hyco != "" && filter != "sas" {
		configs = append(configs, authConfig{
			name: "entra",
			hyco: env.hyco,
		})
	}
	if env.sasHyco != "" && env.sasListenerKeyName != "" && env.sasListenerKey != "" && env.sasSenderKeyName != "" && env.sasSenderKey != "" && filter != "entra" {
		configs = append(configs, authConfig{
			name:        "sas",
			hyco:        env.sasHyco,
			listenerSAS: &sasCredentials{keyName: env.sasListenerKeyName, key: env.sasListenerKey},
			senderSAS:   &sasCredentials{keyName: env.sasSenderKeyName, key: env.sasSenderKey},
		})
	}
	if len(configs) == 0 {
		t.Skip("no auth configured (need E2E_ENTRA_HYCO_NAME or SAS credentials)")
	}
	return configs
}

// startListener starts an aztunnel relay-listener with the given auth config.
func startListener(t testing.TB, env *relayEnv, auth authConfig, extraArgs ...string) *aztunnelProcess {
	t.Helper()
	args := append([]string{
		"relay-listener",
		"--hyco", auth.hyco,
		"--relay", env.relayName,
	}, extraArgs...)
	return startAztunnelWithSAS(t, env, auth.listenerSAS, args...)
}

// startPortForwardSender starts an aztunnel relay-sender port-forward with the given auth config.
func startPortForwardSender(t testing.TB, env *relayEnv, auth authConfig, target string, extraArgs ...string) *aztunnelProcess {
	t.Helper()
	args := append([]string{
		"relay-sender", "port-forward", target,
		"--relay", env.relayName,
		"--hyco", auth.hyco,
		"--bind", "127.0.0.1:0",
	}, extraArgs...)
	return startAztunnelWithSAS(t, env, auth.senderSAS, args...)
}

// startSOCKS5Sender starts an aztunnel relay-sender socks5-proxy with the given auth config.
func startSOCKS5Sender(t testing.TB, env *relayEnv, auth authConfig, extraArgs ...string) *aztunnelProcess {
	t.Helper()
	args := append([]string{
		"relay-sender", "socks5-proxy",
		"--relay", env.relayName,
		"--hyco", auth.hyco,
		"--bind", "127.0.0.1:0",
	}, extraArgs...)
	return startAztunnelWithSAS(t, env, auth.senderSAS, args...)
}

var (
	buildOnce   sync.Once
	builtBinary string
	buildErr    error
)

// aztunnelBinary builds the aztunnel binary once and returns its path.
func aztunnelBinary(t testing.TB) string {
	t.Helper()
	buildOnce.Do(func() {
		// Find the repo root (parent of e2e/).
		dir, _ := os.Getwd()
		root := filepath.Dir(dir)
		if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
			// Try current dir if we're running from root.
			root = dir
		}
		builtBinary = filepath.Join(root, "bin", "aztunnel")
		cmd := exec.Command("go", "build", "-o", builtBinary, "./cmd/aztunnel")
		cmd.Dir = root
		out, err := cmd.CombinedOutput()
		if err != nil {
			buildErr = fmt.Errorf("build: %w\n%s", err, out)
		}
	})
	if buildErr != nil {
		t.Fatalf("build aztunnel: %v", buildErr)
	}
	return builtBinary
}

// aztunnelProcess represents a running aztunnel process with log capture.
type aztunnelProcess struct {
	cmd      *exec.Cmd
	logs     *logBuffer
	cancel   func()
	stopOnce sync.Once
}

// Stop kills the process and waits for it to exit. Safe to call multiple times;
// the second and subsequent calls are no-ops. The Cleanup hook registered by
// startAztunnelWithSAS calls Stop too, so tests only need to call this when
// they want to terminate a process mid-test (e.g. listener restart scenarios).
func (p *aztunnelProcess) Stop(t testing.TB) {
	t.Helper()
	p.stopOnce.Do(func() {
		if p.cmd.Process != nil {
			_ = p.cmd.Process.Kill()
		}
		_ = p.cmd.Wait()
	})
}

// MetricsAddr waits for the "metrics server listening addr=…" log line and
// returns the address. Use this in tests that pass --metrics-addr 127.0.0.1:0.
func (p *aztunnelProcess) MetricsAddr(t testing.TB, timeout time.Duration) string {
	t.Helper()
	return waitForLogAddr(t, p, "metrics server listening", timeout)
}

// logBuffer is a thread-safe buffer that captures log output and supports
// waiting for specific log messages.
type logBuffer struct {
	mu      sync.Mutex
	lines   []string
	partial string // incomplete line from previous Write
	waiters []logWaiter
}

type logWaiter struct {
	substr string
	ch     chan string
}

func (lb *logBuffer) Write(p []byte) (int, error) {
	lb.mu.Lock()
	defer lb.mu.Unlock()

	data := lb.partial + string(p)
	lb.partial = ""

	for {
		i := strings.IndexByte(data, '\n')
		if i == -1 {
			lb.partial = data
			break
		}
		line := data[:i]
		data = data[i+1:]
		lb.lines = append(lb.lines, line)
		// Check if any waiters match.
		remaining := lb.waiters[:0]
		for _, w := range lb.waiters {
			if strings.Contains(line, w.substr) {
				select {
				case w.ch <- line:
				default:
				}
			} else {
				remaining = append(remaining, w)
			}
		}
		lb.waiters = remaining
	}
	return len(p), nil
}

// String returns all captured log lines joined with newlines.
func (lb *logBuffer) String() string {
	lb.mu.Lock()
	defer lb.mu.Unlock()
	return strings.Join(lb.lines, "\n")
}

// waitFor blocks until a log line containing substr appears, or times out.
func (lb *logBuffer) waitFor(substr string, timeout time.Duration) (string, bool) {
	ch := make(chan string, 1)

	lb.mu.Lock()
	// Check existing lines first.
	for _, line := range lb.lines {
		if strings.Contains(line, substr) {
			lb.mu.Unlock()
			return line, true
		}
	}
	lb.waiters = append(lb.waiters, logWaiter{substr: substr, ch: ch})
	lb.mu.Unlock()

	select {
	case line := <-ch:
		return line, true
	case <-time.After(timeout):
		lb.mu.Lock()
		for i, w := range lb.waiters {
			if w.ch == ch {
				lb.waiters = append(lb.waiters[:i], lb.waiters[i+1:]...)
				break
			}
		}
		lb.mu.Unlock()
		return "", false
	}
}

// startAztunnel starts an aztunnel process with the given args and returns
// a handle. The process is killed on test cleanup.
func startAztunnel(t testing.TB, env *relayEnv, args ...string) *aztunnelProcess {
	return startAztunnelWithSAS(t, env, nil, args...)
}

// startAztunnelWithSAS starts an aztunnel process with explicit SAS credentials.
func startAztunnelWithSAS(t testing.TB, env *relayEnv, sas *sasCredentials, args ...string) *aztunnelProcess {
	t.Helper()
	binary := aztunnelBinary(t)

	cmd := exec.Command(binary, args...)
	setAztunnelEnv(cmd, env, sas)

	logs := &logBuffer{}
	cmd.Stderr = logs // aztunnel logs to stderr
	cmd.Stdout = os.Stdout

	if err := cmd.Start(); err != nil {
		t.Fatalf("start aztunnel %v: %v", args, err)
	}

	proc := &aztunnelProcess{
		cmd:  cmd,
		logs: logs,
	}

	t.Cleanup(func() { proc.Stop(t) })

	return proc
}

// metricsScrapeClient is used by scrapeMetricsBest so that polling helpers
// don't get wedged on a stalled /metrics response (the default http.Client
// has no timeout, which would defeat the deadline in waitForMetric /
// waitForMetricsContains). The timeout is generous relative to the 100ms
// polling cadence but well below typical test timeouts.
var metricsScrapeClient = &http.Client{Timeout: 2 * time.Second}

// scrapeMetricsBest fetches /metrics from addr and returns the body, or "" on
// any error. Use this inside polling loops (waitForMetric) where transient
// fetch failures are tolerable. For one-shot reads with hard failure on error,
// use scrapeMetrics (defined in e2e_test.go).
func scrapeMetricsBest(addr string) string {
	resp, err := metricsScrapeClient.Get("http://" + addr + "/metrics")
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return string(body)
}

// waitForMetric polls /metrics on addr at 100ms intervals until sumMetric(text,
// name) satisfies predicate, then returns the satisfying value. Calls
// t.Fatalf if the predicate is not satisfied before timeout. Replaces the
// time.Sleep+scrapeMetrics idiom that was previously sprinkled through e2e
// tests and made them racy on slow CI.
func waitForMetric(t *testing.T, addr, name string, predicate func(float64) bool, timeout time.Duration) float64 {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last float64
	for time.Now().Before(deadline) {
		text := scrapeMetricsBest(addr)
		last = sumMetric(text, name)
		if predicate(last) {
			return last
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("waitForMetric: %s on %s did not satisfy predicate within %v (last value %v)", name, addr, timeout, last)
	return 0 // unreachable; t.Fatalf terminates the goroutine
}

// waitForMetricsContains polls /metrics on addr at 100ms intervals until the
// response body contains want, then returns the body. Calls t.Fatalf on
// timeout. Use this for label-presence checks (e.g. `reason="dial_failed"`)
// or histogram-presence checks (e.g. `aztunnel_dial_duration_seconds`) that
// sumMetric can't express.
func waitForMetricsContains(t *testing.T, addr, want string, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last string
	for time.Now().Before(deadline) {
		last = scrapeMetricsBest(addr)
		if strings.Contains(last, want) {
			return last
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("waitForMetricsContains: /metrics on %s did not contain %q within %v\nlast body:\n%s", addr, want, timeout, last)
	return last // unreachable
}

// sasCredentials holds a SAS key name and key for a specific role.
type sasCredentials struct {
	keyName string
	key     string
}

// setAztunnelEnv configures environment variables for an aztunnel subprocess.
// If sas is non-nil, it sets AZTUNNEL_KEY_NAME and AZTUNNEL_KEY for SAS auth.
func setAztunnelEnv(cmd *exec.Cmd, env *relayEnv, sas *sasCredentials) {
	cmd.Env = append(os.Environ(),
		"AZTUNNEL_RELAY_NAME="+env.relayName,
	)
	if sas != nil {
		cmd.Env = append(cmd.Env,
			"AZTUNNEL_KEY_NAME="+sas.keyName,
			"AZTUNNEL_KEY="+sas.key,
		)
	}
}

// waitForLog waits for a log line containing the given substring.
func waitForLog(t testing.TB, proc *aztunnelProcess, substr string, timeout time.Duration) string {
	t.Helper()
	line, ok := proc.logs.waitFor(substr, timeout)
	if !ok {
		t.Fatalf("timed out waiting for log: %q", substr)
	}
	return line
}

// addrRe extracts addr=host:port from log lines.
var addrRe = regexp.MustCompile(`addr=([^\s]+)`)

// waitForLogAddr waits for a log line and extracts the addr= value.
func waitForLogAddr(t testing.TB, proc *aztunnelProcess, substr string, timeout time.Duration) string {
	t.Helper()
	line := waitForLog(t, proc, substr, timeout)
	m := addrRe.FindStringSubmatch(line)
	if m == nil {
		// Try bind= pattern too.
		bindRe := regexp.MustCompile(`bind=([^\s]+)`)
		m = bindRe.FindStringSubmatch(line)
	}
	if m == nil {
		t.Fatalf("no addr= or bind= in log line: %s", line)
	}
	return m[1]
}

// dialSOCKS5 performs a SOCKS5 handshake through the proxy to reach target.
func dialSOCKS5(t *testing.T, proxyAddr, target string) net.Conn {
	t.Helper()
	conn, err := dialSOCKS5E(proxyAddr, target)
	if err != nil {
		t.Fatalf("socks5 dial %s via %s: %v", target, proxyAddr, err)
	}
	return conn
}

// dialSOCKS5E is like dialSOCKS5 but returns an error instead of calling t.Fatalf.
// Safe to call from goroutines.
func dialSOCKS5E(proxyAddr, target string) (net.Conn, error) {
	conn, err := net.DialTimeout("tcp", proxyAddr, 10*time.Second)
	if err != nil {
		return nil, fmt.Errorf("dial proxy: %w", err)
	}

	host, portStr, err := net.SplitHostPort(target)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("parse target: %w", err)
	}
	var port uint16
	var portInt int
	if n, err := fmt.Sscanf(portStr, "%d", &portInt); err != nil || n != 1 || portInt <= 0 || portInt > 65535 {
		conn.Close()
		return nil, fmt.Errorf("parse port %q: invalid", portStr)
	}
	port = uint16(portInt)

	// Auth negotiation: version=5, 1 method, no-auth.
	if _, err := conn.Write([]byte{0x05, 0x01, 0x00}); err != nil {
		conn.Close()
		return nil, fmt.Errorf("socks5 auth write: %w", err)
	}
	resp := make([]byte, 2)
	if _, err := io.ReadFull(conn, resp); err != nil {
		conn.Close()
		return nil, fmt.Errorf("socks5 auth response: %w", err)
	}
	if resp[0] != 0x05 || resp[1] != 0x00 {
		conn.Close()
		return nil, fmt.Errorf("socks5 auth: unexpected %v", resp)
	}

	// Connect request.
	req := []byte{0x05, 0x01, 0x00} // ver, connect, rsv
	ip := net.ParseIP(host)
	if ip4 := ip.To4(); ip4 != nil {
		req = append(req, 0x01) // IPv4
		req = append(req, ip4...)
	} else if ip != nil {
		req = append(req, 0x04) // IPv6
		req = append(req, ip...)
	} else {
		// Domain name.
		req = append(req, 0x03, byte(len(host)))
		req = append(req, []byte(host)...)
	}
	portBytes := make([]byte, 2)
	binary.BigEndian.PutUint16(portBytes, port)
	req = append(req, portBytes...)

	if _, err := conn.Write(req); err != nil {
		conn.Close()
		return nil, fmt.Errorf("socks5 connect write: %w", err)
	}

	// Read response (at least 4 bytes header).
	connResp := make([]byte, 4)
	if _, err := io.ReadFull(conn, connResp); err != nil {
		conn.Close()
		return nil, fmt.Errorf("socks5 connect response: %w", err)
	}
	// Read remaining address bytes based on address type.
	switch connResp[3] {
	case 0x01: // IPv4
		extra := make([]byte, 4+2) // 4 IP + 2 port
		if _, err := io.ReadFull(conn, extra); err != nil {
			conn.Close()
			return nil, fmt.Errorf("socks5 read ipv4 addr: %w", err)
		}
	case 0x04: // IPv6
		extra := make([]byte, 16+2)
		if _, err := io.ReadFull(conn, extra); err != nil {
			conn.Close()
			return nil, fmt.Errorf("socks5 read ipv6 addr: %w", err)
		}
	case 0x03: // domain
		lenByte := make([]byte, 1)
		if _, err := io.ReadFull(conn, lenByte); err != nil {
			conn.Close()
			return nil, fmt.Errorf("socks5 read domain len: %w", err)
		}
		extra := make([]byte, int(lenByte[0])+2)
		if _, err := io.ReadFull(conn, extra); err != nil {
			conn.Close()
			return nil, fmt.Errorf("socks5 read domain addr: %w", err)
		}
	}

	if connResp[1] != 0x00 {
		// Connection failed but return the conn anyway for error testing.
		return conn, nil
	}

	return conn, nil
}
