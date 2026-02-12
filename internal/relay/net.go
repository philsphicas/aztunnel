package relay

import (
	"context"
	"net"
	"time"
)

// SetTCPKeepAlive enables TCP keepalive on the connection if it is a
// *net.TCPConn and d > 0.
func SetTCPKeepAlive(conn net.Conn, d time.Duration) {
	if d <= 0 {
		return
	}
	tcpConn, ok := conn.(*net.TCPConn)
	if !ok {
		return
	}
	_ = tcpConn.SetKeepAlive(true)
	_ = tcpConn.SetKeepAlivePeriod(d)
}

// connSemaphore limits concurrent connections. A nil channel (from
// newConnSemaphore(0)) imposes no limit.
type connSemaphore struct {
	ch chan struct{}
}

func newConnSemaphore(max int) *connSemaphore {
	if max <= 0 {
		return &connSemaphore{}
	}
	return &connSemaphore{ch: make(chan struct{}, max)}
}

func (s *connSemaphore) tryAcquire(ctx context.Context) bool {
	if s.ch == nil {
		return true
	}
	select {
	case s.ch <- struct{}{}:
		return true
	case <-ctx.Done():
		return false
	default:
		return false
	}
}

func (s *connSemaphore) release() {
	if s.ch == nil {
		return
	}
	<-s.ch
}
