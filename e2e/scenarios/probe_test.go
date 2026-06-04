package scenarios

import (
	"encoding/binary"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"
)

// TestServeProbe_RoundTripIntegrity drives a single hand-built probe
// request through a ServerProbe server and verifies the server echoes
// seq + client timestamp, stamps recv/send, fills RespSize pattern bytes,
// and publishes a non-terminal probeConnStat under the request nonce.
func TestServeProbe_RoundTripIntegrity(t *testing.T) {
	const reqSize, respSize = 32, 48
	srv := StartWorkloadServer(t, ServerBehavior{Mode: ServerProbe, RespSize: respSize})
	conn := dialServer(t, srv)

	nonce := uint64(0xAABBCCDDEEFF0011)
	const seq uint32 = 7
	const clientStamp int64 = 0x0123456789ABCDEF

	req := make([]byte, probeHdrLen+reqSize)
	binary.BigEndian.PutUint32(req[probeOffSeq:], seq)
	binary.BigEndian.PutUint64(req[probeOffClient:], uint64(clientStamp))
	fillPattern(req[probeHdrLen:], nonce, int(seq)*reqSize)
	if err := writeFrame(conn, frameProbeReq, nonce, req); err != nil {
		t.Fatalf("write: %v", err)
	}

	typ, n, payload, err := readFrame(conn)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if typ != frameProbeResp {
		t.Fatalf("type=%d, want frameProbeResp=%d", typ, frameProbeResp)
	}
	if n != nonce {
		t.Fatalf("nonce=%x, want %x", n, nonce)
	}
	if len(payload) != probeHdrLen+respSize {
		t.Fatalf("payload len=%d, want %d", len(payload), probeHdrLen+respSize)
	}
	if gotSeq := binary.BigEndian.Uint32(payload[probeOffSeq:]); gotSeq != seq {
		t.Errorf("echoed seq=%d, want %d", gotSeq, seq)
	}
	if gotClient := int64(binary.BigEndian.Uint64(payload[probeOffClient:])); gotClient != clientStamp {
		t.Errorf("echoed client stamp=%x, want %x", gotClient, clientStamp)
	}
	srvRecv := int64(binary.BigEndian.Uint64(payload[probeOffSrvRecv:]))
	srvSend := int64(binary.BigEndian.Uint64(payload[probeOffSrvSend:]))
	if srvRecv <= 0 || srvSend < srvRecv {
		t.Errorf("server stamps invalid: recv=%d send=%d", srvRecv, srvSend)
	}
	if off, ok := verifyPattern(payload[probeHdrLen:], nonce, int(seq)*respSize); !ok {
		t.Errorf("response pattern mismatch at offset %d", off)
	}

	// The server's per-nonce record must exist with this nonce's seq.
	rec, ok := srv.ProbeRecord(nonce)
	if !ok {
		t.Fatalf("ProbeRecord(nonce) not found")
	}
	if rec.lastSeqSeen != int64(seq) {
		t.Errorf("rec.lastSeqSeen=%d want %d", rec.lastSeqSeen, seq)
	}
	if rec.terminal {
		t.Errorf("rec.terminal=true after a successful exchange; want false (conn still open)")
	}
}

// TestServeProbe_PatternMismatch_FrameError verifies the server replies
// with a frameError frame and closes when a request's pattern is corrupt.
func TestServeProbe_PatternMismatch_FrameError(t *testing.T) {
	const reqSize = 16
	srv := StartWorkloadServer(t, ServerBehavior{Mode: ServerProbe, RespSize: 16})
	conn := dialServer(t, srv)

	nonce := uint64(0xDEADBEEFCAFEBABE)
	req := make([]byte, probeHdrLen+reqSize)
	// header valid, body intentionally wrong (zeros vs nonce-derived pattern)
	if err := writeFrame(conn, frameProbeReq, nonce, req); err != nil {
		t.Fatalf("write: %v", err)
	}
	typ, n, _, err := readFrame(conn)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if typ != frameError {
		t.Fatalf("want frameError, got typ=%d", typ)
	}
	if n != nonce {
		t.Fatalf("nonce mismatch: got %x want %x", n, nonce)
	}
}

// TestProbeFlow_ContinuousProgress drives a probeFlow against a real
// ServerProbe target and verifies acked monotonically advances under a
// short interval, with no broken / writeErr / firstErr state.
func TestProbeFlow_ContinuousProgress(t *testing.T) {
	srv := StartWorkloadServer(t, ServerBehavior{Mode: ServerProbe, RespSize: 32})
	cfg := ProbeConfig{Interval: 5 * time.Millisecond, ReqSize: 32, RespSize: 32}
	f, err := startProbeFlow(srv.Addr(), 5*time.Second, cfg)
	if err != nil {
		t.Fatalf("startProbeFlow: %v", err)
	}
	t.Cleanup(f.Stop)

	base := f.acked()
	if !waitProgress(f, base+10, 3*time.Second) {
		t.Fatalf("flow did not advance past %d within 3s (acked=%d)", base+10, f.acked())
	}
	if f.broken() {
		t.Errorf("flow broken: %v", f.firstError())
	}
	s := f.Summary()
	if s.exchanges <= 0 || s.acked < s.sent-1 {
		t.Errorf("summary inconsistent: %+v", s)
	}
	// Rolling aggregates must reflect a real worst exchange: maxRTT > 0
	// and worstByRTT.rtt == maxRTT, with seq <= acked.
	if s.maxRTT <= 0 || s.worstByRTT.rtt != s.maxRTT {
		t.Errorf("worstByRTT mismatch: maxRTT=%v worst=%+v", s.maxRTT, s.worstByRTT)
	}
	if int64(s.worstByRTT.seq) > s.acked {
		t.Errorf("worstByRTT.seq=%d > acked=%d", s.worstByRTT.seq, s.acked)
	}
}

// TestProbeFlow_BrokenOnPeerClose verifies that when the server closes
// the connection abruptly, the flow flips broken() within a short window
// and Stop is safe to call after.
func TestProbeFlow_BrokenOnPeerClose(t *testing.T) {
	// Stand up a one-shot listener that closes the accepted conn (FIN);
	// the probeFlow's reader observes EOF and must mark broken. We use
	// this stub instead of a real WorkloadServer so the close is
	// unambiguously a peer-side close (not a Stop-with-stopping-flag).
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close() //nolint:errcheck
	accepted := make(chan net.Conn, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		accepted <- c
	}()

	c, err := net.DialTimeout("tcp", ln.Addr().String(), 2*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	// Wrap the client side directly in a probeFlow (skip startProbeFlow's
	// waitFirstAck which would never see a response from this fake peer).
	f := newProbeFlow(c, ProbeConfig{Interval: 1 * time.Millisecond, ReqSize: 16, RespSize: 16})
	f.Start()
	t.Cleanup(f.Stop)

	// Force a server-side close on the accepted conn.
	select {
	case sc := <-accepted:
		_ = sc.Close()
	case <-time.After(2 * time.Second):
		t.Fatalf("server side never accepted")
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if f.broken() {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !f.broken() {
		t.Fatalf("flow did not mark broken within 2s after peer close (firstErr=%v writeErr=%v)",
			f.firstError(), f.Summary().writeErr)
	}
}

// TestProbeFlow_StopDoesNotBreakSurvivor verifies Stop() shuts down a
// healthy flow without flipping its broken() flag. This is the property
// that lets HotDrop count "exactly A0 broken" without false positives.
func TestProbeFlow_StopDoesNotBreakSurvivor(t *testing.T) {
	srv := StartWorkloadServer(t, ServerBehavior{Mode: ServerProbe, RespSize: 16})
	cfg := ProbeConfig{Interval: 5 * time.Millisecond, ReqSize: 16, RespSize: 16}
	f, err := startProbeFlow(srv.Addr(), 5*time.Second, cfg)
	if err != nil {
		t.Fatalf("startProbeFlow: %v", err)
	}
	// Run for a few exchanges, then Stop on a healthy connection.
	if !waitProgress(f, 1, 2*time.Second) {
		t.Fatalf("flow did not advance to seq>1 within 2s")
	}
	f.Stop()
	if f.broken() {
		t.Errorf("Stop() on a healthy flow marked it broken: firstErr=%v", f.firstError())
	}
}

// TestProbeOnce_RoundTrip exercises the one-shot helper end-to-end.
func TestProbeOnce_RoundTrip(t *testing.T) {
	srv := StartWorkloadServer(t, ServerBehavior{Mode: ServerProbe, RespSize: 64})
	if err := probeOnce(srv, srv.Addr(), 64, 64, 5*time.Second); err != nil {
		t.Fatalf("probeOnce: %v", err)
	}
}

// TestProbeOnce_WrongRespSize verifies the one-shot helper surfaces a
// response-size mismatch instead of silently passing.
func TestProbeOnce_WrongRespSize(t *testing.T) {
	srv := StartWorkloadServer(t, ServerBehavior{Mode: ServerProbe, RespSize: 64})
	err := probeOnce(srv, srv.Addr(), 64, 128, 5*time.Second)
	if err == nil {
		t.Fatalf("probeOnce expected error on resp-size mismatch")
	}
	if !strings.Contains(err.Error(), "body") {
		t.Errorf("error %q does not mention body size", err)
	}
}

// TestServeProbe_NonceChange_Rejected verifies that the server rejects a
// frame whose nonce differs from the first frame on the same connection.
// Without this, a buggy client mixing nonces would be silently verified
// and stat-recorded against the original nonce, masking real bugs.
func TestServeProbe_NonceChange_Rejected(t *testing.T) {
	const body = 16
	srv := StartWorkloadServer(t, ServerBehavior{Mode: ServerProbe, RespSize: body})
	conn := dialServer(t, srv)

	// First request: nonce A, seq 0, valid pattern.
	nonceA := uint64(0x1111111111111111)
	reqA := make([]byte, probeHdrLen+body)
	fillPattern(reqA[probeHdrLen:], nonceA, 0)
	if err := writeFrame(conn, frameProbeReq, nonceA, reqA); err != nil {
		t.Fatalf("write A: %v", err)
	}
	if _, _, _, err := readFrame(conn); err != nil {
		t.Fatalf("read A: %v", err)
	}
	// Second request: nonce B (different), seq 1. The pattern is built
	// from nonceB so it would verify against nonceB but NOT nonceA; we
	// want the server to reject on the nonce mismatch before any pattern
	// check, so the test does not depend on which check fires first.
	nonceB := uint64(0x2222222222222222)
	reqB := make([]byte, probeHdrLen+body)
	binary.BigEndian.PutUint32(reqB[probeOffSeq:], 1)
	fillPattern(reqB[probeHdrLen:], nonceB, body)
	if err := writeFrame(conn, frameProbeReq, nonceB, reqB); err != nil {
		t.Fatalf("write B: %v", err)
	}
	// The server should close the connection in response.
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, _, _, err := readFrame(conn); err == nil {
		t.Fatalf("expected server to close after nonce change, got a frame")
	}
}

// TestProbeFlow_Localize_OutboundBreak verifies localize() correctly
// identifies "no record at server" (no request ever reached the server).
func TestProbeFlow_Localize_OutboundBreak(t *testing.T) {
	srv := StartWorkloadServer(t, ServerBehavior{Mode: ServerProbe, RespSize: 16})
	// Build a probeFlow with no traffic — pick an arbitrary nonce the
	// server has never seen.
	c, err := net.DialTimeout("tcp", srv.Addr(), 2*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	f := newProbeFlow(c, ProbeConfig{Interval: 1 * time.Hour, ReqSize: 16, RespSize: 16})
	t.Cleanup(f.Stop)
	got := f.localize(srv)
	if !strings.Contains(got, "no record") || !strings.Contains(got, "outbound") {
		t.Errorf("localize() = %q, want outbound/no-record diagnosis", got)
	}
}

// TestServeProbe_ConsumeProbeRecord verifies that ConsumeProbeRecord
// returns a snapshot of the per-nonce record AND removes it, so future
// lookups (Probe/Consume) report no record. This is the bounded-memory
// path for diagnostic consumers that only need the record once.
func TestServeProbe_ConsumeProbeRecord(t *testing.T) {
	const body = 16
	srv := StartWorkloadServer(t, ServerBehavior{Mode: ServerProbe, RespSize: body})
	conn := dialServer(t, srv)

	nonce := uint64(0x5555555555555555)
	req := make([]byte, probeHdrLen+body)
	fillPattern(req[probeHdrLen:], nonce, 0)
	if err := writeFrame(conn, frameProbeReq, nonce, req); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, _, _, err := readFrame(conn); err != nil {
		t.Fatalf("read: %v", err)
	}
	if _, ok := srv.ProbeRecord(nonce); !ok {
		t.Fatalf("ProbeRecord before consume: not found")
	}
	rec, ok := srv.ConsumeProbeRecord(nonce)
	if !ok {
		t.Fatalf("ConsumeProbeRecord: not found")
	}
	if rec.lastSeqSeen != 0 {
		t.Errorf("rec.lastSeqSeen=%d want 0", rec.lastSeqSeen)
	}
	if _, ok := srv.ProbeRecord(nonce); ok {
		t.Errorf("ProbeRecord after consume: still present")
	}
	if _, ok := srv.ConsumeProbeRecord(nonce); ok {
		t.Errorf("second ConsumeProbeRecord: still present")
	}
}

// TestNewProbeFlow_ClampsNegativeFields verifies that negative
// Interval/ReqSize/RespSize and MaxOutstanding are clamped to safe
// defaults instead of producing a hot-loop sender or a make() panic.
func TestNewProbeFlow_ClampsNegativeFields(t *testing.T) {
	c1, c2 := net.Pipe()
	t.Cleanup(func() { _ = c1.Close(); _ = c2.Close() })
	cfg := ProbeConfig{
		Interval:       -1 * time.Millisecond,
		ReqSize:        -1,
		RespSize:       -1,
		MaxOutstanding: -1,
	}
	f := newProbeFlow(c1, cfg) // must not panic
	if f.cfg.Interval != defaultProbeIntvl {
		t.Errorf("Interval=%v, want %v", f.cfg.Interval, defaultProbeIntvl)
	}
	if f.cfg.ReqSize != defaultProbeBody || f.cfg.RespSize != defaultProbeBody {
		t.Errorf("Req/Resp size=(%d,%d), want (%d,%d)", f.cfg.ReqSize, f.cfg.RespSize, defaultProbeBody, defaultProbeBody)
	}
	if f.cfg.MaxOutstanding != 1 {
		t.Errorf("MaxOutstanding=%d, want 1", f.cfg.MaxOutstanding)
	}
}

// TestProbeFlow_ReaderClosesConnOnBreak verifies that when the reader
// exits due to an integrity error, it closes the underlying connection
// so the server side unblocks promptly (important under MaxOutstanding>1
// where the server might be blocked writing a response).
func TestProbeFlow_ReaderClosesConnOnBreak(t *testing.T) {
	srv, cli := net.Pipe()
	t.Cleanup(func() { _ = srv.Close() })

	f := newProbeFlow(cli, ProbeConfig{Interval: 1 * time.Hour, ReqSize: 8, RespSize: 8})
	f.Start()
	t.Cleanup(f.Stop)

	// Send a complete frame header with a bad magic so readFrame returns
	// errBadMagic. (frameHdrLen=17 bytes: 4-byte magic + 1-byte type +
	// 8-byte nonce + 4-byte length.) Writing fewer bytes would leave
	// readFrame blocked in io.ReadFull and never trigger the break path.
	bad := make([]byte, 17)
	copy(bad, []byte("XXXX"))
	if _, err := srv.Write(bad); err != nil {
		t.Fatalf("server write: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if f.broken() {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !f.broken() {
		t.Fatalf("reader did not flag broken after garbled frame")
	}
	// Verify the conn the reader held is closed: a subsequent write from
	// the server side returns an error.
	_ = srv.SetWriteDeadline(time.Now().Add(500 * time.Millisecond))
	if _, err := srv.Write([]byte{0}); err == nil {
		t.Errorf("server write should fail after reader closed the conn, got nil")
	}
}

// TestServeProbe_NonMonotonicSeq_Rejected verifies the server rejects a
// request whose seq does not strictly exceed the prior request's seq.
// Without this, a buggy client repeating or decreasing seq would regress
// probeConnStat.lastSeqSeen and corrupt localize() output.
func TestServeProbe_NonMonotonicSeq_Rejected(t *testing.T) {
	const body = 16
	srv := StartWorkloadServer(t, ServerBehavior{Mode: ServerProbe, RespSize: body})
	conn := dialServer(t, srv)

	nonce := uint64(0x4444444444444444)
	send := func(seq uint32) {
		req := make([]byte, probeHdrLen+body)
		binary.BigEndian.PutUint32(req[probeOffSeq:], seq)
		fillPattern(req[probeHdrLen:], nonce, int(seq)*body)
		if err := writeFrame(conn, frameProbeReq, nonce, req); err != nil {
			t.Fatalf("write seq %d: %v", seq, err)
		}
	}
	// First request seq=0 should succeed.
	send(0)
	if _, _, _, err := readFrame(conn); err != nil {
		t.Fatalf("read 0: %v", err)
	}
	// Repeat seq=0 — must be rejected.
	send(0)
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	typ, _, _, err := readFrame(conn)
	if err == nil && typ != frameError {
		t.Fatalf("expected frameError or close after duplicate seq, got typ=%d err=%v", typ, err)
	}
}

// TestProbeFlow_WaitFirstAck_FailsFastOnWriteError verifies waitFirstAck
// surfaces a latched write-side failure immediately instead of waiting
// for the full timeout, even when the read side hasn't broken (in real
// use this is the sender returning before the reader observes EOF).
// We exercise the loop's writeErr check directly: construct a flow,
// don't start goroutines, latch a synthetic write error, then call
// waitFirstAck and assert it returns fast with that error.
func TestProbeFlow_WaitFirstAck_FailsFastOnWriteError(t *testing.T) {
	cli, srv := net.Pipe()
	t.Cleanup(func() { _ = cli.Close(); _ = srv.Close() })

	f := newProbeFlow(cli, ProbeConfig{Interval: 1 * time.Hour, ReqSize: 8, RespSize: 8})
	// Don't Start — we don't want the sender/reader goroutines to run.
	f.recordWriteErr(fmt.Errorf("synthetic write failure"))

	start := time.Now()
	err := f.waitFirstAck(5 * time.Second)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatalf("waitFirstAck succeeded despite latched writeErr")
	}
	if elapsed >= 500*time.Millisecond {
		t.Errorf("waitFirstAck took %v; expected fast write-error path", elapsed)
	}
	if !strings.Contains(err.Error(), "write") {
		t.Errorf("error %q does not mention write", err)
	}
}

// TestProbeFlow_Samples_RingBuffer drives a probeFlow with SampleSize > 0
// and asserts the Samples() snapshot is bounded by SampleSize and
// returns the most-recent N exchanges in chronological order. Without
// SampleSize, Samples must return nil to confirm the allocation-free
// path is preserved.
func TestProbeFlow_Samples_RingBuffer(t *testing.T) {
	srv := StartWorkloadServer(t, ServerBehavior{Mode: ServerProbe, RespSize: 16})

	t.Run("Disabled", func(t *testing.T) {
		cfg := ProbeConfig{Interval: 5 * time.Millisecond, ReqSize: 16, RespSize: 16}
		f, err := startProbeFlow(srv.Addr(), 5*time.Second, cfg)
		if err != nil {
			t.Fatalf("startProbeFlow: %v", err)
		}
		t.Cleanup(f.Stop)
		if !waitProgress(f, 5, 2*time.Second) {
			t.Fatalf("flow did not advance past seq 5")
		}
		if s := f.Samples(); s != nil {
			t.Errorf("Samples() with SampleSize=0 = %d entries, want nil", len(s))
		}
	})

	t.Run("Wrapped", func(t *testing.T) {
		const ring = 4
		cfg := ProbeConfig{Interval: 5 * time.Millisecond, ReqSize: 16, RespSize: 16, SampleSize: ring}
		f, err := startProbeFlow(srv.Addr(), 5*time.Second, cfg)
		if err != nil {
			t.Fatalf("startProbeFlow: %v", err)
		}
		t.Cleanup(f.Stop)
		// Drive well past the ring size so it has definitely wrapped.
		if !waitProgress(f, int64(ring*4), 3*time.Second) {
			t.Fatalf("flow did not advance past seq %d", ring*4)
		}
		samples := f.Samples()
		if len(samples) != ring {
			t.Fatalf("Samples() = %d, want %d", len(samples), ring)
		}
		// Chronological order: seqs must be strictly increasing.
		for i := 1; i < len(samples); i++ {
			if samples[i].seq <= samples[i-1].seq {
				t.Errorf("samples not chronological at i=%d: %d <= %d", i, samples[i].seq, samples[i-1].seq)
			}
		}
	})
}
