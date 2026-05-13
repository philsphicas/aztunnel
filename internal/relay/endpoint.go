package relay

import (
	"net"
	"net/url"
	"strconv"
	"strings"
)

// DefaultRelaySuffix is the Azure Relay namespace suffix for the public cloud.
const DefaultRelaySuffix = ".servicebus.windows.net"

// SchemeWSS is the secure WebSocket scheme used by real Azure Relay.
const SchemeWSS = "wss"

// SchemeWS is the insecure WebSocket scheme, useful for mock/self-hosted relays.
const SchemeWS = "ws"

// ParseRelay normalizes a relay input to (host[:port], scheme).
//
// Accepted input formats:
//   - Bare namespace name: "my-relay" → "my-relay" + defaultSuffix, scheme=wss
//   - FQDN: "my-relay.servicebus.windows.net" → used as-is, scheme=wss
//   - Bare host:port: "localhost:8080" → used as-is (no suffix), scheme=wss
//   - URI with scheme: "sb://my-relay" → host extracted, suffix applied if bare, scheme=wss
//   - URI with scheme + FQDN: "wss://my-relay.servicebus.windows.net" → scheme=wss
//   - URI with insecure scheme: "ws://localhost:8080" or "http://..." → scheme=ws
//   - URI with port: "https://relay.example.com:8443/" → port preserved, scheme=wss
//
// The scheme is derived from URI form: "http"/"ws" → "ws", anything else
// ("https"/"wss"/"sb"/"") → "wss". For bare inputs the default is "wss".
//
// Suffix is appended only to bare hostnames with no dot AND no colon (port).
//
// Default ports are stripped (443 for wss/https, 80 for ws/http) so the
// returned host matches the canonical form Azure Relay's SAS audience
// validation expects (a SAS token signed for "https://host/entity" must
// not include the default port).
//
// Returns "" for endpoint and scheme on empty input or malformed URIs.
// Callers should validate the result.
func ParseRelay(input, defaultSuffix string) (endpoint, scheme string) {
	input = strings.TrimSpace(input)
	if input == "" {
		return "", ""
	}

	if strings.Contains(input, "://") {
		u, err := url.Parse(input)
		if err != nil || u.Host == "" {
			return "", ""
		}
		// Reject userinfo: publicSchemeHost/dial only use scheme+host,
		// so credentials would be silently dropped — better to fail fast.
		if u.User != nil {
			return "", ""
		}
		// A trailing colon (e.g. "host:") parses cleanly but means an
		// explicit-but-empty port. url.Parse already rejects non-numeric
		// ports like "host:bad", but the empty form slips through.
		if strings.HasSuffix(u.Host, ":") {
			return "", ""
		}
		// Validate present port is in [1, 65535]. url.Parse accepts
		// "0" and out-of-range numerics that would never dial.
		if p := u.Port(); p != "" {
			if n, err := strconv.ParseUint(p, 10, 16); err != nil || n == 0 {
				return "", ""
			}
		}
		scheme = SchemeWSS
		switch strings.ToLower(u.Scheme) {
		case "http", "ws":
			scheme = SchemeWS
		}
		host := u.Host
		// Apply suffix only to bare hostnames (no dot, no colon). This is
		// decided BEFORE stripping default ports so an explicit URL like
		// "http://localhost:80/" is not treated as a bare name after the
		// port is stripped.
		if !strings.ContainsAny(host, ".:") {
			host += defaultSuffix
		}
		host = stripDefaultPort(host, scheme)
		return host, scheme
	}

	// Bare form: suffix applies only if no dot AND no colon (port).
	if strings.ContainsAny(input, ".:") {
		// Detect a bare IPv6 literal like "::1" or "2001:db8::1" and
		// wrap it in brackets so it can be embedded in a URL host.
		// IPv4 literals (To4 != nil) and "host:port" forms pass through.
		if ip := net.ParseIP(input); ip != nil && ip.To4() == nil {
			return "[" + input + "]", SchemeWSS
		}
		// Bare host:port: a colon here is unambiguous because IPv6
		// literals were handled above and IPv4 forms have no colon.
		// Require a numeric port in [1, 65535] so we fail fast on
		// "host:" / "host:bad" / "host:0" rather than passing them
		// through to a later dial.
		if strings.ContainsRune(input, ':') {
			_, port, err := net.SplitHostPort(input)
			if err != nil || port == "" {
				return "", ""
			}
			if n, err := strconv.ParseUint(port, 10, 16); err != nil || n == 0 {
				return "", ""
			}
		}
		return input, SchemeWSS
	}
	return input + defaultSuffix, SchemeWSS
}

// stripDefaultPort removes the default port for the given scheme from
// host (443 for wss, 80 for ws). Returns host unchanged if the port is
// absent or non-default. Preserves IPv6 bracket form.
func stripDefaultPort(host, scheme string) string {
	h, p, err := net.SplitHostPort(host)
	if err != nil {
		return host
	}
	defaultPort := ""
	switch scheme {
	case SchemeWSS:
		defaultPort = "443"
	case SchemeWS:
		defaultPort = "80"
	}
	if defaultPort == "" || p != defaultPort {
		return host
	}
	// Re-bracket IPv6 literals (SplitHostPort strips the brackets).
	if strings.Contains(h, ":") {
		return "[" + h + "]"
	}
	return h
}

// ParseRelayEndpoint is the host-only equivalent of ParseRelay, retained
// for backward compatibility with code that does not need the scheme.
func ParseRelayEndpoint(input, defaultSuffix string) string {
	endpoint, _ := ParseRelay(input, defaultSuffix)
	return endpoint
}
