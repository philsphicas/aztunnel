//go:build e2e

package e2e

import (
	"io"
	"net"
	"sync"
	"testing"
)

// echoServer is a TCP server that echoes data back to the client.
type echoServer struct {
	ln   net.Listener
	mu   sync.Mutex
	conns int64
}

// startEchoServer starts a TCP echo server on a random port.
func startEchoServer(t *testing.T) *echoServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("echo server listen: %v", err)
	}

	es := &echoServer{ln: ln}

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return // listener closed
			}
			es.mu.Lock()
			es.conns++
			es.mu.Unlock()
			go func() {
				defer conn.Close()
				io.Copy(conn, conn)
			}()
		}
	}()

	t.Cleanup(func() { ln.Close() })
	return es
}

// Addr returns the echo server's listen address as "host:port".
func (es *echoServer) Addr() string {
	return es.ln.Addr().String()
}

// ConnectionCount returns the number of connections accepted.
func (es *echoServer) ConnectionCount() int64 {
	es.mu.Lock()
	defer es.mu.Unlock()
	return es.conns
}
