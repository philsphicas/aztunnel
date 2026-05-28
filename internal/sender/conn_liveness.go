package sender

import (
	"bytes"
	"context"
	"errors"
	"net"
	"sync"
	"time"
)

// 200ms keeps close-detection responsive while avoiding tight polling loops.
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
			// Force any blocked watcher Read to return so stop() can wait.
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
				cancel()
				return
			}

			n, err := conn.Read(buf)
			if n > 0 {
				wrapped.stash(buf[:n])
			}
			if err == nil {
				continue
			}
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
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
	head bytes.Buffer
}

func (c *prefetchedConn) stash(p []byte) {
	if len(p) == 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	_, _ = c.head.Write(p)
}

func (c *prefetchedConn) Read(p []byte) (int, error) {
	c.mu.Lock()
	if c.head.Len() > 0 {
		n, err := c.head.Read(p)
		c.mu.Unlock()
		return n, err
	}
	c.mu.Unlock()
	return c.Conn.Read(p)
}
