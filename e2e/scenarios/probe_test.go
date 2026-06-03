package scenarios

import (
	"encoding/binary"
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
}

// TestProbeFlow_BrokenOnPeerClose verifies that when the server closes
// the connection abruptly, the flow flips broken() within a short window
// and Stop is safe to call after.
func TestProbeFlow_BrokenOnPeerClose(t *testing.T) {
	// Stand up a one-shot proxy that immediately RSTs after accept; this
	// gives us a deterministic peer-close without driving the
	// WorkloadServer through a real probe exchange first (the server's
	// orderly EOF on Stop would NOT mark broken if stopping is set).
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
	if err := probeOnce(srv.Addr(), 64, 64, 5*time.Second); err != nil {
		t.Fatalf("probeOnce: %v", err)
	}
}

// TestProbeOnce_WrongRespSize verifies the one-shot helper surfaces a
// response-size mismatch instead of silently passing.
func TestProbeOnce_WrongRespSize(t *testing.T) {
	srv := StartWorkloadServer(t, ServerBehavior{Mode: ServerProbe, RespSize: 64})
	err := probeOnce(srv.Addr(), 64, 128, 5*time.Second)
	if err == nil {
		t.Fatalf("probeOnce expected error on resp-size mismatch")
	}
	if !strings.Contains(err.Error(), "body") {
		t.Errorf("error %q does not mention body size", err)
	}
}

// TestProbeFlow_Localize_OutboundBreak verifies localize() correctly
// identifies "no record at server" (no request ever reached the server).
func TestProbeFlow_Localize_OutboundBreak(t *testing.T) {
	srv := StartWorkloadServer(t, ServerBehavior{Mode: ServerProbe, RespSize: 16})
	// Build a probeFlow with no traffic — pick an arbitrary nonce the
	// server has never seen.
	c, err := net.Dial("tcp", srv.Addr())
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
