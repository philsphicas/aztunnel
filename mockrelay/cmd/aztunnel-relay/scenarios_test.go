package main_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

// These tests exercise scenarios that the mock relay faithfully
// implements (rendezvous, bridge fan-out, 404 retry, control reconnect)
// from the client's perspective via real subprocesses. They mirror the
// Azure e2e scenarios where the mock's behavior is a meaningful proxy
// for the real relay's. Auth-specific tests, metrics tests, and
// Azure-idle-timeout-dependent tests are intentionally omitted — see
// mockrelay/README.md.
//
// All tests use the relay's TLS path (`wss://...` via `rp.relayURL`)
// with `--relay-insecure-tls` to accept the self-signed cert. aztunnel
// rejects plain `ws://` / `http://` URLs at parse time so there is no
// non-TLS variant.

const (
	defaultDialTimeout = 5 * time.Second
)

func TestSubprocess_SOCKS5(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping subprocess test in -short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	echoAddr := startEcho(t, ctx)
	rp := startRelay(t, ctx)
	const entity = "scenario-socks5"

	listener := startListener(t, ctx, rp.relayURL, entity, "--allow", echoAddr)
	waitForLog(t, listener, "control channel connected", 10*time.Second)

	socksBind := fmt.Sprintf("127.0.0.1:%d", pickFreePort(t))
	_ = startSOCKS5Sender(t, ctx, rp.relayURL, entity, socksBind)

	waitForTCP(t, socksBind, defaultDialTimeout)

	conn := dialSOCKS5(t, socksBind, echoAddr)
	defer conn.Close() //nolint:errcheck

	want := []byte("socks5 round-trip\n")
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := conn.Write(want); err != nil {
		t.Fatalf("write: %v", err)
	}
	got := make([]byte, len(want))
	if _, err := io.ReadFull(conn, got); err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("echo mismatch: got=%q want=%q", got, want)
	}
}

func TestSubprocess_ConnectStdio(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping subprocess test in -short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	echoAddr := startEcho(t, ctx)
	rp := startRelay(t, ctx)
	const entity = "scenario-connect"

	listener := startListener(t, ctx, rp.relayURL, entity, "--allow", echoAddr)
	waitForLog(t, listener, "control channel connected", 10*time.Second)

	_, stdin, stdout := startConnectSender(t, ctx, rp.relayURL, entity, echoAddr)

	// There's no reliable info-level log to wait on for connect-stdio
	// mode, so we poll the echo path by writing and reading. The
	// sender's 404→backoff retry covers the race where the control WS
	// isn't ready yet.
	want := []byte("connect-stdio round-trip\n")
	if err := writeWithRetry(stdin, want, 5*time.Second); err != nil {
		t.Fatalf("write stdin: %v", err)
	}
	got := make([]byte, len(want))
	if err := readFullWithTimeout(stdout, got, 10*time.Second); err != nil {
		t.Fatalf("read stdout: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("echo mismatch: got=%q want=%q", got, want)
	}
}

func TestSubprocess_ConcurrentSameTarget(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping subprocess test in -short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	echoAddr := startEcho(t, ctx)
	rp := startRelay(t, ctx)
	const entity = "scenario-concurrent"

	listener := startListener(t, ctx, rp.relayURL, entity, "--allow", echoAddr)
	waitForLog(t, listener, "control channel connected", 10*time.Second)

	pfBind := fmt.Sprintf("127.0.0.1:%d", pickFreePort(t))
	_ = startPortForwardSender(t, ctx, rp.relayURL, entity, pfBind, echoAddr)
	waitForTCP(t, pfBind, defaultDialTimeout)

	const numConns = 20
	var wg sync.WaitGroup
	errCh := make(chan error, numConns)

	for i := 0; i < numConns; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			conn, err := net.DialTimeout("tcp", pfBind, 15*time.Second)
			if err != nil {
				errCh <- fmt.Errorf("conn %d dial: %w", id, err)
				return
			}
			defer conn.Close() //nolint:errcheck
			_ = conn.SetDeadline(time.Now().Add(15 * time.Second))

			msg := fmt.Sprintf("hello from %d\n", id)
			if _, err := conn.Write([]byte(msg)); err != nil {
				errCh <- fmt.Errorf("conn %d write: %w", id, err)
				return
			}
			buf := make([]byte, len(msg))
			if _, err := io.ReadFull(conn, buf); err != nil {
				errCh <- fmt.Errorf("conn %d read: %w", id, err)
				return
			}
			if string(buf) != msg {
				errCh <- fmt.Errorf("conn %d: got %q, want %q", id, buf, msg)
			}
		}(i)
	}

	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Error(err)
	}
}

func TestSubprocess_MediumPayload(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping subprocess test in -short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	echoAddr := startEcho(t, ctx)
	rp := startRelay(t, ctx)
	const entity = "scenario-medium"

	listener := startListener(t, ctx, rp.relayURL, entity, "--allow", echoAddr)
	waitForLog(t, listener, "control channel connected", 10*time.Second)

	pfBind := fmt.Sprintf("127.0.0.1:%d", pickFreePort(t))
	_ = startPortForwardSender(t, ctx, rp.relayURL, entity, pfBind, echoAddr)
	waitForTCP(t, pfBind, defaultDialTimeout)

	conn, err := dialWithRetry(pfBind, 5*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close() //nolint:errcheck

	// 1 MiB random payload. Echo simultaneously writes back, so write
	// in a goroutine while we read.
	payload := make([]byte, 1<<20)
	if _, err := rand.Read(payload); err != nil {
		t.Fatalf("rand: %v", err)
	}
	wantHash := sha256.Sum256(payload)

	go func() {
		_, _ = conn.Write(payload)
	}()

	got := make([]byte, len(payload))
	_ = conn.SetReadDeadline(time.Now().Add(20 * time.Second))
	if _, err := io.ReadFull(conn, got); err != nil {
		t.Fatalf("read: %v", err)
	}
	gotHash := sha256.Sum256(got)
	if wantHash != gotHash {
		t.Fatal("SHA256 mismatch: payload corrupted through tunnel")
	}
}

// TestSubprocess_BulkTransfer streams 100 MiB through the tunnel.
// The aztunnel client chunks at 32 KiB per WebSocket message, so this
// exercises stream fan-out across thousands of frames in both
// directions and validates that the bridge does not stall, reorder,
// or corrupt data on a large transfer.
func TestSubprocess_BulkTransfer(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping subprocess test in -short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	echoAddr := startEcho(t, ctx)
	rp := startRelay(t, ctx)
	const entity = "scenario-bulk"

	listener := startListener(t, ctx, rp.relayURL, entity, "--allow", echoAddr)
	waitForLog(t, listener, "control channel connected", 10*time.Second)

	pfBind := fmt.Sprintf("127.0.0.1:%d", pickFreePort(t))
	_ = startPortForwardSender(t, ctx, rp.relayURL, entity, pfBind, echoAddr)
	waitForTCP(t, pfBind, defaultDialTimeout)

	conn, err := dialWithRetry(pfBind, 5*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close() //nolint:errcheck

	const totalBytes = 100 << 20 // 100 MiB
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
			if _, err := rand.Read(chunk[:n]); err != nil {
				return
			}
			h.Write(chunk[:n])
			if _, err := conn.Write(chunk[:n]); err != nil {
				return
			}
			remaining -= n
		}
		var sum [sha256.Size]byte
		copy(sum[:], h.Sum(nil))
		sentHash <- sum
	}()

	_ = conn.SetReadDeadline(time.Now().Add(45 * time.Second))
	gotHash := sha256.New()
	n, err := io.CopyN(gotHash, conn, totalBytes)
	if err != nil {
		t.Fatalf("read after %d bytes: %v", n, err)
	}

	want := <-sentHash
	if !bytes.Equal(want[:], gotHash.Sum(nil)) {
		t.Fatal("SHA256 mismatch: bulk transfer data corrupted")
	}
	t.Logf("transferred %d MiB successfully", totalBytes>>20)
}

// TestSubprocess_SenderRetriesUntilListener starts the sender BEFORE the
// listener, verifies the sender logs at least one 404 retry, then
// starts the listener and confirms data flows.
func TestSubprocess_SenderRetriesUntilListener(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping subprocess test in -short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	echoAddr := startEcho(t, ctx)
	rp := startRelay(t, ctx)
	const entity = "scenario-retry"

	// Start the sender FIRST — no listener registered. The sender's
	// DialWithRetry will see 404 and retry with backoff.
	connProc, stdin, stdout := startConnectSender(t, ctx, rp.relayURL, entity, echoAddr)

	// Wait for at least one retry. The log is "relay dial failed (retrying)".
	const retryLog = "relay dial failed (retrying)"
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(connProc.out.String(), retryLog) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !strings.Contains(connProc.out.String(), retryLog) {
		t.Fatalf("sender never logged a retry; output:\n%s", connProc.out.String())
	}

	// NOW start the listener.
	listener := startListener(t, ctx, rp.relayURL, entity, "--allow", echoAddr)
	waitForLog(t, listener, "control channel connected", 10*time.Second)

	// The sender should reconnect on its next retry. Verify data flows.
	want := []byte("after retry\n")
	if err := writeWithRetry(stdin, want, 15*time.Second); err != nil {
		t.Fatalf("write stdin: %v", err)
	}
	got := make([]byte, len(want))
	if err := readFullWithTimeout(stdout, got, 15*time.Second); err != nil {
		t.Fatalf("read stdout: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("echo mismatch: got=%q want=%q", got, want)
	}
}

// TestSubprocess_ListenerRestartRecovery verifies the sender keeps
// working after the listener is killed and restarted.
func TestSubprocess_ListenerRestartRecovery(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping subprocess test in -short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	echoAddr := startEcho(t, ctx)
	rp := startRelay(t, ctx)
	const entity = "scenario-restart"

	listener1 := startListener(t, ctx, rp.relayURL, entity, "--allow", echoAddr)
	waitForLog(t, listener1, "control channel connected", 10*time.Second)

	pfBind := fmt.Sprintf("127.0.0.1:%d", pickFreePort(t))
	_ = startPortForwardSender(t, ctx, rp.relayURL, entity, pfBind, echoAddr)
	waitForTCP(t, pfBind, defaultDialTimeout)

	// First round-trip.
	if err := echoRoundTrip(pfBind, []byte("before restart\n"), 10*time.Second); err != nil {
		t.Fatalf("pre-restart: %v", err)
	}

	// Stop listener1 and wait for the relay to forget about it. The
	// mock's listener idle timeout default is 2m, but a graceful SIGINT
	// closes the control WS immediately.
	listener1.stopAndWait()
	time.Sleep(300 * time.Millisecond)

	// Start listener2. The sender's port-forward keeps its bind alive;
	// new TCP connections trigger fresh rendezvous attempts.
	listener2 := startListener(t, ctx, rp.relayURL, entity, "--allow", echoAddr)
	waitForLog(t, listener2, "control channel connected", 10*time.Second)

	// New connections should round-trip. The sender retries with backoff
	// if listener2's registration races with the dial.
	if err := echoRoundTripWithRetry(pfBind, []byte("after restart\n"), 15*time.Second); err != nil {
		t.Fatalf("post-restart: %v", err)
	}
}

// echoRoundTrip dials addr, writes payload, reads len(payload) bytes
// back, and verifies equality.
func echoRoundTrip(addr string, payload []byte, timeout time.Duration) error {
	conn, err := net.DialTimeout("tcp", addr, timeout)
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	defer conn.Close() //nolint:errcheck
	_ = conn.SetDeadline(time.Now().Add(timeout))
	if _, err := conn.Write(payload); err != nil {
		return fmt.Errorf("write: %w", err)
	}
	got := make([]byte, len(payload))
	if _, err := io.ReadFull(conn, got); err != nil {
		return fmt.Errorf("read: %w", err)
	}
	if !bytes.Equal(got, payload) {
		return fmt.Errorf("mismatch: got=%q want=%q", got, payload)
	}
	return nil
}

// echoRoundTripWithRetry keeps trying echoRoundTrip until timeout. Used
// when the listener has just (re)started and the sender's first dial
// may race with control-WS registration on the relay.
func echoRoundTripWithRetry(addr string, payload []byte, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		if err := echoRoundTrip(addr, payload, 5*time.Second); err == nil {
			return nil
		} else {
			lastErr = err
		}
		time.Sleep(200 * time.Millisecond)
	}
	return lastErr
}

// writeWithRetry writes payload to w, retrying on EAGAIN-like errors.
// Pipe writes to a child process don't generally fail mid-write on
// Unix, but the child may not be ready to read yet — this gives the
// child up to timeout to consume.
func writeWithRetry(w io.Writer, payload []byte, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		_, err := w.Write(payload)
		if err == nil {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("write timed out after %s", timeout)
}

// readFullWithTimeout reads len(buf) bytes from r or fails after
// timeout. Implemented with a goroutine because os.Pipe readers
// don't support SetReadDeadline.
func readFullWithTimeout(r io.Reader, buf []byte, timeout time.Duration) error {
	done := make(chan error, 1)
	go func() {
		_, err := io.ReadFull(r, buf)
		done <- err
	}()
	select {
	case err := <-done:
		return err
	case <-time.After(timeout):
		return fmt.Errorf("read timed out after %s", timeout)
	}
}
