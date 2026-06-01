package scenarios

import (
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
)

// PlainEcho is a TCP server that echoes every byte it receives back
// to the sender, unmodified. Used by scenarios that verify byte-level
// transparency (ordering, integrity, bidirectional throughput).
//
// Lifetimes are tracked separately: serveDone signals when the accept
// loop has exited (and therefore no further connection-goroutine
// spawns can happen), and connWg tracks the connection goroutines
// themselves. Stop waits on serveDone before draining connWg so the
// connection counter never receives a fresh Add concurrent with its
// own Wait.
type PlainEcho struct {
	ln        net.Listener
	serveDone chan struct{}
	connWg    sync.WaitGroup
	done      atomic.Bool
}

// StartPlainEcho starts a plain echo server on a free localhost port.
// It is stopped by t.Cleanup.
//
// Accepts testing.TB so it is callable from every scenario suite.
// Only Cleanup / Helper / Fatalf are used.
func StartPlainEcho(t testing.TB) *PlainEcho {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("plain echo listen: %v", err)
	}
	pe := &PlainEcho{ln: ln, serveDone: make(chan struct{})}
	go pe.serve()
	t.Cleanup(pe.Stop)
	return pe
}

// Addr returns the host:port the echo server is listening on.
func (pe *PlainEcho) Addr() string { return pe.ln.Addr().String() }

// Stop closes the listener, waits for the accept loop to exit so no
// further connection goroutines can be spawned, then drains the
// outstanding connections.
func (pe *PlainEcho) Stop() {
	if pe.done.Swap(true) {
		return
	}
	pe.ln.Close()  //nolint:errcheck // best-effort cleanup
	<-pe.serveDone // accept loop has exited; connWg is now stable
	pe.connWg.Wait()
}

func (pe *PlainEcho) serve() {
	defer close(pe.serveDone)
	for {
		conn, err := pe.ln.Accept()
		if err != nil {
			return
		}
		pe.connWg.Add(1)
		go func(c net.Conn) {
			defer pe.connWg.Done()
			defer c.Close() //nolint:errcheck // best-effort cleanup
			_, _ = io.Copy(c, c)
		}(conn)
	}
}
