package relayparity

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// RunReliabilitySuite runs the negative-path and back-pressure parity
// scenarios against b. Each runs as a sub-test of the caller's t and
// must pass against both the in-process mock backend and the real
// Azure backend — this is the "behavior is the same shape on both
// sides of the relay" parity gate.
//
// ScenarioHalfClose_RequestResponse is registered here for shape
// (i.e. the test name shows up in output) but t.Skip()s until the
// bridge propagates half-close. The skip exists so the suite stays
// green while that capability is missing.
func RunReliabilitySuite(t *testing.T, b Backend) {
	t.Helper()
	scenarios := []struct {
		name string
		run  func(*testing.T, Backend)
	}{
		{"ErrorPropagation_TargetRefused", ScenarioErrorPropagation_TargetRefused},
		{"ErrorPropagation_TargetUnreachable", ScenarioErrorPropagation_TargetUnreachable},
		{"ErrorPropagation_TargetHangs", ScenarioErrorPropagation_TargetHangs},
		{"SlowConsumer_BackPressure", ScenarioSlowConsumer_BackPressure},
		{"HalfClose_RequestResponse", ScenarioHalfClose_RequestResponse},
	}
	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			sc.run(t, b)
		})
	}
}

// SlowConsumerMemBound is the heap-memory ceiling the slow-consumer
// back-pressure scenario asserts against, in bytes. The SAME value is
// applied to mock and Azure backends — that equality is what makes
// the parity claim meaningful: if either backend allocates unbounded
// Go heap when a downstream peer stalls, this bound flags it.
//
// 64 MiB is loose enough to absorb baseline Go runtime overhead plus
// io.Copy 32 KiB buffers across the in-process bridge hops in the
// worst (mock) case where every component lives in the test process.
// A well-behaved bridge keeps additional heap-allocs in the
// sub-megabyte range; kernel TCP buffers can absorb tens of MiB of
// in-flight data under autotune without affecting HeapAlloc.
const SlowConsumerMemBound = 64 * 1024 * 1024

// slowConsumerTestDuration bounds how long the back-pressure scenario
// observes the writer/drain interaction. Long enough that the kernel
// TCP buffer pipeline saturates and the writer settles into steady-
// state behavior, short enough to keep total test runtime tolerable.
const slowConsumerTestDuration = 8 * time.Second

// ScenarioErrorPropagation_TargetRefused asserts that a SOCKS5 client
// dialing a refused target sees REP=0x05 (connection refused), and a
// port-forward client sees a bounded failure within the configured
// deadline. The target is a 127.0.0.1 port the OS will RST on connect
// because nothing is listening — produced by binding then immediately
// closing a listener.
//
// SOCKS5 reply parity: aztunnel propagates the
// listener-side connect-error classification through
// ConnectResponse.Code so the SOCKS5 sender returns the matching REP
// byte instead of the historical "always RepHostUnreachable" default.
func ScenarioErrorPropagation_TargetRefused(t *testing.T, b Backend) {
	t.Helper()
	AssertNoLeaks(t)

	target := refusedAddr(t)
	tun := b.Setup(t, SetupOptions{
		NumListeners:   1,
		SenderMode:     ModeSOCKS5,
		AllowedTargets: []string{target},
		ConnectTimeout: 5 * time.Second,
	})

	_, err := DialSOCKS5(tun.SenderAddr, target, 15*time.Second)
	if err == nil {
		t.Fatalf("expected SOCKS5 dial to refused target to fail")
	}
	var sErr *SOCKS5Error
	if !errors.As(err, &sErr) {
		t.Fatalf("expected SOCKS5Error from refused dial, got %T: %v", err, err)
	}
	if sErr.Rep != 0x05 {
		t.Errorf("SOCKS5 REP for refused target = %#x (%s), want 0x05 (connection refused)",
			sErr.Rep, sErr.Error())
	}

	// Port-forward leg: dial succeeds locally (sender bind always
	// accepts), but read/write must error within a bounded deadline
	// once the relay → listener handshake propagates the failure
	// back. We assert non-nil error within the budget; the exact
	// error class differs across backends (EOF on mock vs. RST on
	// some Azure paths), which is why the scenario does not assert
	// on exact error type.
	target2 := refusedAddr(t)
	tunPF := b.Setup(t, SetupOptions{
		NumListeners:   1,
		SenderMode:     ModePortForward,
		Target:         target2,
		AllowedTargets: []string{target2},
		ConnectTimeout: 5 * time.Second,
	})
	if err := expectPortForwardFails(tunPF.SenderAddr, 15*time.Second); err != nil {
		t.Errorf("port-forward to refused target: %v", err)
	}
}

// ScenarioErrorPropagation_TargetUnreachable asserts that a SOCKS5
// client dialing a black-holed address sees REP=0x03 or 0x04
// (network/host unreachable) within the configured deadline, and a
// port-forward client also fails inside the budget. The target is
// 192.0.2.1 from RFC 5737 TEST-NET-1, guaranteed not routable on
// production networks.
//
// On Linux the kernel typically reports ETIMEDOUT for a SYN that
// never receives SYN-ACK, classified as CodeTimeout → mapped to
// RepHostUnreachable (0x04). Some networks instead emit ICMP
// host/network unreachable, surfacing as 0x03 or 0x04 directly. The
// assertion accepts either, since both are valid responses for an
// unreachable target.
func ScenarioErrorPropagation_TargetUnreachable(t *testing.T, b Backend) {
	t.Helper()
	AssertNoLeaks(t)

	const target = "192.0.2.1:9"
	tun := b.Setup(t, SetupOptions{
		NumListeners:   1,
		SenderMode:     ModeSOCKS5,
		AllowedTargets: []string{target},
		ConnectTimeout: 4 * time.Second,
	})

	start := time.Now()
	_, err := DialSOCKS5(tun.SenderAddr, target, 30*time.Second)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatalf("expected SOCKS5 dial to unreachable target to fail")
	}
	var sErr *SOCKS5Error
	if !errors.As(err, &sErr) {
		t.Fatalf("expected SOCKS5Error from unreachable dial, got %T: %v", err, err)
	}
	if sErr.Rep != 0x03 && sErr.Rep != 0x04 {
		t.Errorf("SOCKS5 REP for unreachable target = %#x, want 0x03 (net unreachable) or 0x04 (host unreachable)",
			sErr.Rep)
	}
	if elapsed > 20*time.Second {
		t.Errorf("SOCKS5 dial to unreachable target took %v, want <= 20s", elapsed)
	}

	// Port-forward: assert error within bounded deadline.
	tunPF := b.Setup(t, SetupOptions{
		NumListeners:   1,
		SenderMode:     ModePortForward,
		Target:         target,
		AllowedTargets: []string{target},
		ConnectTimeout: 4 * time.Second,
	})
	if err := expectPortForwardFails(tunPF.SenderAddr, 20*time.Second); err != nil {
		t.Errorf("port-forward to unreachable target: %v", err)
	}
}

// ScenarioErrorPropagation_TargetHangs verifies the bounded-deadline
// path for targets that accept TCP but never read or write. The
// port-forward sender bind accepts the client conn, the listener
// successfully dials the target (TCP three-way handshake completes),
// and the bridge is established; the client then writes a small probe
// and tries to read, and times out on its own read deadline.
// AssertNoLeaks covers the bridge-cleanup half: the listener's hung
// target.Read must be unblocked when the bridge cancels.
func ScenarioErrorPropagation_TargetHangs(t *testing.T, b Backend) {
	t.Helper()
	AssertNoLeaks(t)

	hang := startHangTarget(t)
	tun := b.Setup(t, SetupOptions{
		NumListeners:   1,
		SenderMode:     ModePortForward,
		Target:         hang.Addr(),
		AllowedTargets: []string{hang.Addr()},
		ConnectTimeout: 5 * time.Second,
	})

	conn := dialWithRetry(t, tun.SenderAddr, 10*time.Second)
	defer conn.Close() //nolint:errcheck // best-effort cleanup

	// Try to read; the target never writes, so we expect a deadline
	// error within the budget. A small write first to make sure the
	// bridge is fully established before we measure read behavior.
	if err := writeFull(conn, []byte("ping\n")); err != nil {
		t.Fatalf("write to hang target: %v", err)
	}

	const readBudget = 3 * time.Second
	_ = conn.SetReadDeadline(time.Now().Add(readBudget))
	start := time.Now()
	buf := make([]byte, 8)
	n, err := conn.Read(buf)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatalf("expected read from hung target to fail; got n=%d", n)
	}
	var ne net.Error
	if !errors.As(err, &ne) || !ne.Timeout() {
		t.Logf("read returned non-timeout error %v (still a valid bounded failure)", err)
	}
	if elapsed > readBudget+2*time.Second {
		t.Errorf("read returned after %v, want close to %v", elapsed, readBudget)
	}
}

// ScenarioSlowConsumer_BackPressure verifies the tunnel applies
// back-pressure end-to-end when the downstream consumer drains
// slowly. The target reads at ~1 KiB/s; the client attempts to push
// 100 MiB through the tunnel as fast as it can.
//
// What the test asserts:
//
//   - Bytes accepted by the tunnel (writtenMid) stays under
//     SlowConsumerMemBound. This is the primary parity gate: it works
//     identically on the mock backend (sender/listener in-process) and
//     on the Azure backend (sender/listener as subprocesses). If
//     back-pressure fails anywhere along the chain — in the sender,
//     in the relay, or in the listener — the writer will push past
//     the bound; otherwise TCP back-pressure on the local proxy
//     socket keeps writtenMid bounded by accumulated kernel + Go
//     buffer capacity, which is well below 64 MiB in practice.
//   - Tunnel heap stays under SlowConsumerMemBound (sampled mid-flow,
//     post-GC). This is a stronger signal on the mock backend (where
//     all bridge goroutines live in the test process) and is mostly
//     informational on the Azure backend (subprocess heaps are not
//     visible from runtime.ReadMemStats here). A regression that
//     buffers unbounded data in Go memory on the in-process mock
//     would balloon HeapAlloc past 64 MiB. Kernel TCP buffers can
//     absorb tens of MiB of in-flight data under autotune — that's
//     not a regression; we only care that the bridge itself does not
//     allocate unbounded Go heap.
//   - The flow eventually completes — the bridge tears down cleanly
//     when the client closes, even with hundreds of MiB of unwritten
//     data on the source side. AssertNoLeaks confirms no goroutine
//     leak; the writer-goroutine-exit budget catches a deadlocked
//     bridge.
//   - At least some bytes flowed (>= 1 KiB) — sanity that the test
//     isn't reporting a no-op as success.
//
// SlowConsumerMemBound is the SAME constant for mock and Azure;
// the comparability of the two backends under the same bound is
// the parity claim.
func ScenarioSlowConsumer_BackPressure(t *testing.T, b Backend) {
	t.Helper()
	AssertNoLeaks(t)

	drain := startSlowDrainTarget(t, 1024) // ~1 KiB/s
	tun := b.Setup(t, SetupOptions{
		NumListeners:   1,
		SenderMode:     ModePortForward,
		Target:         drain.Addr(),
		AllowedTargets: []string{drain.Addr()},
	})

	// Closing the slow-target accepted conns MUST happen before the
	// backend's cleanup tears the listener goroutines down: the
	// listener-side bridge can be stuck inside tcp.Write to the slow
	// target (TCP send buffer full at ~64 KiB, target draining at
	// 1 KiB/s), and the current bridge.Bridge waits on both
	// directions before returning. Cleanups run LIFO, so registering
	// drain.closeAcceptedConns AFTER b.Setup makes it run BEFORE the
	// backend cancel+wg.Wait, unblocking the stuck write.
	t.Cleanup(drain.closeAcceptedConns)

	conn := dialWithRetry(t, tun.SenderAddr, 10*time.Second)
	defer conn.Close() //nolint:errcheck // best-effort cleanup

	const totalToWrite = 100 * 1024 * 1024 // 100 MiB (we will NOT push this much)

	writerCtx, writerCancel := context.WithCancel(context.Background())
	defer writerCancel()

	var bytesWritten int64
	writerDone := make(chan struct{})
	writerErr := make(chan error, 1)
	go func() {
		defer close(writerDone)
		// Deterministic filler so the writer doesn't allocate large
		// random buffers each iteration; keeps writer-side heap
		// negligible so HeapAlloc reflects tunnel buffering only.
		buf := make([]byte, 32*1024)
		for i := range buf {
			buf[i] = byte(i)
		}
		var written int64
		for written < totalToWrite {
			if writerCtx.Err() != nil {
				writerErr <- nil
				return
			}
			_ = conn.SetWriteDeadline(time.Now().Add(slowConsumerTestDuration * 2))
			n, err := conn.Write(buf)
			if n > 0 {
				atomic.AddInt64(&bytesWritten, int64(n))
				written += int64(n)
			}
			if err != nil {
				writerErr <- err
				return
			}
		}
		writerErr <- nil
	}()

	var peakHeap uint64
	sampleHeap := func() uint64 {
		runtime.GC()
		var ms runtime.MemStats
		runtime.ReadMemStats(&ms)
		return ms.HeapAlloc
	}

	baseHeap := sampleHeap()
	peakHeap = baseHeap

	sampler := time.NewTicker(250 * time.Millisecond)
	defer sampler.Stop()
	deadline := time.After(slowConsumerTestDuration)
sampling:
	for {
		select {
		case <-sampler.C:
			h := sampleHeap()
			if h > peakHeap {
				peakHeap = h
			}
		case <-deadline:
			break sampling
		}
	}

	writtenMid := atomic.LoadInt64(&bytesWritten)

	// Tear down: cancel the writer goroutine, close the conn, wait
	// for the writer to exit. "Flow eventually completes" in the
	// sense that the bridge does not deadlock even when the writer
	// has hundreds of MiB still queued.
	writerCancel()
	_ = conn.Close()
	select {
	case <-writerDone:
	case <-time.After(15 * time.Second):
		t.Fatalf("writer goroutine did not exit within 15s after Close; likely bridge deadlock")
	}
	<-writerErr // drain; we don't fail on writer errors (Close races are expected)

	t.Logf("slow-consumer: baseHeap=%d peakHeap=%d delta=%d writtenMid=%d (drained ~%d at %d B/s)",
		baseHeap, peakHeap, int64(peakHeap)-int64(baseHeap), writtenMid,
		int64(slowConsumerTestDuration/time.Second)*1024, 1024)

	if peakHeap > SlowConsumerMemBound {
		t.Errorf("HeapAlloc peak=%d exceeded SlowConsumerMemBound=%d during slow-consumer flow",
			peakHeap, SlowConsumerMemBound)
	}
	// Parity-gated throughput bound: this assertion works identically
	// on the mock backend (in-process bridge) and the Azure backend
	// (subprocess sender/listener). If back-pressure is broken anywhere
	// along the chain, the writer would push the full 100 MiB through
	// in seconds; with back-pressure, writtenMid stays bounded by the
	// accumulated TCP + Go buffer capacity, well below this bound.
	if writtenMid > int64(SlowConsumerMemBound) {
		t.Errorf("writer pushed %d bytes during %v window; exceeded SlowConsumerMemBound=%d, indicating end-to-end back-pressure failure",
			writtenMid, slowConsumerTestDuration, SlowConsumerMemBound)
	}
	if writtenMid < 1024 {
		t.Errorf("writer pushed %d bytes during %v window; expected >= 1 KiB",
			writtenMid, slowConsumerTestDuration)
	}
}

// ScenarioHalfClose_RequestResponse is the acceptance-contract test
// for half-close propagation. The current bridge tears down both
// directions whenever either side returns EOF (see internal/relay/
// bridge.go), so a client that does CloseWrite to signal "I'm done
// writing, please send the response" loses its response stream.
//
// The body is fully written; only the t.Skip itself gates this
// scenario — when the bridge supports half-close, deleting the skip
// is sufficient to enable it.
func ScenarioHalfClose_RequestResponse(t *testing.T, b Backend) {
	t.Helper()
	t.Skip("current bridge tears down both directions on EOF instead of preserving the response stream after client CloseWrite")

	AssertNoLeaks(t)

	const (
		request  = "REQUEST\n"
		response = "RESPONSE\n"
	)
	target := startRequestResponseTarget(t, request, response)
	tun := b.Setup(t, SetupOptions{
		NumListeners:   1,
		SenderMode:     ModePortForward,
		Target:         target.Addr(),
		AllowedTargets: []string{target.Addr()},
	})

	conn := dialWithRetry(t, tun.SenderAddr, 10*time.Second)
	defer conn.Close() //nolint:errcheck // best-effort cleanup
	_ = conn.SetDeadline(time.Now().Add(15 * time.Second))

	if err := writeFull(conn, []byte(request)); err != nil {
		t.Fatalf("write request: %v", err)
	}
	cw, ok := conn.(interface{ CloseWrite() error })
	if !ok {
		t.Fatalf("client conn %T does not support CloseWrite", conn)
	}
	if err := cw.CloseWrite(); err != nil {
		t.Fatalf("CloseWrite: %v", err)
	}

	got, err := io.ReadAll(conn)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	if string(got) != response {
		t.Fatalf("response mismatch: got %q want %q", got, response)
	}
	// Half-close contract: the request payload must have reached the
	// target in full. Without this check, a bridge regression that
	// drops or truncates the client→server stream but still propagates
	// EOF would let the response come back and silently pass the test.
	received, mismatch := target.Received()
	if mismatch != nil {
		t.Fatalf("target reported request delivery failure: %v (received %q)", mismatch, received)
	}
	if string(received) != request {
		t.Fatalf("request payload not delivered intact: got %q want %q", received, request)
	}
}

// --- helpers --------------------------------------------------------

// refusedAddr returns a 127.0.0.1:<port> the OS will respond to with
// ECONNREFUSED. It does this by binding a fresh listener to :0 and
// immediately closing it; the same port is unlikely (but not
// impossible) to be reassigned in the moments before the test dials.
// To defend against the rare race, the helper retries with a fresh
// closed-listener address up to a small bound.
func refusedAddr(t *testing.T) string {
	t.Helper()
	const maxAttempts = 8
	for attempt := 0; attempt < maxAttempts; attempt++ {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("listen: %v", err)
		}
		addr := ln.Addr().String()
		if err := ln.Close(); err != nil {
			t.Fatalf("close listener: %v", err)
		}
		// Probe: a fresh local connect should fail with ECONNREFUSED.
		// If it succeeds, the port has been reassigned to another
		// process between Close and our probe — extremely rare but
		// possible. Retry with a new address.
		probe, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err != nil {
			return addr
		}
		_ = probe.Close()
	}
	t.Fatalf("refusedAddr: could not obtain a closed port after %d attempts", maxAttempts)
	return ""
}

// hangTarget is a TCP server that accepts connections but never reads
// or writes; accepted conns are held open until cleanup. Used by the
// TargetHangs scenario to provoke the "tunnel established, peer
// silent" condition.
type hangTarget struct {
	ln       net.Listener
	mu       sync.Mutex
	accepted []net.Conn
	done     chan struct{}
}

func startHangTarget(t *testing.T) *hangTarget {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("hang target listen: %v", err)
	}
	ht := &hangTarget{ln: ln, done: make(chan struct{})}
	go ht.serve()
	t.Cleanup(ht.stop)
	return ht
}

func (h *hangTarget) Addr() string { return h.ln.Addr().String() }

func (h *hangTarget) serve() {
	defer close(h.done)
	for {
		c, err := h.ln.Accept()
		if err != nil {
			return
		}
		h.mu.Lock()
		h.accepted = append(h.accepted, c)
		h.mu.Unlock()
		// Do NOT read or write — the whole point is silence. The
		// conn is closed during stop().
	}
}

func (h *hangTarget) stop() {
	_ = h.ln.Close()
	<-h.done
	h.mu.Lock()
	conns := h.accepted
	h.accepted = nil
	h.mu.Unlock()
	for _, c := range conns {
		_ = c.Close()
	}
}

// slowDrainTarget is a TCP server that, for each accepted connection,
// reads bytes at a fixed rate and discards them. Used by the slow-
// consumer back-pressure scenario.
type slowDrainTarget struct {
	ln      net.Listener
	rateBps int
	mu      sync.Mutex
	accepts []net.Conn
	wg      sync.WaitGroup
	done    chan struct{}
}

func startSlowDrainTarget(t *testing.T, rateBps int) *slowDrainTarget {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("slow-drain listen: %v", err)
	}
	s := &slowDrainTarget{ln: ln, rateBps: rateBps, done: make(chan struct{})}
	go s.serve()
	t.Cleanup(s.stop)
	return s
}

func (s *slowDrainTarget) Addr() string { return s.ln.Addr().String() }

func (s *slowDrainTarget) serve() {
	defer close(s.done)
	for {
		c, err := s.ln.Accept()
		if err != nil {
			return
		}
		s.mu.Lock()
		s.accepts = append(s.accepts, c)
		s.mu.Unlock()
		s.wg.Add(1)
		go func(c net.Conn) {
			defer s.wg.Done()
			defer c.Close() //nolint:errcheck // best-effort
			s.drain(c)
		}(c)
	}
}

// drain reads at ~rateBps by ticking every 50 ms and consuming the
// matching slice of the rate budget on each tick. Read blocks if
// fewer bytes are available — that's fine, it just means the drain
// went slower than rateBps when the upstream pipe was empty, which
// matches the "slow consumer" model.
func (s *slowDrainTarget) drain(c net.Conn) {
	const tickMs = 50
	chunk := s.rateBps * tickMs / 1000
	if chunk < 1 {
		chunk = 1
	}
	buf := make([]byte, chunk)
	ticker := time.NewTicker(tickMs * time.Millisecond)
	defer ticker.Stop()
	for range ticker.C {
		_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
		_, err := io.ReadFull(c, buf)
		if err != nil {
			return
		}
	}
}

func (s *slowDrainTarget) stop() {
	_ = s.ln.Close()
	<-s.done
	s.closeAcceptedConns()
	s.wg.Wait()
}

// closeAcceptedConns closes every accepted target connection without
// touching the listener or waiting for drain goroutines to exit. The
// slow-consumer scenario calls this from a t.Cleanup registered AFTER
// the backend's Setup so the listener-side stuck tcp.Write to the slow
// target unblocks before the backend tears itself down (see comment in
// ScenarioSlowConsumer_BackPressure). Safe to call concurrently with
// stop() — both grab the same mutex and only close each conn once.
func (s *slowDrainTarget) closeAcceptedConns() {
	s.mu.Lock()
	conns := s.accepts
	s.accepts = nil
	s.mu.Unlock()
	for _, c := range conns {
		_ = c.Close()
	}
}

// requestResponseTarget is a TCP server that, on each accepted conn,
// reads until EOF, compares the received bytes against an expected
// request, and only writes its response when the request matches.
// Used by the half-close scenario (currently skipped); on a bridge
// that propagates EOF in one direction only, this exercises the
// full request-then-response contract — both directions must
// deliver intact payloads, not just an EOF + reply.
type requestResponseTarget struct {
	ln       net.Listener
	request  string
	response string
	wg       sync.WaitGroup
	done     chan struct{}

	mu          sync.Mutex
	received    []byte
	mismatchErr error
}

func startRequestResponseTarget(t *testing.T, request, response string) *requestResponseTarget {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("req-resp listen: %v", err)
	}
	rr := &requestResponseTarget{
		ln:       ln,
		request:  request,
		response: response,
		done:     make(chan struct{}),
	}
	go rr.serve()
	t.Cleanup(rr.stop)
	return rr
}

func (rr *requestResponseTarget) Addr() string { return rr.ln.Addr().String() }

// Received returns the last bytes the target read from a client and
// any mismatch error recorded when the bytes diverged from the
// expected request. Intended for diagnostic assertions when the
// half-close scenario runs.
func (rr *requestResponseTarget) Received() ([]byte, error) {
	rr.mu.Lock()
	defer rr.mu.Unlock()
	out := make([]byte, len(rr.received))
	copy(out, rr.received)
	return out, rr.mismatchErr
}

func (rr *requestResponseTarget) serve() {
	defer close(rr.done)
	for {
		c, err := rr.ln.Accept()
		if err != nil {
			return
		}
		rr.wg.Add(1)
		go func(c net.Conn) {
			defer rr.wg.Done()
			defer c.Close() //nolint:errcheck // best-effort
			_ = c.SetDeadline(time.Now().Add(30 * time.Second))
			got, err := io.ReadAll(c)
			rr.mu.Lock()
			rr.received = got
			if err != nil {
				rr.mismatchErr = fmt.Errorf("read: %w", err)
				rr.mu.Unlock()
				return
			}
			if string(got) != rr.request {
				rr.mismatchErr = fmt.Errorf("request mismatch: got %q want %q", got, rr.request)
				rr.mu.Unlock()
				// Refuse to write the response: the half-close contract
				// requires that the full request payload reaches the
				// server, not just an EOF. Closing without a response
				// makes the client-side ReadAll return empty bytes and
				// the scenario fail on the response check.
				return
			}
			rr.mu.Unlock()
			_, _ = c.Write([]byte(rr.response))
		}(c)
	}
}

func (rr *requestResponseTarget) stop() {
	_ = rr.ln.Close()
	<-rr.done
	rr.wg.Wait()
}

// expectPortForwardFails dials addr (a port-forward sender bind),
// attempts a tiny write+read, and asserts the operation does not
// hang past timeout and produces an error. It does not assert on the
// exact error type — that's backend-specific (mock surfaces EOF;
// Azure subprocesses sometimes surface RST or short read first).
func expectPortForwardFails(senderAddr string, timeout time.Duration) error {
	conn, err := net.DialTimeout("tcp", senderAddr, timeout)
	if err != nil {
		// Local sender bind dial should always succeed; if it doesn't
		// the test setup is broken, not the assertion.
		return fmt.Errorf("dial sender: %w", err)
	}
	defer conn.Close() //nolint:errcheck // best-effort
	deadline := time.Now().Add(timeout)
	_ = conn.SetDeadline(deadline)

	// A small write may succeed and be buffered locally; the failure
	// surfaces on read.
	_, _ = conn.Write([]byte("probe\n"))
	buf := make([]byte, 32)
	n, err := conn.Read(buf)
	if err == nil && n > 0 {
		return fmt.Errorf("expected error/EOF, got %d bytes echoed back", n)
	}
	if err == nil {
		return fmt.Errorf("expected error/EOF, got nil error and n=0")
	}
	return nil
}
