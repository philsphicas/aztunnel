package server

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Default SAS credentials applied when Config.SASKeyName / Config.SASKey
// are empty. These exist so the mock can run zero-config locally and so
// docs can name the dummy values that aztunnel must use to match.
//
// They are NOT secret. Anyone running aztunnel-relay with defaults
// effectively trusts every client — the mock is for local dev/CI only.
const (
	DefaultSASKeyName = "dev"
	DefaultSASKey     = "dev-secret-do-not-use-in-prod"
)

// validateSAS verifies the sb-hc-token query parameter on an inbound
// listener or sender request. The token format matches aztunnel's
// internal/relay.GenerateSASToken output:
//
//	SharedAccessSignature sr=<urlencoded-lowercase-uri>&sig=<urlencoded-base64>&se=<unix>&skn=<urlencoded-keyname>
//
// Validation steps:
//  1. token has the SharedAccessSignature prefix;
//  2. skn matches the configured key name (constant-time);
//  3. se (expiry) is in the future;
//  4. sig matches HMAC-SHA256("<urlencoded-sr>\n<se>", key), base64
//     standard encoding (constant-time).
//
// The audience (sr) is intentionally NOT compared against the request
// URL: the mock is a single-tenant test fixture and any signature-valid
// token grants access to all entities. This trades one Azure-Relay
// fidelity behavior for a much simpler test setup.
//
// Returns nil on success, or an error describing the failure on
// rejection. Callers MUST NOT echo the returned error message into the
// HTTP response — it can leak parse details that help an attacker
// fingerprint the validator. Use a generic 401 reason instead.
func (s *Server) validateSAS(r *http.Request) error {
	if s.cfg.SkipAuth {
		return nil
	}
	raw := r.URL.Query().Get("sb-hc-token")
	if raw == "" {
		return fmt.Errorf("missing sb-hc-token")
	}
	const prefix = "SharedAccessSignature "
	if !strings.HasPrefix(raw, prefix) {
		return fmt.Errorf("token has wrong prefix")
	}
	vals, err := url.ParseQuery(raw[len(prefix):])
	if err != nil {
		return fmt.Errorf("parse token: %w", err)
	}
	sr := vals.Get("sr")
	sig := vals.Get("sig")
	seStr := vals.Get("se")
	skn := vals.Get("skn")
	if sr == "" || sig == "" || seStr == "" || skn == "" {
		return fmt.Errorf("missing token field")
	}
	if !hmac.Equal([]byte(skn), []byte(s.cfg.SASKeyName)) {
		return fmt.Errorf("wrong skn")
	}
	se, err := strconv.ParseInt(seStr, 10, 64)
	if err != nil {
		return fmt.Errorf("bad se: %w", err)
	}
	if time.Now().Unix() >= se {
		return fmt.Errorf("token expired")
	}
	// Signature was computed over the URL-encoded lowercase URI.
	// url.ParseQuery already decoded sr, so re-encode to recover the
	// exact bytes that were signed.
	signedURI := url.QueryEscape(strings.ToLower(sr))
	expected := signSAS(signedURI, se, s.cfg.SASKey)
	if !hmac.Equal([]byte(expected), []byte(sig)) {
		return fmt.Errorf("signature mismatch")
	}
	return nil
}

// signSAS computes the SAS HMAC-SHA256 signature in the same form as
// aztunnel's internal/relay.GenerateSASToken (which base64-encodes the
// HMAC over "<encoded-uri>\n<expiry>"). It is intentionally duplicated
// here so the mock does not have to depend on an unexported helper.
func signSAS(encodedURI string, expiry int64, key string) string {
	str := encodedURI + "\n" + strconv.FormatInt(expiry, 10)
	mac := hmac.New(sha256.New, []byte(key))
	mac.Write([]byte(str))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}
