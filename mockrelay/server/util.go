package server

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// parseEntity extracts the entity from the *escaped* path of a
// /$hc/<entity> request. Callers must pass r.URL.EscapedPath() (not
// r.URL.Path) so that a name containing a percent-encoded '/' is
// preserved verbatim rather than splitting at the decoded slash.
// Empty entity is rejected.
func parseEntity(escapedPath string) (string, error) {
	const prefix = "/$hc/"
	if !strings.HasPrefix(escapedPath, prefix) {
		return "", fmt.Errorf("path %q does not start with %q", escapedPath, prefix)
	}
	rest := strings.TrimPrefix(escapedPath, prefix)
	// Split on raw '/' on the wire — a '/' inside the entity name
	// appears as %2F here and survives this split.
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		rest = rest[:i]
	}
	entity, err := url.PathUnescape(rest)
	if err != nil {
		return "", fmt.Errorf("decode entity: %w", err)
	}
	if entity == "" {
		return "", fmt.Errorf("empty entity")
	}
	return entity, nil
}

// validatePublicURL checks that a configured PublicURL is absolute, has
// a supported scheme, and includes a host. It also rejects URLs with a
// non-trivial path, query, or fragment, since rendezvousURL only honors
// the scheme+host pair and would silently drop a configured base path.
func validatePublicURL(s string) error {
	u, err := url.Parse(s)
	if err != nil {
		return err
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https", "ws", "wss":
	default:
		return fmt.Errorf("scheme %q not one of http, https, ws, wss", u.Scheme)
	}
	if u.Host == "" {
		return fmt.Errorf("missing host")
	}
	// Reject userinfo: rendezvousURL only honors scheme+host, so any
	// credentials would be silently dropped — better to fail fast.
	if u.User != nil {
		return fmt.Errorf("userinfo not supported in PublicURL")
	}
	// A trailing colon ("host:") parses cleanly but means an explicit
	// empty port. url.Parse already rejects non-numeric ports.
	if strings.HasSuffix(u.Host, ":") {
		return fmt.Errorf("empty port in PublicURL host %q", u.Host)
	}
	if p := u.Port(); p != "" {
		n, err := strconv.ParseUint(p, 10, 16)
		if err != nil || n == 0 {
			return fmt.Errorf("invalid port %q in PublicURL: must be numeric 1-65535", p)
		}
	}
	// Empty or single-slash paths are normal artifacts of url.Parse on
	// "https://host" vs "https://host/"; anything else would be silently
	// dropped by publicSchemeHost, which is worse than failing fast.
	if u.Path != "" && u.Path != "/" {
		return fmt.Errorf("path %q not supported in PublicURL (only scheme+host are honored)", u.Path)
	}
	if u.RawQuery != "" {
		return fmt.Errorf("query string not supported in PublicURL")
	}
	if u.Fragment != "" {
		return fmt.Errorf("fragment not supported in PublicURL")
	}
	return nil
}

// newRendezvousID returns a 128-bit cryptographically random ID encoded
// as a 32-character lowercase hex string. Used for accept message IDs.
// Note this is *not* RFC-4122 UUID formatting (no dashes), just an opaque
// random identifier.
func newRendezvousID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// redactURL strips sb-hc-token from a URL string before logging.
func redactURL(s string) string {
	const marker = "sb-hc-token="
	const replacement = "REDACTED"
	var out strings.Builder
	out.Grow(len(s))
	for {
		i := strings.Index(s, marker)
		if i < 0 {
			out.WriteString(s)
			return out.String()
		}
		out.WriteString(s[:i+len(marker)])
		out.WriteString(replacement)
		rest := s[i+len(marker):]
		end := strings.IndexAny(rest, "& ")
		if end < 0 {
			return out.String()
		}
		s = rest[end:]
	}
}
