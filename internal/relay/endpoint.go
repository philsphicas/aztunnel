package relay

import (
	"net/url"
	"strconv"
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
// An explicit :443 on https/wss URIs is stripped so the canonical
// host string (and the SAS resource URI derived from it) matches the
// portless form Azure Relay expects. Non-default ports are preserved
// — the mock relay binds on 127.0.0.1:8080 by default (the TLS
// test-bed examples use :8443), and the demo stack relies on those
// explicit ports round-tripping unchanged.
//
// URL inputs are validated strictly: userinfo, non-root paths, query
// strings, fragments, trailing colons, and out-of-range ports
// (anything outside 1-65535) are rejected as malformed rather than
// silently dropped, so a misconfigured --relay fails fast instead of
// signing and dialing a subtly different resource than the user
// supplied.
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
		scheme := strings.ToLower(u.Scheme)
		switch scheme {
		case "sb", "https", "wss":
		default:
			return ""
		}
		// Reject URL components that ParseRelay would otherwise
		// silently discard — only scheme://host[:port] is supported.
		if u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.Contains(input, "#") {
			return ""
		}
		if u.Path != "" && u.Path != "/" {
			return ""
		}
		// Reject trailing colon (e.g. "wss://host:") and out-of-range
		// explicit ports so the failure surfaces here rather than as
		// an opaque WebSocket/SAS error later.
		if strings.HasSuffix(u.Host, ":") {
			return ""
		}
		if p := u.Port(); p != "" {
			n, err := strconv.Atoi(p)
			if err != nil || n < 1 || n > 65535 {
				return ""
			}
		}
		host := u.Host
		if u.Port() == "443" && (scheme == "https" || scheme == "wss") {
			host = u.Hostname()
		}
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
