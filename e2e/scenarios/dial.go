package scenarios

import (
	"fmt"
	"net"
	"time"
)

// dialSender opens a connection to the local aztunnel sender bind for
// the given mode. For SOCKS5 it performs the CONNECT handshake to the
// supplied target; for port-forward it returns the raw TCP
// connection. This is the client-side entry point the characterization
// harness measures from.
func dialSender(senderAddr, target string, mode SenderMode, timeout time.Duration) (net.Conn, error) {
	switch mode {
	case ModePortForward:
		return net.DialTimeout("tcp", senderAddr, timeout)
	case ModeSOCKS5:
		return DialSOCKS5(senderAddr, target, timeout)
	default:
		return nil, fmt.Errorf("unknown SenderMode %v", mode)
	}
}
