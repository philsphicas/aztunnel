package relayparity

import (
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"strconv"
	"time"
)

// DialSOCKS5 performs a no-auth SOCKS5 CONNECT through proxyAddr to
// target ("host:port"). On success it returns a net.Conn already
// addressed to target. On a SOCKS5-level failure (non-zero REP byte)
// it returns an error containing the REP value so error-propagation
// scenarios can distinguish refused vs unreachable vs other.
//
// timeout bounds the entire operation (TCP dial + SOCKS5 negotiation)
// against a single absolute deadline computed at entry. A stalled
// proxy that accepts TCP but never replies to method negotiation,
// and a slow DNS resolution that eats most of the budget, both fail
// inside the same overall deadline rather than each getting a fresh
// timeout. The conn deadline is cleared on successful return; on
// error the conn is closed.
//
// This is a minimal SOCKS5 client suitable for parity tests; it does
// not support auth methods, BIND, or UDP ASSOCIATE.
func DialSOCKS5(proxyAddr, target string, timeout time.Duration) (net.Conn, error) {
	deadline := time.Now().Add(timeout)
	dialer := net.Dialer{Deadline: deadline}
	conn, err := dialer.Dial("tcp", proxyAddr)
	if err != nil {
		return nil, fmt.Errorf("dial proxy: %w", err)
	}
	if err := conn.SetDeadline(deadline); err != nil {
		conn.Close() //nolint:errcheck // best-effort cleanup on error
		return nil, fmt.Errorf("set deadline: %w", err)
	}
	if err := socks5Negotiate(conn, target); err != nil {
		conn.Close() //nolint:errcheck // best-effort cleanup on error
		return nil, err
	}
	if err := conn.SetDeadline(time.Time{}); err != nil {
		conn.Close() //nolint:errcheck // best-effort cleanup on error
		return nil, fmt.Errorf("clear deadline: %w", err)
	}
	return conn, nil
}

func socks5Negotiate(conn net.Conn, target string) error {
	host, portStr, err := net.SplitHostPort(target)
	if err != nil {
		return fmt.Errorf("parse target %q: %w", target, err)
	}
	portInt, err := strconv.Atoi(portStr)
	if err != nil || portInt <= 0 || portInt > 65535 {
		return fmt.Errorf("parse port %q: invalid", portStr)
	}
	port := uint16(portInt)

	// Method negotiation: VER=5, NMETHODS=1, NO-AUTH(0x00).
	if err := writeFull(conn, []byte{0x05, 0x01, 0x00}); err != nil {
		return fmt.Errorf("socks5 method write: %w", err)
	}
	resp := make([]byte, 2)
	if _, err := io.ReadFull(conn, resp); err != nil {
		return fmt.Errorf("socks5 method response: %w", err)
	}
	if resp[0] != 0x05 || resp[1] != 0x00 {
		return fmt.Errorf("socks5 method rejected: %v", resp)
	}

	// CONNECT request: VER=5, CMD=CONNECT(0x01), RSV=0, ATYP+ADDR+PORT.
	req := []byte{0x05, 0x01, 0x00}
	if ip := net.ParseIP(host); ip != nil {
		if ip4 := ip.To4(); ip4 != nil {
			req = append(req, 0x01)
			req = append(req, ip4...)
		} else {
			req = append(req, 0x04)
			req = append(req, ip...)
		}
	} else {
		if len(host) > 255 {
			return fmt.Errorf("socks5 hostname too long: %d", len(host))
		}
		req = append(req, 0x03, byte(len(host)))
		req = append(req, []byte(host)...)
	}
	portBytes := make([]byte, 2)
	binary.BigEndian.PutUint16(portBytes, port)
	req = append(req, portBytes...)

	if err := writeFull(conn, req); err != nil {
		return fmt.Errorf("socks5 connect write: %w", err)
	}

	// Reply: VER, REP, RSV, ATYP, BND.ADDR, BND.PORT.
	head := make([]byte, 4)
	if _, err := io.ReadFull(conn, head); err != nil {
		return fmt.Errorf("socks5 connect response: %w", err)
	}
	if head[0] != 0x05 {
		return fmt.Errorf("socks5 reply VER=%#x, want 0x05", head[0])
	}
	if head[2] != 0x00 {
		return fmt.Errorf("socks5 reply RSV=%#x, want 0x00", head[2])
	}
	// Drain the bound address regardless of REP so the conn is
	// positioned at the start of the data stream.
	switch head[3] {
	case 0x01:
		_, err = io.ReadFull(conn, make([]byte, 4+2))
	case 0x04:
		_, err = io.ReadFull(conn, make([]byte, 16+2))
	case 0x03:
		ln := make([]byte, 1)
		if _, err = io.ReadFull(conn, ln); err == nil {
			_, err = io.ReadFull(conn, make([]byte, int(ln[0])+2))
		}
	default:
		return fmt.Errorf("socks5 unknown ATYP %#x", head[3])
	}
	if err != nil {
		return fmt.Errorf("socks5 read bnd addr: %w", err)
	}

	if head[1] != 0x00 {
		return &SOCKS5Error{Rep: head[1]}
	}
	return nil
}

// SOCKS5Error is returned by DialSOCKS5 when the proxy replies with a
// non-success REP code. Scenarios that test error propagation
// (T17 in the plan) inspect Rep directly.
type SOCKS5Error struct {
	Rep byte
}

func (e *SOCKS5Error) Error() string {
	return fmt.Sprintf("socks5 reply rejected: REP=%#x (%s)", e.Rep, socks5RepName(e.Rep))
}

// socks5RepName returns the RFC 1928 name for a SOCKS5 reply code.
func socks5RepName(rep byte) string {
	switch rep {
	case 0x00:
		return "succeeded"
	case 0x01:
		return "general SOCKS server failure"
	case 0x02:
		return "connection not allowed by ruleset"
	case 0x03:
		return "network unreachable"
	case 0x04:
		return "host unreachable"
	case 0x05:
		return "connection refused"
	case 0x06:
		return "TTL expired"
	case 0x07:
		return "command not supported"
	case 0x08:
		return "address type not supported"
	default:
		return "unknown"
	}
}
