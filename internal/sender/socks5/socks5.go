// Package socks5 implements a minimal SOCKS5 server (RFC 1928) that supports
// the CONNECT command with no authentication. It is used by the sender to
// accept dynamic forwarding requests from clients like ssh -D.
package socks5

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
)

// SOCKS5 protocol constants.
const (
	Version5 = 0x05

	AuthNone         = 0x00
	AuthNoAcceptable = 0xFF

	CmdConnect = 0x01

	AddrIPv4   = 0x01
	AddrDomain = 0x03
	AddrIPv6   = 0x04

	RepSuccess              = 0x00
	RepGeneralFailure       = 0x01
	RepConnectionNotAllowed = 0x02
	RepNetworkUnreachable   = 0x03
	RepHostUnreachable      = 0x04
	RepConnectionRefused    = 0x05
	RepTTLExpired           = 0x06
	RepCommandNotSupported  = 0x07
	RepAddressNotSupported  = 0x08
)

// Handshake performs the server-side SOCKS5 negotiation on conn.
// It handles auth method negotiation (accepting only "no auth") and
// parses a CONNECT request. On success it returns the target host:port.
// The caller is responsible for sending the reply via SendReply.
func Handshake(conn io.ReadWriter) (string, error) {
	// Auth method negotiation: VER | NMETHODS | METHODS...
	header := make([]byte, 2)
	if _, err := io.ReadFull(conn, header); err != nil {
		return "", fmt.Errorf("read auth header: %w", err)
	}
	if header[0] != Version5 {
		return "", fmt.Errorf("unsupported SOCKS version: %d", header[0])
	}
	nMethods := int(header[1])
	if nMethods == 0 {
		return "", errors.New("no auth methods offered")
	}
	methods := make([]byte, nMethods)
	if _, err := io.ReadFull(conn, methods); err != nil {
		return "", fmt.Errorf("read auth methods: %w", err)
	}

	hasNoAuth := false
	for _, m := range methods {
		if m == AuthNone {
			hasNoAuth = true
			break
		}
	}
	if !hasNoAuth {
		_, _ = conn.Write([]byte{Version5, AuthNoAcceptable})
		return "", errors.New("client does not support no-auth")
	}

	if _, err := conn.Write([]byte{Version5, AuthNone}); err != nil {
		return "", fmt.Errorf("write auth reply: %w", err)
	}

	// CONNECT request: VER | CMD | RSV | ATYP | DST.ADDR | DST.PORT
	reqHeader := make([]byte, 4)
	if _, err := io.ReadFull(conn, reqHeader); err != nil {
		return "", fmt.Errorf("read request header: %w", err)
	}
	if reqHeader[0] != Version5 {
		return "", fmt.Errorf("unsupported SOCKS version in request: %d", reqHeader[0])
	}
	if reqHeader[1] != CmdConnect {
		_ = SendReply(conn, RepCommandNotSupported, nil)
		return "", fmt.Errorf("unsupported SOCKS command: %d", reqHeader[1])
	}

	var host string
	switch reqHeader[3] {
	case AddrIPv4:
		addr := make([]byte, 4)
		if _, err := io.ReadFull(conn, addr); err != nil {
			return "", fmt.Errorf("read IPv4: %w", err)
		}
		host = net.IP(addr).String()
	case AddrIPv6:
		addr := make([]byte, 16)
		if _, err := io.ReadFull(conn, addr); err != nil {
			return "", fmt.Errorf("read IPv6: %w", err)
		}
		host = net.IP(addr).String()
	case AddrDomain:
		lenBuf := make([]byte, 1)
		if _, err := io.ReadFull(conn, lenBuf); err != nil {
			return "", fmt.Errorf("read domain length: %w", err)
		}
		domain := make([]byte, lenBuf[0])
		if _, err := io.ReadFull(conn, domain); err != nil {
			return "", fmt.Errorf("read domain: %w", err)
		}
		host = string(domain)
	default:
		_ = SendReply(conn, RepAddressNotSupported, nil)
		return "", fmt.Errorf("unsupported address type: %d", reqHeader[3])
	}

	portBuf := make([]byte, 2)
	if _, err := io.ReadFull(conn, portBuf); err != nil {
		return "", fmt.Errorf("read port: %w", err)
	}
	port := int(binary.BigEndian.Uint16(portBuf))

	return net.JoinHostPort(host, strconv.Itoa(port)), nil
}

// SendReply sends a SOCKS5 reply to the client.
func SendReply(conn io.Writer, rep byte, bindAddr *net.TCPAddr) error {
	var addrBytes []byte
	var port uint16

	if bindAddr != nil {
		ip := bindAddr.IP.To4()
		if ip != nil {
			addrBytes = append([]byte{AddrIPv4}, ip...)
		} else {
			addrBytes = append([]byte{AddrIPv6}, bindAddr.IP.To16()...)
		}
		port = uint16(bindAddr.Port)
	}

	if addrBytes == nil {
		addrBytes = []byte{AddrIPv4, 0, 0, 0, 0}
		port = 0
	}

	reply := make([]byte, 0, 3+len(addrBytes)+2)
	reply = append(reply, Version5, rep, 0x00)
	reply = append(reply, addrBytes...)
	reply = binary.BigEndian.AppendUint16(reply, port)

	_, err := conn.Write(reply)
	return err
}
