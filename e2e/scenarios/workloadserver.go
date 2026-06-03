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

	// probeMu guards probeStats (ServerProbe mode only). Each probe
	// connection is keyed by its stream nonce so a test holding a
	// probeFlow can read the server's last-seen sequence and terminating
	// error to localize a break the client only sees as silence.
	probeMu    sync.Mutex
	probeStats map[uint64]*probeConnStat
}

// ServerMode selects a WorkloadServer's per-connection behavior. The zero
// value is ServerEcho so a zero-value ServerBehavior reproduces the
// legacy plain-echo target exactly.
type ServerMode int

const (
	ServerEcho ServerMode = iota
	ServerRespond
	ServerStream
	// ServerProbe is a continuous full-duplex request/response loop used
	// by the held-flow topology scenarios and (later) a bidirectional
	// perf shape. Each request frame carries a sequence number and the
	// client's monotonic send timestamp; the server echoes both back
	// alongside its own receive and send timestamps, so the client can
	// split the round-trip into request leg, server think, and response
	// leg. Unlike ServerEcho it is framed, so it is never the byte-
	// transparency oracle.
	ServerProbe
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
	switch behavior.Mode {
	case ServerEcho, ServerRespond, ServerStream, ServerProbe:
	default:
		t.Fatalf("workload server: unknown ServerBehavior.Mode %d", behavior.Mode)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("workload server listen: %v", err)
	}
	s := &WorkloadServer{
		behavior:   behavior,
		ln:         ln,
		serveDone:  make(chan struct{}),
		probeStats: make(map[uint64]*probeConnStat),
	}
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
	case ServerProbe:
		_ = s.serveProbe(c)
	default:
		// Modes are validated at construction (StartWorkloadServer); a
		// value reaching here means the server was built another way with
		// an invalid Mode. Fail loudly rather than silently closing.
		panic(fmt.Sprintf("workload server: unknown ServerBehavior.Mode %d", s.behavior.Mode))
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
	frameProbeReq    byte = 7 // client -> server (ServerProbe)
	frameProbeResp   byte = 8 // server -> client (ServerProbe)
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
// stream's deterministic payload pattern. Falls back to a process-monotonic
// counter if the system RNG is unavailable: the nonce only needs to be
// distinct within this process (client and server are the same test binary),
// not unpredictable, and the counter is strictly distinct even under
// concurrency or coarse clock resolution.
func randNonce() uint64 {
	var b [8]byte
	if _, err := crand.Read(b[:]); err != nil {
		return nonceFallbackSeq.Add(1)
	}
	return binary.BigEndian.Uint64(b[:])
}

// nonceFallbackSeq guarantees distinct nonces on the crypto/rand fallback path.
var nonceFallbackSeq atomic.Uint64

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

// probeConnStat is the server's per-connection record for one ServerProbe
// flow, keyed by the flow's nonce. It exists so that when a flow breaks
// and the client only observes silence, the test can ask the server what
// it last saw and localize the break: no record at all means the request
// never reached the server; lastSeqSeen set with no matching write means
// the server stalled before replying; lastWriteStart set without
// lastWriteDone means the server->tunnel write itself blocked.
//
// All fields and the terminal flag are written under WorkloadServer.probeMu.
// Timestamps are probeNanos() values (one shared monotonic epoch across the
// whole test process), directly comparable with the client's stamps.
type probeConnStat struct {
	lastSeqSeen    int64 // highest request seq read (-1 before the first request)
	lastRecvNano   int64 // probeNanos at the read of lastSeqSeen's request
	lastWriteStart int64 // probeNanos just before writing lastSeqSeen's response
	lastWriteDone  int64 // probeNanos just after that write returned
	termErr        error // serveProbe's terminating error (nil on clean EOF)
	terminal       bool  // true once serveProbe has returned for this conn
}

// ProbeRecord returns a snapshot copy of the server's record for the
// probe flow identified by nonce, and whether such a record exists. The
// terminal field reports whether the server has finished with the
// connection; a non-terminal record is a live snapshot and its
// lastSeqSeen may still advance.
//
// ProbeRecord is non-destructive: the entry stays in probeStats until
// the WorkloadServer is stopped. Diagnostic consumers that only need
// the record once should prefer ConsumeProbeRecord so the entry does
// not linger for the rest of the server's lifetime — important when
// many short-lived probes (e.g., probeOnce dials) share one server.
func (s *WorkloadServer) ProbeRecord(nonce uint64) (probeConnStat, bool) {
	s.probeMu.Lock()
	defer s.probeMu.Unlock()
	st, ok := s.probeStats[nonce]
	if !ok {
		return probeConnStat{}, false
	}
	return *st, true
}

// ConsumeProbeRecord is like ProbeRecord but removes the entry from
// probeStats after returning the snapshot. It is the right call from a
// diagnostic path that reports a record exactly once (probeFlow.Dump /
// localize) so the record does not linger for the rest of the server's
// lifetime.
//
// Note: probeOnce dials do not consume their record. In test-only
// servers with single-test lifetimes the resulting accumulation is
// bounded by the test duration; long-running scenarios that hammer
// probeOnce should consider a periodic eviction strategy.
func (s *WorkloadServer) ConsumeProbeRecord(nonce uint64) (probeConnStat, bool) {
	s.probeMu.Lock()
	defer s.probeMu.Unlock()
	st, ok := s.probeStats[nonce]
	if !ok {
		return probeConnStat{}, false
	}
	delete(s.probeStats, nonce)
	return *st, true
}

// serveProbe runs the ServerProbe per-connection loop: read a request
// frame, stamp the receive time, verify its pattern, optionally think,
// then reply with a frame echoing the request's seq and client-send
// timestamp plus the server's receive and send timestamps and RespSize
// bytes of the nonce's pattern. The per-connection record is published
// under probeMu so a test can localize a break by nonce.
func (s *WorkloadServer) serveProbe(c net.Conn) error {
	b := s.behavior
	var (
		nonce    uint64
		stat     *probeConnStat
		haveStat bool
		retErr   error
	)
	defer func() {
		if haveStat {
			s.probeMu.Lock()
			stat.termErr = retErr
			stat.terminal = true
			s.probeMu.Unlock()
		}
	}()

	for {
		typ, n, payload, err := readFrame(c)
		recv := probeNanos()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			retErr = err
			return err
		}
		if typ != frameProbeReq {
			retErr = fmt.Errorf("serveProbe: unexpected frame type %d", typ)
			return retErr
		}
		if len(payload) < probeHdrLen {
			retErr = fmt.Errorf("serveProbe: short probe header: %d < %d", len(payload), probeHdrLen)
			return retErr
		}
		if !haveStat {
			nonce = n
			stat = &probeConnStat{lastSeqSeen: -1}
			s.probeMu.Lock()
			s.probeStats[nonce] = stat
			s.probeMu.Unlock()
			haveStat = true
		} else if n != nonce {
			// A probe connection is keyed by the first frame's nonce; a
			// later frame with a different nonce would otherwise be
			// verified and stat-recorded against the original nonce,
			// silently producing confusing integrity failures and
			// misleading localization data. Reject the connection.
			retErr = fmt.Errorf("serveProbe: nonce changed mid-connection: got %d want %d", n, nonce)
			return retErr
		}
		seq := binary.BigEndian.Uint32(payload[0:4])
		// Enforce strict per-connection monotonic seq. lastSeqSeen starts
		// at -1, so the first request (seq=0) passes; thereafter only
		// strictly greater seqs are accepted. Without this, a buggy
		// client repeating or decreasing seq would regress the stat
		// record and produce misleading localize() output. Read
		// lastSeqSeen under probeMu since ProbeRecord() may snapshot
		// the struct concurrently from the test goroutine.
		s.probeMu.Lock()
		prevSeen := stat.lastSeqSeen
		s.probeMu.Unlock()
		if int64(seq) <= prevSeen {
			_ = writeFrame(c, frameError, nonce, nil)
			retErr = fmt.Errorf("serveProbe: non-monotonic seq %d (last seen %d)", seq, prevSeen)
			return retErr
		}
		reqBody := payload[probeHdrLen:]
		if off, ok := verifyPattern(reqBody, nonce, int(seq)*len(reqBody)); !ok {
			_ = writeFrame(c, frameError, nonce, nil)
			retErr = fmt.Errorf("serveProbe: request pattern mismatch at offset %d (seq %d)", off, seq)
			return retErr
		}

		s.probeMu.Lock()
		stat.lastSeqSeen = int64(seq)
		stat.lastRecvNano = recv
		s.probeMu.Unlock()

		if b.ProcessingDelay > 0 {
			time.Sleep(b.ProcessingDelay)
		}

		// Response payload: [seq | clientSend | serverRecv | serverSend | pattern].
		resp := make([]byte, probeHdrLen+b.RespSize)
		copy(resp[0:4], payload[0:4])   // echo seq
		copy(resp[4:12], payload[4:12]) // echo client-send timestamp
		binary.BigEndian.PutUint64(resp[12:20], uint64(recv))
		fillPattern(resp[probeHdrLen:], nonce, int(seq)*b.RespSize)

		writeStart := probeNanos()
		binary.BigEndian.PutUint64(resp[20:28], uint64(writeStart))
		s.probeMu.Lock()
		stat.lastWriteStart = writeStart
		s.probeMu.Unlock()

		if err := writeFrame(c, frameProbeResp, nonce, resp); err != nil {
			retErr = err
			return err
		}
		writeDone := probeNanos()
		s.probeMu.Lock()
		stat.lastWriteDone = writeDone
		s.probeMu.Unlock()
	}
}
