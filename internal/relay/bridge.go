package relay

import (
	"context"
	"errors"
	"io"
	"net"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
)

const (
	// bridgePingInterval is how often we send WebSocket pings on data channels
	// to prevent Azure Relay from dropping idle connections (~120s timeout).
	bridgePingInterval = 30 * time.Second
	bridgePingTimeout  = 10 * time.Second
)

// BridgeStats holds byte counters for a completed bridge.
type BridgeStats struct {
	TCPToWS int64 // bytes copied from the TCP/local side to the WebSocket
	WSToTCP int64 // bytes copied from the WebSocket to the TCP/local side
}

// Bridge copies data bidirectionally between a WebSocket connection and a
// TCP connection until one side closes or the context is cancelled.
// It returns byte-transfer statistics and the first error from either direction.
func Bridge(ctx context.Context, ws *websocket.Conn, tcp net.Conn) (BridgeStats, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var tcpToWSBytes, wsToTCPBytes atomic.Int64
	errc := make(chan error, 2)

	// WebSocket → TCP
	go func() {
		errc <- wsToTCP(ctx, ws, tcp, &wsToTCPBytes)
	}()

	// TCP → WebSocket
	go func() {
		errc <- tcpToWS(ctx, ws, tcp, &tcpToWSBytes)
	}()

	// WebSocket keepalive pings to prevent Azure Relay idle timeout.
	go bridgePingLoop(ctx, ws)

	// Wait for the first direction to finish, then cancel the other.
	err := <-errc
	cancel()
	// Unblock tcp.Read in tcpToWS by closing the read side.
	_ = tcp.SetReadDeadline(time.Now())
	<-errc

	stats := BridgeStats{
		TCPToWS: tcpToWSBytes.Load(),
		WSToTCP: wsToTCPBytes.Load(),
	}
	return stats, err
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

func wsToTCP(ctx context.Context, ws *websocket.Conn, tcp net.Conn, count *atomic.Int64) error {
	for {
		_, r, err := ws.Reader(ctx)
		if err != nil {
			return ignoreNormalClose(err)
		}
		n, err := io.Copy(tcp, r)
		count.Add(n)
		if err != nil {
			return err
		}
	}
}

func tcpToWS(ctx context.Context, ws *websocket.Conn, tcp net.Conn, count *atomic.Int64) error {
	buf := make([]byte, 32*1024)
	for {
		n, err := tcp.Read(buf)
		if n > 0 {
			if wErr := ws.Write(ctx, websocket.MessageBinary, buf[:n]); wErr != nil {
				return wErr
			}
			count.Add(int64(n))
		}
		if err != nil {
			return ignoreEOF(err)
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

func ignoreEOF(err error) error {
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return nil
	}
	return err
}
