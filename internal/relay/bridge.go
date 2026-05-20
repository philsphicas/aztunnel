package relay

import (
	"context"
	"errors"
	"io"
	"net"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"

	"github.com/philsphicas/aztunnel/internal/bridgecause"
)

const (
	// bridgePingInterval is how often we send WebSocket pings on data channels
	// to prevent Azure Relay from dropping idle connections (~120s timeout).
	bridgePingInterval = 30 * time.Second
	bridgePingTimeout  = 10 * time.Second
)

// BridgeStats holds the bidirectional byte counts of a completed
// bridge. The TCPToWS / WSToTCP fields tally bytes copied in each
// direction across the bridge's lifetime; the end-cause classifier
// lives on BridgeResult.EndCause so per-direction errors and the
// overall cause sit on the same result struct.
type BridgeStats struct {
	TCPToWS int64 // bytes copied from the TCP/local side to the WebSocket
	WSToTCP int64 // bytes copied from the WebSocket to the TCP/local side
}

// BridgeResult is the outcome of one completed bridge. It carries the
// byte counters (Stats) plus the per-direction terminating errors and
// the bridgecause classifier that ended the overall bridge.
//
// TCPToWS / WSToTCP are nil on a clean direction (normal close, EOF,
// or an "induced cancellation" — see below). A non-nil value is the
// terminating error of that direction's pump after the normal-close
// filters (ignoreNormalClose, ignoreEOF) have run.
//
// Induced cancellations are suppressed to nil so the per-direction
// fields surface only that direction's own failure: when the first
// pump returns the bridge cancels its internal ctx (interrupting the
// second pump's ws.Reader with context.Canceled) and calls
// tcp.SetReadDeadline(time.Now()) (interrupting the second pump's
// tcp.Read with net.Error.Timeout()). Both shapes match
// isInducedCancellation and are filtered out before BridgeResult is
// returned. The same filter also catches both pumps' ctx.Canceled /
// ctx.DeadlineExceeded under a parent-ctx cancel/timeout, so
// user-cancel and timeout bridges report nil per-direction errors
// alongside an EndCause of "user_cancel" / "timeout".
//
// EndCause is bridgecause.Name(context.Cause(bridgeCtx)) at return
// time — one of the stable short labels operators grep on.
type BridgeResult struct {
	// Stats is the byte counters for this bridge.
	Stats BridgeStats

	// TCPToWS is the terminating error of the TCP→WebSocket
	// direction (nil on normal close / induced cancellation).
	TCPToWS error

	// WSToTCP is the terminating error of the WebSocket→TCP
	// direction (nil on normal close / induced cancellation).
	WSToTCP error

	// EndCause is the bridgecause label for the overall bridge end.
	EndCause string
}

// pumpResult identifies which I/O operation a bridge pump exited on
// so the cause classifier can distinguish a peer-side failure
// (ws.Write to a dead peer) from a local-side failure (tcp.Read EOF).
type pumpResult struct {
	op  string // "ws_read" / "ws_write" / "tcp_read" / "tcp_write"
	err error
}

// Bridge copies data bidirectionally between a WebSocket connection
// and a TCP connection until one side closes or the context is
// cancelled. It returns a BridgeResult with byte counters, the
// per-direction terminating errors, and an EndCause classifier; the
// second return is the first non-nil pump error for backward-compat
// with callers that just check `if err != nil`.
//
// The classifier comes from context.Cause on an internal
// WithCancelCause-derived child context. Cause-cancel is first-cancel-
// wins: whichever site cancels the child ctx first sets the cause for
// the rest of the bridge's lifetime, and subsequent cancels are no-ops.
//
//   - If a pump returns first (peer close, local EOF, I/O error), the
//     bridge stamps peer_close / local_close / timeout via cancel(...)
//     before draining the other pump.
//   - If the parent ctx is cancelled with a cause (e.g.
//     CauseRenewFailure from the control loop) while both pumps are
//     still running, that cause propagates down to the child first;
//     the bridge's later pump-exit cancel is then the no-op, and the
//     parent's cause wins.
//
// Bridge waits for every spawned goroutine (both pumps and the ping
// loop) before returning, so it does not leak goroutines on its
// caller.
func Bridge(ctx context.Context, ws *websocket.Conn, tcp net.Conn) (BridgeResult, error) {
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)

	var tcpToWSBytes, wsToTCPBytes atomic.Int64
	// One single-slot channel per direction so the bridge can
	// attribute each error to the pump that produced it.
	wsToTCPCh := make(chan pumpResult, 1)
	tcpToWSCh := make(chan pumpResult, 1)
	pingDone := make(chan struct{})

	// WebSocket → TCP
	go func() {
		op, err := wsToTCP(ctx, ws, tcp, &wsToTCPBytes)
		wsToTCPCh <- pumpResult{op: op, err: err}
	}()

	// TCP → WebSocket
	go func() {
		op, err := tcpToWS(ctx, ws, tcp, &tcpToWSBytes)
		tcpToWSCh <- pumpResult{op: op, err: err}
	}()

	// WebSocket keepalive pings to prevent Azure Relay idle timeout.
	// Wrapped in a tracking goroutine so Bridge can join it before
	// returning and not leak this goroutine past the caller.
	go func() {
		defer close(pingDone)
		bridgePingLoop(ctx, ws)
	}()

	// Wait for the first direction to finish, stamp cause, then
	// unblock the other pump.
	var first, second pumpResult
	var firstWasWSToTCP bool
	select {
	case r := <-wsToTCPCh:
		first = r
		firstWasWSToTCP = true
	case r := <-tcpToWSCh:
		first = r
	}
	cancel(causeFromPumpExit(first.op, first.err))
	// Unblock tcp.Read in the second pump (if it was tcpToWS) by
	// expiring its read deadline. The ws-side pump's ws.Reader sees
	// the cancel via the internal ctx.
	_ = tcp.SetReadDeadline(time.Now())

	if firstWasWSToTCP {
		second = <-tcpToWSCh
	} else {
		second = <-wsToTCPCh
	}

	// Join the ping loop before returning. ctx is already cancelled,
	// so the loop's select returns on the next iteration; any
	// in-flight ws.Ping aborts via its pingCtx (derived from ctx).
	<-pingDone

	var wsErr, tcpErr error
	if firstWasWSToTCP {
		wsErr = first.err
		tcpErr = second.err
	} else {
		tcpErr = first.err
		wsErr = second.err
	}
	if isInducedCancellation(wsErr) {
		wsErr = nil
	}
	if isInducedCancellation(tcpErr) {
		tcpErr = nil
	}

	result := BridgeResult{
		Stats: BridgeStats{
			TCPToWS: tcpToWSBytes.Load(),
			WSToTCP: wsToTCPBytes.Load(),
		},
		TCPToWS:  tcpErr,
		WSToTCP:  wsErr,
		EndCause: bridgecause.Name(context.Cause(ctx)),
	}

	// Backward-compat primary error: the first pump's raw err.
	// Returning nil on a normal close preserves the single-error
	// callers' existing observable behavior (WARN-on-cancel sender
	// callers still fire on a parent-ctx cancellation; a normal
	// peer-close stays at DEBUG). The second pump's err is reported
	// via result.{TCPToWS,WSToTCP} for the diagnostic log.
	return result, first.err
}

// isInducedCancellation reports whether err is the artifact of the
// bridge tearing the other direction down — context.Canceled /
// context.DeadlineExceeded propagated to ws.Reader/ws.Write, or a
// net.Error.Timeout() from tcp.Read after the bridge expired its
// deadline. The bridge's only sources of pump-level timeouts are the
// parent ctx deadline (also classified into EndCause) and the
// SetReadDeadline call the bridge itself makes on the second pump;
// in both cases the per-direction error is noise, so filtering it to
// nil keeps BridgeResult.{TCPToWS,WSToTCP} focused on each direction's
// own failure.
func isInducedCancellation(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return true
	}
	return false
}

// causeFromPumpExit picks the sentinel a pump's exit should stamp on
// the bridge ctx. The bridge ctx's cancel is idempotent, so the
// returned sentinel only wins when the bridge ctx has not already
// been cancelled (e.g. by a WithCancelCause parent).
//
// Classification rules:
//
//   - nil + ws_read: the WebSocket peer closed cleanly → peer_close.
//   - nil + tcp_read: the local TCP side EOF'd → local_close.
//   - net.Error.Timeout(): timeout regardless of side.
//   - websocket.CloseError: the peer surfaced a close frame
//     (including the synthetic 1006 the websocket layer reports on
//     abrupt drop) → peer_close.
//   - ws_read/ws_write non-nil: an I/O failure on the peer-facing
//     half of the bridge → peer_close.
//   - tcp_read/tcp_write non-nil: an I/O failure on the local half →
//     local_close.
//   - context.Canceled / DeadlineExceeded: return as-is; the bridge
//     ctx's existing cause (or the stdlib alias mapping in
//     bridgecause.Name) wins.
func causeFromPumpExit(op string, err error) error {
	if err == nil {
		if op == "ws_read" {
			return bridgecause.CausePeerClose
		}
		return bridgecause.CauseLocalClose
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return bridgecause.CauseTimeout
	}
	var closeErr websocket.CloseError
	if errors.As(err, &closeErr) {
		return bridgecause.CausePeerClose
	}
	switch op {
	case "ws_read", "ws_write":
		return bridgecause.CausePeerClose
	default:
		return bridgecause.CauseLocalClose
	}
}

// bridgePingLoop sends periodic WebSocket pings to keep the data channel alive.
func bridgePingLoop(ctx context.Context, ws *websocket.Conn) {
	ticker := time.NewTicker(bridgePingInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			pingCtx, cancel := context.WithTimeout(ctx, bridgePingTimeout)
			_ = ws.Ping(pingCtx) // best-effort; data flow or context cancel will clean up
			cancel()
		}
	}
}

// wsToTCP pumps data from the WebSocket to the local TCP side and
// returns the operation tag plus its terminating error. The op tag
// is what causeFromPumpExit consults to distinguish a peer-side
// failure (ws_read) from a local-side failure (tcp_write).
func wsToTCP(ctx context.Context, ws *websocket.Conn, tcp net.Conn, count *atomic.Int64) (string, error) {
	for {
		_, r, err := ws.Reader(ctx)
		if err != nil {
			return "ws_read", ignoreNormalClose(err)
		}
		n, err := io.Copy(tcp, r)
		count.Add(n)
		if err != nil {
			return "tcp_write", err
		}
	}
}

// tcpToWS pumps data from the local TCP side to the WebSocket and
// returns the operation tag plus its terminating error. ws.Write
// failures here are peer-side (the peer's read half died), not
// local-side; the op tag preserves that distinction.
func tcpToWS(ctx context.Context, ws *websocket.Conn, tcp net.Conn, count *atomic.Int64) (string, error) {
	buf := make([]byte, 32*1024)
	for {
		n, err := tcp.Read(buf)
		if n > 0 {
			if wErr := ws.Write(ctx, websocket.MessageBinary, buf[:n]); wErr != nil {
				return "ws_write", wErr
			}
			count.Add(int64(n))
		}
		if err != nil {
			return "tcp_read", ignoreEOF(err)
		}
	}
}

func ignoreNormalClose(err error) error {
	var closeErr websocket.CloseError
	if errors.As(err, &closeErr) && closeErr.Code == websocket.StatusNormalClosure {
		return nil
	}
	return err
}

// WSCloseCode extracts the WebSocket close-status code from a Bridge
// error when the error unwraps to a websocket.CloseError. Returns
// (code, true) when it does, (0, false) for nil errors and for any
// error that does not unwrap to a websocket.CloseError. The
// websocket.CloseError set includes synthesised codes that the
// library reports without an on-the-wire close frame (notably 1006
// StatusAbnormalClosure when the connection drops). Use this on the
// result of Bridge() to attach a structured close_code field to
// bridge-end log lines so operators can distinguish remote-initiated
// close from policy violation from relay server error without
// parsing the error string.
func WSCloseCode(err error) (int, bool) {
	if err == nil {
		return 0, false
	}
	var closeErr websocket.CloseError
	if errors.As(err, &closeErr) {
		return int(closeErr.Code), true
	}
	return 0, false
}

func ignoreEOF(err error) error {
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return nil
	}
	return err
}
