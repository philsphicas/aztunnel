package relay

import (
	"context"
	"crypto/tls"
	"log/slog"
	"net/http/httptrace"
	"sync/atomic"
	"time"
)

// dialTrace captures coarse timing of a single rendezvous WebSocket
// dial's HTTP setup via net/http/httptrace. Each field stores the
// nanoseconds elapsed since the dial's start at the moment the
// corresponding hook fires. The hooks run on net/http's transport
// goroutines, so the fields are accessed atomically.
//
// Capture is deliberately best-effort: a hook that never fires (e.g.
// because a pooled connection was reused, or because the websocket
// library's context propagation changed) leaves its field at zero and
// the corresponding phase is simply omitted from the log line. The
// trace records only durations — never the dial URL, query string, or
// token, since the sender connect URL embeds an SAS token.
//
// Under Happy Eyeballs (dual-stack dialing) ConnectStart/ConnectDone
// fire once per attempted address; connectStart keeps the first fire
// and connectDone keeps the successful one, so the reported tcp span is
// "first-attempt start → successful-attempt done" and can overstate
// real TCP setup by the Happy Eyeballs fallback delay. This is a
// diagnostic-only line, never an assertion input, so the slack is
// acceptable.
//
// The split it enables: for an Azure Relay rendezvous the relay holds
// the HTTP 101 until the listener has rendezvoused, so reqWritten marks
// the end of the sender's own DNS/TCP/TLS/WS-GET work ("Phase 2") and
// respWait = firstByte - reqWritten is the relay-side hold ("Phase 3"
// onward). A large respWait with small reqWritten localises a slow
// rendezvous to the relay; a large tls localises it to the sender's
// cold dial.
type dialTrace struct {
	start        time.Time
	dnsStart     atomic.Int64
	dnsDone      atomic.Int64
	connectStart atomic.Int64
	connectDone  atomic.Int64
	tlsStart     atomic.Int64
	tlsDone      atomic.Int64
	reqWritten   atomic.Int64
	firstByte    atomic.Int64
	reused       atomic.Bool
}

// newDialTrace attaches an httptrace.ClientTrace to ctx and returns the
// derived context to pass into websocket.Dial along with the trace to
// read once the dial returns. start should be captured immediately
// before the dial so all deltas are relative to it.
func newDialTrace(ctx context.Context, start time.Time) (context.Context, *dialTrace) {
	dt := &dialTrace{start: start}
	since := func() int64 { return int64(time.Since(start)) }
	trace := &httptrace.ClientTrace{
		DNSStart:     func(httptrace.DNSStartInfo) { dt.dnsStart.CompareAndSwap(0, since()) },
		DNSDone:      func(httptrace.DNSDoneInfo) { dt.dnsDone.Store(since()) },
		ConnectStart: func(_, _ string) { dt.connectStart.CompareAndSwap(0, since()) },
		// Under Happy Eyeballs ConnectDone fires once per attempted
		// address; record the successful one so connect timing reflects
		// the connection actually used.
		ConnectDone: func(_, _ string, err error) {
			if err == nil {
				dt.connectDone.Store(since())
			}
		},
		TLSHandshakeStart: func() { dt.tlsStart.CompareAndSwap(0, since()) },
		TLSHandshakeDone: func(_ tls.ConnectionState, err error) {
			if err == nil {
				dt.tlsDone.Store(since())
			}
		},
		WroteRequest:         func(httptrace.WroteRequestInfo) { dt.reqWritten.Store(since()) },
		GotFirstResponseByte: func() { dt.firstByte.CompareAndSwap(0, since()) },
		GotConn:              func(info httptrace.GotConnInfo) { dt.reused.Store(info.Reused) },
	}
	return httptrace.WithClientTrace(ctx, trace), dt
}

// log emits a single DEBUG line with the per-phase rendezvous timings.
// Phase durations are only included when both of their bounding hooks
// fired; cumulative marks (req_written, first_byte) are included when
// present. The caller is expected to have already gated on DEBUG (the
// trace is only attached when DEBUG is enabled), but log re-checks so
// it is safe to call unconditionally.
func (dt *dialTrace) log(ctx context.Context, logger *slog.Logger, msg string) {
	if dt == nil || logger == nil || !logger.Enabled(ctx, slog.LevelDebug) {
		return
	}
	attrs := make([]any, 0, 8)
	span := func(name string, s, d int64) {
		if s > 0 && d >= s {
			attrs = append(attrs, slog.Duration(name, time.Duration(d-s)))
		}
	}
	mark := func(name string, t int64) {
		if t > 0 {
			attrs = append(attrs, slog.Duration(name, time.Duration(t)))
		}
	}
	span("dns", dt.dnsStart.Load(), dt.dnsDone.Load())
	span("tcp", dt.connectStart.Load(), dt.connectDone.Load())
	span("tls", dt.tlsStart.Load(), dt.tlsDone.Load())
	mark("req_written", dt.reqWritten.Load())
	mark("first_byte", dt.firstByte.Load())
	// resp_wait is the gap between the request being fully written and
	// the first response byte — for an Azure Relay rendezvous this is
	// the relay holding the 101 (accept dispatch + listener rendezvous).
	if req, fb := dt.reqWritten.Load(), dt.firstByte.Load(); req > 0 && fb >= req {
		attrs = append(attrs, slog.Duration("resp_wait", time.Duration(fb-req)))
	}
	attrs = append(attrs, slog.Bool("reused", dt.reused.Load()))
	logger.Debug(msg, attrs...)
}
