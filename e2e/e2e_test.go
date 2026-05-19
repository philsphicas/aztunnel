//go:build e2e

// Package e2e contains end-to-end tests for aztunnel that run against a real
// Azure Relay. Tests are gated behind the "e2e" build tag and require:
//
//   - E2E_RELAY_NAME: Azure Relay namespace name
//   - E2E_RESOURCE_GROUP: resource group containing the namespace
//   - AZURE_SUBSCRIPTION_ID: subscription ID for ARM API calls
//   - Valid Azure credentials (az login, managed identity, OIDC, etc.)
//
// Run `eval "$(make e2e-infra-env)"` to export all three from the
// resource group provisioned by `make e2e-infra-setup`; the
// `e2e-infra env` tool resolves AZURE_SUBSCRIPTION_ID from the
// Azure CLI default if not already exported.
//
// Each test provisions its own pair of ephemeral hybrid connections —
// e2e-entra-<hex> for Entra ID auth and e2e-sas-<hex> for SAS key auth —
// via azrelay.Provider in TestMain and tears them down via t.Cleanup.
// SAS keys are not configured by hand: TestMain acquires two namespace-
// scoped authorization rules once per `go test` invocation (Listen-only
// and Send-only, named e2e-listener and e2e-sender) via
// azrelay.AcquireRunRules, and Provider.Provision stamps the resulting
// keys onto every per-test SAS hyco. The rules are permanent fixtures
// of the namespace, not torn down on TestMain exit; the orphan janitor
// sweeps stale hycos only.
//
// Functional tests run against all available auth methods (entra, sas)
// unless E2E_AUTH=entra or E2E_AUTH=sas is set to pin one.
//
// Optional:
//
//   - E2E_AUTH: restrict to "entra" or "sas" (default: both)
//   - E2E_PROVISIONER_CONCURRENCY: cap on parallel hyco provisions (default 4)
//   - E2E_LARGE_TRANSFER=1: enable 100MB bulk transfer test
//   - E2E_LONG_LIVED=1: enable >2min keepalive test
//
// Run: go test -tags=e2e -timeout=10m ./e2e/...
package e2e

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestPortForwardBasic verifies a simple echo round-trip through port-forward mode.
func TestPortForwardBasic(t *testing.T) {
	t.Parallel()
	env := requireDedicatedHyco(t)

	for _, auth := range availableAuths(t, env) {
		t.Run(auth.name, func(t *testing.T) {
			echo := startEchoServer(t)
			listener := startListener(t, env, auth,
				"--allow", echo.Addr(),
				"--metrics-addr", ":0",
				"--log-level", "debug",
			)
			sender := startPortForwardSender(t, env, auth, echo.Addr(),
				"--log-level", "debug",
			)

			waitForLog(t, listener, "control channel connected", 30*time.Second)
			senderAddr := waitForLogAddr(t, sender, "port-forward listening", 15*time.Second)

			conn, err := net.DialTimeout("tcp", senderAddr, 10*time.Second)
			if err != nil {
				t.Fatalf("dial sender: %v", err)
			}
			defer conn.Close()

			payload := []byte("hello aztunnel\n")
			if _, err := conn.Write(payload); err != nil {
				t.Fatalf("write: %v", err)
			}

			buf := make([]byte, len(payload))
			if _, err := io.ReadFull(conn, buf); err != nil {
				t.Fatalf("read: %v", err)
			}
			if !bytes.Equal(payload, buf) {
				t.Fatalf("echo mismatch: got %q, want %q", buf, payload)
			}
		})
	}
}

// TestSOCKS5Basic verifies a SOCKS5 proxy echo round-trip.
func TestSOCKS5Basic(t *testing.T) {
	t.Parallel()
	env := requireDedicatedHyco(t)

	for _, auth := range availableAuths(t, env) {
		t.Run(auth.name, func(t *testing.T) {
			echo := startEchoServer(t)
			listener := startListener(t, env, auth,
				"--allow", echo.Addr(),
				"--log-level", "debug",
			)
			sender := startSOCKS5Sender(t, env, auth,
				"--log-level", "debug",
			)

			waitForLog(t, listener, "control channel connected", 30*time.Second)
			senderAddr := waitForLogAddr(t, sender, "socks5-proxy listening", 15*time.Second)

			conn := dialSOCKS5(t, senderAddr, echo.Addr())
			defer conn.Close()

			payload := []byte("socks5 test\n")
			if _, err := conn.Write(payload); err != nil {
				t.Fatalf("write: %v", err)
			}

			buf := make([]byte, len(payload))
			if _, err := io.ReadFull(conn, buf); err != nil {
				t.Fatalf("read: %v", err)
			}
			if !bytes.Equal(payload, buf) {
				t.Fatalf("echo mismatch: got %q, want %q", buf, payload)
			}
		})
	}
}

// TestConnectStdio verifies the connect (stdin/stdout) mode with raw TCP data.
func TestConnectStdio(t *testing.T) {
	t.Parallel()
	env := requireDedicatedHyco(t)

	for _, auth := range availableAuths(t, env) {
		t.Run(auth.name, func(t *testing.T) {
			echo := startEchoServer(t)
			listener := startListener(t, env, auth,
				"--allow", echo.Addr(),
				"--log-level", "debug",
			)
			waitForLog(t, listener, "control channel connected", 30*time.Second)

			// Run connect mode as a subprocess with piped stdin/stdout.
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			cmd := exec.CommandContext(ctx, aztunnelBinary(t),
				"relay-sender", "connect", echo.Addr(),
				"--relay", env.relayName,
				"--hyco", auth.hyco,
				"--log-level", "debug",
			)
			setAztunnelEnv(cmd, env, auth.senderSAS)
			stdin, err := cmd.StdinPipe()
			if err != nil {
				t.Fatalf("stdin pipe: %v", err)
			}
			stdout, err := cmd.StdoutPipe()
			if err != nil {
				t.Fatalf("stdout pipe: %v", err)
			}
			connectLogs := &logBuffer{}
			cmd.Stderr = connectLogs
			if err := cmd.Start(); err != nil {
				t.Fatalf("start connect: %v", err)
			}
			t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })

			// Wait for the connect command to establish the relay connection.
			if _, ok := connectLogs.waitFor("connected", 15*time.Second); !ok {
				t.Fatalf("timed out waiting for connect to establish relay connection")
			}

			payload := []byte("connect test\n")
			if _, err := stdin.Write(payload); err != nil {
				t.Fatalf("write stdin: %v", err)
			}

			buf := make([]byte, len(payload))
			if _, err := io.ReadFull(stdout, buf); err != nil {
				t.Fatalf("read stdout: %v", err)
			}
			if !bytes.Equal(payload, buf) {
				t.Fatalf("echo mismatch: got %q, want %q", buf, payload)
			}
		})
	}
}

// TestSSHProxyCommand runs a real SSH session through the tunnel.
func TestSSHProxyCommand(t *testing.T) {
	t.Parallel()
	env := requireDedicatedHyco(t)

	// Check that ssh client is available.
	if _, err := exec.LookPath("ssh"); err != nil {
		t.Skip("ssh not found in PATH")
	}

	for _, auth := range availableAuths(t, env) {
		t.Run(auth.name, func(t *testing.T) {
			sshd := startSSHServer(t)

			_ = startListener(t, env, auth,
				"--allow", sshd.Addr(),
				"--log-level", "debug",
			)

			// Build the ProxyCommand string.
			binary := aztunnelBinary(t)
			proxyCmd := fmt.Sprintf("%s relay-sender connect --relay %s --hyco %s %%h:%%p",
				binary, env.relayName, auth.hyco)

			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()

			host, port, _ := net.SplitHostPort(sshd.Addr())
			cmd := exec.CommandContext(ctx, "ssh",
				"-o", "StrictHostKeyChecking=no",
				"-o", "UserKnownHostsFile=/dev/null",
				"-o", fmt.Sprintf("ProxyCommand=%s", proxyCmd),
				"-o", "BatchMode=yes",
				"-p", port,
				"-i", sshd.HostKeyPath(),
				fmt.Sprintf("e2etest@%s", host),
				"echo", "tunnel-works",
			)
			setAztunnelEnv(cmd, env, auth.senderSAS)

			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("ssh failed: %v\noutput: %s", err, out)
			}
			if !strings.Contains(string(out), "tunnel-works") {
				t.Fatalf("unexpected ssh output: %s", out)
			}
		})
	}
}

// TestSASKeyAuth verifies port-forward works with separate listener/sender SAS keys.
func TestSASKeyAuth(t *testing.T) {
	t.Parallel()
	requireAuth(t, "sas")
	env := requireDedicatedHyco(t)
	// requireDedicatedHyco always returns a fully-populated SAS pair:
	// the per-test SAS hyco is created by Provider.Provision, and the
	// listener/sender keys are stamped from the permanent namespace
	// rules whose keys were read once by AcquireRunRules in TestMain.
	// The defensive check here flags a Provider contract regression
	// rather than a contributor mis-configuration.
	if env.sasHyco == "" || env.sasListenerKeyName == "" || env.sasListenerKey == "" || env.sasSenderKeyName == "" || env.sasSenderKey == "" {
		var missing []string
		if env.sasHyco == "" {
			missing = append(missing, "sasHyco")
		}
		if env.sasListenerKeyName == "" {
			missing = append(missing, "sasListenerKeyName")
		}
		if env.sasListenerKey == "" {
			missing = append(missing, "sasListenerKey")
		}
		if env.sasSenderKeyName == "" {
			missing = append(missing, "sasSenderKeyName")
		}
		if env.sasSenderKey == "" {
			missing = append(missing, "sasSenderKey")
		}
		// Report only the names of the empty fields, never the populated key
		// values themselves — this branch shouldn't trigger in CI, but %+v
		// of relayEnv would leak live SAS keys if it ever did.
		t.Fatalf("provisioner returned env with empty SAS fields: %v", missing)
	}

	echo := startEchoServer(t)

	// Use the SAS hyco for both sides.
	sasEnv := *env
	sasEnv.hyco = env.sasHyco

	// Start listener with Listen-only SAS key.
	listenerSAS := &sasCredentials{keyName: env.sasListenerKeyName, key: env.sasListenerKey}
	listener := startAztunnelWithSAS(t, &sasEnv, listenerSAS,
		"relay-listener",
		"--hyco", sasEnv.hyco,
		"--relay", sasEnv.relayName,
		"--allow", echo.Addr(),
		"--log-level", "debug",
	)

	// Start sender with Send-only SAS key.
	senderSAS := &sasCredentials{keyName: env.sasSenderKeyName, key: env.sasSenderKey}
	sender := startAztunnelWithSAS(t, &sasEnv, senderSAS,
		"relay-sender", "port-forward", echo.Addr(),
		"--relay", sasEnv.relayName,
		"--hyco", sasEnv.hyco,
		"--bind", "127.0.0.1:0",
		"--log-level", "debug",
	)

	waitForLog(t, listener, "control channel connected", 30*time.Second)
	senderAddr := waitForLogAddr(t, sender, "port-forward listening", 15*time.Second)

	conn, err := net.DialTimeout("tcp", senderAddr, 10*time.Second)
	if err != nil {
		t.Fatalf("dial sender: %v", err)
	}
	defer conn.Close()

	payload := []byte("sas-auth-test\n")
	if _, err := conn.Write(payload); err != nil {
		t.Fatalf("write: %v", err)
	}
	buf := make([]byte, len(payload))
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(payload, buf) {
		t.Fatalf("echo mismatch: got %q, want %q", buf, payload)
	}
}

// TestAllowlistAllow verifies that an allowed target connects successfully.
func TestAllowlistAllow(t *testing.T) {
	// This is covered by TestSOCKS5Basic (which uses --allow).
	// Explicit test with CIDR notation.
	t.Parallel()
	env := requireDedicatedHyco(t)

	for _, auth := range availableAuths(t, env) {
		t.Run(auth.name, func(t *testing.T) {
			echo := startEchoServer(t)
			// Use CIDR allowlist instead of exact host:port.
			listener := startListener(t, env, auth,
				"--allow", "127.0.0.0/8:*",
				"--log-level", "debug",
			)
			sender := startPortForwardSender(t, env, auth, echo.Addr(),
				"--log-level", "debug",
			)

			waitForLog(t, listener, "control channel connected", 30*time.Second)
			senderAddr := waitForLogAddr(t, sender, "port-forward listening", 15*time.Second)

			conn, err := net.DialTimeout("tcp", senderAddr, 10*time.Second)
			if err != nil {
				t.Fatalf("dial sender: %v", err)
			}
			defer conn.Close()

			payload := []byte("cidr-test\n")
			if _, err := conn.Write(payload); err != nil {
				t.Fatalf("write: %v", err)
			}
			buf := make([]byte, len(payload))
			if _, err := io.ReadFull(conn, buf); err != nil {
				t.Fatalf("read: %v", err)
			}
			if !bytes.Equal(payload, buf) {
				t.Fatalf("echo mismatch: got %q, want %q", buf, payload)
			}
		})
	}
}

// TestAllowlistDeny verifies that connections to non-allowed targets are rejected.
func TestAllowlistDeny(t *testing.T) {
	t.Parallel()
	env := requireDedicatedHyco(t)

	for _, auth := range availableAuths(t, env) {
		t.Run(auth.name, func(t *testing.T) {
			echo := startEchoServer(t)
			// Allowlist only lets through 192.0.2.0/24 (TEST-NET), not 127.0.0.1.
			listener := startListener(t, env, auth,
				"--allow", "192.0.2.0/24:*",
				"--log-level", "debug",
			)
			sender := startSOCKS5Sender(t, env, auth,
				"--log-level", "debug",
			)

			waitForLog(t, listener, "control channel connected", 30*time.Second)
			senderAddr := waitForLogAddr(t, sender, "socks5-proxy listening", 15*time.Second)

			// This should fail because the echo server is on 127.0.0.1, not 192.0.2.x.
			conn := dialSOCKS5(t, senderAddr, echo.Addr())
			defer conn.Close()

			// The connection should be rejected. Try to read — should get an error or EOF.
			buf := make([]byte, 64)
			conn.SetReadDeadline(time.Now().Add(10 * time.Second))
			_, err := conn.Read(buf)
			if err == nil {
				t.Fatal("expected connection to be rejected by allowlist, but read succeeded")
			}
		})
	}
}

// TestMaxConnections verifies the --max-connections limit.
func TestMaxConnections(t *testing.T) {
	t.Parallel()
	env := requireDedicatedHyco(t)

	for _, auth := range availableAuths(t, env) {
		t.Run(auth.name, func(t *testing.T) {
			echo := startEchoServer(t)
			listener := startListener(t, env, auth,
				"--allow", echo.Addr(),
				"--max-connections", "2",
				"--metrics-addr", "127.0.0.1:0",
				"--log-level", "debug",
			)
			sender := startPortForwardSender(t, env, auth, echo.Addr(),
				"--log-level", "debug",
			)

			waitForLog(t, listener, "control channel connected", 30*time.Second)
			listenerMetrics := listener.MetricsAddr(t, 15*time.Second)
			senderAddr := waitForLogAddr(t, sender, "port-forward listening", 15*time.Second)

			// Open 2 connections; both must succeed cleanly. The previous version
			// ignored Write/ReadFull errors here, which could mask broken setup.
			var conns []net.Conn
			for i := 0; i < 2; i++ {
				conn, err := net.DialTimeout("tcp", senderAddr, 10*time.Second)
				if err != nil {
					t.Fatalf("dial %d: %v", i, err)
				}
				conns = append(conns, conn)
				msg := fmt.Sprintf("conn%d\n", i)
				if _, err := conn.Write([]byte(msg)); err != nil {
					t.Fatalf("write conn %d: %v", i, err)
				}
				buf := make([]byte, len(msg))
				if _, err := io.ReadFull(conn, buf); err != nil {
					t.Fatalf("read conn %d: %v", i, err)
				}
				if string(buf) != msg {
					t.Fatalf("conn %d echo mismatch: got %q, want %q", i, buf, msg)
				}
			}
			t.Cleanup(func() {
				for _, c := range conns {
					c.Close()
				}
			})

			// Wait for listener-side active_connections to reach the limit (2),
			// rather than guessing with time.Sleep. This is deterministic on slow CI.
			waitForMetric(t, listenerMetrics, "aztunnel_active_connections",
				func(v float64) bool { return v >= 2 },
				15*time.Second,
			)

			// The 3rd connection should TCP-connect to the sender (sender accepts
			// locally before consulting the relay) but data must NOT round-trip,
			// because the listener-side semaphore is full.
			conn3, err := net.DialTimeout("tcp", senderAddr, 10*time.Second)
			if err != nil {
				t.Fatalf("dial 3rd: %v", err)
			}
			defer conn3.Close()

			// Three valid enforcement signals: (a) Write fails because the
			// sender already closed the local TCP socket in response to the
			// listener rejecting the rendezvous, (b) Read returns an error
			// (EOF/RST/timeout) with no data, or (c) Read times out. Only fail
			// if the overflow payload actually round-trips. Write success
			// alone is not enough — the bytes may sit in the local TCP send
			// buffer indefinitely, so the Read is what proves enforcement.
			if _, err := conn3.Write([]byte("overflow\n")); err != nil {
				t.Logf("3rd connection write failed (expected enforcement signal): %v", err)
			}
			conn3.SetReadDeadline(time.Now().Add(5 * time.Second))
			buf := make([]byte, 64)
			n, err := conn3.Read(buf)
			if n > 0 {
				t.Fatalf("3rd connection unexpectedly read %d bytes (%q) — max-connections limit not enforced (read err=%v)", n, buf[:n], err)
			}
		})
	}
}

// TestSmallPayload sends 1-byte writes.
func TestSmallPayload(t *testing.T) {
	t.Parallel()
	env := requireDedicatedHyco(t)

	for _, auth := range availableAuths(t, env) {
		t.Run(auth.name, func(t *testing.T) {
			echo := startEchoServer(t)
			listener := startListener(t, env, auth,
				"--allow", echo.Addr(),
			)
			sender := startPortForwardSender(t, env, auth, echo.Addr())

			waitForLog(t, listener, "control channel connected", 30*time.Second)
			senderAddr := waitForLogAddr(t, sender, "port-forward listening", 15*time.Second)

			conn, err := net.DialTimeout("tcp", senderAddr, 10*time.Second)
			if err != nil {
				t.Fatalf("dial: %v", err)
			}
			defer conn.Close()

			// Send 256 individual 1-byte writes.
			for i := 0; i < 256; i++ {
				b := byte(i)
				if _, err := conn.Write([]byte{b}); err != nil {
					t.Fatalf("write byte %d: %v", i, err)
				}
				got := make([]byte, 1)
				if _, err := io.ReadFull(conn, got); err != nil {
					t.Fatalf("read byte %d: %v", i, err)
				}
				if got[0] != b {
					t.Fatalf("byte %d: got %d, want %d", i, got[0], b)
				}
			}
		})
	}
}

// TestLargePayload sends a 1MB payload and verifies SHA256.
func TestLargePayload(t *testing.T) {
	t.Parallel()
	env := requireDedicatedHyco(t)

	for _, auth := range availableAuths(t, env) {
		t.Run(auth.name, func(t *testing.T) {
			echo := startEchoServer(t)
			listener := startListener(t, env, auth,
				"--allow", echo.Addr(),
			)
			sender := startPortForwardSender(t, env, auth, echo.Addr())

			waitForLog(t, listener, "control channel connected", 30*time.Second)
			senderAddr := waitForLogAddr(t, sender, "port-forward listening", 15*time.Second)

			conn, err := net.DialTimeout("tcp", senderAddr, 10*time.Second)
			if err != nil {
				t.Fatalf("dial: %v", err)
			}
			defer conn.Close()

			// 1MB random payload.
			payload := make([]byte, 1<<20)
			rand.Read(payload)
			wantHash := sha256.Sum256(payload)

			// Write in a goroutine since echo will be sending back simultaneously.
			go func() {
				conn.Write(payload)
				// Half-close write side via deadline trick — we can't half-close TCP
				// but we can stop writing.
			}()

			got := make([]byte, len(payload))
			if _, err := io.ReadFull(conn, got); err != nil {
				t.Fatalf("read: %v", err)
			}
			gotHash := sha256.Sum256(got)
			if wantHash != gotHash {
				t.Fatalf("SHA256 mismatch: payload corrupted through tunnel")
			}
		})
	}
}

// TestBulkTransfer streams 100MB through the tunnel. Opt-in via E2E_LARGE_TRANSFER=1.
func TestBulkTransfer(t *testing.T) {
	if os.Getenv("E2E_LARGE_TRANSFER") != "1" {
		t.Skip("set E2E_LARGE_TRANSFER=1 to enable (sends 100MB through relay)")
	}
	t.Parallel()
	env := requireDedicatedHyco(t)

	for _, auth := range availableAuths(t, env) {
		t.Run(auth.name, func(t *testing.T) {
			echo := startEchoServer(t)
			_ = startListener(t, env, auth,
				"--allow", echo.Addr(),
			)
			sender := startPortForwardSender(t, env, auth, echo.Addr())
			senderAddr := waitForLogAddr(t, sender, "port-forward listening", 30*time.Second)

			conn, err := net.DialTimeout("tcp", senderAddr, 10*time.Second)
			if err != nil {
				t.Fatalf("dial: %v", err)
			}
			defer conn.Close()

			const totalBytes = 100 << 20 // 100MB
			sentHash := make(chan [sha256.Size]byte, 1)

			// Write random data in chunks.
			go func() {
				h := sha256.New()
				chunk := make([]byte, 64*1024)
				remaining := totalBytes
				for remaining > 0 {
					n := len(chunk)
					if n > remaining {
						n = remaining
					}
					rand.Read(chunk[:n])
					h.Write(chunk[:n])
					conn.Write(chunk[:n])
					remaining -= n
				}
				sentHash <- [sha256.Size]byte(h.Sum(nil))
			}()

			// Read it all back.
			gotHash := sha256.New()
			n, err := io.CopyN(gotHash, conn, totalBytes)
			if err != nil {
				t.Fatalf("read after %d bytes: %v", n, err)
			}

			want := <-sentHash
			if !bytes.Equal(want[:], gotHash.Sum(nil)) {
				t.Fatal("SHA256 mismatch: bulk transfer data corrupted")
			}
			t.Logf("transferred %dMB successfully", totalBytes>>20)
		})
	}
}

// TestLongLivedConnection keeps a connection open past the relay idle timeout.
// Opt-in via E2E_LONG_LIVED=1.
func TestLongLivedConnection(t *testing.T) {
	if os.Getenv("E2E_LONG_LIVED") != "1" {
		t.Skip("set E2E_LONG_LIVED=1 to enable (runs for >2 minutes)")
	}
	t.Parallel()
	env := requireDedicatedHyco(t)

	for _, auth := range availableAuths(t, env) {
		t.Run(auth.name, func(t *testing.T) {
			echo := startEchoServer(t)
			_ = startListener(t, env, auth,
				"--allow", echo.Addr(),
			)
			sender := startPortForwardSender(t, env, auth, echo.Addr())
			senderAddr := waitForLogAddr(t, sender, "port-forward listening", 30*time.Second)

			conn, err := net.DialTimeout("tcp", senderAddr, 10*time.Second)
			if err != nil {
				t.Fatalf("dial: %v", err)
			}
			defer conn.Close()

			// Send data, wait >2 minutes (past Azure Relay ~120s idle timeout), then send again.
			payload := []byte("before-idle\n")
			conn.Write(payload)
			buf := make([]byte, len(payload))
			io.ReadFull(conn, buf)

			t.Log("waiting 130 seconds to test keepalive...")
			time.Sleep(130 * time.Second)

			payload2 := []byte("after-idle\n")
			if _, err := conn.Write(payload2); err != nil {
				t.Fatalf("write after idle: %v (keepalive pings may have failed)", err)
			}
			buf2 := make([]byte, len(payload2))
			if _, err := io.ReadFull(conn, buf2); err != nil {
				t.Fatalf("read after idle: %v", err)
			}
			if !bytes.Equal(payload2, buf2) {
				t.Fatalf("echo mismatch after idle: got %q, want %q", buf2, payload2)
			}
		})
	}
}

// TestConcurrentSameTarget opens 50 simultaneous connections through port-forward.
func TestConcurrentSameTarget(t *testing.T) {
	t.Parallel()
	env := requireDedicatedHyco(t)

	for _, auth := range availableAuths(t, env) {
		t.Run(auth.name, func(t *testing.T) {
			echo := startEchoServer(t)
			listener := startListener(t, env, auth,
				"--allow", echo.Addr(),
			)
			sender := startPortForwardSender(t, env, auth, echo.Addr())

			waitForLog(t, listener, "control channel connected", 30*time.Second)
			senderAddr := waitForLogAddr(t, sender, "port-forward listening", 15*time.Second)

			const numConns = 50
			var wg sync.WaitGroup
			errors := make(chan error, numConns)

			for i := 0; i < numConns; i++ {
				wg.Add(1)
				go func(id int) {
					defer wg.Done()
					conn, err := net.DialTimeout("tcp", senderAddr, 30*time.Second)
					if err != nil {
						errors <- fmt.Errorf("conn %d dial: %w", id, err)
						return
					}
					defer conn.Close()

					msg := fmt.Sprintf("hello from %d\n", id)
					if _, err := conn.Write([]byte(msg)); err != nil {
						errors <- fmt.Errorf("conn %d write: %w", id, err)
						return
					}
					buf := make([]byte, len(msg))
					if _, err := io.ReadFull(conn, buf); err != nil {
						errors <- fmt.Errorf("conn %d read: %w", id, err)
						return
					}
					if string(buf) != msg {
						errors <- fmt.Errorf("conn %d: got %q, want %q", id, buf, msg)
					}
				}(i)
			}

			wg.Wait()
			close(errors)
			for err := range errors {
				t.Error(err)
			}
		})
	}
}

// TestConcurrentDistinctTargets opens 50 SOCKS5 connections to 50 different echo servers.
func TestConcurrentDistinctTargets(t *testing.T) {
	t.Parallel()
	env := requireDedicatedHyco(t)

	for _, auth := range availableAuths(t, env) {
		t.Run(auth.name, func(t *testing.T) {
			const numTargets = 50
			echos := make([]*echoServer, numTargets)
			allowList := make([]string, numTargets)
			for i := 0; i < numTargets; i++ {
				echos[i] = startEchoServer(t)
				allowList[i] = echos[i].Addr()
			}

			listenerArgs := []string{
				"--log-level", "debug",
			}
			for _, a := range allowList {
				listenerArgs = append(listenerArgs, "--allow", a)
			}
			listener := startListener(t, env, auth, listenerArgs...)

			sender := startSOCKS5Sender(t, env, auth,
				"--log-level", "debug",
			)

			waitForLog(t, listener, "control channel connected", 30*time.Second)
			senderAddr := waitForLogAddr(t, sender, "socks5-proxy listening", 15*time.Second)

			var wg sync.WaitGroup
			errors := make(chan error, numTargets)

			for i := 0; i < numTargets; i++ {
				wg.Add(1)
				go func(id int) {
					defer wg.Done()
					conn, err := dialSOCKS5E(senderAddr, echos[id].Addr())
					if err != nil {
						errors <- fmt.Errorf("target %d socks5: %w", id, err)
						return
					}
					defer conn.Close()

					msg := fmt.Sprintf("target-%d\n", id)
					if _, err := conn.Write([]byte(msg)); err != nil {
						errors <- fmt.Errorf("target %d write: %w", id, err)
						return
					}
					buf := make([]byte, len(msg))
					if _, err := io.ReadFull(conn, buf); err != nil {
						errors <- fmt.Errorf("target %d read: %w", id, err)
						return
					}
					if string(buf) != msg {
						errors <- fmt.Errorf("target %d: got %q, want %q", id, buf, msg)
					}
				}(i)
			}

			wg.Wait()
			close(errors)
			for err := range errors {
				t.Error(err)
			}
		})
	}
}

// TestMetricsEndpoint verifies the metrics HTTP endpoint returns valid Prometheus data.
func TestMetricsEndpoint(t *testing.T) {
	t.Parallel()
	env := requireDedicatedHyco(t)

	for _, auth := range availableAuths(t, env) {
		t.Run(auth.name, func(t *testing.T) {
			echo := startEchoServer(t)
			listener := startListener(t, env, auth,
				"--allow", echo.Addr(),
				"--metrics-addr", "127.0.0.1:0",
				"--log-level", "debug",
			)

			metricsAddr := waitForLogAddr(t, listener, "metrics server listening", 15*time.Second)
			metricsURL := "http://" + metricsAddr + "/metrics"

			resp, err := http.Get(metricsURL)
			if err != nil {
				t.Fatalf("GET /metrics: %v", err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != 200 {
				t.Fatalf("GET /metrics: status %d", resp.StatusCode)
			}

			body, _ := io.ReadAll(resp.Body)
			text := string(body)
			if !strings.Contains(text, "aztunnel_control_channel_connected") {
				t.Fatal("metrics output missing aztunnel_control_channel_connected")
			}
			if !strings.Contains(text, "go_goroutines") {
				t.Fatal("metrics output missing go_goroutines")
			}
		})
	}
}

// TestMetricsConnectionCount verifies connection counters after traffic.
func TestMetricsConnectionCount(t *testing.T) {
	t.Parallel()
	env := requireDedicatedHyco(t)

	for _, auth := range availableAuths(t, env) {
		t.Run(auth.name, func(t *testing.T) {
			echo := startEchoServer(t)
			listener := startListener(t, env, auth,
				"--allow", echo.Addr(),
				"--metrics-addr", "127.0.0.1:0",
				"--log-level", "debug",
			)
			sender := startPortForwardSender(t, env, auth, echo.Addr(),
				"--metrics-addr", "127.0.0.1:0",
				"--log-level", "debug",
			)

			waitForLog(t, listener, "control channel connected", 30*time.Second)
			listenerMetrics := listener.MetricsAddr(t, 15*time.Second)
			senderAddr := waitForLogAddr(t, sender, "port-forward listening", 15*time.Second)
			senderMetrics := sender.MetricsAddr(t, 15*time.Second)

			// Run 3 connections back-to-back. No per-connection time.Sleep —
			// we wait on metrics deterministically below.
			for i := 0; i < 3; i++ {
				conn, err := net.DialTimeout("tcp", senderAddr, 10*time.Second)
				if err != nil {
					t.Fatalf("dial %d: %v", i, err)
				}
				if _, err := conn.Write([]byte("x")); err != nil {
					t.Fatalf("write %d: %v", i, err)
				}
				buf := make([]byte, 1)
				if _, err := io.ReadFull(conn, buf); err != nil {
					t.Fatalf("read %d: %v", i, err)
				}
				conn.Close()
			}

			// Poll until both sides report >=3 total connections.
			waitForMetric(t, listenerMetrics, "aztunnel_connections_total",
				func(v float64) bool { return v >= 3 }, 15*time.Second)
			waitForMetric(t, senderMetrics, "aztunnel_connections_total",
				func(v float64) bool { return v >= 3 }, 15*time.Second)

			// Active connections should be 0 once all closes have propagated.
			waitForMetric(t, listenerMetrics, "aztunnel_active_connections",
				func(v float64) bool { return v == 0 }, 15*time.Second)
			waitForMetric(t, senderMetrics, "aztunnel_active_connections",
				func(v float64) bool { return v == 0 }, 15*time.Second)
		})
	}
}

// TestMetricsErrorReason verifies error reason labels from allowlist rejection.
func TestMetricsErrorReason(t *testing.T) {
	t.Parallel()
	env := requireDedicatedHyco(t)

	for _, auth := range availableAuths(t, env) {
		t.Run(auth.name, func(t *testing.T) {
			echo := startEchoServer(t)
			listener := startListener(t, env, auth,
				"--allow", "192.0.2.0/24:*", // deny the echo server
				"--metrics-addr", "127.0.0.1:0",
				"--log-level", "debug",
			)
			sender := startSOCKS5Sender(t, env, auth,
				"--log-level", "debug",
			)

			waitForLog(t, listener, "control channel connected", 30*time.Second)
			listenerMetrics := listener.MetricsAddr(t, 15*time.Second)
			senderAddr := waitForLogAddr(t, sender, "socks5-proxy listening", 15*time.Second)

			// Attempt connection (will be rejected by allowlist).
			conn := dialSOCKS5(t, senderAddr, echo.Addr())
			conn.SetReadDeadline(time.Now().Add(10 * time.Second))
			io.ReadAll(conn)
			conn.Close()

			// Wait until the allowlist_rejected reason label appears in metrics.
			lm := waitForMetricsContains(t, listenerMetrics,
				`reason="allowlist_rejected"`, 15*time.Second)
			_ = lm // available for diagnostics if a later assertion fails
		})
	}
}

// TestMetricsDialDuration verifies the dial duration histogram has observations.
func TestMetricsDialDuration(t *testing.T) {
	t.Parallel()
	env := requireDedicatedHyco(t)

	for _, auth := range availableAuths(t, env) {
		t.Run(auth.name, func(t *testing.T) {
			echo := startEchoServer(t)
			listener := startListener(t, env, auth,
				"--allow", echo.Addr(),
				"--metrics-addr", "127.0.0.1:0",
				"--log-level", "debug",
			)
			sender := startPortForwardSender(t, env, auth, echo.Addr(),
				"--metrics-addr", "127.0.0.1:0",
				"--log-level", "debug",
			)

			waitForLog(t, listener, "control channel connected", 30*time.Second)
			senderAddr := waitForLogAddr(t, sender, "port-forward listening", 15*time.Second)
			senderMetrics := sender.MetricsAddr(t, 15*time.Second)

			conn, err := net.DialTimeout("tcp", senderAddr, 10*time.Second)
			if err != nil {
				t.Fatalf("dial: %v", err)
			}
			if _, err := conn.Write([]byte("x")); err != nil {
				t.Fatalf("write: %v", err)
			}
			buf := make([]byte, 1)
			if _, err := io.ReadFull(conn, buf); err != nil {
				t.Fatalf("read: %v", err)
			}
			conn.Close()

			waitForMetricsContains(t, senderMetrics,
				"aztunnel_dial_duration_seconds", 15*time.Second)
		})
	}
}

// --- retry / resilience tests ---

// TestSenderRetriesUntilListenerReady verifies that the sender connect mode
// retries on 404 and succeeds once the listener becomes available.
func TestSenderRetriesUntilListenerReady(t *testing.T) {
	t.Parallel()
	env := requireDedicatedHyco(t)

	for _, auth := range availableAuths(t, env) {
		t.Run(auth.name, func(t *testing.T) {
			echo := startEchoServer(t)

			// Start the sender FIRST — no listener is running yet.
			binary := aztunnelBinary(t)
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()

			cmd := exec.CommandContext(ctx, binary,
				"relay-sender", "connect", echo.Addr(),
				"--relay", env.relayName,
				"--hyco", auth.hyco,
				"--log-level", "debug",
			)
			setAztunnelEnv(cmd, env, auth.senderSAS)
			stdin, err := cmd.StdinPipe()
			if err != nil {
				t.Fatalf("stdin pipe: %v", err)
			}
			stdout, err := cmd.StdoutPipe()
			if err != nil {
				t.Fatalf("stdout pipe: %v", err)
			}
			connectLogs := &logBuffer{}
			cmd.Stderr = connectLogs
			if err := cmd.Start(); err != nil {
				t.Fatalf("start connect: %v", err)
			}
			t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })

			// Wait for at least one retry (404).
			if _, ok := connectLogs.waitFor("retrying", 15*time.Second); !ok {
				t.Fatal("timed out waiting for sender to retry on 404")
			}

			// NOW start the listener.
			listener := startListener(t, env, auth,
				"--allow", echo.Addr(),
				"--log-level", "debug",
			)
			waitForLog(t, listener, "control channel connected", 30*time.Second)

			// The sender should eventually connect.
			if _, ok := connectLogs.waitFor("connected", 15*time.Second); !ok {
				t.Fatal("timed out waiting for sender to connect after listener started")
			}

			// Verify data flows through.
			payload := []byte("retry test\n")
			if _, err := stdin.Write(payload); err != nil {
				t.Fatalf("write stdin: %v", err)
			}
			buf := make([]byte, len(payload))
			if _, err := io.ReadFull(stdout, buf); err != nil {
				t.Fatalf("read stdout: %v", err)
			}
			if !bytes.Equal(payload, buf) {
				t.Fatalf("echo mismatch: got %q, want %q", buf, payload)
			}
		})
	}
}

// TestPortForwardRecoveryAfterListenerRestart verifies that port-forward
// mode recovers after the listener restarts.
func TestPortForwardRecoveryAfterListenerRestart(t *testing.T) {
	t.Parallel()
	env := requireDedicatedHyco(t)

	for _, auth := range availableAuths(t, env) {
		t.Run(auth.name, func(t *testing.T) {
			echo := startEchoServer(t)

			// Start listener and sender.
			listener := startListener(t, env, auth,
				"--allow", echo.Addr(),
				"--log-level", "debug",
			)
			sender := startPortForwardSender(t, env, auth, echo.Addr(),
				"--log-level", "debug",
			)

			waitForLog(t, listener, "control channel connected", 30*time.Second)
			senderAddr := waitForLogAddr(t, sender, "port-forward listening", 15*time.Second)

			// Verify traffic works.
			conn, err := net.DialTimeout("tcp", senderAddr, 10*time.Second)
			if err != nil {
				t.Fatalf("dial sender: %v", err)
			}
			payload := []byte("before restart\n")
			if _, err := conn.Write(payload); err != nil {
				t.Fatalf("write: %v", err)
			}
			buf := make([]byte, len(payload))
			if _, err := io.ReadFull(conn, buf); err != nil {
				t.Fatalf("read: %v", err)
			}
			if !bytes.Equal(payload, buf) {
				t.Fatalf("pre-restart echo mismatch: got %q, want %q", buf, payload)
			}
			conn.Close()

			// Stop the listener cleanly via the idempotent Stop helper. The
			// previous implementation called Process.Kill() + Wait() directly,
			// racing with the t.Cleanup hook that also calls Wait — double-Wait
			// returns an error and made cleanup ordering fragile.
			listener.Stop(t)

			// Restart the listener.
			listener2 := startListener(t, env, auth,
				"--allow", echo.Addr(),
				"--log-level", "debug",
			)
			waitForLog(t, listener2, "control channel connected", 30*time.Second)

			// Traffic should work again.
			conn2, err := net.DialTimeout("tcp", senderAddr, 10*time.Second)
			if err != nil {
				t.Fatalf("dial sender after restart: %v", err)
			}
			defer conn2.Close()

			payload2 := []byte("after restart\n")
			if _, err := conn2.Write(payload2); err != nil {
				t.Fatalf("write after restart: %v", err)
			}
			buf2 := make([]byte, len(payload2))
			if _, err := io.ReadFull(conn2, buf2); err != nil {
				t.Fatalf("read after restart: %v", err)
			}
			if !bytes.Equal(payload2, buf2) {
				t.Fatalf("post-restart echo mismatch: got %q, want %q", buf2, payload2)
			}
		})
	}
}

// --- negative tests ---

// runExpectFail runs an aztunnel command expecting non-zero exit. Returns stderr output.
func runExpectFail(t *testing.T, timeout time.Duration, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, aztunnelBinary(t), args...)
	cmd.Env = os.Environ()
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	cmd.Stdout = io.Discard

	err := cmd.Run()
	if err == nil {
		t.Fatal("expected non-zero exit, but command succeeded")
	}
	return stderr.String()
}

// assertNoTokenLeak checks that output doesn't contain raw relay tokens.
func assertNoTokenLeak(t *testing.T, output string) {
	t.Helper()
	// Remove legitimate redacted tokens, then check for any remaining raw ones.
	stripped := strings.ReplaceAll(output, "sb-hc-token=REDACTED", "")
	if strings.Contains(stripped, "sb-hc-token=") {
		t.Error("output contains raw sb-hc-token (not redacted)")
	}
}

// assertNoUsageDump checks that stderr doesn't contain CLI usage output.
func assertNoUsageDump(t *testing.T, output string) {
	t.Helper()
	if strings.Contains(output, "Usage:") && strings.Contains(output, "Flags:") {
		t.Error("stderr contains usage dump; expected clean error only")
	}
}

// TestConnectNoListener verifies that a sender retries when no listener is
// registered for the hyco and that the error/retry behavior is logged. The
// real Azure Relay returns HTTP 401 (Unauthorized) — NOT 404 — when no
// listener is registered for the hyco at handshake time, which the relay
// client surfaces as a non-retryable failure. The test name is preserved for
// continuity with the original behavior the suite was designed around.
func TestConnectNoListener(t *testing.T) {
	t.Parallel()
	env := requireDedicatedHyco(t)

	for _, auth := range availableAuths(t, env) {
		t.Run(auth.name, func(t *testing.T) {
			output := runExpectFail(t, 15*time.Second,
				"relay-sender", "connect", "127.0.0.1:9999",
				"--relay", env.relayName,
				"--hyco", auth.hyco,
				"--log-level", "warn",
			)

			assertNoUsageDump(t, output)
			assertNoTokenLeak(t, output)
			// Match case-insensitively against the slog "error:" line OR the
			// "retrying" log used when the relay returns 404/503. The original
			// matcher looked for "Error:" (capital) which never matched the
			// actual lowercase slog output.
			lower := strings.ToLower(output)
			if !strings.Contains(lower, "error:") && !strings.Contains(lower, "retrying") && !strings.Contains(lower, "relay dial failed") {
				t.Errorf("expected error or retry log in output, got: %s", output)
			}
		})
	}
}

// TestBadRelayName verifies a clean error for a non-existent relay namespace.
func TestBadRelayName(t *testing.T) {
	t.Parallel()
	env := requireDedicatedHyco(t)

	for _, auth := range availableAuths(t, env) {
		t.Run(auth.name, func(t *testing.T) {
			badEnv := *env
			badEnv.relayName = "nonexistent-relay-xxxxx"
			proc := startListener(t, &badEnv, auth)

			// Listener will fail on dial and log the error before retrying.
			waitForLog(t, proc, "control channel disconnected", 30*time.Second)

			logs := proc.logs.String()
			assertNoTokenLeak(t, logs)
			assertNoUsageDump(t, logs)
		})
	}
}

// TestBadHycoName verifies a clean error for a non-existent hybrid connection.
func TestBadHycoName(t *testing.T) {
	t.Parallel()
	env := requireDedicatedHyco(t)

	for _, auth := range availableAuths(t, env) {
		t.Run(auth.name, func(t *testing.T) {
			badAuth := auth
			badAuth.hyco = "nonexistent-hyco-xxxxx"
			proc := startListener(t, env, badAuth)

			waitForLog(t, proc, "control channel disconnected", 30*time.Second)

			logs := proc.logs.String()
			assertNoTokenLeak(t, logs)
			assertNoUsageDump(t, logs)
		})
	}
}

// TestBadSASKey verifies a clean error and no key leakage for an invalid SAS key.
func TestBadSASKey(t *testing.T) {
	t.Parallel()
	requireAuth(t, "sas")
	env := requireDedicatedHyco(t)
	if env.sasHyco == "" || env.sasListenerKeyName == "" {
		t.Skip("SAS hyco / listener key name not configured")
	}

	badKey := "dGhpcyBpcyBhIGJhZCBrZXk=" // base64("this is a bad key")
	sasEnv := *env
	sasEnv.hyco = env.sasHyco

	proc := startAztunnelWithSAS(t, &sasEnv,
		&sasCredentials{keyName: env.sasListenerKeyName, key: badKey},
		"relay-listener",
		"--hyco", env.sasHyco,
		"--relay", env.relayName,
	)

	waitForLog(t, proc, "control channel disconnected", 30*time.Second)

	logs := proc.logs.String()
	assertNoTokenLeak(t, logs)
	assertNoUsageDump(t, logs)
	if strings.Contains(logs, badKey) {
		t.Error("logs contain the bad SAS key value")
	}
}

// TestMissingRequiredArgs verifies helpful errors for missing CLI arguments.
func TestMissingRequiredArgs(t *testing.T) {
	// Strip AZTUNNEL_* from env so resolveAuth doesn't pick up values.
	var cleanEnv []string
	for _, e := range os.Environ() {
		if !strings.HasPrefix(e, "AZTUNNEL_") {
			cleanEnv = append(cleanEnv, e)
		}
	}

	runClean := func(t *testing.T, args ...string) string {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, aztunnelBinary(t), args...)
		cmd.Env = cleanEnv
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		cmd.Stdout = io.Discard
		err := cmd.Run()
		if err == nil {
			t.Fatal("expected non-zero exit")
		}
		return stderr.String()
	}

	t.Run("listener_no_relay", func(t *testing.T) {
		output := runClean(t, "relay-listener", "--hyco", "some-hyco")
		if !strings.Contains(output, "relay") {
			t.Errorf("expected error mentioning 'relay', got: %s", output)
		}
	})

	t.Run("sender_no_relay", func(t *testing.T) {
		output := runClean(t, "relay-sender", "port-forward", "127.0.0.1:22")
		// Command may check hyco before relay; either error is acceptable.
		if !strings.Contains(output, "relay") && !strings.Contains(output, "hyco") {
			t.Errorf("expected error mentioning 'relay' or 'hyco', got: %s", output)
		}
	})

	t.Run("sender_no_target", func(t *testing.T) {
		output := runClean(t, "relay-sender", "connect")
		wantAny := strings.Contains(output, "arg") || strings.Contains(output, "expected")
		if !wantAny {
			t.Errorf("expected error about missing argument, got: %s", output)
		}
	})
}

// TestBadSASKeySender verifies a clean error when the sender has an invalid SAS key.
// The sender uses connect mode, which dials the relay at startup; a bad SAS
// produces an HTTP 401 from the relay, which the client treats as
// non-retryable. We assert a positive failure signal (the "relay dial failed"
// log line WITHOUT the "(retrying)" suffix) rather than just sleeping and
// checking for the absence of a leak.
func TestBadSASKeySender(t *testing.T) {
	t.Parallel()
	requireAuth(t, "sas")
	env := requireDedicatedHyco(t)
	if env.sasHyco == "" || env.sasSenderKeyName == "" {
		t.Skip("SAS hyco / sender key name not configured")
	}

	badKey := "dGhpcyBpcyBhIGJhZCBrZXk=" // base64("this is a bad key")
	sasEnv := *env
	sasEnv.hyco = env.sasHyco

	proc := startAztunnelWithSAS(t, &sasEnv,
		&sasCredentials{keyName: env.sasSenderKeyName, key: badKey},
		"relay-sender", "connect", "127.0.0.1:9999",
		"--relay", env.relayName,
		"--hyco", env.sasHyco,
		"--log-level", "debug",
	)

	// Positive assertion: the relay must log a dial failure. We do not assert
	// process exit because that depends on how the runner reaps subprocesses;
	// the log line is the contract.
	waitForLog(t, proc, "relay dial failed", 15*time.Second)

	logs := proc.logs.String()
	assertNoTokenLeak(t, logs)
	if strings.Contains(logs, badKey) {
		t.Error("logs contain the bad SAS key value")
	}
}

// TestWrongSASClaim verifies that using a listener key for sending (and vice versa) fails.
func TestWrongSASClaim(t *testing.T) {
	t.Parallel()
	requireAuth(t, "sas")
	env := requireDedicatedHyco(t)
	if env.sasHyco == "" ||
		env.sasListenerKeyName == "" || env.sasListenerKey == "" ||
		env.sasSenderKeyName == "" || env.sasSenderKey == "" {
		t.Skip("SAS credentials not fully configured")
	}

	sasEnv := *env
	sasEnv.hyco = env.sasHyco

	t.Run("listener_key_as_sender", func(t *testing.T) {
		// port-forward sender lazily dials the relay on first connection, so
		// running it with no client traffic never exercises auth. We start it,
		// then dial in once to force a relay dial attempt and observe failure.
		proc := startAztunnelWithSAS(t, &sasEnv,
			&sasCredentials{keyName: env.sasListenerKeyName, key: env.sasListenerKey},
			"relay-sender", "port-forward", "127.0.0.1:9999",
			"--relay", env.relayName,
			"--hyco", env.sasHyco,
			"--bind", "127.0.0.1:0",
			"--log-level", "debug",
		)

		senderAddr := waitForLogAddr(t, proc, "port-forward listening", 15*time.Second)

		// Force a relay rendezvous attempt by connecting to the sender's
		// local TCP bind. The wrong-claim dial will fail and the forward
		// goroutine will close the connection.
		conn, err := net.DialTimeout("tcp", senderAddr, 10*time.Second)
		if err != nil {
			t.Fatalf("dial sender: %v", err)
		}
		defer conn.Close()
		_, _ = conn.Write([]byte("trigger\n"))
		conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		buf := make([]byte, 64)
		if _, rerr := conn.Read(buf); rerr == nil {
			t.Fatal("expected sender->relay dial to fail with wrong claim, but read succeeded")
		}

		// Positive assertion: the sender logged a relay dial failure.
		waitForLog(t, proc, "relay dial failed", 15*time.Second)

		logs := proc.logs.String()
		assertNoTokenLeak(t, logs)
	})

	t.Run("sender_key_as_listener", func(t *testing.T) {
		// Use sender (Send-only) key for listening — should fail.
		proc := startAztunnelWithSAS(t, &sasEnv,
			&sasCredentials{keyName: env.sasSenderKeyName, key: env.sasSenderKey},
			"relay-listener",
			"--hyco", env.sasHyco,
			"--relay", env.relayName,
			"--log-level", "debug",
		)

		waitForLog(t, proc, "control channel disconnected", 30*time.Second)

		logs := proc.logs.String()
		assertNoTokenLeak(t, logs)
		assertNoUsageDump(t, logs)
	})
}

// TestPortForwardClosedPort verifies clean behavior when the listener dials a closed port.
func TestPortForwardClosedPort(t *testing.T) {
	t.Parallel()
	env := requireDedicatedHyco(t)

	for _, auth := range availableAuths(t, env) {
		t.Run(auth.name, func(t *testing.T) {
			// Pick an ephemeral port that nobody is listening on.
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			closedAddr := ln.Addr().String()
			ln.Close() // now it's closed

			listener := startListener(t, env, auth,
				"--allow", closedAddr,
				"--metrics-addr", "127.0.0.1:0",
				"--log-level", "debug",
			)
			sender := startPortForwardSender(t, env, auth, closedAddr,
				"--log-level", "debug",
			)

			waitForLog(t, listener, "control channel connected", 30*time.Second)
			listenerMetrics := listener.MetricsAddr(t, 15*time.Second)
			senderAddr := waitForLogAddr(t, sender, "port-forward listening", 15*time.Second)

			// Connect through the tunnel — listener will fail to dial the closed port.
			conn, err := net.DialTimeout("tcp", senderAddr, 10*time.Second)
			if err != nil {
				t.Fatalf("dial sender: %v", err)
			}
			defer conn.Close()

			// The connection should fail cleanly: read should return error or EOF.
			conn.Write([]byte("hello\n"))
			conn.SetReadDeadline(time.Now().Add(10 * time.Second))
			buf := make([]byte, 64)
			_, err = conn.Read(buf)
			if err == nil {
				t.Fatal("expected read to fail (target port is closed), but it succeeded")
			}

			waitForMetricsContains(t, listenerMetrics, `reason="dial_failed"`, 15*time.Second)
		})
	}
}

// TestSOCKS5ClosedPort verifies clean behavior when SOCKS5 targets a closed port.
func TestSOCKS5ClosedPort(t *testing.T) {
	t.Parallel()
	env := requireDedicatedHyco(t)

	for _, auth := range availableAuths(t, env) {
		t.Run(auth.name, func(t *testing.T) {
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			closedAddr := ln.Addr().String()
			ln.Close()

			listener := startListener(t, env, auth,
				"--allow", closedAddr,
				"--log-level", "debug",
			)
			sender := startSOCKS5Sender(t, env, auth,
				"--log-level", "debug",
			)

			waitForLog(t, listener, "control channel connected", 30*time.Second)
			senderAddr := waitForLogAddr(t, sender, "socks5-proxy listening", 15*time.Second)

			// SOCKS5 handshake to the closed port.
			conn := dialSOCKS5(t, senderAddr, closedAddr)
			defer conn.Close()

			conn.SetReadDeadline(time.Now().Add(10 * time.Second))
			buf := make([]byte, 64)
			_, err = conn.Read(buf)
			if err == nil {
				t.Fatal("expected read to fail (target port is closed), but it succeeded")
			}
		})
	}
}

// TestPortForwardUnreachable verifies timeout behavior when the target is unreachable.
func TestPortForwardUnreachable(t *testing.T) {
	t.Parallel()
	env := requireDedicatedHyco(t)

	for _, auth := range availableAuths(t, env) {
		t.Run(auth.name, func(t *testing.T) {
			// 192.0.2.1 is TEST-NET-1 (RFC 5737) — guaranteed non-routable.
			unreachable := "192.0.2.1:9999"

			listener := startListener(t, env, auth,
				"--allow", "192.0.2.0/24:*",
				"--connect-timeout", "3s",
				"--metrics-addr", "127.0.0.1:0",
				"--log-level", "debug",
			)
			sender := startPortForwardSender(t, env, auth, unreachable,
				"--log-level", "debug",
			)

			waitForLog(t, listener, "control channel connected", 30*time.Second)
			listenerMetrics := listener.MetricsAddr(t, 15*time.Second)
			senderAddr := waitForLogAddr(t, sender, "port-forward listening", 15*time.Second)

			conn, err := net.DialTimeout("tcp", senderAddr, 10*time.Second)
			if err != nil {
				t.Fatalf("dial sender: %v", err)
			}
			defer conn.Close()

			conn.Write([]byte("hello\n"))
			conn.SetReadDeadline(time.Now().Add(15 * time.Second))
			buf := make([]byte, 64)
			_, err = conn.Read(buf)
			if err == nil {
				t.Fatal("expected read to fail (target is unreachable), but it succeeded")
			}

			// Either dial_timeout (preferred) or dial_failed is acceptable —
			// some platforms surface unreachable as a fast failure rather than timeout.
			deadline := time.Now().Add(15 * time.Second)
			var lm string
			for time.Now().Before(deadline) {
				lm = scrapeMetricsBest(listenerMetrics)
				if strings.Contains(lm, `reason="dial_timeout"`) || strings.Contains(lm, `reason="dial_failed"`) {
					return
				}
				time.Sleep(100 * time.Millisecond)
			}
			t.Logf("metrics output:\n%s", lm)
			t.Fatal("expected dial_timeout or dial_failed error reason in metrics within 15s")
		})
	}
}

// scrapeMetrics fetches the Prometheus metrics text from the given address.
func scrapeMetrics(t *testing.T, addr string) string {
	t.Helper()
	resp, err := http.Get("http://" + addr + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics from %s: %v", addr, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return string(body)
}

// assertMetricGE checks that the sum of all samples for a metric name is >= want.
func assertMetricGE(t *testing.T, metricsText, metricName string, want float64) {
	t.Helper()
	total := sumMetric(metricsText, metricName)
	if total < want {
		t.Errorf("%s = %v, want >= %v", metricName, total, want)
	}
}

// assertMetricEQ checks that the sum of all samples for a metric name equals want.
func assertMetricEQ(t *testing.T, metricsText, metricName string, want float64) {
	t.Helper()
	total := sumMetric(metricsText, metricName)
	if total != want {
		t.Errorf("%s = %v, want %v", metricName, total, want)
	}
}

// sumMetric sums all sample values for lines matching the metric name (not comments/histograms).
func sumMetric(text, name string) float64 {
	var total float64
	scanner := bufio.NewScanner(strings.NewReader(text))
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "#") {
			continue
		}
		// Match lines like: metric_name{labels} value or metric_name value
		if strings.HasPrefix(line, name+"{") || strings.HasPrefix(line, name+" ") {
			// Skip histogram bucket/sum/count lines for non-histogram queries.
			if strings.HasPrefix(line, name+"_bucket") ||
				strings.HasPrefix(line, name+"_sum") ||
				strings.HasPrefix(line, name+"_count") {
				continue
			}
			parts := strings.Fields(line)
			if len(parts) >= 2 {
				var v float64
				fmt.Sscanf(parts[len(parts)-1], "%f", &v)
				total += v
			}
		}
	}
	return total
}

// sumMetricByLabel sums sample values for metric `name` whose labels
// include the requested labelName=labelValue pair. Label order in the
// scraped /metrics output is not stable across Prometheus versions, so
// the match is a substring check against the well-formed `name="value"`
// form rather than a position-sensitive parse.
func sumMetricByLabel(text, name, labelName, labelValue string) float64 {
	var total float64
	needle := labelName + `="` + labelValue + `"`
	scanner := bufio.NewScanner(strings.NewReader(text))
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "#") {
			continue
		}
		if !strings.HasPrefix(line, name+"{") {
			continue
		}
		if !strings.Contains(line, needle) {
			continue
		}
		parts := strings.Fields(line)
		if len(parts) >= 2 {
			var v float64
			fmt.Sscanf(parts[len(parts)-1], "%f", &v)
			total += v
		}
	}
	return total
}
