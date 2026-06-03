package scenarios

import (
	"encoding/binary"
	"fmt"
	"net"
	"sort"
	"sync"
	"testing"
	"time"
)

// StreamShape configures a server-paced trickle-streaming workload: a set
// of concurrent long-lived streams, each driven by a WorkloadServer in
// ServerStream mode. It is deliberately a separate type from
// WorkloadShape because streaming measures a different metric family
// (start latency, inter-chunk jitter, fairness, goodput) and needs its
// own runner and result path rather than the request/response RTT loop.
type StreamShape struct {
	// Streams is the number of concurrent streams (one goroutine each).
	// Every stream runs simultaneously after a start barrier, so this is
	// also the fan-out width under which fairness is measured.
	Streams int

	// NumTargets is the count of distinct WorkloadServer targets the
	// streams fan out across (round-robin). Streaming is SOCKS5-only, so
	// NumTargets may exceed 1.
	NumTargets int

	// TrickleInterval is the server's pacing gap between successive
	// chunks. The first chunk is pushed immediately on stream start; the
	// remaining StreamChunks-1 are spaced by this interval.
	TrickleInterval time.Duration

	// ProcessingDelay is the server's initial think time before the first
	// chunk is pushed — a deterministic, subtractable stand-in for a
	// backend that computes initial state before it starts streaming.
	// Zero means the first chunk is pushed immediately, in which case
	// first-response latency degenerates to a warm round-trip.
	ProcessingDelay time.Duration

	// TrickleChunkSize is the application payload bytes per chunk
	// (excluding the 4-byte per-chunk sequence header the protocol adds).
	TrickleChunkSize int

	// StreamChunks is the number of chunks each stream receives before
	// the server sends its clean stream-end frame.
	StreamChunks int

	// Mode is the sender mode; streaming requires ModeSOCKS5.
	Mode SenderMode

	// RepeatRounds runs the whole barrier-and-stream cycle this many
	// times (default 1).
	RepeatRounds int
}

func (s StreamShape) serverBehavior() ServerBehavior {
	return ServerBehavior{
		Mode:             ServerStream,
		TrickleInterval:  s.TrickleInterval,
		TrickleChunkSize: s.TrickleChunkSize,
		StreamChunks:     s.StreamChunks,
		ProcessingDelay:  s.ProcessingDelay,
	}
}

// validate checks the stream-shape invariants the runner relies on, after
// the zero-value defaults (Streams, NumTargets, RepeatRounds) are filled.
func (s StreamShape) validate() error {
	if s.Streams < 1 {
		return fmt.Errorf("StreamShape.Streams must be >= 1, got %d", s.Streams)
	}
	if s.StreamChunks < 1 {
		return fmt.Errorf("StreamChunks must be >= 1, got %d", s.StreamChunks)
	}
	if s.TrickleChunkSize < 1 {
		return fmt.Errorf("TrickleChunkSize must be >= 1, got %d", s.TrickleChunkSize)
	}
	if s.TrickleInterval < 0 {
		return fmt.Errorf("TrickleInterval must be >= 0, got %v", s.TrickleInterval)
	}
	if s.NumTargets < 1 {
		return fmt.Errorf("NumTargets must be >= 1, got %d", s.NumTargets)
	}
	if s.Mode != ModeSOCKS5 {
		return fmt.Errorf("streaming workloads require ModeSOCKS5 (fan-out over distinct targets), got %v", s.Mode)
	}
	return nil
}

func runStreamWorkload(t *testing.T, b Backend, s StreamShape) {
	t.Helper()
	AssertNoLeaks(t)
	if s.Streams <= 0 {
		s.Streams = 1
	}
	if s.NumTargets <= 0 {
		s.NumTargets = 1
	}
	if s.RepeatRounds <= 0 {
		s.RepeatRounds = 1
	}
	if err := s.validate(); err != nil {
		t.Fatalf("invalid StreamShape: %v", err)
	}
	addrs := make([]string, s.NumTargets)
	behavior := s.serverBehavior()
	for i := range addrs {
		addrs[i] = StartWorkloadServer(t, behavior).Addr()
	}
	tun := b.Setup(t, SetupOptions{NumListeners: 1, SenderMode: s.Mode, AllowedTargets: addrs})
	threshold := b.ConnectLatencyThreshold()
	for r := 0; r < s.RepeatRounds; r++ {
		if s.RepeatRounds > 1 {
			t.Logf("--- stream round %d/%d ---", r+1, s.RepeatRounds)
		}
		runStreamRound(t, tun, addrs, threshold, s)
	}
}

// streamResult is one stream goroutine's outcome. All time offsets are
// measured from a single shared release instant so per-stream completion
// times are directly comparable for the fairness metrics (a per-stream t0
// would fold goroutine-wakeup skew into the spread).
type streamResult struct {
	firstResp       time.Duration   // first chunk fully read, relative to release
	interGaps       []time.Duration // gaps between successive chunk arrivals
	lastChunkOffset time.Duration   // last data chunk arrival, relative to release
	endOffset       time.Duration   // clean stream-end frame arrival, relative to release
	payloadBytes    int             // application bytes delivered (excludes framing/seq header)
	err             error
}

func runStreamRound(t *testing.T, tun *Tunnel, addrs []string, threshold time.Duration, s StreamShape) {
	results := make([]streamResult, s.Streams)
	conns := make([]net.Conn, s.Streams)

	budget := streamBudget(threshold, s)
	start := time.Now()
	roundDeadline := start.Add(budget)

	// Phase 1: every stream dials and completes its SOCKS5 CONNECT, then
	// reports ready (or failure) and blocks on the release barrier. Phase
	// 2: release fires once all streams are connected, so the trickle and
	// its fairness/jitter metrics measure steady-state behavior under
	// simultaneous load rather than dial/rendezvous ordering skew.
	//
	// releaseTime is written once by the coordinator before close(release)
	// and read by every stream only after it observes the closed channel,
	// so the channel close supplies the happens-before edge — no extra
	// synchronization (or fragile global) is needed.
	var ready sync.WaitGroup
	var done sync.WaitGroup
	release := make(chan struct{})
	var releaseTime time.Time
	ready.Add(s.Streams)
	done.Add(s.Streams)

	for i := 0; i < s.Streams; i++ {
		go func(i int) {
			defer done.Done()
			target := addrs[i%len(addrs)]

			dialTimeout := time.Until(roundDeadline)
			if dialTimeout > 60*time.Second {
				dialTimeout = 60 * time.Second
			}
			if dialTimeout <= 0 {
				results[i] = streamResult{err: fmt.Errorf("stream[%d]: round budget exhausted before dial", i)}
				ready.Done()
				return
			}
			conn, err := DialSOCKS5(tun.SenderAddr, target, dialTimeout)
			if err != nil {
				results[i] = streamResult{err: fmt.Errorf("stream[%d] dial: %w", i, err)}
				ready.Done()
				return
			}
			conns[i] = conn
			// DialSOCKS5 clears the deadline on a successful CONNECT, so
			// re-arm it (clamped to the round) to bound the whole stream.
			if err := conn.SetDeadline(roundDeadline); err != nil {
				results[i] = streamResult{err: fmt.Errorf("stream[%d] set deadline: %w", i, err)}
				ready.Done()
				return
			}

			// Connected: report ready, then wait for the simultaneous
			// release. A failed stream above never reaches here, so it
			// cannot stall the barrier; ready.Wait() always completes
			// within the (bounded) dial timeout.
			ready.Done()
			<-release

			results[i] = runOneStream(conn, s, releaseTime)
		}(i)
	}

	ready.Wait()
	releaseTime = time.Now()
	close(release)
	done.Wait()
	wall := time.Since(start)

	for i := range conns {
		if conns[i] != nil {
			_ = conns[i].Close() //nolint:errcheck // best-effort cleanup
		}
	}

	m := aggregateStreams(results, s, wall)
	logStreamSummary(t, s, addrs, m)
	if m.successN < s.Streams {
		t.Errorf("%d/%d streams failed", s.Streams-m.successN, s.Streams)
	}
	recordStreamMatrixRow(t.Name(), m)

	const sanityEpsilon = 2 * time.Second
	if wall > budget+sanityEpsilon {
		t.Fatalf("sanity: stream round wall=%v exceeded budget=%v + epsilon=%v (shape: %+v)", wall, budget, sanityEpsilon, s)
	}
}

// runOneStream drives one stream after the barrier release: it sends the
// start frame, then reads StreamChunks chunks (verifying sequence,
// length, and the continuous nonce-keyed pattern) followed by a clean
// stream-end frame. All offsets are relative to the shared release time.
func runOneStream(conn net.Conn, s StreamShape, release time.Time) streamResult {
	nonce := randNonce()
	if err := writeFrame(conn, frameStreamStart, nonce, nil); err != nil {
		return streamResult{err: fmt.Errorf("write start: %w", err)}
	}

	res := streamResult{interGaps: make([]time.Duration, 0, s.StreamChunks)}
	var prevArrival time.Time
	for seq := 0; seq < s.StreamChunks; seq++ {
		typ, rnonce, payload, err := readFrame(conn)
		arrival := time.Now()
		if err != nil {
			res.err = fmt.Errorf("read chunk %d: %w", seq, err)
			return res
		}
		if typ != frameStreamChunk {
			res.err = fmt.Errorf("chunk %d: unexpected frame type %d", seq, typ)
			return res
		}
		if rnonce != nonce {
			res.err = fmt.Errorf("chunk %d: nonce %d != stream nonce %d", seq, rnonce, nonce)
			return res
		}
		if len(payload) != chunkSeqLen+s.TrickleChunkSize {
			res.err = fmt.Errorf("chunk %d: length %d != want %d", seq, len(payload), chunkSeqLen+s.TrickleChunkSize)
			return res
		}
		if gotSeq := binary.BigEndian.Uint32(payload[0:chunkSeqLen]); gotSeq != uint32(seq) {
			res.err = fmt.Errorf("chunk out of order: got seq %d want %d", gotSeq, seq)
			return res
		}
		if off, ok := verifyPattern(payload[chunkSeqLen:], nonce, seq*s.TrickleChunkSize); !ok {
			res.err = fmt.Errorf("chunk %d: pattern mismatch at offset %d", seq, off)
			return res
		}
		res.payloadBytes += s.TrickleChunkSize

		offset := arrival.Sub(release)
		if seq == 0 {
			res.firstResp = offset
		} else {
			res.interGaps = append(res.interGaps, arrival.Sub(prevArrival))
		}
		res.lastChunkOffset = offset
		prevArrival = arrival
	}

	typ, _, _, err := readFrame(conn)
	if err != nil {
		res.err = fmt.Errorf("read stream-end: %w", err)
		return res
	}
	if typ != frameStreamEnd {
		res.err = fmt.Errorf("expected stream-end frame, got type %d", typ)
		return res
	}
	res.endOffset = time.Since(release)
	return res
}

// streamMetrics is the aggregate of one stream round, the streaming
// counterpart to the RTT family's cold/warm distribution.
type streamMetrics struct {
	firstRespP50       time.Duration // start latency (first chunk), median across streams
	firstRespP95       time.Duration // start latency tail
	gapP95             time.Duration // pooled inter-chunk gap p95 (uniform degradation)
	maxStreamGapP95    time.Duration // worst single stream's gap p95 (starvation-sensitive)
	maxGap             time.Duration // largest single inter-chunk gap (diagnostic)
	completionSpread   time.Duration // spread of clean stream-end arrivals
	finalChunkSpread   time.Duration // spread of final data-chunk arrivals (data fairness)
	goodputBytesPerSec int64         // total payload bytes / active stream window
	streamN            int
	successN           int
	wall               time.Duration
}

// aggregateStreams distils per-stream results into the round's streaming
// metrics. Only successful streams contribute to the distributions.
func aggregateStreams(results []streamResult, s StreamShape, wall time.Duration) streamMetrics {
	m := streamMetrics{streamN: s.Streams, wall: wall}
	var firstResps []time.Duration
	var pooledGaps []time.Duration
	var lastOffsets, endOffsets []time.Duration
	var totalBytes int
	var windowEnd time.Duration // latest final-chunk arrival across streams (active window end)

	for i := range results {
		r := results[i]
		if r.err != nil {
			continue
		}
		m.successN++
		firstResps = append(firstResps, r.firstResp)
		pooledGaps = append(pooledGaps, r.interGaps...)
		lastOffsets = append(lastOffsets, r.lastChunkOffset)
		endOffsets = append(endOffsets, r.endOffset)
		totalBytes += r.payloadBytes
		if r.lastChunkOffset > windowEnd {
			windowEnd = r.lastChunkOffset
		}
		if g := maxDur(r.interGaps); g > m.maxGap {
			m.maxGap = g
		}
		if p := repr(r.interGaps, 0.95); p > m.maxStreamGapP95 {
			m.maxStreamGapP95 = p
		}
	}

	m.firstRespP50 = repr(firstResps, 0.50)
	m.firstRespP95 = repr(firstResps, 0.95)
	m.gapP95 = repr(pooledGaps, 0.95)
	m.completionSpread = spread(endOffsets)
	m.finalChunkSpread = spread(lastOffsets)
	// Goodput uses the active stream window (release → last chunk
	// delivered), not the full round wall, so it does not contradict the
	// barrier's exclusion of pre-stream dial skew.
	if windowEnd > 0 {
		m.goodputBytesPerSec = int64(float64(totalBytes) / windowEnd.Seconds())
	}
	return m
}

func logStreamSummary(t *testing.T, s StreamShape, addrs []string, m streamMetrics) {
	t.Logf("stream-summary scenario=%s first_resp_p50=%v first_resp_p95=%v gap_p95=%v max_stream_gap_p95=%v max_gap=%v final_chunk_spread=%v completion_spread=%v goodput_bytes_per_sec=%d wall=%v streams=%d num_targets=%d success=%d/%d",
		t.Name(), m.firstRespP50, m.firstRespP95, m.gapP95, m.maxStreamGapP95, m.maxGap, m.finalChunkSpread, m.completionSpread, m.goodputBytesPerSec,
		m.wall, s.Streams, len(addrs), m.successN, s.Streams)
}

// streamBudget bounds a stream round's wall time. It covers the
// concurrent cold connect (a couple of connect thresholds — all streams
// dial at once, so this is not multiplied by stream count), the server's
// initial think time before first output, its total trickle duration
// with a generous safety factor for tunnel and scheduler jitter, plus
// fixed slack, with a 60s floor. The first chunk ships immediately after
// ProcessingDelay and only the gaps between subsequent chunks cost a
// TrickleInterval, so the trickle term uses StreamChunks-1 intervals.
func streamBudget(threshold time.Duration, s StreamShape) time.Duration {
	connect := 2 * threshold
	intervals := s.StreamChunks - 1
	if intervals < 0 {
		intervals = 0
	}
	trickle := time.Duration(intervals) * s.TrickleInterval
	budget := connect + s.ProcessingDelay + trickle*3 + 10*time.Second
	if budget < 60*time.Second {
		budget = 60 * time.Second
	}
	return budget
}

func maxDur(ds []time.Duration) time.Duration {
	var max time.Duration
	for _, d := range ds {
		if d > max {
			max = d
		}
	}
	return max
}

// spread returns max−min of a sample (0 for fewer than two elements).
func spread(ds []time.Duration) time.Duration {
	if len(ds) < 2 {
		return 0
	}
	sorted := make([]time.Duration, len(ds))
	copy(sorted, ds)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	return sorted[len(sorted)-1] - sorted[0]
}
