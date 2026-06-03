package scenarios

import (
	"encoding/binary"
	"net"
	"testing"
	"time"
)

// streamShape returns a small, fast StreamShape suitable for offline
// runOneStream tests (no trickle delay, single stream).
func streamShape(chunks, chunkSize int) StreamShape {
	return StreamShape{
		Streams:          1,
		NumTargets:       1,
		TrickleInterval:  0,
		TrickleChunkSize: chunkSize,
		StreamChunks:     chunks,
		Mode:             ModeSOCKS5,
		RepeatRounds:     1,
	}
}

// pipePair returns a connected client/server net.Pipe with test-scoped
// deadlines so a protocol bug fails fast instead of hanging the suite.
func pipePair(t *testing.T) (cli, srv net.Conn) {
	t.Helper()
	cli, srv = net.Pipe()
	deadline := time.Now().Add(5 * time.Second)
	_ = cli.SetDeadline(deadline)
	_ = srv.SetDeadline(deadline)
	t.Cleanup(func() { _ = cli.Close(); _ = srv.Close() })
	return cli, srv
}

func TestRunOneStream_HappyPath(t *testing.T) {
	const chunks, chunkSize = 5, 256
	s := streamShape(chunks, chunkSize)
	cli, srv := pipePair(t)

	srvErr := make(chan error, 1)
	go func() { srvErr <- serveStream(srv, s.serverBehavior()) }()

	release := time.Now()
	res := runOneStream(cli, s, release)
	if res.err != nil {
		t.Fatalf("runOneStream: %v", res.err)
	}
	if err := <-srvErr; err != nil {
		t.Fatalf("serveStream: %v", err)
	}
	if res.payloadBytes != chunks*chunkSize {
		t.Errorf("payloadBytes=%d want %d", res.payloadBytes, chunks*chunkSize)
	}
	if got := len(res.interGaps); got != chunks-1 {
		t.Errorf("interGaps=%d want %d", got, chunks-1)
	}
	if res.firstResp <= 0 {
		t.Errorf("firstResp=%v want > 0", res.firstResp)
	}
	if res.endOffset < res.lastChunkOffset {
		t.Errorf("endOffset=%v before lastChunkOffset=%v", res.endOffset, res.lastChunkOffset)
	}
}

// streamServer runs a server-side closure against the server end of a pipe
// and returns its error on a channel, so tests can craft adversarial
// frame sequences runOneStream must reject.
func streamServer(srv net.Conn, fn func(c net.Conn, nonce uint64) error) <-chan error {
	ch := make(chan error, 1)
	go func() {
		typ, nonce, _, err := readFrame(srv)
		if err != nil {
			ch <- err
			return
		}
		if typ != frameStreamStart {
			ch <- errBadMagic
			return
		}
		ch <- fn(srv, nonce)
	}()
	return ch
}

func TestRunOneStream_DetectsCorruptChunk(t *testing.T) {
	const chunkSize = 64
	s := streamShape(3, chunkSize)
	cli, srv := pipePair(t)

	streamServer(srv, func(c net.Conn, nonce uint64) error {
		chunk := make([]byte, chunkSeqLen+chunkSize)
		binary.BigEndian.PutUint32(chunk[0:chunkSeqLen], 0)
		fillPattern(chunk[chunkSeqLen:], nonce, 0)
		chunk[chunkSeqLen+10] ^= 0xFF // flip a payload byte
		return writeFrame(c, frameStreamChunk, nonce, chunk)
	})

	res := runOneStream(cli, s, time.Now())
	if res.err == nil {
		t.Fatal("expected pattern-mismatch error, got nil")
	}
}

func TestRunOneStream_DetectsReorder(t *testing.T) {
	const chunkSize = 64
	s := streamShape(3, chunkSize)
	cli, srv := pipePair(t)

	streamServer(srv, func(c net.Conn, nonce uint64) error {
		// Send seq 1 first instead of seq 0.
		chunk := make([]byte, chunkSeqLen+chunkSize)
		binary.BigEndian.PutUint32(chunk[0:chunkSeqLen], 1)
		fillPattern(chunk[chunkSeqLen:], nonce, 1*chunkSize)
		return writeFrame(c, frameStreamChunk, nonce, chunk)
	})

	res := runOneStream(cli, s, time.Now())
	if res.err == nil {
		t.Fatal("expected out-of-order error, got nil")
	}
}

func TestRunOneStream_DetectsTruncation(t *testing.T) {
	const chunkSize = 64
	s := streamShape(3, chunkSize)
	cli, srv := pipePair(t)

	streamServer(srv, func(c net.Conn, nonce uint64) error {
		// Send one valid chunk then close before the rest.
		chunk := make([]byte, chunkSeqLen+chunkSize)
		binary.BigEndian.PutUint32(chunk[0:chunkSeqLen], 0)
		fillPattern(chunk[chunkSeqLen:], nonce, 0)
		if err := writeFrame(c, frameStreamChunk, nonce, chunk); err != nil {
			return err
		}
		return c.Close()
	})

	res := runOneStream(cli, s, time.Now())
	if res.err == nil {
		t.Fatal("expected truncation read error, got nil")
	}
}

func TestRunOneStream_RejectsMissingStreamEnd(t *testing.T) {
	const chunkSize = 32
	s := streamShape(1, chunkSize)
	cli, srv := pipePair(t)

	streamServer(srv, func(c net.Conn, nonce uint64) error {
		chunk := make([]byte, chunkSeqLen+chunkSize)
		binary.BigEndian.PutUint32(chunk[0:chunkSeqLen], 0)
		fillPattern(chunk[chunkSeqLen:], nonce, 0)
		if err := writeFrame(c, frameStreamChunk, nonce, chunk); err != nil {
			return err
		}
		// Send a stray chunk where the stream-end frame is expected.
		return writeFrame(c, frameStreamChunk, nonce, chunk)
	})

	res := runOneStream(cli, s, time.Now())
	if res.err == nil {
		t.Fatal("expected missing-stream-end error, got nil")
	}
}

func TestStreamShape_Validate(t *testing.T) {
	base := streamShape(4, 128)
	if err := base.validate(); err != nil {
		t.Fatalf("valid shape rejected: %v", err)
	}
	cases := []struct {
		name string
		mut  func(*StreamShape)
	}{
		{"zero streams", func(s *StreamShape) { s.Streams = 0 }},
		{"zero chunks", func(s *StreamShape) { s.StreamChunks = 0 }},
		{"zero chunk size", func(s *StreamShape) { s.TrickleChunkSize = 0 }},
		{"negative interval", func(s *StreamShape) { s.TrickleInterval = -1 }},
		{"zero targets", func(s *StreamShape) { s.NumTargets = 0 }},
		{"non-socks5 mode", func(s *StreamShape) { s.Mode = ModePortForward }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := base
			tc.mut(&s)
			if err := s.validate(); err == nil {
				t.Errorf("%s: expected validation error", tc.name)
			}
		})
	}
}

func TestAggregateStreams_MetricsAndSuccessCount(t *testing.T) {
	ms := time.Millisecond
	results := []streamResult{
		{firstResp: 10 * ms, interGaps: []time.Duration{20 * ms, 22 * ms}, lastChunkOffset: 52 * ms, endOffset: 53 * ms, payloadBytes: 1000},
		{firstResp: 14 * ms, interGaps: []time.Duration{21 * ms, 40 * ms}, lastChunkOffset: 75 * ms, endOffset: 76 * ms, payloadBytes: 1000},
		{err: errBadMagic}, // failed stream must not contribute
	}
	s := StreamShape{Streams: 3}
	m := aggregateStreams(results, s, 100*ms)

	if m.streamN != 3 {
		t.Errorf("streamN=%d want 3", m.streamN)
	}
	if m.successN != 2 {
		t.Errorf("successN=%d want 2", m.successN)
	}
	// finalChunkSpread = 75ms - 52ms = 23ms across the two successful streams.
	if m.finalChunkSpread != 23*ms {
		t.Errorf("finalChunkSpread=%v want %v", m.finalChunkSpread, 23*ms)
	}
	// completionSpread = 76ms - 53ms = 23ms.
	if m.completionSpread != 23*ms {
		t.Errorf("completionSpread=%v want %v", m.completionSpread, 23*ms)
	}
	// maxGap is the largest single inter-chunk gap (40ms).
	if m.maxGap != 40*ms {
		t.Errorf("maxGap=%v want %v", m.maxGap, 40*ms)
	}
	// goodput = 2000 bytes / windowEnd(75ms) ≈ 26666 B/s.
	wantGoodput := int64(float64(2000) / (75 * ms).Seconds())
	if m.goodputBytesPerSec != wantGoodput {
		t.Errorf("goodputBytesPerSec=%d want %d", m.goodputBytesPerSec, wantGoodput)
	}
}

func TestSpread(t *testing.T) {
	ms := time.Millisecond
	if got := spread(nil); got != 0 {
		t.Errorf("spread(nil)=%v want 0", got)
	}
	if got := spread([]time.Duration{5 * ms}); got != 0 {
		t.Errorf("spread(single)=%v want 0", got)
	}
	if got := spread([]time.Duration{5 * ms, 2 * ms, 9 * ms}); got != 7*ms {
		t.Errorf("spread=%v want %v", got, 7*ms)
	}
}

func TestMaxDur(t *testing.T) {
	ms := time.Millisecond
	if got := maxDur(nil); got != 0 {
		t.Errorf("maxDur(nil)=%v want 0", got)
	}
	if got := maxDur([]time.Duration{3 * ms, 9 * ms, 1 * ms}); got != 9*ms {
		t.Errorf("maxDur=%v want %v", got, 9*ms)
	}
}

func TestStreamBudget(t *testing.T) {
	// Small shape: budget pinned to the 60s floor.
	small := StreamShape{StreamChunks: 5, TrickleInterval: time.Millisecond}
	if got := streamBudget(time.Second, small); got != 60*time.Second {
		t.Errorf("small budget=%v want 60s floor", got)
	}
	// Large trickle: budget exceeds the floor and scales with the
	// (StreamChunks-1) inter-chunk intervals (the first chunk is immediate).
	large := StreamShape{StreamChunks: 100, TrickleInterval: time.Second}
	got := streamBudget(time.Second, large)
	want := 2*time.Second + 99*time.Second*3 + 10*time.Second
	if got != want {
		t.Errorf("large budget=%v want %v", got, want)
	}
}
