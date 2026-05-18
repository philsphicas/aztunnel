package relayparity

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"testing"
	"time"
)

// RunCoreSuite runs every single-flow parity scenario against b as a
// set of sub-tests under the caller's t. New scenarios get added here.
//
// The scenarios run sequentially, each in its own t.Run; backends
// register their own t.Cleanup, so each sub-test gets a freshly built
// topology and tears it down before the next starts.
//
// Scenarios in the core suite are required to pass on current main.
// Behaviors that depend on capabilities the bridge does not yet
// implement (e.g. half-close propagation) live outside the core
// suite so this gate stays green regardless of bridge architecture.
func RunCoreSuite(t *testing.T, b Backend) {
	t.Helper()
	scenarios := []struct {
		name string
		run  func(*testing.T, Backend)
	}{
		{"Echo_PortForward", ScenarioEcho_PortForward},
		{"Echo_SOCKS5", ScenarioEcho_SOCKS5},
		{"Ordering_PortForward", ScenarioOrdering_PortForward},
		{"Bidirectional_PortForward", ScenarioBidirectional_PortForward},
	}
	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			sc.run(t, b)
		})
	}
}

// ScenarioEcho_PortForward: open one port-forward connection, write a
// short payload, read it back unchanged. The minimum proof that the
// tunnel routes bytes end-to-end.
func ScenarioEcho_PortForward(t *testing.T, b Backend) {
	t.Helper()
	AssertNoLeaks(t)
	echo := StartPlainEcho(t)
	tun := b.Setup(t, SetupOptions{
		NumListeners:   1,
		SenderMode:     ModePortForward,
		Target:         echo.Addr(),
		AllowedTargets: []string{echo.Addr()},
	})

	conn := dialWithRetry(t, tun.SenderAddr, 5*time.Second)
	defer conn.Close() //nolint:errcheck // best-effort cleanup

	want := []byte("hello aztunnel parity\n")
	writeAll(t, conn, want)
	got := readN(t, conn, len(want), 10*time.Second)
	if !bytes.Equal(got, want) {
		t.Fatalf("echo mismatch\n got=%q\nwant=%q", got, want)
	}
}

// ScenarioEcho_SOCKS5: same as ScenarioEcho_PortForward but the
// sender is a SOCKS5 proxy. Verifies the SOCKS5 CONNECT path round-
// trips bytes correctly.
func ScenarioEcho_SOCKS5(t *testing.T, b Backend) {
	t.Helper()
	AssertNoLeaks(t)
	echo := StartPlainEcho(t)
	tun := b.Setup(t, SetupOptions{
		NumListeners:   1,
		SenderMode:     ModeSOCKS5,
		AllowedTargets: []string{echo.Addr()},
	})

	conn, err := dialSOCKS5WithRetry(tun.SenderAddr, echo.Addr(), 5*time.Second)
	if err != nil {
		t.Fatalf("socks5 dial: %v", err)
	}
	defer conn.Close() //nolint:errcheck // best-effort cleanup

	want := []byte("hello aztunnel socks5 parity\n")
	writeAll(t, conn, want)
	got := readN(t, conn, len(want), 10*time.Second)
	if !bytes.Equal(got, want) {
		t.Fatalf("echo mismatch\n got=%q\nwant=%q", got, want)
	}
}

// ScenarioOrdering_PortForward sends 1024 sequenced 1 KB chunks
// through one connection. Each chunk's first 4 bytes are the chunk
// index in big-endian; the remaining 1020 bytes are deterministic
// filler. Read-back must arrive in the same order, identical bytes.
//
// Catches reordering or interleaving inside the bridge.
func ScenarioOrdering_PortForward(t *testing.T, b Backend) {
	t.Helper()
	AssertNoLeaks(t)
	echo := StartPlainEcho(t)
	tun := b.Setup(t, SetupOptions{
		NumListeners:   1,
		SenderMode:     ModePortForward,
		Target:         echo.Addr(),
		AllowedTargets: []string{echo.Addr()},
	})

	const nChunks = 1024
	const chunkSize = 1024
	total := nChunks * chunkSize

	conn := dialWithRetry(t, tun.SenderAddr, 5*time.Second)
	defer conn.Close() //nolint:errcheck // best-effort cleanup
	_ = conn.SetDeadline(time.Now().Add(60 * time.Second))

	// Writer goroutine.
	writeErr := make(chan error, 1)
	go func() {
		buf := make([]byte, chunkSize)
		for i := uint32(0); i < nChunks; i++ {
			binary.BigEndian.PutUint32(buf[:4], i)
			for j := 4; j < chunkSize; j++ {
				buf[j] = byte(i + uint32(j))
			}
			if err := writeFull(conn, buf); err != nil {
				writeErr <- fmt.Errorf("write chunk %d: %w", i, err)
				return
			}
		}
		writeErr <- nil
	}()

	// Reader on main goroutine.
	got := make([]byte, total)
	if _, err := io.ReadFull(conn, got); err != nil {
		t.Fatalf("read full %d bytes: %v", total, err)
	}
	if err := <-writeErr; err != nil {
		t.Fatalf("writer: %v", err)
	}

	for i := uint32(0); i < nChunks; i++ {
		off := int(i) * chunkSize
		seen := binary.BigEndian.Uint32(got[off : off+4])
		if seen != i {
			t.Fatalf("chunk %d header mismatch: got %d at offset %d", i, seen, off)
		}
		for j := 4; j < chunkSize; j++ {
			want := byte(i + uint32(j))
			if got[off+j] != want {
				t.Fatalf("chunk %d byte %d: got %#x want %#x", i, j, got[off+j], want)
			}
		}
	}
}

// ScenarioBidirectional_PortForward writes 256 KB upstream while
// reading 256 KB downstream simultaneously, comparing both sides
// against an in-memory random body via SHA256. The target is a plain
// (full-duplex) echo, so what the client reads is exactly what it
// wrote — a SHA256 match in both directions, end-to-end.
//
// Catches half-duplex bugs, back-pressure deadlocks, and silent body
// corruption (random body + hash + exact length defends against the
// "echo passes but bytes got rearranged" failure mode).
func ScenarioBidirectional_PortForward(t *testing.T, b Backend) {
	t.Helper()
	AssertNoLeaks(t)
	echo := StartPlainEcho(t)
	tun := b.Setup(t, SetupOptions{
		NumListeners:   1,
		SenderMode:     ModePortForward,
		Target:         echo.Addr(),
		AllowedTargets: []string{echo.Addr()},
	})

	const size = 256 * 1024
	body := make([]byte, size)
	if _, err := rand.Read(body); err != nil {
		t.Fatalf("rand: %v", err)
	}
	want := sha256.Sum256(body)

	conn := dialWithRetry(t, tun.SenderAddr, 5*time.Second)
	defer conn.Close() //nolint:errcheck // best-effort cleanup
	_ = conn.SetDeadline(time.Now().Add(60 * time.Second))

	// Each goroutine reports its result on its own channel; the main
	// goroutine drains both before asserting. This keeps the
	// happens-before story explicit (channel send → channel receive)
	// rather than relying on WaitGroup memory ordering around shared
	// variables.
	type readResult struct {
		err error
		sum [32]byte
		n   int
	}
	wErr := make(chan error, 1)
	rRes := make(chan readResult, 1)

	go func() {
		if err := writeFull(conn, body); err != nil {
			wErr <- fmt.Errorf("write: %w", err)
			return
		}
		wErr <- nil
	}()
	go func() {
		got := make([]byte, size)
		if _, err := io.ReadFull(conn, got); err != nil {
			rRes <- readResult{err: fmt.Errorf("read: %w", err)}
			return
		}
		rRes <- readResult{sum: sha256.Sum256(got), n: size}
	}()

	if err := <-wErr; err != nil {
		t.Fatalf("%v", err)
	}
	res := <-rRes
	if res.err != nil {
		t.Fatalf("%v", res.err)
	}
	if res.n != size {
		t.Fatalf("length mismatch: got %d want %d", res.n, size)
	}
	if res.sum != want {
		t.Fatalf("body sha256 mismatch:\n got=%x\nwant=%x", res.sum, want)
	}
}

// dialWithRetry dials addr with a short backoff so the sender's bind
// has time to come up after Setup returns. Setup blocks until ready,
// but tests still see occasional ECONNREFUSED races on slow runners;
// 200 ms of backoff makes the test deterministic.
//
// Each attempt is bounded by the remaining budget against a single
// absolute deadline (computed at entry), so the helper respects its
// timeout parameter precisely — no stacked per-attempt timeouts.
func dialWithRetry(t *testing.T, addr string, timeout time.Duration) net.Conn {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var lastErr error
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			break
		}
		attemptTimeout := remaining
		if attemptTimeout > 1*time.Second {
			attemptTimeout = 1 * time.Second
		}
		conn, err := net.DialTimeout("tcp", addr, attemptTimeout)
		if err == nil {
			return conn
		}
		lastErr = err
		const backoff = 25 * time.Millisecond
		if time.Until(deadline) <= backoff {
			break
		}
		time.Sleep(backoff)
	}
	t.Fatalf("dial %s after %v: %v", addr, timeout, lastErr)
	return nil
}

// dialSOCKS5WithRetry is the SOCKS5 analogue of dialWithRetry.
// SOCKS5 negotiation can transiently fail if the listener's control
// channel has not yet attached on the relay side; retry until the
// timeout. Each attempt is bounded by the remaining budget so a
// stalled SOCKS5 handshake cannot block past the overall deadline.
//
// A SOCKS5-level reply (any *SOCKS5Error — e.g. REP=0x02
// "not allowed by ruleset", REP=0x07 "command not supported") is a
// definitive answer from the proxy, not a transient race, so it
// short-circuits the retry loop and is returned immediately. Only
// dial/IO errors are retried.
func dialSOCKS5WithRetry(proxyAddr, target string, timeout time.Duration) (net.Conn, error) {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			break
		}
		attemptTimeout := remaining
		if attemptTimeout > 2*time.Second {
			attemptTimeout = 2 * time.Second
		}
		conn, err := DialSOCKS5(proxyAddr, target, attemptTimeout)
		if err == nil {
			return conn, nil
		}
		var sErr *SOCKS5Error
		if errors.As(err, &sErr) {
			return nil, err
		}
		lastErr = err
		// Cap the inter-attempt backoff to whatever budget is left so
		// the wrapper never overshoots its overall deadline. If less
		// than the full backoff remains, there's no time for another
		// attempt anyway, so break out instead of sleeping into the
		// final break check.
		const backoff = 50 * time.Millisecond
		if time.Until(deadline) <= backoff {
			break
		}
		time.Sleep(backoff)
	}
	return nil, fmt.Errorf("socks5 dial %s via %s after %v: %w", target, proxyAddr, timeout, lastErr)
}

func writeAll(t *testing.T, w io.Writer, b []byte) {
	t.Helper()
	if err := writeFull(w, b); err != nil {
		t.Fatalf("write: %v", err)
	}
}

// writeFull is the goroutine-safe counterpart to writeAll: it writes
// all of buf to w and returns an explicit error on short write. Per
// the io.Writer contract a short write must come with a non-nil
// error, but checking n explicitly surfaces contract violations as a
// clear failure rather than a deadlocked reader.
func writeFull(w io.Writer, buf []byte) error {
	n, err := w.Write(buf)
	if err != nil {
		return err
	}
	if n != len(buf) {
		return io.ErrShortWrite
	}
	return nil
}

func readN(t *testing.T, c net.Conn, n int, timeout time.Duration) []byte {
	t.Helper()
	_ = c.SetReadDeadline(time.Now().Add(timeout))
	defer func() { _ = c.SetReadDeadline(time.Time{}) }()
	buf := make([]byte, n)
	if _, err := io.ReadFull(c, buf); err != nil {
		t.Fatalf("read %d bytes: %v", n, err)
	}
	return buf
}
