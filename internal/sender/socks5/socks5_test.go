package socks5

import (
	"bytes"
	"encoding/binary"
	"net"
	"testing"
)

func TestHandshakeIPv4(t *testing.T) {
	// Build a SOCKS5 CONNECT request for 10.0.0.5:22
	var buf bytes.Buffer
	// Auth: version 5, 1 method, no-auth
	buf.Write([]byte{0x05, 0x01, 0x00})
	// Request: version 5, CONNECT, RSV, IPv4
	buf.Write([]byte{0x05, 0x01, 0x00, 0x01})
	buf.Write(net.IPv4(10, 0, 0, 5).To4())
	binary.Write(&buf, binary.BigEndian, uint16(22))

	var resp bytes.Buffer
	rw := &readWriter{in: &buf, out: &resp}

	target, err := Handshake(rw)
	if err != nil {
		t.Fatalf("handshake: %v", err)
	}
	if target != "10.0.0.5:22" {
		t.Errorf("target = %q, want 10.0.0.5:22", target)
	}

	// Check auth reply
	if resp.Len() < 2 {
		t.Fatal("no auth reply")
	}
	authReply := resp.Bytes()[:2]
	if authReply[0] != 0x05 || authReply[1] != 0x00 {
		t.Errorf("auth reply = %x, want 0500", authReply)
	}
}

func TestHandshakeDomain(t *testing.T) {
	var buf bytes.Buffer
	buf.Write([]byte{0x05, 0x01, 0x00})
	buf.Write([]byte{0x05, 0x01, 0x00, 0x03})
	domain := "example.com"
	buf.WriteByte(byte(len(domain)))
	buf.WriteString(domain)
	binary.Write(&buf, binary.BigEndian, uint16(443))

	var resp bytes.Buffer
	rw := &readWriter{in: &buf, out: &resp}

	target, err := Handshake(rw)
	if err != nil {
		t.Fatalf("handshake: %v", err)
	}
	if target != "example.com:443" {
		t.Errorf("target = %q, want example.com:443", target)
	}
}

func TestHandshakeNoAuth(t *testing.T) {
	// Client offers only username/password auth (0x02), no no-auth
	var buf bytes.Buffer
	buf.Write([]byte{0x05, 0x01, 0x02})

	var resp bytes.Buffer
	rw := &readWriter{in: &buf, out: &resp}

	_, err := Handshake(rw)
	if err == nil {
		t.Fatal("expected error for no no-auth support")
	}
}

func TestSendReply(t *testing.T) {
	var buf bytes.Buffer
	addr := &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 1080}
	err := SendReply(&buf, RepSuccess, addr)
	if err != nil {
		t.Fatal(err)
	}
	data := buf.Bytes()
	if data[0] != 0x05 {
		t.Errorf("version = %d, want 5", data[0])
	}
	if data[1] != RepSuccess {
		t.Errorf("rep = %d, want %d", data[1], RepSuccess)
	}
	if data[3] != AddrIPv4 {
		t.Errorf("atyp = %d, want %d", data[3], AddrIPv4)
	}
}

// readWriter combines a Reader and Writer for testing.
type readWriter struct {
	in  *bytes.Buffer
	out *bytes.Buffer
}

func (rw *readWriter) Read(p []byte) (int, error)  { return rw.in.Read(p) }
func (rw *readWriter) Write(p []byte) (int, error) { return rw.out.Write(p) }
