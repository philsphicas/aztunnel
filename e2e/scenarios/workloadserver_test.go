package scenarios

import (
	"encoding/binary"
	"errors"
	"io"
	"net"
	"testing"
	"time"
)

// dialServer opens a TCP connection to s with a test-scoped deadline and
// registers cleanup.
func dialServer(t *testing.T, s *WorkloadServer) net.Conn {
	t.Helper()
	conn, err := net.DialTimeout("tcp", s.Addr(), 5*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func TestWorkloadServer_Echo_IsLiteralCopy(t *testing.T) {
	s := StartWorkloadServer(t, ServerBehavior{}) // zero = ServerEcho
	conn := dialServer(t, s)

	want := []byte("the quick brown fox")
	if err := writeFull(conn, want); err != nil {
		t.Fatalf("write: %v", err)
	}
	got := make([]byte, len(want))
	if _, err := io.ReadFull(conn, got); err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != string(want) {
		t.Fatalf("echo mismatch: got %q want %q", got, want)
	}
}

// StartPlainEcho must remain the zero-config echo oracle.
func TestStartPlainEcho_EchoesBytes(t *testing.T) {
	s := StartPlainEcho(t)
	conn := dialServer(t, s)
	if err := writeFull(conn, []byte("ping")); err != nil {
		t.Fatalf("write: %v", err)
	}
	got := make([]byte, 4)
	if _, err := io.ReadFull(conn, got); err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != "ping" {
		t.Fatalf("got %q", got)
	}
}

func TestWorkloadServer_Respond_Asymmetric(t *testing.T) {
	const respSize = 4096
	s := StartWorkloadServer(t, ServerBehavior{Mode: ServerRespond, RespSize: respSize})
	conn := dialServer(t, s)

	// Reuse one connection for several requests with distinct nonces and
	// asymmetric (small request, large response) sizes.
	base := randNonce()
	for i := 0; i < 5; i++ {
		if err := doRespondRequest(conn, base+uint64(i), 64, respSize); err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
	}
}

func TestWorkloadServer_Respond_ProcessingDelay(t *testing.T) {
	const delay = 60 * time.Millisecond
	s := StartWorkloadServer(t, ServerBehavior{Mode: ServerRespond, RespSize: 16, ProcessingDelay: delay})
	conn := dialServer(t, s)

	start := time.Now()
	if err := doRespondRequest(conn, 7, 16, 16); err != nil {
		t.Fatalf("request: %v", err)
	}
	if elapsed := time.Since(start); elapsed < delay {
		t.Fatalf("response returned in %v, expected >= ProcessingDelay %v", elapsed, delay)
	}
}

// A corrupted request body must be rejected by the server (frameError),
// surfacing as a client error rather than a silently-trusted response.
func TestWorkloadServer_Respond_RejectsCorruptRequest(t *testing.T) {
	s := StartWorkloadServer(t, ServerBehavior{Mode: ServerRespond, RespSize: 16})
	conn := dialServer(t, s)

	// Hand-craft a request frame whose payload does NOT match the nonce's
	// pattern.
	const nonce = 42
	bad := make([]byte, 16) // all zero; pattern[0] = byte(42) != 0
	if err := writeFrame(conn, frameRequest, nonce, bad); err != nil {
		t.Fatalf("write bad frame: %v", err)
	}
	typ, gotNonce, _, err := readFrame(conn)
	if err != nil {
		t.Fatalf("read reply: %v", err)
	}
	if typ != frameError {
		t.Fatalf("got frame type %d, want frameError (%d)", typ, frameError)
	}
	if gotNonce != nonce {
		t.Fatalf("error frame nonce %d, want %d", gotNonce, nonce)
	}
}

func TestWorkloadServer_Stream_OrderedChunksAndEnd(t *testing.T) {
	const (
		chunks    = 6
		chunkSize = 512
		interval  = 5 * time.Millisecond
	)
	s := StartWorkloadServer(t, ServerBehavior{
		Mode: ServerStream, StreamChunks: chunks, TrickleChunkSize: chunkSize, TrickleInterval: interval,
	})
	conn := dialServer(t, s)

	nonce := randNonce()
	if err := writeFrame(conn, frameStreamStart, nonce, nil); err != nil {
		t.Fatalf("write start: %v", err)
	}

	start := time.Now()
	var firstResp time.Duration
	for seq := 0; seq < chunks; seq++ {
		typ, n, payload, err := readFrame(conn)
		if err != nil {
			t.Fatalf("read chunk %d: %v", seq, err)
		}
		if seq == 0 {
			firstResp = time.Since(start)
		}
		if typ != frameStreamChunk {
			t.Fatalf("chunk %d: type %d, want frameStreamChunk", seq, typ)
		}
		if n != nonce {
			t.Fatalf("chunk %d: nonce %d, want %d (cross-stream leakage?)", seq, n, nonce)
		}
		if len(payload) != chunkSeqLen+chunkSize {
			t.Fatalf("chunk %d: payload len %d, want %d", seq, len(payload), chunkSeqLen+chunkSize)
		}
		gotSeq := binary.BigEndian.Uint32(payload[:chunkSeqLen])
		if int(gotSeq) != seq {
			t.Fatalf("chunk out of order: got seq %d, want %d", gotSeq, seq)
		}
		if off, ok := verifyPattern(payload[chunkSeqLen:], nonce, seq*chunkSize); !ok {
			t.Fatalf("chunk %d: pattern mismatch at offset %d", seq, off)
		}
	}
	// Clean stream-end frame must follow the last chunk.
	typ, _, _, err := readFrame(conn)
	if err != nil {
		t.Fatalf("read end frame: %v", err)
	}
	if typ != frameStreamEnd {
		t.Fatalf("got type %d after last chunk, want frameStreamEnd", typ)
	}
	if firstResp > interval {
		t.Logf("note: first-response latency %v exceeded one interval %v (acceptable under load)", firstResp, interval)
	}
}

func TestWorkloadServer_Stream_ProcessingDelayBeforeFirstChunk(t *testing.T) {
	const (
		chunks    = 3
		chunkSize = 256
		delay     = 120 * time.Millisecond
	)
	// No trickle interval: without the initial think time the first chunk
	// would arrive in well under delay, so a first-chunk arrival >= delay
	// isolates ProcessingDelay rather than inter-chunk pacing.
	s := StartWorkloadServer(t, ServerBehavior{
		Mode: ServerStream, StreamChunks: chunks, TrickleChunkSize: chunkSize, ProcessingDelay: delay,
	})
	conn := dialServer(t, s)

	nonce := randNonce()
	if err := writeFrame(conn, frameStreamStart, nonce, nil); err != nil {
		t.Fatalf("write start: %v", err)
	}

	start := time.Now()
	typ, _, _, err := readFrame(conn)
	firstResp := time.Since(start)
	if err != nil {
		t.Fatalf("read first chunk: %v", err)
	}
	if typ != frameStreamChunk {
		t.Fatalf("first frame type %d, want frameStreamChunk", typ)
	}
	if firstResp < delay {
		t.Errorf("first-response latency %v < ProcessingDelay %v: initial think time not applied", firstResp, delay)
	}
}

func TestFrame_RoundTrip(t *testing.T) {
	srv, cli := net.Pipe()
	defer srv.Close() //nolint:errcheck
	defer cli.Close() //nolint:errcheck

	payload := []byte("hello frame")
	go func() {
		_ = writeFrame(cli, frameRequest, 0xdeadbeef, payload)
	}()
	typ, nonce, got, err := readFrame(srv)
	if err != nil {
		t.Fatalf("readFrame: %v", err)
	}
	if typ != frameRequest || nonce != 0xdeadbeef || string(got) != string(payload) {
		t.Fatalf("round-trip mismatch: type=%d nonce=%x payload=%q", typ, nonce, got)
	}
}

func TestReadFrame_BadMagic(t *testing.T) {
	srv, cli := net.Pipe()
	defer srv.Close() //nolint:errcheck
	defer cli.Close() //nolint:errcheck

	go func() {
		_ = writeFull(cli, []byte("XXXX\x01garbage-header-bytes"))
	}()
	_, _, _, err := readFrame(srv)
	if !errors.Is(err, errBadMagic) {
		t.Fatalf("got err %v, want errBadMagic", err)
	}
}

func TestReadFrame_LengthCapRejected(t *testing.T) {
	srv, cli := net.Pipe()
	defer srv.Close() //nolint:errcheck
	defer cli.Close() //nolint:errcheck

	// Valid magic + type, but an absurd length prefix.
	hdr := make([]byte, frameHdrLen)
	copy(hdr[0:4], frameMagic)
	hdr[4] = frameRequest
	binary.BigEndian.PutUint32(hdr[13:17], frameMaxLen+1)
	go func() {
		_ = writeFull(cli, hdr)
	}()
	_, _, _, err := readFrame(srv)
	if err == nil {
		t.Fatal("expected error for over-cap length, got nil")
	}
}

// readFrame must surface a clean io.EOF when the peer closes exactly at a
// frame boundary, so serveRespond/serveStream can distinguish graceful
// close from truncation.
func TestReadFrame_CleanEOF(t *testing.T) {
	srv, cli := net.Pipe()
	defer srv.Close() //nolint:errcheck

	go func() { _ = cli.Close() }()
	_, _, _, err := readFrame(srv)
	if !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("got err %v, want EOF/closed", err)
	}
}

// TestPatternByte_DetectsAlignedReorder guards against a periodic pattern
// (e.g. byte(seed+off)) that would let a power-of-two-aligned block swap
// pass verifyPattern undetected. It builds a seed's pattern over a buffer,
// swaps two 256-byte-aligned blocks, and asserts the corruption is caught.
func TestPatternByte_DetectsAlignedReorder(t *testing.T) {
	const seed = uint64(0x0123456789ABCDEF)
	const blk = 256
	buf := make([]byte, 4*blk)
	fillPattern(buf, seed, 0)

	// Sanity: an untouched buffer verifies.
	if off, ok := verifyPattern(buf, seed, 0); !ok {
		t.Fatalf("clean buffer failed verification at offset %d", off)
	}

	// Swap the first two 256-byte-aligned blocks.
	tmp := make([]byte, blk)
	copy(tmp, buf[0:blk])
	copy(buf[0:blk], buf[blk:2*blk])
	copy(buf[blk:2*blk], tmp)

	if _, ok := verifyPattern(buf, seed, 0); ok {
		t.Fatal("256-aligned block swap passed verification; pattern has a 256-byte period")
	}
}

// TestPatternByte_SeedSeparation guards against cross-stream leakage being
// invisible: two distinct seeds must produce different patterns at the
// same offset for the overwhelming majority of bytes (the deterministic
// hash mixes the full 64-bit seed, not just its low byte).
func TestPatternByte_SeedSeparation(t *testing.T) {
	// Seeds differing only above the low byte must still diverge.
	a := uint64(0x1100)
	b := uint64(0x2200)
	diff := 0
	const n = 1024
	for off := 0; off < n; off++ {
		if patternByte(a, off) != patternByte(b, off) {
			diff++
		}
	}
	if diff < n*3/4 {
		t.Fatalf("patterns for distinct seeds diverged in only %d/%d bytes; seed mixing too weak", diff, n)
	}
}
