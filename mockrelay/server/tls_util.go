package server

import (
	"crypto/sha256"
	"encoding/hex"
	"net"
)

// parseIPs returns net.IPs parsed from string form, dropping any that
// fail to parse. Used by SelfSignedTLS to populate the cert IP SAN list.
func parseIPs(ips ...string) []net.IP {
	out := make([]net.IP, 0, len(ips))
	for _, s := range ips {
		if ip := net.ParseIP(s); ip != nil {
			out = append(out, ip)
		}
	}
	return out
}

// fingerprintHex returns a colon-separated hex SHA-256 fingerprint of a
// DER-encoded certificate, in the format used by openssl x509 -fingerprint.
func fingerprintHex(derBytes []byte) string {
	sum := sha256.Sum256(derBytes)
	out := make([]byte, 0, len(sum)*3-1)
	for i, b := range sum {
		if i > 0 {
			out = append(out, ':')
		}
		out = append(out, hex.EncodeToString([]byte{b})...)
	}
	// Uppercase to match openssl's output.
	for i := range out {
		if out[i] >= 'a' && out[i] <= 'f' {
			out[i] -= 32
		}
	}
	return string(out)
}
