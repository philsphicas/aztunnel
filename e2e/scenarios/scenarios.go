package scenarios

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"
)

// RunAllScenarios runs every scenario suite (Core, Topology,
// Reliability, Observability, Performance) against b. It enumerates
// the full axis stack (b.Axes() outer + the mux v1/v2 axis innermost)
// exactly once, so each suite sees a fully-cell-pinned backend and
// the rendered sub-test path reads as
// `<entry>/<outer-cell-values>/<mux>/<scenario>` without Go's #01/#02
// disambiguation suffixes.
//
// EVERY suite runs under both v1 (legacy) and v2 (mux) sender code
// paths. This is the only place the project asserts v2 actually works
// end-to-end for non-perf functionality — half-close, SOCKS5
// distinct-targets, error propagation, bridge cause logs, listener-id
// and bridge-id correlation, the metrics surface (other than the
// explicitly-mux scenarios). Wrapping only the perf suite (as an
// earlier iteration did) leaves the bulk of v2 behavior unverified.
//
// Scenarios that pin SenderMaxProtocolVersion explicitly (e.g. the
// topology / observability scenarios whose contracts depend on
// per-rendezvous semantics, or the mux-fallback scenarios that need
// v2 specifically) keep their pin in BOTH the v1 and v2 cells —
// WithMuxAxis only fills SenderMaxProtocolVersion when the caller
// hasn't already set it. Those scenarios run twice with the same
// effective configuration; the duplication is harmless and avoids the
// alternative (a per-scenario skip mechanism) until it's actually felt.
//
// Call this from test entry points (TestE2E_Azure, TestE2E_Mock)
// rather than calling the individual Run*Scenarios in sequence —
// each Run*Scenarios expects a fully-pinned backend (no axes left to
// enumerate); driving them directly would skip the mux fan-out.
func RunAllScenarios(t *testing.T, b Backend) {
	t.Helper()
	axes := b.Axes()
	names := make([]string, 0, len(axes)+1)
	for _, a := range axes {
		names = append(names, a.Name())
	}
	// The mux axis is innermost (added inside each outer cell).
	names = append(names, MuxAxisName)
	perfMatrixSink.setAxisNames(names)
	t.Cleanup(func() {
		finishPerfMatrix(t)
	})
	forEachCell(t, axes, func(t *testing.T, outerCell map[string]string) {
		muxBackend := WithMuxAxis(b.Cell(outerCell))
		forEachCell(t, muxBackend.Axes(), func(t *testing.T, muxCell map[string]string) {
			pinned := muxBackend.Cell(muxCell)
			RunCoreScenarios(t, pinned)
			RunTopologyScenarios(t, pinned)
			RunReliabilityScenarios(t, pinned)
			RunObservabilityScenarios(t, pinned)
			RunPerformanceScenarios(t, pinned)
		})
	})
}

// RunCoreScenarios runs every single-flow e2e scenario against b as a
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
func RunCoreScenarios(t *testing.T, b Backend) {
	t.Helper()
	runScenarioCases(t, b, coreCases())
}

// coreCases is the metadata-only registry of core scenarios. Split
// from RunCoreScenarios so scenarios_test.go can pin the registry
// shape without standing up a topology.
func coreCases() []scenarioCase {
	return []scenarioCase{
		{name: "Echo_PortForward", scope: AnyBackend, run: ScenarioEcho_PortForward},
		{name: "Echo_PortForward_CIDRAllow", scope: AnyBackend, run: ScenarioEcho_PortForward_CIDRAllow},
		{name: "Echo_SOCKS5", scope: AnyBackend, run: ScenarioEcho_SOCKS5},
		{name: "Echo_Connect", scope: AnyBackend, run: ScenarioEcho_Connect},
		{name: "SSH_ProxyCommand", scope: AnyBackend, run: ScenarioSSH_ProxyCommand},
		{name: "SOCKS5_DistinctTargets", scope: AnyBackend, run: ScenarioSOCKS5_DistinctTargets},
		{name: "Ordering_PortForward", scope: AnyBackend, run: ScenarioOrdering_PortForward},
		{name: "Bidirectional_PortForward", scope: AnyBackend, run: ScenarioBidirectional_PortForward},
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

	want := []byte("hello aztunnel e2e\n")
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

	want := []byte("hello aztunnel socks5 e2e\n")
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

// ScenarioEcho_PortForward_CIDRAllow: same as ScenarioEcho_PortForward
// but the listener's --allow value is CIDR-form (127.0.0.0/8:*)
// rather than an exact host:port. Subsumes the legacy
// TestAllowlistAllow.
func ScenarioEcho_PortForward_CIDRAllow(t *testing.T, b Backend) {
	t.Helper()
	AssertNoLeaks(t)
	echo := StartPlainEcho(t)
	tun := b.Setup(t, SetupOptions{
		NumListeners:   1,
		SenderMode:     ModePortForward,
		Target:         echo.Addr(),
		AllowedTargets: []string{"127.0.0.0/8:*"},
	})

	conn := dialWithRetry(t, tun.SenderAddr, 5*time.Second)
	defer conn.Close() //nolint:errcheck // best-effort cleanup

	want := []byte("cidr-allow\n")
	writeAll(t, conn, want)
	got := readN(t, conn, len(want), 10*time.Second)
	if !bytes.Equal(got, want) {
		t.Fatalf("CIDR-allow echo mismatch\n got=%q\nwant=%q", got, want)
	}
}

// ScenarioEcho_Connect: ModeConnect sender opens; 1 KB echo; close.
// Mirror of ScenarioEcho_PortForward for connect mode. Each backend's
// ConnectClient bridges stdin/stdout to the same shape: write payload
// → read echo. Logs are not asserted here — that's covered by
// connect-mode failure scenarios.
func ScenarioEcho_Connect(t *testing.T, b Backend) {
	t.Helper()
	AssertNoLeaks(t)
	echo := StartPlainEcho(t)
	tun := b.Setup(t, SetupOptions{
		NumListeners:   1,
		SenderMode:     ModeConnect,
		AllowedTargets: []string{echo.Addr()},
	})

	cc := tun.OpenConnect(t, echo.Addr())
	defer cc.Close() //nolint:errcheck // best-effort cleanup

	payload := make([]byte, 1024)
	for i := range payload {
		payload[i] = byte(i % 251)
	}
	if _, err := cc.Write(payload); err != nil {
		t.Fatalf("connect write: %v", err)
	}
	got := make([]byte, len(payload))
	if _, err := io.ReadFull(cc, got); err != nil {
		t.Fatalf("connect read: %v\n--- logs ---\n%s", err, cc.Logs())
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("connect echo mismatch: first 16 want %x got %x", payload[:16], got[:16])
	}
}

// ScenarioSSH_ProxyCommand: drive a real SSH session through the
// tunnel using ssh's ProxyCommand to launch `aztunnel relay-sender
// connect <target>`. Skipped on backends whose Tunnel doesn't
// populate SSHProxyCommand (mock — its in-process control channel
// is not reachable from a freshly-launched aztunnel subprocess).
//
// The Tunnel-side builder ensures the ssh-spawned subprocess uses
// the SAME hyco coordinates as the listener Setup created; the
// scenario layer cannot synthesize that without backend-specific
// state.
func ScenarioSSH_ProxyCommand(t *testing.T, b Backend) {
	t.Helper()
	requireExternalTool(t, "ssh")
	AssertNoLeaks(t)

	sshd := startSSHServer(t)
	tun := b.Setup(t, SetupOptions{
		NumListeners:   1,
		SenderMode:     ModeConnect,
		AllowedTargets: []string{sshd.Addr()},
	})

	proxyArgs, proxyExtraEnv, ok := tun.SSHProxyCommand("%h:%p")
	if !ok {
		t.Skipf("SSH ProxyCommand: %s backend's Tunnel does not expose SSHProxyCommand", b.Name())
	}
	proxyCmdStr := joinShellSafe(proxyArgs)

	host, port, err := net.SplitHostPort(sshd.Addr())
	if err != nil {
		t.Fatalf("split sshd addr %q: %v", sshd.Addr(), err)
	}
	args := []string{
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "ProxyCommand=" + proxyCmdStr,
		"-o", "BatchMode=yes",
		"-p", port,
		"-i", sshd.HostKeyPath(),
		fmt.Sprintf("e2etest@%s", host),
		"echo", "tunnel-works",
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "ssh", args...) //nolint:gosec // test-controlled args
	cmd.Env = append(os.Environ(), proxyExtraEnv...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("ssh failed: %v\noutput: %s", err, out)
	}
	if !bytes.Contains(out, []byte("tunnel-works")) {
		t.Fatalf("unexpected ssh output: %s", out)
	}
}

// ScenarioSOCKS5_DistinctTargets: open 8 concurrent SOCKS5
// connections each to a different echo target through the same
// SOCKS5 sender, assert all 8 echo correctly. Subsumes the legacy
// TestConcurrentDistinctTargets (which used 50 targets — 8 is
// enough to cover the parallel-distinct path on both backends and
// keeps Azure walltime bounded).
func ScenarioSOCKS5_DistinctTargets(t *testing.T, b Backend) {
	t.Helper()
	AssertNoLeaks(t)

	const numTargets = 8
	echos := make([]*PlainEcho, numTargets)
	allowList := make([]string, numTargets)
	for i := 0; i < numTargets; i++ {
		echos[i] = StartPlainEcho(t)
		allowList[i] = echos[i].Addr()
	}

	tun := b.Setup(t, SetupOptions{
		NumListeners:   1,
		SenderMode:     ModeSOCKS5,
		AllowedTargets: allowList,
	})

	var wg sync.WaitGroup
	errs := make(chan error, numTargets)
	for i := 0; i < numTargets; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			conn, err := dialSOCKS5WithRetry(tun.SenderAddr, echos[id].Addr(), 15*time.Second)
			if err != nil {
				errs <- fmt.Errorf("target %d socks5: %w", id, err)
				return
			}
			defer conn.Close() //nolint:errcheck // best-effort cleanup
			_ = conn.SetDeadline(time.Now().Add(30 * time.Second))
			msg := []byte(fmt.Sprintf("target-%d\n", id))
			if _, err := conn.Write(msg); err != nil {
				errs <- fmt.Errorf("target %d write: %w", id, err)
				return
			}
			buf := make([]byte, len(msg))
			if _, err := io.ReadFull(conn, buf); err != nil {
				errs <- fmt.Errorf("target %d read: %w", id, err)
				return
			}
			if !bytes.Equal(buf, msg) {
				errs <- fmt.Errorf("target %d: got %q want %q", id, buf, msg)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

// joinShellSafe joins argv with spaces, single-quoting any element
// that contains shell-significant characters so ssh's ProxyCommand
// (parsed by /bin/sh -c) preserves the elements as-is. The ssh
// ProxyCommand option is the only callsite; production code uses
// os/exec which does not need this.
func joinShellSafe(argv []string) string {
	var sb strings.Builder
	for i, a := range argv {
		if i > 0 {
			sb.WriteByte(' ')
		}
		// %h / %p are sshd format substitutions and stay
		// unquoted; anything else with a metacharacter gets
		// single-quoted.
		if strings.ContainsAny(a, " \t\"\\$`") || strings.Contains(a, "'") {
			sb.WriteByte('\'')
			sb.WriteString(strings.ReplaceAll(a, "'", `'\''`))
			sb.WriteByte('\'')
		} else {
			sb.WriteString(a)
		}
	}
	return sb.String()
}
