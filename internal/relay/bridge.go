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

// BridgeStats holds the post-mortem of a completed bridge: bidirectional
// byte counts and a short classifier ("peer_close", "local_close",
// "renew_failure", ...) that operators log to triage why the bridge
// ended. Cause is one of the labels returned by bridgecause.Name.
type BridgeStats struct {
	TCPToWS int64  // bytes copied from the TCP/local side to the WebSocket
	WSToTCP int64  // bytes copied from the WebSocket to the TCP/local side
	Cause   string // classifier from bridgecause.Name(context.Cause(...))
}

// pumpResult identifies which I/O operation a bridge pump exited on
// so the cause classifier can distinguish a peer-side failure
// (ws.Write to a dead peer) from a local-side failure (tcp.Read EOF)
// when the two pumps share a single error channel.
type pumpResult struct {
	op  string // "ws_read" / "ws_write" / "tcp_read" / "tcp_write"
	err error
}

// Bridge copies data bidirectionally between a WebSocket connection
// and a TCP connection until one side closes or the context is
// cancelled. It returns byte-transfer statistics including a Cause
// classifier and the first error from either direction.
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
func Bridge(ctx context.Context, ws *websocket.Conn, tcp net.Conn) (BridgeStats, error) {
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)

	var tcpToWSBytes, wsToTCPBytes atomic.Int64
	errc := make(chan pumpResult, 2)

	// WebSocket → TCP
	go func() {
		op, err := wsToTCP(ctx, ws, tcp, &wsToTCPBytes)
		errc <- pumpResult{op: op, err: err}
	}()

	// TCP → WebSocket
	go func() {
		op, err := tcpToWS(ctx, ws, tcp, &tcpToWSBytes)
		errc <- pumpResult{op: op, err: err}
	}()

	// WebSocket keepalive pings to prevent Azure Relay idle timeout.
	go bridgePingLoop(ctx, ws)

	// Wait for the first direction to finish, then cancel the other.
	first := <-errc
	cancel(causeFromPumpExit(first.op, first.err))
	// Unblock tcp.Read in tcpToWS by closing the read side.
	_ = tcp.SetReadDeadline(time.Now())
	<-errc

	stats := BridgeStats{
		TCPToWS: tcpToWSBytes.Load(),
		WSToTCP: wsToTCPBytes.Load(),
		Cause:   bridgecause.Name(context.Cause(ctx)),
	}
	return stats, first.err
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
