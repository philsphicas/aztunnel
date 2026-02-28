//go:build e2e

package e2e

import (
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
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

// requireRelayEnv reads config from env vars and skips if nothing is configured.
func requireRelayEnv(t *testing.T) *relayEnv {
	t.Helper()
	relay := os.Getenv("E2E_RELAY_NAME")
	if relay == "" {
		t.Skip("E2E_RELAY_NAME must be set for e2e tests")
	}
	hyco := os.Getenv("E2E_ENTRA_HYCO_NAME")
	sasHyco := os.Getenv("E2E_SAS_HYCO_NAME")
	if hyco == "" && sasHyco == "" {
		t.Skip("at least one of E2E_ENTRA_HYCO_NAME or E2E_SAS_HYCO_NAME must be set")
	}
	return &relayEnv{
		relayName:          relay,
		hyco:               hyco,
		sasHyco:            sasHyco,
		sasListenerKeyName: os.Getenv("E2E_SAS_LISTENER_KEY_NAME"),
		sasListenerKey:     os.Getenv("E2E_SAS_LISTENER_KEY"),
		sasSenderKeyName:   os.Getenv("E2E_SAS_SENDER_KEY_NAME"),
		sasSenderKey:       os.Getenv("E2E_SAS_SENDER_KEY"),
	}
}

// availableAuths returns auth configurations for each available method.
// Tests can iterate over these to run against both Entra and SAS.
// Set E2E_AUTH=entra or E2E_AUTH=sas to restrict to a single method.
func availableAuths(t *testing.T, env *relayEnv) []authConfig {
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
func startListener(t *testing.T, env *relayEnv, auth authConfig, extraArgs ...string) *aztunnelProcess {
	t.Helper()
	args := append([]string{
		"relay-listener", auth.hyco,
		"--relay", env.relayName,
	}, extraArgs...)
	return startAztunnelWithSAS(t, env, auth.listenerSAS, args...)
}

// startPortForwardSender starts an aztunnel relay-sender port-forward with the given auth config.
func startPortForwardSender(t *testing.T, env *relayEnv, auth authConfig, target string, extraArgs ...string) *aztunnelProcess {
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
func startSOCKS5Sender(t *testing.T, env *relayEnv, auth authConfig, extraArgs ...string) *aztunnelProcess {
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
func aztunnelBinary(t *testing.T) string {
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
	cmd    *exec.Cmd
	logs   *logBuffer
	cancel func()
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
func startAztunnel(t *testing.T, env *relayEnv, args ...string) *aztunnelProcess {
	return startAztunnelWithSAS(t, env, nil, args...)
}

// startAztunnelWithSAS starts an aztunnel process with explicit SAS credentials.
func startAztunnelWithSAS(t *testing.T, env *relayEnv, sas *sasCredentials, args ...string) *aztunnelProcess {
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

	t.Cleanup(func() {
		if cmd.Process != nil {
			cmd.Process.Kill()
		}
		cmd.Wait()
	})

	return proc
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
func waitForLog(t *testing.T, proc *aztunnelProcess, substr string, timeout time.Duration) string {
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
func waitForLogAddr(t *testing.T, proc *aztunnelProcess, substr string, timeout time.Duration) string {
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
