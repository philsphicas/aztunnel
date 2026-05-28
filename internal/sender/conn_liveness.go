package sender

import (
	"context"
	"net"
	"sync"
	"time"
)

const connLivenessPollInterval = 200 * time.Millisecond

// connBoundContext returns a child context tied to the peer-liveness of conn.
// It also returns a wrapped conn that preserves any bytes read by the liveness
// watcher before the real bridge starts.
func connBoundContext(parent context.Context, conn net.Conn) (context.Context, net.Conn, func()) {
	ctx, cancel := context.WithCancel(parent)
	wrapped := &prefetchedConn{Conn: conn}
	done := make(chan struct{})
	var stopOnce sync.Once

	stop := func() {
		stopOnce.Do(func() {
			cancel()
			_ = conn.SetReadDeadline(time.Now())
			<-done
		})
	}

	go func() {
		defer close(done)
		defer func() { _ = conn.SetReadDeadline(time.Time{}) }()

		buf := make([]byte, 1)
		for {
			select {
			case <-ctx.Done():
				return
			default:
			}

			if err := conn.SetReadDeadline(time.Now().Add(connLivenessPollInterval)); err != nil {
				return
			}

			n, err := conn.Read(buf)
			if n > 0 {
				wrapped.prepend(buf[:n])
			}
			if err == nil {
				continue
			}
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				continue
			}
			cancel()
			return
		}
	}()

	return ctx, wrapped, stop
}

type prefetchedConn struct {
	net.Conn
	mu   sync.Mutex
	head []byte
}

func (c *prefetchedConn) prepend(p []byte) {
	if len(p) == 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	h := make([]byte, 0, len(c.head)+len(p))
	h = append(h, p...)
	h = append(h, c.head...)
	c.head = h
}

func (c *prefetchedConn) Read(p []byte) (int, error) {
	c.mu.Lock()
	if len(c.head) > 0 {
		n := copy(p, c.head)
		c.head = c.head[n:]
		c.mu.Unlock()
		return n, nil
	}
	c.mu.Unlock()
	return c.Conn.Read(p)
}
