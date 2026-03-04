package main

import (
	"errors"
	"io"
	"net"
	"time"
)

// arcStdioConn adapts stdin/stdout to net.Conn for use with relay.Bridge.
type arcStdioConn struct {
	in  io.ReadCloser
	out io.WriteCloser
}

func (c *arcStdioConn) Read(b []byte) (int, error)       { return c.in.Read(b) }
func (c *arcStdioConn) Write(b []byte) (int, error)      { return c.out.Write(b) }
func (c *arcStdioConn) Close() error                     { return errors.Join(c.in.Close(), c.out.Close()) }
func (c *arcStdioConn) LocalAddr() net.Addr              { return arcStubAddr{} }
func (c *arcStdioConn) RemoteAddr() net.Addr             { return arcStubAddr{} }
func (c *arcStdioConn) SetDeadline(time.Time) error      { return nil }
func (c *arcStdioConn) SetReadDeadline(time.Time) error  { return nil }
func (c *arcStdioConn) SetWriteDeadline(time.Time) error { return nil }

type arcStubAddr struct{}

func (arcStubAddr) Network() string { return "stdio" }
func (arcStubAddr) String() string  { return "stdio" }
