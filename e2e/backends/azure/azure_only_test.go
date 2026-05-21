//go:build e2e

package azure

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"testing"
	"time"
)

// TestAzureOnly_BulkTransfer streams 100 MB through the tunnel.
// Opt-in via E2E_LARGE_TRANSFER=1. Azure-only because the bulk
// transfer exercises the real Azure Relay's throughput envelope,
// which the in-process mock cannot meaningfully reproduce.
func TestAzureOnly_BulkTransfer(t *testing.T) {
	if os.Getenv("E2E_LARGE_TRANSFER") != "1" {
		t.Skip("set E2E_LARGE_TRANSFER=1 to enable (sends 100MB through relay)")
	}
	t.Parallel()
	env := requireDedicatedHyco(t)

	for _, auth := range availableAuths(t, env) {
		auth := auth
		t.Run(auth.name, func(t *testing.T) {
			echo := startBulkEcho(t)
			_ = startListener(t, env, auth, "--allow", echo.Addr())
			sender := startPortForwardSender(t, env, auth, echo.Addr())
			senderAddr := waitForLogAddr(t, sender, "port-forward listening", 30*time.Second)

			conn, err := net.DialTimeout("tcp", senderAddr, 10*time.Second)
			if err != nil {
				t.Fatalf("dial: %v", err)
			}
			defer conn.Close() //nolint:errcheck // best-effort cleanup

			const totalBytes = 100 << 20
			sentHash := make(chan [sha256.Size]byte, 1)
			go func() {
				h := sha256.New()
				chunk := make([]byte, 64*1024)
				remaining := totalBytes
				for remaining > 0 {
					n := len(chunk)
					if n > remaining {
						n = remaining
					}
					_, _ = rand.Read(chunk[:n])
					h.Write(chunk[:n])
					_, _ = conn.Write(chunk[:n])
					remaining -= n
				}
				var sum [sha256.Size]byte
				copy(sum[:], h.Sum(nil))
				sentHash <- sum
			}()

			gotHash := sha256.New()
			n, err := io.CopyN(gotHash, conn, totalBytes)
			if err != nil {
				t.Fatalf("read after %d bytes: %v", n, err)
			}
			want := <-sentHash
			if !bytes.Equal(want[:], gotHash.Sum(nil)) {
				t.Fatal("SHA256 mismatch: bulk transfer data corrupted")
			}
			t.Logf("transferred %d MB successfully", totalBytes>>20)
		})
	}
}

// TestAzureOnly_LongLivedConnection keeps a connection open past
// the Azure Relay idle timeout (~120 s). Opt-in via
// E2E_LONG_LIVED=1. Azure-only because the timeout / keepalive
// pinging path is Azure-specific.
func TestAzureOnly_LongLivedConnection(t *testing.T) {
	if os.Getenv("E2E_LONG_LIVED") != "1" {
		t.Skip("set E2E_LONG_LIVED=1 to enable (runs for >2 minutes)")
	}
	t.Parallel()
	env := requireDedicatedHyco(t)

	for _, auth := range availableAuths(t, env) {
		auth := auth
		t.Run(auth.name, func(t *testing.T) {
			echo := startBulkEcho(t)
			_ = startListener(t, env, auth, "--allow", echo.Addr())
			sender := startPortForwardSender(t, env, auth, echo.Addr())
			senderAddr := waitForLogAddr(t, sender, "port-forward listening", 30*time.Second)

			conn, err := net.DialTimeout("tcp", senderAddr, 10*time.Second)
			if err != nil {
				t.Fatalf("dial: %v", err)
			}
			defer conn.Close() //nolint:errcheck // best-effort cleanup

			payload := []byte("before-idle\n")
			_, _ = conn.Write(payload)
			buf := make([]byte, len(payload))
			_, _ = io.ReadFull(conn, buf)

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

// TestAzureOnly_TokenFetchMetric verifies that
// aztunnel_token_fetch_seconds (histogram) and
// aztunnel_token_fetch_total (counter) appear on the sender
// subprocess's /metrics surface after a successful dial, with the
// provider label matching the auth method in use. Azure-only
// because the production TokenProvider implementations (Entra +
// real SAS) only run in this path; the in-process mock's
// emulation lives in TestMockEmulates_TokenFetchMetric.
func TestAzureOnly_TokenFetchMetric(t *testing.T) {
	t.Parallel()
	env := requireDedicatedHyco(t)

	for _, auth := range availableAuths(t, env) {
		auth := auth
		t.Run(auth.name, func(t *testing.T) {
			echo := startBulkEcho(t)
			listener := startListener(t, env, auth, "--allow", echo.Addr())
			sender := startPortForwardSender(t, env, auth, echo.Addr(),
				"--metrics-addr", "127.0.0.1:0",
			)

			waitForLog(t, listener, "control_started", 30*time.Second)
			senderAddr := waitForLogAddr(t, sender, "port-forward listening", 15*time.Second)
			senderMetrics := sender.MetricsAddr(t, 15*time.Second)

			conn, err := net.DialTimeout("tcp", senderAddr, 10*time.Second)
			if err != nil {
				t.Fatalf("dial sender: %v", err)
			}
			defer conn.Close() //nolint:errcheck // best-effort cleanup
			payload := []byte("token-fetch-metric\n")
			if _, err := conn.Write(payload); err != nil {
				t.Fatalf("write: %v", err)
			}
			buf := make([]byte, len(payload))
			if _, err := io.ReadFull(conn, buf); err != nil {
				t.Fatalf("read: %v", err)
			}

			counterLine := fmt.Sprintf(`aztunnel_token_fetch_total{provider="%s",result="ok"}`, auth.name)
			waitForMetricsContains(t, senderMetrics, counterLine, 15*time.Second)

			histCountLine := fmt.Sprintf(`aztunnel_token_fetch_seconds_count{provider="%s",result="ok"}`, auth.name)
			body := waitForMetricsContains(t, senderMetrics, histCountLine, 5*time.Second)

			counterValue := metricLineValue(t, body, counterLine)
			if counterValue < 1 {
				t.Errorf("%s = %v, want >= 1", counterLine, counterValue)
			}
			histCountValue := metricLineValue(t, body, histCountLine)
			if histCountValue < 1 {
				t.Errorf("%s = %v, want >= 1", histCountLine, histCountValue)
			}
			if histCountValue != counterValue {
				t.Errorf("histogram count %v != counter %v (wrapper must observe both per call)",
					histCountValue, counterValue)
			}
			assertNoOtherProvider(t, body, "aztunnel_token_fetch_total", auth.name)
		})
	}
}

// TestAzureOnly_CrossClaim: cross-uses a listener-direction SAS
// key as the sender key (and the sender-direction key as the
// listener key) and asserts the relay rejects each. Azure-only
// because mock relay does not track per-key Listen vs Send
// direction.
func TestAzureOnly_CrossClaim(t *testing.T) {
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
		proc := startAztunnelWithSAS(t, &sasEnv,
			&sasCredentials{keyName: env.sasListenerKeyName, key: env.sasListenerKey},
			"relay-sender", "port-forward", "127.0.0.1:9999",
			"--relay", env.relayName,
			"--hyco", env.sasHyco,
			"--bind", "127.0.0.1:0",
			"--log-level", "debug",
		)
		senderAddr := waitForLogAddr(t, proc, "port-forward listening", 15*time.Second)

		conn, err := net.DialTimeout("tcp", senderAddr, 10*time.Second)
		if err != nil {
			t.Fatalf("dial sender: %v", err)
		}
		defer conn.Close() //nolint:errcheck // best-effort cleanup
		_, _ = conn.Write([]byte("trigger\n"))
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		buf := make([]byte, 64)
		if _, rerr := conn.Read(buf); rerr == nil {
			t.Fatal("expected sender->relay dial to fail with wrong claim")
		}
		waitForLog(t, proc, "relay dial failed", 15*time.Second)
		assertNoTokenLeak(t, proc.logs.String())
	})

	t.Run("sender_key_as_listener", func(t *testing.T) {
		proc := startAztunnelWithSAS(t, &sasEnv,
			&sasCredentials{keyName: env.sasSenderKeyName, key: env.sasSenderKey},
			"relay-listener",
			"--hyco", env.sasHyco,
			"--relay", env.relayName,
			"--log-level", "debug",
		)
		waitForLog(t, proc, "control channel disconnected", 30*time.Second)
		assertNoTokenLeak(t, proc.logs.String())
	})
}

// startBulkEcho is a TCP echo server local to the Azure-only
// tests; the scenarios package's StartPlainEcho would also work
// but using a local helper keeps these tests self-contained
// against future scenarios changes.
func startBulkEcho(t *testing.T) *bulkEcho {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("bulk echo listen: %v", err)
	}
	e := &bulkEcho{ln: ln}
	go e.serve()
	t.Cleanup(func() { _ = ln.Close() })
	return e
}

type bulkEcho struct{ ln net.Listener }

func (e *bulkEcho) Addr() string { return e.ln.Addr().String() }

func (e *bulkEcho) serve() {
	for {
		c, err := e.ln.Accept()
		if err != nil {
			return
		}
		go func(c net.Conn) {
			defer c.Close() //nolint:errcheck // best-effort cleanup
			_, _ = io.Copy(c, c)
		}(c)
	}
}

// metricLineValue parses the value off the end of the first
// Prometheus text line in body that starts with linePrefix.
func metricLineValue(t *testing.T, body, linePrefix string) float64 {
	t.Helper()
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, linePrefix) {
			continue
		}
		idx := strings.LastIndex(line, " ")
		if idx == -1 || idx == len(line)-1 {
			continue
		}
		var v float64
		if _, err := fmt.Sscanf(line[idx+1:], "%f", &v); err != nil {
			t.Logf("could not parse value from line %q: %v", line, err)
			continue
		}
		return v
	}
	return 0
}

// assertNoOtherProvider fails the test if family appears in body
// with a `provider="..."` label that is not wantProvider.
func assertNoOtherProvider(t *testing.T, body, family, wantProvider string) {
	t.Helper()
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, family+"{") {
			continue
		}
		const marker = `provider="`
		i := strings.Index(line, marker)
		if i == -1 {
			continue
		}
		rest := line[i+len(marker):]
		end := strings.IndexByte(rest, '"')
		if end == -1 {
			continue
		}
		got := rest[:end]
		if got != wantProvider {
			t.Errorf("%s has unexpected provider label %q (want only %q): %s",
				family, got, wantProvider, line)
		}
	}
}

// assertNoTokenLeak fails the test if output contains a raw
// sb-hc-token (the redacted form is fine). Shared between the
// auth-failure azure-only tests.
func assertNoTokenLeak(t *testing.T, output string) {
	t.Helper()
	stripped := strings.ReplaceAll(output, "sb-hc-token=REDACTED", "")
	if strings.Contains(stripped, "sb-hc-token=") {
		t.Error("output contains raw sb-hc-token (not redacted)")
	}
}
