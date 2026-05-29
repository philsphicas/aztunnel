package scenarios

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// RunReliabilityScenarios runs the negative-path and back-pressure
// e2e scenarios against b. Each runs as a sub-test of the caller's t and
// must pass against both the in-process mock backend and the real
// Azure backend — this is the "behavior is the same shape on both
// sides of the relay" parity gate.
//
// ScenarioHalfClose_RequestResponse is registered here for shape
// (i.e. the test name shows up in output) but t.Skip()s until the
// bridge propagates half-close. The skip exists so the suite stays
// green while that capability is missing.
func RunReliabilityScenarios(t *testing.T, b Backend) {
	t.Helper()
	runScenarioCases(t, b, reliabilityCases())
}

// reliabilityCases is the metadata-only registry of reliability
// scenarios.
//
// Most entries are AnyBackend — the parity guarantee is what makes
// the suite meaningful, and the mock backend implements the same
// log shapes as Azure Relay for negative paths.
//
// AzureOnly entries:
//   - AuthRejection_BadHyco: the mock's hyco model is dynamic, so
//     the "nonexistent hyco" rejection shape only exists on the
//     real namespace.
//   - LongLivedConnection: validates Azure Relay's ~120s
//     idle-connection timeout (documented at internal/relay/bridge.go)
//     by holding a tunnel open across two keepalive intervals.
//     Mock relay has no idle timeout, so the assertion would be a
//     no-op on mock.
func reliabilityCases() []scenarioCase {
	return []scenarioCase{
		{name: "ErrorPropagation_TargetRefused", scope: AnyBackend, run: ScenarioErrorPropagation_TargetRefused},
		{name: "ErrorPropagation_TargetUnreachable", scope: AnyBackend, run: ScenarioErrorPropagation_TargetUnreachable},
		{name: "ErrorPropagation_TargetDNSFailure", scope: AnyBackend, run: ScenarioErrorPropagation_TargetDNSFailure},
		{name: "ErrorPropagation_TargetHangs", scope: AnyBackend, run: ScenarioErrorPropagation_TargetHangs},
		{name: "SlowConsumer_BackPressure", scope: AnyBackend, run: ScenarioSlowConsumer_BackPressure},
		{name: "HalfClose_RequestResponse", scope: AnyBackend, run: ScenarioHalfClose_RequestResponse},
		{name: "SenderRetriesUntilListenerReady", scope: AnyBackend, run: ScenarioSenderRetriesUntilListenerReady},
		{name: "Connect_NoListener_ErrorsCleanly", scope: AnyBackend, run: ScenarioConnect_NoListener_ErrorsCleanly},
		{name: "NoListener_RetriesUntilListenerAppears", scope: AnyBackend, run: ScenarioNoListener_RetriesUntilListenerAppears},
		{
			name:   "AuthRejection_BadHyco",
			scope:  AzureOnly,
			reason: "mock relay's hyco model is dynamic; no nonexistent-hyco shape to provoke",
			run:    ScenarioAuthRejection_BadHyco,
		},
		{name: "AuthRejection_BadListenerSAS", scope: AnyBackend, run: ScenarioAuthRejection_BadListenerSAS},
		{name: "AuthRejection_BadSenderSAS", scope: AnyBackend, run: ScenarioAuthRejection_BadSenderSAS},
		{
			name:   "AuthRejection_CrossClaim",
			scope:  AzureOnly,
			reason: "mock relay uses one shared SAS key for both directions; per-key Listen vs Send claim enforcement is Azure-only",
			run:    ScenarioAuthRejection_CrossClaim,
		},
		{name: "Allowlist_Reject", scope: AnyBackend, run: ScenarioAllowlist_Reject},
		{
			name:   "LongLivedConnection",
			scope:  AzureOnly,
			reason: "validates Azure Relay's ~120s idle-connection timeout via keepalive pings; mock relay has no idle timeout",
			run:    ScenarioLongLivedConnection,
		},
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

	// Metric assertion: the listener must classify the refused dial
	// into the `dial_failed` reason bucket. Subsumes the legacy
	// TestPortForwardClosedPort. Use the second tunnel's listener
	// (the port-forward leg above) because that's the leg that
	// actually drove a dial to a refused target.
	if got := waitForConnectionErrorReason(tunPF.Listeners[0], "dial_failed", 1, 15*time.Second); got < 1 {
		t.Errorf("listener metric aztunnel_connection_errors_total{reason=\"dial_failed\"} = %d, want >= 1", got)
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

	// Metric assertion: the listener must classify the unreachable
	// dial into either `dial_timeout` (timeout-classified by the
	// listener) or `dial_failed` (ICMP-host-unreachable surfaces as
	// a fast failure on some platforms). Subsumes the legacy
	// TestPortForwardUnreachable.
	deadline := time.Now().Add(15 * time.Second)
	var gotTimeout, gotFailed int64
	for time.Now().Before(deadline) {
		gotTimeout = tunPF.Listeners[0].ConnectionErrors("dial_timeout")
		gotFailed = tunPF.Listeners[0].ConnectionErrors("dial_failed")
		if gotTimeout+gotFailed >= 1 {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Errorf("listener metric aztunnel_connection_errors_total{reason=dial_timeout|dial_failed}: dial_timeout=%d, dial_failed=%d; want sum >= 1",
		gotTimeout, gotFailed)
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

// dnsFailureTarget is the host:port the DNS-failure scenario asks the
// listener to dial. The hostname uses the RFC 6761 reserved ".invalid"
// TLD with a trailing dot so resolver search-domain expansion cannot
// turn it into a real lookup. Every conformant resolver should answer
// NXDOMAIN, surfacing as *net.DNSError{IsNotFound:true} inside
// dialer.DialContext.
const dnsFailureTarget = "nonexistent.invalid.:80"

// ScenarioErrorPropagation_TargetDNSFailure asserts that when the
// listener fails to resolve the target hostname, the failure is
// classified as a DNS error in the listener metrics rather than as a
// generic dial failure or a network-layer timeout. This is the
// operator-visible signal that lets dashboards and alerts separate
// "DNS misconfigured" from "target unreachable on the network".
//
// The scenario uses SOCKS5 so the client receives a clean SOCKS5-reply
// failure (current sender maps unknown protocol codes to
// RepHostUnreachable, so the REP byte itself is not asserted; the
// metric reason label is the contract under test). The listener's
// connection_errors_total{reason="dns_not_found"} counter is polled
// via the Listener.ConnectionErrors accessor, which both backends
// implement — in-process Registry.Gather for the mock backend and
// /metrics scrape for the Azure backend.
func ScenarioErrorPropagation_TargetDNSFailure(t *testing.T, b Backend) {
	t.Helper()
	AssertNoLeaks(t)

	tun := b.Setup(t, SetupOptions{
		NumListeners:   1,
		SenderMode:     ModeSOCKS5,
		AllowedTargets: []string{dnsFailureTarget},
		ConnectTimeout: 5 * time.Second,
	})

	_, err := DialSOCKS5(tun.SenderAddr, dnsFailureTarget, 15*time.Second)
	if err == nil {
		t.Fatalf("expected SOCKS5 dial to DNS-NXDOMAIN target to fail")
	}
	var sErr *SOCKS5Error
	if !errors.As(err, &sErr) {
		// A SOCKS5-level reply is the expected outcome; anything else
		// (e.g. a dial-proxy IO error) would mean the listener never
		// got far enough to even attempt the dial, which would mask
		// the metric we're trying to assert.
		t.Fatalf("expected SOCKS5Error from DNS-failed dial, got %T: %v", err, err)
	}

	if tun.Listeners[0].ConnectionErrors == nil {
		t.Skip("backend does not expose ConnectionErrors on Listener")
	}
	deadline := time.Now().Add(15 * time.Second)
	var seen int64
	for time.Now().Before(deadline) {
		seen = tun.Listeners[0].ConnectionErrors("dns_not_found")
		if seen > 0 {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("listener connection_errors_total{reason=dns_not_found} did not increment within 15s after a SOCKS5 dial to %s (last value %d)",
		dnsFailureTarget, seen)
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

// waitForConnectionErrorReason polls the listener's
// ConnectionErrors closure until it reports want or more samples for
// reason. Returns the last observed value (zero on timeout). Used by
// reliability scenarios that need a deterministic wait for the
// listener-side connection-error classification to propagate.
func waitForConnectionErrorReason(lst *Listener, reason string, want int64, timeout time.Duration) int64 {
	deadline := time.Now().Add(timeout)
	var got int64
	for time.Now().Before(deadline) {
		got = lst.ConnectionErrors(reason)
		if got >= want {
			return got
		}
		time.Sleep(200 * time.Millisecond)
	}
	return got
}

// ScenarioSenderRetriesUntilListenerReady: open a connect-mode
// sender first (no listener yet), assert the sender logs a retry,
// then attach a listener and assert the sender connects and bridges
// data through stdio. Subsumes the legacy
// TestSenderRetriesUntilListenerReady.
func ScenarioSenderRetriesUntilListenerReady(t *testing.T, b Backend) {
	t.Helper()
	AssertNoLeaks(t)

	echo := StartPlainEcho(t)
	tun := b.Setup(t, SetupOptions{
		NumListeners:   0,
		SenderMode:     ModeConnect,
		AllowedTargets: []string{echo.Addr()},
	})

	cc := tun.OpenConnect(t, echo.Addr())
	defer cc.Close() //nolint:errcheck // best-effort cleanup

	// Wait for at least one retry hint in the sender's logs. The
	// production wording is "retrying" (Azure relay returns 404 with
	// no listener) or "relay dial failed" with retry hint on the
	// next attempt. Match either.
	if !waitForLogSubstrAny(cc.Logs, []string{"retrying", "relay dial failed"}, 15*time.Second) {
		t.Fatalf("sender never logged a retry within 15s\n--- logs ---\n%s", cc.Logs())
	}

	// Attach the listener.
	if tun.AddListener == nil {
		t.Fatalf("backend does not support hot-attach (Tunnel.AddListener is nil)")
	}
	_ = tun.AddListener(t)

	// Sender should connect; assert a successful echo round-trip
	// through stdio.
	payload := []byte("retry-then-bridge\n")
	if _, err := cc.Write(payload); err != nil {
		t.Fatalf("write after listener: %v\n--- logs ---\n%s", err, cc.Logs())
	}
	got := make([]byte, len(payload))
	if _, err := io.ReadFull(cc, got); err != nil {
		t.Fatalf("read after listener: %v\n--- logs ---\n%s", err, cc.Logs())
	}
	if string(got) != string(payload) {
		t.Fatalf("echo mismatch: got %q want %q", got, payload)
	}
}

// ScenarioConnect_NoListener_ErrorsCleanly: connect-mode sender with
// no listener attached and no plan to attach one. Sender must
// either exit with an error OR log a clean "relay dial failed"
// shape within a bounded budget, and the captured logs MUST NOT
// contain the raw SAS key (token-redaction contract).
//
// "retrying" alone is not accepted as proof of clean failure
// because a sender that retries forever without ever logging a
// dial-failure line would satisfy it; the harness requires a
// terminal dial-failure log so we know the sender's error path
// actually fired.
func ScenarioConnect_NoListener_ErrorsCleanly(t *testing.T, b Backend) {
	t.Helper()
	AssertNoLeaks(t)

	tun := b.Setup(t, SetupOptions{
		NumListeners: 0,
		SenderMode:   ModeConnect,
	})

	cc := tun.OpenConnect(t, "127.0.0.1:9999")
	defer cc.Close() //nolint:errcheck // best-effort cleanup

	// Wait for the sender to exit OR to log "relay dial failed".
	// Both backends emit that log line on each failed dial; on
	// Azure the sender exits non-zero after the 401 (non-
	// retryable); on mock it retries on 404 but every retry
	// emits the log first.
	waitCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	waitDone := make(chan error, 1)
	go func() { waitDone <- cc.Wait(waitCtx) }()

	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-waitDone:
			goto checkLogs
		default:
		}
		if strings.Contains(cc.Logs(), "relay dial failed") {
			goto checkLogs
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("connect with no listener never produced a 'relay dial failed' log or sender exit within 30s\n--- logs ---\n%s", cc.Logs())

checkLogs:
	logs := cc.Logs()
	stripped := strings.ReplaceAll(logs, "sb-hc-token=REDACTED", "")
	if strings.Contains(stripped, "sb-hc-token=") {
		t.Errorf("connect-no-listener logs contain a non-redacted sb-hc-token\n--- logs ---\n%s", logs)
	}
}

// ScenarioNoListener_RetriesUntilListenerAppears: SOCKS5 variant
// of the sender-retries scenario. The sender stays up across the
// no-listener / listener-attached transition; we drive one SOCKS5
// dial that fails (no listener), attach the listener, then drive a
// second dial that succeeds.
func ScenarioNoListener_RetriesUntilListenerAppears(t *testing.T, b Backend) {
	t.Helper()
	AssertNoLeaks(t)

	echo := StartPlainEcho(t)
	tun := b.Setup(t, SetupOptions{
		NumListeners:   0,
		NumSenders:     1,
		SenderMode:     ModeSOCKS5,
		AllowedTargets: []string{echo.Addr()},
	})

	// First dial: no listener → sender's relay dial fails. We
	// don't strictly require this dial to produce a specific
	// error; we just trigger it so the sender exercises its retry
	// path.
	_, _ = dialSOCKS5WithRetry(tun.SenderAddr, echo.Addr(), 3*time.Second)

	if tun.AddListener == nil {
		t.Fatalf("backend does not support hot-attach (Tunnel.AddListener is nil)")
	}
	_ = tun.AddListener(t)

	// Second dial: listener attached → SOCKS5 dial should succeed.
	conn, err := dialSOCKS5WithRetry(tun.SenderAddr, echo.Addr(), 30*time.Second)
	if err != nil {
		t.Fatalf("post-attach SOCKS5 dial: %v", err)
	}
	defer conn.Close() //nolint:errcheck // best-effort cleanup

	payload := []byte("after-attach\n")
	_ = conn.SetDeadline(time.Now().Add(15 * time.Second))
	if _, err := conn.Write(payload); err != nil {
		t.Fatalf("write: %v", err)
	}
	got := make([]byte, len(payload))
	if _, err := io.ReadFull(conn, got); err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != string(payload) {
		t.Fatalf("echo mismatch: got %q want %q", got, payload)
	}
}

// ScenarioAuthRejection_BadHyco: listener bound to a nonexistent
// hyco name on Azure must log a control-channel failure. The mock
// relay's hyco model is dynamic — every name is treated as valid
// pre-listener-attach (which is the "no-listener" path, covered by
// other scenarios + by TestMockEmulates_NoListenerReturns404) — so
// this case carries scope=AzureOnly in the reliability registry.
func ScenarioAuthRejection_BadHyco(t *testing.T, b Backend) {
	t.Helper()
	AssertNoLeaks(t)

	h := b.SetupExpectingFailure(t, SetupOptions{
		NumListeners:     1,
		SenderMode:       ModePortForward,
		OverrideHycoName: "nonexistent-hyco-xxxxx",
	})
	defer h.Close()
	assertNoTokenLeak(t, h.ListenerLogs())
}

// ScenarioAuthRejection_BadListenerSAS: listener with an invalid
// SAS key fails authentication. Both backends share the
// "control channel disconnected" log shape; the mock's SAS
// validation matches Azure's per mockrelay/server/auth_test.go.
func ScenarioAuthRejection_BadListenerSAS(t *testing.T, b Backend) {
	t.Helper()
	AssertNoLeaks(t)

	badKey := "dGhpcyBpcyBhIGJhZCBrZXk=" // base64("this is a bad key")
	h := b.SetupExpectingFailure(t, SetupOptions{
		NumListeners:         1,
		SenderMode:           ModePortForward,
		OverrideListenerAuth: &AuthOverride{BadSASKey: badKey},
	})
	defer h.Close()

	logs := h.ListenerLogs()
	assertNoTokenLeak(t, logs)
	if strings.Contains(logs, badKey) {
		t.Errorf("listener logs contain the bad SAS key value")
	}
}

// ScenarioAuthRejection_BadSenderSAS: sender with an invalid SAS
// key. Listener is healthy; the sender's relay dial fails on auth.
// Both backends share the "relay dial failed" log shape.
func ScenarioAuthRejection_BadSenderSAS(t *testing.T, b Backend) {
	t.Helper()
	AssertNoLeaks(t)

	badKey := "dGhpcyBpcyBhIGJhZCBrZXk="
	h := b.SetupExpectingFailure(t, SetupOptions{
		NumListeners:       1,
		SenderMode:         ModePortForward,
		Target:             "127.0.0.1:9999",
		AllowedTargets:     []string{"127.0.0.1:9999"},
		OverrideSenderAuth: &AuthOverride{BadSASKey: badKey},
	})
	defer h.Close()

	logs := h.SenderLogs()
	assertNoTokenLeak(t, logs)
	if strings.Contains(logs, badKey) {
		t.Errorf("sender logs contain the bad SAS key value")
	}
}

// ScenarioAllowlist_Reject: listener with an allowlist that
// excludes the echo target; SOCKS5 dial to the non-allowed target
// is rejected; the listener metric
// aztunnel_connection_errors_total{reason="allowlist_rejected"}
// increments. Subsumes the legacy TestAllowlistDeny +
// TestMetricsErrorReason.
func ScenarioAllowlist_Reject(t *testing.T, b Backend) {
	t.Helper()
	AssertNoLeaks(t)

	echo := StartPlainEcho(t)
	tun := b.Setup(t, SetupOptions{
		NumListeners: 1,
		SenderMode:   ModeSOCKS5,
		// 192.0.2.0/24 is TEST-NET-1; echo lives on 127.0.0.1.
		AllowedTargets: []string{"192.0.2.0/24:*"},
	})

	// The SOCKS5 dial may return either a SOCKS5Error (proxy
	// rejected with REP byte) or a connection-close error if the
	// listener short-circuits the bridge. Both are valid
	// rejections; what we assert is the metric counter on the
	// listener side.
	if _, err := dialSOCKS5WithRetry(tun.SenderAddr, echo.Addr(), 10*time.Second); err == nil {
		t.Fatalf("expected SOCKS5 dial to non-allowed target to fail")
	}

	if got := waitForConnectionErrorReason(tun.Listeners[0], "allowlist_rejected", 1, 15*time.Second); got < 1 {
		t.Errorf("listener metric aztunnel_connection_errors_total{reason=\"allowlist_rejected\"} = %d, want >= 1", got)
	}
}

// waitForLogSubstrAny polls logs() until any of substrs appears or
// timeout elapses.
func waitForLogSubstrAny(logs func() string, substrs []string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		s := logs()
		for _, n := range substrs {
			if strings.Contains(s, n) {
				return true
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	return false
}

// assertNoTokenLeak fails the test if output contains a raw
// sb-hc-token (the redacted form sb-hc-token=REDACTED is fine).
// Shared between the failure-mode scenarios so the redaction
// contract is asserted consistently.
func assertNoTokenLeak(t *testing.T, output string) {
	t.Helper()
	stripped := strings.ReplaceAll(output, "sb-hc-token=REDACTED", "")
	if strings.Contains(stripped, "sb-hc-token=") {
		t.Errorf("output contains raw sb-hc-token (not redacted)")
	}
}

// LongLivedIdleSleep is the wall-clock duration ScenarioLongLivedConnection
// holds an established tunnel idle for. 130 s clears the documented
// Azure Relay ~120 s idle-connection timeout (internal/relay/bridge.go)
// with a 10 s margin, so a regression in the bridge's keepalive
// pinger surfaces here as a write/read failure on the second echo.
const LongLivedIdleSleep = 130 * time.Second

// ScenarioLongLivedConnection opens a port-forward connection,
// performs one echo round-trip, sleeps LongLivedIdleSleep (longer
// than the Azure Relay idle timeout), then performs a second echo
// round-trip on the SAME connection. Asserts the connection
// survives — i.e. the bridge's WebSocket-ping keepalive kept the
// data plane alive across the idle window.
//
// scope=AzureOnly via the reliability registry: the mock relay has
// no idle timeout, so the assertion would be a no-op there.
func ScenarioLongLivedConnection(t *testing.T, b Backend) {
	t.Helper()
	AssertNoLeaks(t)

	echo := StartPlainEcho(t)
	tun := b.Setup(t, SetupOptions{
		NumListeners:   1,
		SenderMode:     ModePortForward,
		Target:         echo.Addr(),
		AllowedTargets: []string{echo.Addr()},
	})

	conn, err := net.DialTimeout("tcp", tun.SenderAddr, 10*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close() //nolint:errcheck // best-effort cleanup

	echoRoundTrip := func(label string, payload []byte) {
		t.Helper()
		_ = conn.SetDeadline(time.Now().Add(30 * time.Second))
		if err := writeFull(conn, payload); err != nil {
			t.Fatalf("%s write: %v (keepalive pings may have failed)", label, err)
		}
		buf := make([]byte, len(payload))
		if _, err := io.ReadFull(conn, buf); err != nil {
			t.Fatalf("%s read: %v", label, err)
		}
		if !bytes.Equal(buf, payload) {
			t.Fatalf("%s echo mismatch: got %q, want %q", label, buf, payload)
		}
	}

	echoRoundTrip("before-idle", []byte("before-idle\n"))
	t.Logf("idle for %s to test keepalive…", LongLivedIdleSleep)
	time.Sleep(LongLivedIdleSleep)
	echoRoundTrip("after-idle", []byte("after-idle\n"))
}

// ScenarioAuthRejection_CrossClaim asserts that a SAS key valid in
// one direction is rejected when presented for the other direction
// — i.e. the listener-direction key cannot be used to send, and
// the sender-direction key cannot be used to listen. The token
// signature itself is valid; the relay rejects it because the
// claim does not authorize the action.
//
// AzureOnly: the mock relay uses one shared key for both
// directions and so cannot distinguish Listen vs Send claims; per
// the SetupExpectingFailure contract, the listener-side case
// observes "control channel disconnected" on the listener log,
// and the sender-side case observes "relay dial failed" on the
// sender log.
//
// Cells whose Backend auth is not SAS (e.g. the entra cell) skip
// at Setup time because the per-direction keys exist only on the
// SAS hyco.
func ScenarioAuthRejection_CrossClaim(t *testing.T, b Backend) {
	t.Helper()
	AssertNoLeaks(t)

	t.Run("sender_uses_listener_key", func(t *testing.T) {
		// Sender presents the listener-direction key; sender's
		// relay dial must fail because the claim does not
		// authorize the Send role.
		h := b.SetupExpectingFailure(t, SetupOptions{
			NumListeners:       1,
			SenderMode:         ModePortForward,
			Target:             "127.0.0.1:9999",
			AllowedTargets:     []string{"127.0.0.1:9999"},
			OverrideSenderAuth: &AuthOverride{UseOppositeSASDirection: true},
		})
		defer h.Close()
		assertNoTokenLeak(t, h.SenderLogs())
	})

	t.Run("listener_uses_sender_key", func(t *testing.T) {
		// Listener presents the sender-direction key; listener's
		// control channel must fail because the claim does not
		// authorize the Listen role.
		h := b.SetupExpectingFailure(t, SetupOptions{
			NumListeners:         1,
			SenderMode:           ModePortForward,
			OverrideListenerAuth: &AuthOverride{UseOppositeSASDirection: true},
		})
		defer h.Close()
		assertNoTokenLeak(t, h.ListenerLogs())
	})
}
