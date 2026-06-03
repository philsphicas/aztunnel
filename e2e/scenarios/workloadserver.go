package scenarios

import (
	crand "crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// WorkloadServer is a configurable TCP target for the perf harness. It
// is the single downstream endpoint behind the tunnel; its per-connection
// behavior is selected by ServerBehavior.Mode:
//
//	ServerEcho    (zero value) — literal io.Copy(c, c). Byte-for-byte
//	                             transparent; this is what StartPlainEcho
//	                             returns and what the correctness /
//	                             transparency scenarios assert against.
//	ServerRespond              — framed request/response. For each request
//	                             frame the client sends, the server waits
//	                             ProcessingDelay then replies with RespSize
//	                             bytes. Models asymmetric (eAPI-like) shapes.
//	ServerStream               — long-lived server-paced trickle. After a
//	                             single start frame the server waits
//	                             ProcessingDelay (initial think time before
//	                             first output), then pushes StreamChunks
//	                             chunks of TrickleChunkSize bytes, spaced by
//	                             TrickleInterval. Models gNMI-subscribe-like
//	                             streaming.
//
// The zero-config (ServerEcho) path deliberately short-circuits to a
// literal io.Copy and never touches the framed protocol, so a bug in the
// framed engine can neither mask nor fake a tunnel transparency
// violation: the dumb echo stays the honest oracle because its code path
// stays dumb.
//
// Lifetimes are tracked separately: serveDone signals when the accept
// loop has exited (so no further connection goroutines can spawn) and
// connWg tracks the connection goroutines themselves. Stop waits on
// serveDone before draining connWg so the connection counter never
// receives a fresh Add concurrent with its own Wait.
type WorkloadServer struct {
	behavior  ServerBehavior
	ln        net.Listener
	serveDone chan struct{}
	connWg    sync.WaitGroup
	done      atomic.Bool
}

// ServerMode selects a WorkloadServer's per-connection behavior. The zero
// value is ServerEcho so a zero-value ServerBehavior reproduces the
// legacy plain-echo target exactly.
type ServerMode int

const (
	ServerEcho ServerMode = iota
	ServerRespond
	ServerStream
)

// ServerBehavior is the construction-time configuration of a
// WorkloadServer. Knobs are orthogonal; each mode reads only the subset
// it needs. The zero value is a literal echo server.
type ServerBehavior struct {
	Mode ServerMode

	// RespSize is the response payload size (ServerRespond). The
	// response is independent of the request size, so ReqSize != RespSize
	// (asymmetric) workloads are expressible.
	RespSize int

	// ProcessingDelay is the server-side think time — a deterministic,
	// subtractable stand-in for backend processing. In ServerRespond it
	// is inserted before each response; in ServerStream it is the initial
	// think time before the first chunk is pushed (models a backend that
	// computes initial state before it starts streaming).
	ProcessingDelay time.Duration

	// TrickleInterval is the gap between successive pushed chunks
	// (ServerStream). The first chunk is sent immediately after
	// ProcessingDelay; the remaining StreamChunks-1 chunks are spaced by
	// this interval.
	TrickleInterval time.Duration

	// TrickleChunkSize is the payload size of each pushed chunk
	// (ServerStream), excluding the per-chunk sequence header.
	TrickleChunkSize int

	// StreamChunks is the total number of chunks pushed before the server
	// sends the stream-end frame and closes (ServerStream).
	StreamChunks int
}

// StartWorkloadServer starts a WorkloadServer on a free localhost port,
// registers t.Cleanup to stop it, and returns it. Accepts testing.TB so
// it is callable from every scenario suite (only Helper / Fatalf /
// Cleanup are used).
func StartWorkloadServer(t testing.TB, behavior ServerBehavior) *WorkloadServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("workload server listen: %v", err)
	}
	s := &WorkloadServer{behavior: behavior, ln: ln, serveDone: make(chan struct{})}
	go s.serve()
	t.Cleanup(s.Stop)
	return s
}

// Addr returns the host:port the server is listening on.
func (s *WorkloadServer) Addr() string { return s.ln.Addr().String() }

// Stop closes the listener, waits for the accept loop to exit so no
// further connection goroutines can be spawned, then drains the
// outstanding connections.
func (s *WorkloadServer) Stop() {
	if s.done.Swap(true) {
		return
	}
	s.ln.Close()  //nolint:errcheck // best-effort cleanup
	<-s.serveDone // accept loop has exited; connWg is now stable
	s.connWg.Wait()
}

func (s *WorkloadServer) serve() {
	defer close(s.serveDone)
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return
		}
		s.connWg.Add(1)
		go func(c net.Conn) {
			defer s.connWg.Done()
			defer c.Close() //nolint:errcheck // best-effort cleanup
			s.handle(c)
		}(conn)
	}
}

func (s *WorkloadServer) handle(c net.Conn) {
	switch s.behavior.Mode {
	case ServerEcho:
		// Literal echo — the transparency oracle. No framing.
		_, _ = io.Copy(c, c)
	case ServerRespond:
		_ = serveRespond(c, s.behavior)
	case ServerStream:
		_ = serveStream(c, s.behavior)
	}
}

// ----------------------------------------------------------------------------
// Framed workload protocol
//
// A tiny length-prefixed protocol used by the ServerRespond and
// ServerStream modes (never by ServerEcho). It models traffic SHAPE, not
// any real wire protocol: every payload is a deterministic byte pattern
// keyed by a per-request/per-stream nonce, so both ends can verify
// integrity (ordering, corruption, truncation, cross-stream leakage)
// without an echo.
// ----------------------------------------------------------------------------

const (
	frameMagic  = "AZW1"
	frameHdrLen = 4 + 1 + 8 + 4 // magic + type + nonce + length
	frameMaxLen = 64 << 20      // refuse absurd lengths from a desynced peer
	chunkSeqLen = 4             // per-chunk sequence header inside a stream chunk payload
)

const (
	frameRequest     byte = 1 // client -> server (ServerRespond)
	frameResponse    byte = 2 // server -> client (ServerRespond)
	frameStreamStart byte = 3 // client -> server (ServerStream)
	frameStreamChunk byte = 4 // server -> client (ServerStream)
	frameStreamEnd   byte = 5 // server -> client (ServerStream): clean completion
	frameError       byte = 6 // either direction: integrity failure detected
)

var errBadMagic = errors.New("workload frame: bad magic")

// patternByte is the deterministic payload byte at absolute offset off
// for a stream/request keyed by seed. Every output byte depends on the
// full 64-bit seed and the full offset via an integer hash, so the
// pattern has no short period: a block swap or duplication (even one
// aligned to a power-of-two boundary), a truncation, or leakage from a
// different seed's payload all change the expected byte and are caught by
// a plain equality check at the receiver. (A naive byte(seed+off) would
// repeat every 256 bytes, letting 256-aligned reorders slip through.)
func patternByte(seed uint64, off int) byte {
	x := seed*0x9E3779B97F4A7C15 + uint64(off)*0x2545F4914F6CDD1D
	x ^= x >> 29
	x *= 0xBF58476D1CE4E5B9
	x ^= x >> 32
	return byte(x)
}

// fillPattern writes seed's deterministic pattern into buf starting at
// absolute offset base.
func fillPattern(buf []byte, seed uint64, base int) {
	for i := range buf {
		buf[i] = patternByte(seed, base+i)
	}
}

// verifyPattern checks buf matches seed's deterministic pattern starting
// at absolute offset base, returning the first mismatching absolute
// offset (and false) on failure.
func verifyPattern(buf []byte, seed uint64, base int) (int, bool) {
	for i, b := range buf {
		if b != patternByte(seed, base+i) {
			return base + i, false
		}
	}
	return 0, true
}

// writeFrame encodes one frame (header + payload) and writes it in a
// single buffer so a record is never split across writes.
func writeFrame(w io.Writer, typ byte, nonce uint64, payload []byte) error {
	buf := make([]byte, frameHdrLen+len(payload))
	copy(buf[0:4], frameMagic)
	buf[4] = typ
	binary.BigEndian.PutUint64(buf[5:13], nonce)
	binary.BigEndian.PutUint32(buf[13:17], uint32(len(payload)))
	copy(buf[frameHdrLen:], payload)
	return writeFull(w, buf)
}

// readFrame reads one frame's header and payload. It returns io.EOF
// verbatim when the stream ends cleanly at a frame boundary so callers
// can distinguish a graceful close from a truncated frame.
func readFrame(r io.Reader) (typ byte, nonce uint64, payload []byte, err error) {
	var hdr [frameHdrLen]byte
	if _, err = io.ReadFull(r, hdr[:]); err != nil {
		return 0, 0, nil, err
	}
	if string(hdr[0:4]) != frameMagic {
		return 0, 0, nil, errBadMagic
	}
	typ = hdr[4]
	nonce = binary.BigEndian.Uint64(hdr[5:13])
	n := binary.BigEndian.Uint32(hdr[13:17])
	if n > frameMaxLen {
		return 0, 0, nil, fmt.Errorf("workload frame: length %d exceeds max %d", n, frameMaxLen)
	}
	if n > 0 {
		payload = make([]byte, n)
		if _, err = io.ReadFull(r, payload); err != nil {
			return 0, 0, nil, fmt.Errorf("workload frame: short payload: %w", err)
		}
	}
	return typ, nonce, payload, nil
}

// serveRespond runs the ServerRespond per-connection loop: read a request
// frame, verify its pattern, wait ProcessingDelay, reply with RespSize
// bytes of the same nonce's pattern. Returns when the client closes.
func serveRespond(c net.Conn, b ServerBehavior) error {
	resp := make([]byte, b.RespSize)
	for {
		typ, nonce, payload, err := readFrame(c)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		if typ != frameRequest {
			return fmt.Errorf("serveRespond: unexpected frame type %d", typ)
		}
		if off, ok := verifyPattern(payload, nonce, 0); !ok {
			// Request corrupted in flight — tell the client so it
			// fails loudly instead of trusting a silent response.
			_ = writeFrame(c, frameError, nonce, nil)
			return fmt.Errorf("serveRespond: request pattern mismatch at offset %d", off)
		}
		if b.ProcessingDelay > 0 {
			time.Sleep(b.ProcessingDelay)
		}
		fillPattern(resp, nonce, 0)
		if err := writeFrame(c, frameResponse, nonce, resp); err != nil {
			return err
		}
	}
}

// serveStream runs the ServerStream per-connection flow: await the start
// frame, wait ProcessingDelay (initial think time before first output),
// then push StreamChunks chunks (first immediately, the rest spaced by
// TrickleInterval), then a clean stream-end frame. Each chunk
// carries a 4-byte big-endian sequence header followed by
// TrickleChunkSize bytes of the stream nonce's continuous pattern (the
// pattern offset spans chunks, so a reorder across chunk boundaries is
// detectable).
func serveStream(c net.Conn, b ServerBehavior) error {
	typ, nonce, _, err := readFrame(c)
	if err != nil {
		if errors.Is(err, io.EOF) {
			return nil
		}
		return err
	}
	if typ != frameStreamStart {
		return fmt.Errorf("serveStream: expected start frame, got type %d", typ)
	}
	chunk := make([]byte, chunkSeqLen+b.TrickleChunkSize)
	if b.ProcessingDelay > 0 {
		time.Sleep(b.ProcessingDelay)
	}
	for seq := 0; seq < b.StreamChunks; seq++ {
		if seq > 0 && b.TrickleInterval > 0 {
			time.Sleep(b.TrickleInterval)
		}
		binary.BigEndian.PutUint32(chunk[0:chunkSeqLen], uint32(seq))
		fillPattern(chunk[chunkSeqLen:], nonce, seq*b.TrickleChunkSize)
		if err := writeFrame(c, frameStreamChunk, nonce, chunk); err != nil {
			return err
		}
	}
	return writeFrame(c, frameStreamEnd, nonce, nil)
}

// randNonce returns a random 64-bit nonce used to seed a request's or
// stream's deterministic payload pattern. Falls back to the wall clock if
// the system RNG is unavailable (the nonce only needs to be distinct, not
// unpredictable).
func randNonce() uint64 {
	var b [8]byte
	if _, err := crand.Read(b[:]); err != nil {
		return uint64(time.Now().UnixNano())
	}
	return binary.BigEndian.Uint64(b[:])
}

// doRespondRequest performs one client-side framed request/response
// exchange against a ServerRespond target: it sends reqSize bytes of the
// nonce-keyed pattern and verifies the reply's type, echoed nonce, length
// (== wantRespSize), and payload pattern. Any mismatch is an integrity
// failure (corruption, truncation, or a stale/misrouted response).
func doRespondRequest(conn net.Conn, nonce uint64, reqSize, wantRespSize int) error {
	req := make([]byte, reqSize)
	fillPattern(req, nonce, 0)
	if err := writeFrame(conn, frameRequest, nonce, req); err != nil {
		return fmt.Errorf("write request: %w", err)
	}
	typ, rnonce, payload, err := readFrame(conn)
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}
	switch {
	case typ == frameError:
		return fmt.Errorf("server reported request integrity error (nonce %d)", rnonce)
	case typ != frameResponse:
		return fmt.Errorf("unexpected response frame type %d", typ)
	case rnonce != nonce:
		return fmt.Errorf("response nonce %d != request nonce %d", rnonce, nonce)
	case len(payload) != wantRespSize:
		return fmt.Errorf("response length %d != want %d", len(payload), wantRespSize)
	}
	if off, ok := verifyPattern(payload, nonce, 0); !ok {
		return fmt.Errorf("response pattern mismatch at offset %d", off)
	}
	return nil
}
