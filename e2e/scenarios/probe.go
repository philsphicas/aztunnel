package scenarios

import (
	"encoding/binary"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// The probe protocol is a continuous, sequenced, full-duplex
// request/response flow against a ServerProbe target. It exists so the
// held-flow topology scenarios (and, later, a bidirectional perf shape)
// carry observable traffic across a topology change instead of a single
// blind ping-pong: every exchange records a request leg, server think,
// and response leg, and a stalled or broken flow can be localized to the
// outbound or return path.
//
// All timestamps in the wire frames are probeNanos() values measured from
// one process-wide monotonic epoch. Client and target server run in the
// same test process on the same host, so these stamps are directly
// subtractable across the two ends; the monotonic epoch (rather than wall
// clock) makes the per-leg durations immune to NTP slews and never
// negative for in-order events.

// probe payload header layout, prepended to the pattern bytes of every
// probe request and response frame.
const (
	probeOffSeq       = 0  // uint32  request/echoed sequence number
	probeOffClient    = 4  // int64   client send timestamp (probeNanos)
	probeOffSrvRecv   = 12 // int64   server receive timestamp (response only)
	probeOffSrvSend   = 20 // int64   server send timestamp (response only)
	probeHdrLen       = 28 // = 4 + 8 + 8 + 8
	defaultProbeIntvl = 100 * time.Millisecond
	defaultProbeBody  = 64 // request/response body pattern bytes (excludes header)
)

// probeEpoch is the single monotonic reference shared by every probe
// timestamp in this test process. time.Since preserves the monotonic
// reading, so deltas are stable across the whole run.
var probeEpoch = time.Now()

func probeNanos() int64 { return int64(time.Since(probeEpoch)) }

func dur(n int64) time.Duration { return time.Duration(n) }

// ProbeConfig configures a probeFlow's cadence and payload sizes. The
// zero value is filled with calibrated defaults (100ms interval, 64-byte
// bodies, one outstanding request) by newProbeFlow.
type ProbeConfig struct {
	// Interval is the minimum gap between successive request writes. The
	// sender enforces it after each write via a time.Ticker. A zero
	// value is replaced with defaultProbeIntvl (100ms) by newProbeFlow;
	// callers that want unpaced (window-bound) sending must set a small
	// non-zero value such as time.Microsecond.
	Interval time.Duration

	// ReqSize / RespSize are the request and response body pattern bytes,
	// excluding the 28-byte probe header. RespSize MUST equal the target
	// WorkloadServer's ServerBehavior.RespSize.
	ReqSize  int
	RespSize int

	// MaxOutstanding bounds in-flight (sent but unacked) requests. The
	// topology scenarios use 1 so each exchange's legs are uncontaminated
	// by self-induced send-queue depth; the perf shape may widen it,
	// in which case more than one request may sit in flight between an
	// Interval tick and the matching ack.
	MaxOutstanding int

	// SampleSize, if > 0, enables per-exchange retention: probeFlow keeps
	// a bounded ring of the most-recent SampleSize exchanges so a
	// percentile aggregator (e.g., the duplex perf shape) can compute
	// p50/p95 over the steady-state tail. SampleSize == 0 keeps the
	// allocation-free rolling-aggregates-only behavior; topology
	// scenarios that only need maxes and a worst-by-RTT snapshot leave
	// it zero. "Most recent N" is a steady-state tail snapshot — not a
	// statistical reservoir — which is what diagnostic percentiles
	// want here.
	SampleSize int
}

// probeExchange is one completed request/response, with the round-trip
// split into its constituent legs. All durations share the monotonic
// epoch, so requestLeg + serverThink + responseLeg == rtt.
type probeExchange struct {
	seq         uint32
	arrival     time.Duration // clientRecv relative to epoch (for inter-ack gap)
	requestLeg  time.Duration // serverRecv - clientSend
	serverThink time.Duration // serverSend - serverRecv
	responseLeg time.Duration // clientRecv - serverSend
	rtt         time.Duration // clientRecv - clientSend
}

// probeFlow drives one held-open connection with a sender and a reader
// goroutine. broken() reports only a read-side break (EOF/RST/integrity
// failure); a write-side error is recorded for diagnostics but never
// marks the flow broken, because the property under test is whether the
// bridge survives, and the canonical signal for a torn bridge is a
// read-side close.
type probeFlow struct {
	conn  net.Conn
	nonce uint64
	cfg   ProbeConfig

	stopping atomic.Bool
	stopOnce sync.Once
	stopCh   chan struct{}
	slots    chan struct{}
	wg       sync.WaitGroup

	lastSeqSent atomic.Int64 // highest seq written (-1 before first)
	lastSeqAck  atomic.Int64 // highest seq acked (-1 before first)
	brokenFlag  atomic.Bool  // read-side break observed

	mu          sync.Mutex
	numExch     int64         // total exchanges completed
	maxReqLeg   time.Duration // rolling aggregates, updated under mu by reader
	maxRespLeg  time.Duration
	maxThink    time.Duration
	maxRTT      time.Duration
	maxGap      time.Duration   // largest gap between successive acked arrivals
	prevArrival time.Duration   // arrival of the previous exchange, for gap calc
	worstByRTT  probeExchange   // single highest-RTT exchange, for Dump
	samples     []probeExchange // ring of most-recent exchanges if cfg.SampleSize > 0
	sampleHead  int             // next write index into samples; samples[head] is oldest
	sampleFull  bool            // true once samples has wrapped at least once
	firstErr    error           // first read-side integrity/break error
	writeErr    error           // first write-side error (does not mark broken)
}

// newProbeFlow wraps an established connection in a probeFlow. It does
// not start traffic; call Start. The caller owns the connection's
// lifetime via Stop (which closes it).
func newProbeFlow(conn net.Conn, cfg ProbeConfig) *probeFlow {
	// Clamp non-positive values so callers can't accidentally request a
	// disabled ticker (Interval <= 0) which would spin the sender, or
	// a non-positive payload size which would panic make.
	if cfg.Interval <= 0 {
		cfg.Interval = defaultProbeIntvl
	}
	if cfg.ReqSize <= 0 {
		cfg.ReqSize = defaultProbeBody
	}
	if cfg.RespSize <= 0 {
		cfg.RespSize = defaultProbeBody
	}
	if cfg.MaxOutstanding < 1 {
		cfg.MaxOutstanding = 1
	}
	if cfg.SampleSize < 0 {
		cfg.SampleSize = 0
	}
	f := &probeFlow{
		conn:   conn,
		nonce:  randNonce(),
		cfg:    cfg,
		stopCh: make(chan struct{}),
		slots:  make(chan struct{}, cfg.MaxOutstanding),
	}
	if cfg.SampleSize > 0 {
		f.samples = make([]probeExchange, cfg.SampleSize)
	}
	f.lastSeqSent.Store(-1)
	f.lastSeqAck.Store(-1)
	return f
}

// Start launches the sender and reader goroutines.
func (f *probeFlow) Start() {
	f.wg.Add(2)
	go f.sender()
	go f.reader()
}

// Stop signals shutdown, closes the connection to unblock any in-flight
// read/write, and waits for both goroutines. It is idempotent. The
// reader may have already signalled done via recordReadErr; Stop's
// stopOnce coordinates with it so close(stopCh) and conn.Close are each
// invoked at most once.
func (f *probeFlow) Stop() {
	f.stopping.Store(true)
	f.signalDone()
	f.wg.Wait()
}

// signalDone closes stopCh and the underlying connection at most once,
// waking any goroutine selecting on stopCh (e.g. the duplex round loop)
// and unblocking peer-side read/writes that would otherwise hang until
// the runtime closed the conn at process exit. Called by both Stop()
// (external caller) and recordReadErr() (the flow itself observing a
// read-side break), so a broken flow doesn't have to wait for an
// external Stop before its waiters wake up.
func (f *probeFlow) signalDone() {
	f.stopOnce.Do(func() {
		close(f.stopCh)
		_ = f.conn.Close()
	})
}

// broken reports whether the flow has observed a read-side break. It is a
// pure atomic load and never touches the connection, so it is safe to
// call concurrently with the running flow and after Stop.
func (f *probeFlow) broken() bool { return f.brokenFlag.Load() }

// acked returns the highest sequence number acked so far (-1 if none).
func (f *probeFlow) acked() int64 { return f.lastSeqAck.Load() }

// waitFirstAck blocks until the flow has acked at least one exchange, the
// flow breaks, or the timeout elapses. It is used to confirm the bridge
// is established before a scenario proceeds.
func (f *probeFlow) waitFirstAck(timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if f.acked() >= 0 {
			return nil
		}
		if f.broken() {
			return fmt.Errorf("flow broke before first ack: %w", f.firstError())
		}
		// A write-side failure doesn't flip brokenFlag (broken is
		// read-side only by design), so check it explicitly here. The
		// sender goroutine will have already returned after recording
		// the error, so no further progress is possible.
		if we := f.writeError(); we != nil {
			return fmt.Errorf("flow write failed before first ack: %w", we)
		}
		time.Sleep(20 * time.Millisecond)
	}
	return fmt.Errorf("no ack within %v (sent up to seq %d)", timeout, f.lastSeqSent.Load())
}

func (f *probeFlow) firstError() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.firstErr
}

func (f *probeFlow) writeError() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.writeErr
}

// recordReadErr latches the first read-side error, marks the flow
// broken, and signals done so waiters (and the peer-side server) unblock
// promptly instead of waiting for an external Stop. recordWriteErr only
// latches the error — the flow is not considered broken on write failures
// because the canonical signal for a torn bridge is a read-side close.
func (f *probeFlow) recordReadErr(err error) {
	f.mu.Lock()
	if f.firstErr == nil {
		f.firstErr = err
	}
	f.mu.Unlock()
	f.brokenFlag.Store(true)
	f.signalDone()
}

func (f *probeFlow) recordWriteErr(err error) {
	f.mu.Lock()
	if f.writeErr == nil {
		f.writeErr = err
	}
	f.mu.Unlock()
}

func (f *probeFlow) sender() {
	defer f.wg.Done()
	var ticker *time.Ticker
	if f.cfg.Interval > 0 {
		ticker = time.NewTicker(f.cfg.Interval)
		defer ticker.Stop()
	}
	body := f.cfg.ReqSize
	var seq uint32
	for {
		select {
		case <-f.stopCh:
			return
		default:
		}
		// Acquire an outstanding-request slot (bounds in-flight depth).
		select {
		case f.slots <- struct{}{}:
		case <-f.stopCh:
			return
		}

		buf := make([]byte, probeHdrLen+body)
		binary.BigEndian.PutUint32(buf[probeOffSeq:], seq)
		fillPattern(buf[probeHdrLen:], f.nonce, int(seq)*body)
		binary.BigEndian.PutUint64(buf[probeOffClient:], uint64(probeNanos()))
		if err := writeFrame(f.conn, frameProbeReq, f.nonce, buf); err != nil {
			if !f.stopping.Load() {
				f.recordWriteErr(fmt.Errorf("probe write seq %d: %w", seq, err))
			}
			return
		}
		f.lastSeqSent.Store(int64(seq))
		seq++

		if ticker != nil {
			select {
			case <-f.stopCh:
				return
			case <-ticker.C:
			}
		}
	}
}

func (f *probeFlow) reader() {
	defer f.wg.Done()
	// Close the connection on exit so the server's serveProbe goroutine
	// for this nonce unblocks promptly (especially under MaxOutstanding>1
	// where the server may be blocked writing a response). conn.Close is
	// idempotent, so this is safe even when Stop already closed it.
	defer func() { _ = f.conn.Close() }()
	for {
		typ, n, payload, err := readFrame(f.conn)
		clientRecv := probeNanos()
		if err != nil {
			if f.stopping.Load() {
				return
			}
			f.recordReadErr(fmt.Errorf("probe read: %w", err))
			return
		}
		if n != f.nonce {
			f.recordReadErr(fmt.Errorf("probe nonce %d != flow nonce %d", n, f.nonce))
			return
		}
		if typ == frameError {
			f.recordReadErr(fmt.Errorf("server reported integrity error (nonce %d)", n))
			return
		}
		if typ != frameProbeResp {
			f.recordReadErr(fmt.Errorf("unexpected probe frame type %d", typ))
			return
		}
		if len(payload) < probeHdrLen {
			f.recordReadErr(fmt.Errorf("short probe response header: %d < %d", len(payload), probeHdrLen))
			return
		}
		seq := binary.BigEndian.Uint32(payload[probeOffSeq:])
		if want := f.lastSeqAck.Load() + 1; int64(seq) != want {
			f.recordReadErr(fmt.Errorf("probe out of order: got seq %d want %d", seq, want))
			return
		}
		respBody := payload[probeHdrLen:]
		if len(respBody) != f.cfg.RespSize {
			f.recordReadErr(fmt.Errorf("probe resp body %d != want %d (seq %d)", len(respBody), f.cfg.RespSize, seq))
			return
		}
		if off, ok := verifyPattern(respBody, f.nonce, int(seq)*f.cfg.RespSize); !ok {
			f.recordReadErr(fmt.Errorf("probe resp pattern mismatch at offset %d (seq %d)", off, seq))
			return
		}

		clientSend := int64(binary.BigEndian.Uint64(payload[probeOffClient:]))
		srvRecv := int64(binary.BigEndian.Uint64(payload[probeOffSrvRecv:]))
		srvSend := int64(binary.BigEndian.Uint64(payload[probeOffSrvSend:]))
		ex := probeExchange{
			seq:         seq,
			arrival:     dur(clientRecv),
			requestLeg:  dur(srvRecv - clientSend),
			serverThink: dur(srvSend - srvRecv),
			responseLeg: dur(clientRecv - srvSend),
			rtt:         dur(clientRecv - clientSend),
		}
		// Update rolling aggregates incrementally — no slice growth, and
		// Summary stays O(1) even for long-running flows.
		f.mu.Lock()
		if ex.requestLeg > f.maxReqLeg {
			f.maxReqLeg = ex.requestLeg
		}
		if ex.responseLeg > f.maxRespLeg {
			f.maxRespLeg = ex.responseLeg
		}
		if ex.serverThink > f.maxThink {
			f.maxThink = ex.serverThink
		}
		if ex.rtt > f.maxRTT {
			f.maxRTT = ex.rtt
			f.worstByRTT = ex
		}
		if f.numExch > 0 {
			if gap := ex.arrival - f.prevArrival; gap > f.maxGap {
				f.maxGap = gap
			}
		}
		f.prevArrival = ex.arrival
		f.numExch++
		// Append into the bounded ring sample if enabled. Wrapping keeps
		// only the most-recent cfg.SampleSize exchanges.
		if f.samples != nil {
			f.samples[f.sampleHead] = ex
			f.sampleHead++
			if f.sampleHead >= len(f.samples) {
				f.sampleHead = 0
				f.sampleFull = true
			}
		}
		f.mu.Unlock()
		f.lastSeqAck.Store(int64(seq))

		// Release the slot the matching request consumed.
		<-f.slots
	}
}

// probeSummary is a snapshot of a flow's progress and worst-case legs,
// for logging and failure dumps. In PR1 the latencies are diagnostic
// only — never an assertion threshold.
type probeSummary struct {
	sent, acked int64
	exchanges   int64
	maxReqLeg   time.Duration
	maxRespLeg  time.Duration
	maxThink    time.Duration
	maxRTT      time.Duration
	maxGap      time.Duration // largest gap between successive acked arrivals
	worstByRTT  probeExchange // exchange that observed maxRTT (zero if none)
	broken      bool
	firstErr    error
	writeErr    error
}

func (f *probeFlow) Summary() probeSummary {
	f.mu.Lock()
	defer f.mu.Unlock()
	return probeSummary{
		sent:       f.lastSeqSent.Load(),
		acked:      f.lastSeqAck.Load(),
		exchanges:  f.numExch,
		maxReqLeg:  f.maxReqLeg,
		maxRespLeg: f.maxRespLeg,
		maxThink:   f.maxThink,
		maxRTT:     f.maxRTT,
		maxGap:     f.maxGap,
		worstByRTT: f.worstByRTT,
		broken:     f.brokenFlag.Load(),
		firstErr:   f.firstErr,
		writeErr:   f.writeErr,
	}
}

// Samples returns a chronological snapshot of the per-exchange ring
// buffer, oldest first. The slice is empty when SampleSize == 0 or no
// exchange has completed yet. Callers can safely retain the returned
// slice; it is a fresh copy.
func (f *probeFlow) Samples() []probeExchange {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.samples) == 0 {
		return nil
	}
	if !f.sampleFull {
		// samples[0:sampleHead] are the only valid entries, in order.
		out := make([]probeExchange, f.sampleHead)
		copy(out, f.samples[:f.sampleHead])
		return out
	}
	// Ring has wrapped: oldest entry is at sampleHead, newest is at
	// sampleHead-1 (mod len). Walk from oldest forward.
	out := make([]probeExchange, len(f.samples))
	n := copy(out, f.samples[f.sampleHead:])
	copy(out[n:], f.samples[:f.sampleHead])
	return out
}

// Dump logs a compact diagnostic: the client-side summary, the worst
// exchange, and — combining the client's last-acked seq with the server's
// per-nonce record — a localization of where a break or stall sat.
func (f *probeFlow) Dump(t *testing.T, srv *WorkloadServer, label string) {
	t.Helper()
	s := f.Summary()
	t.Logf("probe-dump %s: sent=%d acked=%d exchanges=%d broken=%v max_req_leg=%v max_resp_leg=%v max_think=%v max_rtt=%v max_gap=%v first_err=%v write_err=%v",
		label, s.sent, s.acked, s.exchanges, s.broken, s.maxReqLeg, s.maxRespLeg, s.maxThink, s.maxRTT, s.maxGap, s.firstErr, s.writeErr)
	if s.exchanges > 0 {
		w := s.worstByRTT
		t.Logf("probe-dump %s: worst-by-rtt seq=%d rtt=%v req_leg=%v server_think=%v resp_leg=%v",
			label, w.seq, w.rtt, w.requestLeg, w.serverThink, w.responseLeg)
	}
	t.Logf("probe-dump %s: %s", label, f.localize(srv))
}

// localize correlates the client's last-acked sequence with the server's
// last-seen record to attribute a break to the outbound (request) or
// return (response) path, mirroring the diagnostic ladder the design
// targets.
//
// localize consumes the server-side record (ConsumeProbeRecord) so it
// does not linger after the diagnostic has been emitted. It is intended
// to be called at most once per flow; subsequent calls will see no
// record and report "no record for nonce".
func (f *probeFlow) localize(srv *WorkloadServer) string {
	acked := f.lastSeqAck.Load()
	rec, ok := srv.ConsumeProbeRecord(f.nonce)
	if !ok {
		return fmt.Sprintf("server has no record for nonce %d: no request reached the server (outbound/request-leg break for seq %d)", f.nonce, acked+1)
	}
	state := "live"
	if rec.terminal {
		state = fmt.Sprintf("terminal(err=%v)", rec.termErr)
	}
	switch {
	case rec.lastSeqSeen <= acked:
		return fmt.Sprintf("server[%s] last_seq_seen=%d <= client_acked=%d: next request never reached the server (outbound/request-leg break for seq %d)",
			state, rec.lastSeqSeen, acked, acked+1)
	case rec.lastWriteStart == 0 || rec.lastWriteStart < rec.lastRecvNano:
		return fmt.Sprintf("server[%s] read seq %d (recv=%v) but had not started its reply (server-path stall; client_acked=%d)",
			state, rec.lastSeqSeen, dur(rec.lastRecvNano), acked)
	case rec.lastWriteDone < rec.lastWriteStart:
		// write_start was stamped but write_done was not. The cause
		// depends on whether the server has terminated: a non-terminal
		// record means the write is still in flight (the conn is open
		// and the bridge has not yet returned write_done); a terminal
		// record with non-nil termErr names the write failure. Don't
		// imply the server has given up when the record is still live.
		return fmt.Sprintf("server[%s] began writing seq %d (write_start=%v) but the write has not completed (return-leg failure or server->tunnel write stuck; client_acked=%d)",
			state, rec.lastSeqSeen, dur(rec.lastWriteStart), acked)
	default:
		return fmt.Sprintf("server[%s] completed reply for seq %d (write_done=%v) but client only acked %d (return/response-leg break)",
			state, rec.lastSeqSeen, dur(rec.lastWriteDone), acked)
	}
}

// probeOnce performs a single framed request/response exchange against a
// ServerProbe target reached at addr (one dial, one request, one verified
// response). It is the framed replacement for runEchoOnce in scenarios
// whose target is a ServerProbe server.
//
// On failure, if srv is non-nil, the returned error is enriched with the
// server's per-nonce record interpretation (request reached server vs.
// server stalled vs. return-leg break), giving the same per-leg
// localization the held-flow scenarios get from probeFlow.Dump. Pass nil
// for srv when the caller can't easily get a *WorkloadServer reference;
// the dial/read/integrity error still surfaces, just without leg
// attribution.
func probeOnce(srv *WorkloadServer, addr string, reqSize, respSize int, timeout time.Duration) error {
	nonce := randNonce()
	return localizeProbeError(srv, nonce, probeOnceCore(addr, nonce, reqSize, respSize, timeout))
}

// probeOnceCore is the raw single-exchange driver; probeOnce wraps it
// with optional server-side localization on failure.
func probeOnceCore(addr string, nonce uint64, reqSize, respSize int, timeout time.Duration) error {
	conn, err := net.DialTimeout("tcp", addr, timeout)
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	defer conn.Close() //nolint:errcheck // best-effort
	_ = conn.SetDeadline(time.Now().Add(timeout))
	return probeExchangeOnConn(conn, nonce, reqSize, respSize)
}

// probeExchangeOnConn drives one probe request/response on an already-
// dialed connection. Factored out so callers that need to keep the conn
// open after the exchange (e.g. probeWithHold for the metric sampler)
// can reuse the same validation as probeOnceCore.
func probeExchangeOnConn(conn net.Conn, nonce uint64, reqSize, respSize int) error {
	buf := make([]byte, probeHdrLen+reqSize)
	binary.BigEndian.PutUint64(buf[probeOffClient:], uint64(probeNanos()))
	fillPattern(buf[probeHdrLen:], nonce, 0)
	if err := writeFrame(conn, frameProbeReq, nonce, buf); err != nil {
		return fmt.Errorf("write: %w", err)
	}
	typ, n, payload, err := readFrame(conn)
	if err != nil {
		return fmt.Errorf("read: %w", err)
	}
	switch {
	case n != nonce:
		return fmt.Errorf("response nonce %d != request nonce %d", n, nonce)
	case typ == frameError:
		return fmt.Errorf("server reported request integrity error")
	case typ != frameProbeResp:
		return fmt.Errorf("unexpected response frame type %d", typ)
	case len(payload) < probeHdrLen:
		return fmt.Errorf("short probe response header: %d < %d", len(payload), probeHdrLen)
	}
	body := payload[probeHdrLen:]
	if len(body) != respSize {
		return fmt.Errorf("response body %d != want %d", len(body), respSize)
	}
	if off, ok := verifyPattern(body, nonce, 0); !ok {
		return fmt.Errorf("response pattern mismatch at offset %d", off)
	}
	return nil
}

// localizeProbeError consumes the server-side record for nonce and
// returns err enriched with a per-leg attribution (or the original err
// when srv is nil). Always consumes the record on a non-nil srv so a
// failed exchange does not leak a probeStats entry.
func localizeProbeError(srv *WorkloadServer, nonce uint64, err error) error {
	if err == nil || srv == nil {
		return err
	}
	rec, ok := srv.ConsumeProbeRecord(nonce)
	if !ok {
		return fmt.Errorf("%w: server has no record for nonce %d (request never reached the server: outbound break)", err, nonce)
	}
	switch {
	case rec.lastSeqSeen < 0:
		return fmt.Errorf("%w: server recorded the connection but read no request (outbound break before first frame)", err)
	case rec.lastWriteStart == 0 || rec.lastWriteStart < rec.lastRecvNano:
		return fmt.Errorf("%w: server read the request (recv=%v) but had not started its reply (server-path stall)", err, dur(rec.lastRecvNano))
	case rec.lastWriteDone < rec.lastWriteStart:
		return fmt.Errorf("%w: server began writing (write_start=%v) but the write did not complete (return-leg failure or server->tunnel write stuck; termErr=%v)", err, dur(rec.lastWriteStart), rec.termErr)
	default:
		return fmt.Errorf("%w: server completed reply (write_done=%v) but the client never read it (return/response-leg break)", err, dur(rec.lastWriteDone))
	}
}

// startProbeFlow dials addr, wraps the connection in a probeFlow, starts
// it, and waits for the first ack so the caller knows the bridge is
// established before proceeding. On failure it stops the flow and returns
// the error.
func startProbeFlow(addr string, dialTimeout time.Duration, cfg ProbeConfig) (*probeFlow, error) {
	c, err := net.DialTimeout("tcp", addr, dialTimeout)
	if err != nil {
		return nil, err
	}
	f := newProbeFlow(c, cfg)
	f.Start()
	if err := f.waitFirstAck(dialTimeout); err != nil {
		f.Stop()
		return nil, err
	}
	return f, nil
}

// waitProgress reports whether the flow's acked sequence advances beyond
// base within timeout. A read-side break ends the wait immediately
// (returns false): a broken flow makes no further progress.
func waitProgress(f *probeFlow, base int64, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if f.broken() {
			return false
		}
		if f.acked() > base {
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return f.acked() > base
}
