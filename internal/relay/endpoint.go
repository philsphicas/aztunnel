package relay

import (
	"net"
	"net/url"
	"strconv"
	"strings"
)

// DefaultRelaySuffix is the Azure Relay namespace suffix for the public cloud.
const DefaultRelaySuffix = ".servicebus.windows.net"

// SchemeWSS is the secure WebSocket scheme used to dial relays.
// aztunnel only supports TLS-protected relay endpoints; plain ws://
// is rejected at parse time.
const SchemeWSS = "wss"

// ParseRelay normalizes a relay input to a host[:port] string ready to
// concatenate after "wss://".
//
// Accepted input formats:
//   - Bare namespace name: "my-relay" → "my-relay" + defaultSuffix
//   - FQDN: "my-relay.servicebus.windows.net" → used as-is
//   - Bare host:port: "localhost:8443" → used as-is (no suffix)
//   - URI with sb/https/wss scheme: "wss://my-relay" → host extracted,
//     suffix applied if bare; "https://relay.example.com:8443/" →
//     port preserved
//
// Inputs with any other scheme (ws://, http://, ftp://, …) are
// rejected: aztunnel does not connect to plain-text relays. Use the
// `wss://` form (or omit the scheme) for self-signed mock relays and
// pair with --relay-insecure-tls.
//
// Suffix is appended only to bare hostnames with no dot AND no colon
// (port).
//
// The default port (443) is stripped so the returned host matches the
// canonical form Azure Relay's SAS audience validation expects (a SAS
// token signed for "https://host/entity" must not include the default
// port).
//
// Returns "" on empty input, malformed URI, unknown scheme, or invalid
// port. Callers should validate the result.
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
		// Reject userinfo: publicSchemeHost/dial only use scheme+host,
		// so credentials would be silently dropped — better to fail fast.
		if u.User != nil {
			return ""
		}
		// A trailing colon (e.g. "host:") parses cleanly but means an
		// explicit-but-empty port. url.Parse already rejects non-numeric
		// ports like "host:bad", but the empty form slips through.
		if strings.HasSuffix(u.Host, ":") {
			return ""
		}
		// Validate present port is in [1, 65535]. url.Parse accepts
		// "0" and out-of-range numerics that would never dial.
		if p := u.Port(); p != "" {
			if n, err := strconv.ParseUint(p, 10, 16); err != nil || n == 0 {
				return ""
			}
		}
		// Allow only TLS-protected schemes (and Service Bus's sb://,
		// which historically aliases to wss). Anything else — ws://,
		// http://, ftp://, … — is rejected: see package docs.
		switch strings.ToLower(u.Scheme) {
		case "sb", "https", "wss":
		default:
			return ""
		}
		host := u.Host
		// Apply suffix only to bare hostnames (no dot, no colon). This is
		// decided BEFORE stripping default ports so an explicit URL like
		// "https://localhost:443/" is not treated as a bare name after
		// the port is stripped.
		if !strings.ContainsAny(host, ".:") {
			host += defaultSuffix
		}
		return stripDefaultPort(host)
	}

	// Bare form: suffix applies only if no dot AND no colon (port).
	if strings.ContainsAny(input, ".:") {
		// Detect a bare IPv6 literal like "::1" or "2001:db8::1" and
		// wrap it in brackets so it can be embedded in a URL host.
		// IPv4 literals (To4 != nil) and "host:port" forms pass through.
		if ip := net.ParseIP(input); ip != nil && ip.To4() == nil {
			return "[" + input + "]"
		}
		// Bare host:port: a colon here is unambiguous because IPv6
		// literals were handled above and IPv4 forms have no colon.
		// Require a numeric port in [1, 65535] so we fail fast on
		// "host:" / "host:bad" / "host:0" rather than passing them
		// through to a later dial.
		if strings.ContainsRune(input, ':') {
			_, port, err := net.SplitHostPort(input)
			if err != nil || port == "" {
				return ""
			}
			if n, err := strconv.ParseUint(port, 10, 16); err != nil || n == 0 {
				return ""
			}
		}
		// Strip default port (443) so the returned host matches the
		// URL-form behaviour and the SAS audience canonicalisation.
		// E.g. "host:443" → "host".
		return stripDefaultPort(input)
	}
	return input + defaultSuffix
}

// stripDefaultPort removes the default wss port (443) from host.
// Returns host unchanged if the port is absent or non-default.
// Preserves IPv6 bracket form.
func stripDefaultPort(host string) string {
	h, p, err := net.SplitHostPort(host)
	if err != nil {
		return host
	}
	if p != "443" {
		return host
	}
	// Re-bracket IPv6 literals (SplitHostPort strips the brackets).
	if strings.Contains(h, ":") {
		return "[" + h + "]"
	}
	return h
}
