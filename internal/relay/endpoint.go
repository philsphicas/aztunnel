package relay

import (
	"net/url"
	"strings"
)

// DefaultRelaySuffix is the Azure Relay namespace suffix for the public cloud.
const DefaultRelaySuffix = ".servicebus.windows.net"

// ParseRelay normalizes a relay input to a host[:port] string ready to
// concatenate after "wss://".
//
// Accepted input formats:
//   - Bare namespace name: "my-relay" → "my-relay" + defaultSuffix
//   - FQDN: "my-relay.servicebus.windows.net" → used as-is
//   - URI with sb/https/wss scheme: "wss://relay.example.com:8443" →
//     "relay.example.com:8443"; "sb://my-relay" → bare host gets the
//     suffix appended.
//
// Inputs with any other scheme (ws://, http://, ftp://, …) are
// rejected: aztunnel only dials TLS-protected relays. For self-signed
// mock relays use the wss:// form together with --relay-insecure-tls.
//
// Bare host:port (or bare IPv6 literal) is not accepted — use the
// wss:// URL form for those.
//
// Returns "" on empty input, malformed URI, or unknown scheme.
func ParseRelay(input, defaultSuffix string) string {
	input = strings.TrimSpace(input)
	if input == "" {
		return ""
	}

	if strings.Contains(input, "://") {
		u, err := url.Parse(input)
		if err != nil || u.Host == "" {
			return ""
		}
		switch strings.ToLower(u.Scheme) {
		case "sb", "https", "wss":
		default:
			return ""
		}
		host := u.Host
		if !strings.ContainsAny(host, ".:") {
			host += defaultSuffix
		}
		return host
	}

	if strings.Contains(input, ":") {
		// Bare host:port (and bare IPv6 literals) are not accepted —
		// use a wss:// URL with explicit port instead.
		return ""
	}
	if strings.Contains(input, ".") {
		return input
	}
	return input + defaultSuffix
}
