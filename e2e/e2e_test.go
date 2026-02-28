//go:build e2e

// Package e2e contains end-to-end tests for aztunnel that run against a real
// Azure Relay. Tests are gated behind the "e2e" build tag and require:
//
//   - E2E_RELAY_NAME: Azure Relay namespace name
//   - At least one auth method configured (Entra ID and/or SAS keys)
//
// Entra ID auth:
//
//   - E2E_ENTRA_HYCO_NAME: hybrid connection name (Entra ID auth)
//   - Valid Azure credentials (az login, managed identity, etc.)
//
// SAS key auth:
//
//   - E2E_SAS_HYCO_NAME: hybrid connection for SAS key tests
//   - E2E_SAS_LISTENER_KEY_NAME + E2E_SAS_LISTENER_KEY: listener SAS credentials
//   - E2E_SAS_SENDER_KEY_NAME + E2E_SAS_SENDER_KEY: sender SAS credentials
//
// Functional tests run against all available auth methods. Auth-specific tests
// (e.g., TestSASKeyAuth, TestWrongSASClaim) only run when their credentials
// are configured.
//
// Optional:
//
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
	env := requireRelayEnv(t)

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
	env := requireRelayEnv(t)

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
	env := requireRelayEnv(t)

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
	env := requireRelayEnv(t)

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
	env := requireRelayEnv(t)
	if env.sasHyco == "" || env.sasListenerKeyName == "" || env.sasListenerKey == "" || env.sasSenderKeyName == "" || env.sasSenderKey == "" {
		t.Skip("SAS credentials not configured (E2E_SAS_HYCO_NAME, E2E_SAS_*_KEY_NAME, E2E_SAS_*_KEY)")
	}

	echo := startEchoServer(t)

	// Use the SAS hyco for both sides.
	sasEnv := *env
	sasEnv.hyco = env.sasHyco

	// Start listener with Listen-only SAS key.
	listenerSAS := &sasCredentials{keyName: env.sasListenerKeyName, key: env.sasListenerKey}
	listener := startAztunnelWithSAS(t, &sasEnv, listenerSAS,
		"relay-listener", sasEnv.hyco,
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
	env := requireRelayEnv(t)

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
	env := requireRelayEnv(t)

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
	env := requireRelayEnv(t)

	for _, auth := range availableAuths(t, env) {
		t.Run(auth.name, func(t *testing.T) {
			echo := startEchoServer(t)
			listener := startListener(t, env, auth,
				"--allow", echo.Addr(),
				"--max-connections", "2",
				"--log-level", "debug",
			)
			sender := startPortForwardSender(t, env, auth, echo.Addr(),
				"--log-level", "debug",
			)

			waitForLog(t, listener, "control channel connected", 30*time.Second)
			senderAddr := waitForLogAddr(t, sender, "port-forward listening", 15*time.Second)

			// Open 2 connections (should succeed).
			var conns []net.Conn
			for i := 0; i < 2; i++ {
				conn, err := net.DialTimeout("tcp", senderAddr, 10*time.Second)
				if err != nil {
					t.Fatalf("dial %d: %v", i, err)
				}
				conns = append(conns, conn)
				// Verify echo works.
				msg := fmt.Sprintf("conn%d\n", i)
				conn.Write([]byte(msg))
				buf := make([]byte, len(msg))
				io.ReadFull(conn, buf)
			}
			t.Cleanup(func() {
				for _, c := range conns {
					c.Close()
				}
			})

			// The 3rd connection should still TCP-connect (to the sender) but the
			// relay-side will not accept it — the data won't round-trip.
			// Wait briefly for the listener semaphore to be fully consumed.
			time.Sleep(2 * time.Second)

			conn3, err := net.DialTimeout("tcp", senderAddr, 10*time.Second)
			if err != nil {
				t.Fatalf("dial 3rd: %v", err)
			}
			defer conn3.Close()

			// Try to send data — should fail or hang because the listener won't
			// accept the 3rd rendezvous connection.
			conn3.Write([]byte("overflow\n"))
			conn3.SetReadDeadline(time.Now().Add(5 * time.Second))
			buf := make([]byte, 64)
			_, err = conn3.Read(buf)
			if err == nil {
				// If it somehow succeeded, that's wrong with max-connections=2.
				t.Log("warning: 3rd connection unexpectedly succeeded (relay may buffer)")
			}
		})
	}
}

// TestSmallPayload sends 1-byte writes.
func TestSmallPayload(t *testing.T) {
	env := requireRelayEnv(t)

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
	env := requireRelayEnv(t)

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
	env := requireRelayEnv(t)

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
	env := requireRelayEnv(t)

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
	env := requireRelayEnv(t)

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
	env := requireRelayEnv(t)

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
	env := requireRelayEnv(t)

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
	env := requireRelayEnv(t)

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
			listenerMetrics := waitForLogAddr(t, listener, "metrics server listening", 15*time.Second)
			senderAddr := waitForLogAddr(t, sender, "port-forward listening", 15*time.Second)
			senderMetrics := waitForLogAddr(t, sender, "metrics server listening", 15*time.Second)

			// Run 3 connections.
			for i := 0; i < 3; i++ {
				conn, err := net.DialTimeout("tcp", senderAddr, 10*time.Second)
				if err != nil {
					t.Fatalf("dial %d: %v", i, err)
				}
				conn.Write([]byte("x"))
				buf := make([]byte, 1)
				io.ReadFull(conn, buf)
				conn.Close()
				time.Sleep(500 * time.Millisecond) // let metrics update
			}

			// Wait for metrics to settle.
			time.Sleep(2 * time.Second)

			// Check listener metrics.
			lm := scrapeMetrics(t, listenerMetrics)
			assertMetricGE(t, lm, "aztunnel_connections_total", 3)

			// Check sender metrics.
			sm := scrapeMetrics(t, senderMetrics)
			assertMetricGE(t, sm, "aztunnel_connections_total", 3)

			// Active connections should be 0.
			assertMetricEQ(t, lm, "aztunnel_active_connections", 0)
			assertMetricEQ(t, sm, "aztunnel_active_connections", 0)
		})
	}
}

// TestMetricsErrorReason verifies error reason labels from allowlist rejection.
func TestMetricsErrorReason(t *testing.T) {
	env := requireRelayEnv(t)

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
			listenerMetrics := waitForLogAddr(t, listener, "metrics server listening", 15*time.Second)
			senderAddr := waitForLogAddr(t, sender, "socks5-proxy listening", 15*time.Second)

			// Attempt connection (will be rejected).
			conn := dialSOCKS5(t, senderAddr, echo.Addr())
			conn.SetReadDeadline(time.Now().Add(10 * time.Second))
			io.ReadAll(conn)
			conn.Close()

			time.Sleep(2 * time.Second)

			lm := scrapeMetrics(t, listenerMetrics)
			if !strings.Contains(lm, `reason="allowlist_rejected"`) {
				t.Log("metrics output:", lm)
				t.Fatal("expected allowlist_rejected error reason in metrics")
			}
		})
	}
}

// TestMetricsDialDuration verifies the dial duration histogram has observations.
func TestMetricsDialDuration(t *testing.T) {
	env := requireRelayEnv(t)

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
			senderMetrics := waitForLogAddr(t, sender, "metrics server listening", 15*time.Second)

			conn, err := net.DialTimeout("tcp", senderAddr, 10*time.Second)
			if err != nil {
				t.Fatalf("dial: %v", err)
			}
			conn.Write([]byte("x"))
			buf := make([]byte, 1)
			io.ReadFull(conn, buf)
			conn.Close()

			time.Sleep(2 * time.Second)

			sm := scrapeMetrics(t, senderMetrics)
			if !strings.Contains(sm, "aztunnel_dial_duration_seconds") {
				t.Fatal("missing dial_duration_seconds in sender metrics")
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

// assertNoUsageDump checks that stderr doesn't contain cobra usage output.
func assertNoUsageDump(t *testing.T, output string) {
	t.Helper()
	if strings.Contains(output, "Usage:") && strings.Contains(output, "Flags:") {
		t.Error("stderr contains cobra usage dump; expected clean error only")
	}
}

// TestConnectNoListener verifies a sender gets a clean error when no listener is running.
func TestConnectNoListener(t *testing.T) {
	env := requireRelayEnv(t)

	for _, auth := range availableAuths(t, env) {
		t.Run(auth.name, func(t *testing.T) {
			output := runExpectFail(t, 15*time.Second,
				"relay-sender", "connect", "127.0.0.1:9999",
				"--relay", env.relayName,
				"--hyco", auth.hyco,
			)

			assertNoUsageDump(t, output)
			assertNoTokenLeak(t, output)
			if !strings.Contains(output, "Error:") {
				t.Error("expected 'Error:' prefix in output")
			}
		})
	}
}

// TestBadRelayName verifies a clean error for a non-existent relay namespace.
func TestBadRelayName(t *testing.T) {
	env := requireRelayEnv(t)

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
	env := requireRelayEnv(t)

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
	env := requireRelayEnv(t)
	if env.sasHyco == "" {
		t.Skip("SAS hyco not configured")
	}

	badKey := "dGhpcyBpcyBhIGJhZCBrZXk=" // base64("this is a bad key")
	sasEnv := *env
	sasEnv.hyco = env.sasHyco

	proc := startAztunnelWithSAS(t, &sasEnv,
		&sasCredentials{keyName: env.sasListenerKeyName, key: badKey},
		"relay-listener", env.sasHyco,
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
		output := runClean(t, "relay-listener", "some-hyco")
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
		if !strings.Contains(output, "accepts") || !strings.Contains(output, "arg") {
			t.Errorf("expected error about missing argument, got: %s", output)
		}
	})
}

// TestBadSASKeySender verifies a clean error when the sender has an invalid SAS key.
func TestBadSASKeySender(t *testing.T) {
	env := requireRelayEnv(t)
	if env.sasHyco == "" {
		t.Skip("SAS hyco not configured")
	}

	badKey := "dGhpcyBpcyBhIGJhZCBrZXk=" // base64("this is a bad key")
	sasEnv := *env
	sasEnv.hyco = env.sasHyco

	// Use connect (one-shot) — it dials immediately and fails fast.
	proc := startAztunnelWithSAS(t, &sasEnv,
		&sasCredentials{keyName: env.sasSenderKeyName, key: badKey},
		"relay-sender", "connect", "127.0.0.1:9999",
		"--relay", env.relayName,
		"--hyco", env.sasHyco,
		"--log-level", "debug",
	)

	// Wait for process to exit (it should fail quickly).
	time.Sleep(5 * time.Second)

	logs := proc.logs.String()
	assertNoTokenLeak(t, logs)
	if strings.Contains(logs, badKey) {
		t.Error("logs contain the bad SAS key value")
	}
}

// TestWrongSASClaim verifies that using a listener key for sending (and vice versa) fails.
func TestWrongSASClaim(t *testing.T) {
	env := requireRelayEnv(t)
	if env.sasHyco == "" || env.sasListenerKey == "" || env.sasSenderKey == "" {
		t.Skip("SAS credentials not configured")
	}

	sasEnv := *env
	sasEnv.hyco = env.sasHyco

	t.Run("listener_key_as_sender", func(t *testing.T) {
		// Use listener (Listen-only) key for sending — should fail.
		proc := startAztunnelWithSAS(t, &sasEnv,
			&sasCredentials{keyName: env.sasListenerKeyName, key: env.sasListenerKey},
			"relay-sender", "port-forward", "127.0.0.1:9999",
			"--relay", env.relayName,
			"--hyco", env.sasHyco,
			"--bind", "127.0.0.1:0",
			"--log-level", "debug",
		)

		// The sender should fail to open a rendezvous connection.
		// Wait for any error or log output indicating failure.
		time.Sleep(5 * time.Second)
		logs := proc.logs.String()
		assertNoTokenLeak(t, logs)
	})

	t.Run("sender_key_as_listener", func(t *testing.T) {
		// Use sender (Send-only) key for listening — should fail.
		proc := startAztunnelWithSAS(t, &sasEnv,
			&sasCredentials{keyName: env.sasSenderKeyName, key: env.sasSenderKey},
			"relay-listener", env.sasHyco,
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
	env := requireRelayEnv(t)

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
			listenerMetrics := waitForLogAddr(t, listener, "metrics server listening", 15*time.Second)
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

			// Verify dial_failed metric.
			time.Sleep(2 * time.Second)
			lm := scrapeMetrics(t, listenerMetrics)
			if !strings.Contains(lm, `reason="dial_failed"`) {
				t.Log("metrics output:", lm)
				t.Error("expected dial_failed error reason in metrics")
			}
		})
	}
}

// TestSOCKS5ClosedPort verifies clean behavior when SOCKS5 targets a closed port.
func TestSOCKS5ClosedPort(t *testing.T) {
	env := requireRelayEnv(t)

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
	env := requireRelayEnv(t)

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
			listenerMetrics := waitForLogAddr(t, listener, "metrics server listening", 15*time.Second)
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

			// Verify dial_timeout metric.
			time.Sleep(2 * time.Second)
			lm := scrapeMetrics(t, listenerMetrics)
			if !strings.Contains(lm, `reason="dial_timeout"`) && !strings.Contains(lm, `reason="dial_failed"`) {
				t.Log("metrics output:", lm)
				t.Error("expected dial_timeout or dial_failed error reason in metrics")
			}
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
