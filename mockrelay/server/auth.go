package server

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
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

// authKind classifies an inbound sb-hc-token by shape so the relay can
// charge the right validation cost and run the right validator. The
// real Azure Relay accepts either a SAS token or an Entra (OAuth2 JWT)
// bearer token in the same slot; aztunnel sends whichever the
// configured auth method produced (see internal/relay/dial.go).
//
// Classification is by shape, not by trying each validator:
//   - a "SharedAccessSignature " prefix => authSAS;
//   - else a JWT shape (three non-empty dot-separated parts, first
//     starting "eyJ") => authEntra;
//   - else => authSAS (fallback).
//
// The SAS fallback for unrecognised tokens is deliberate: missing,
// truncated, or garbage tokens flow down the SAS validator, which
// rejects them with 401 and charges AuthInternal — preserving the
// pre-existing auth-failure timing and the malformed-token behaviour
// exercised by auth_test.go. The heuristic is a test-fixture
// convenience, not a security boundary; it can misclassify a SAS token
// that happens to be JWT-shaped, which cannot occur for tokens this
// mock actually receives.
type authMethod int

const (
	authSAS authMethod = iota
	authEntra
)

func authKind(tok string) authMethod {
	if strings.HasPrefix(tok, "SharedAccessSignature ") {
		return authSAS
	}
	if looksLikeJWT(tok) {
		return authEntra
	}
	return authSAS
}

// looksLikeJWT reports whether tok has the compact-JWS shape: exactly
// three non-empty dot-separated segments with the first beginning
// "eyJ" (base64url of '{"'). It does not validate the signature — that
// is validateBearer's job.
func looksLikeJWT(tok string) bool {
	if !strings.HasPrefix(tok, "eyJ") {
		return false
	}
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		return false
	}
	for _, p := range parts {
		if p == "" {
			return false
		}
	}
	return true
}

// validateBearer verifies a fake Entra JWT minted by MintFakeBearerToken:
// an HS256 compact JWS signed with the relay's SAS key, with a payload
// carrying a future "exp". This is NOT how the real Azure Relay validates
// Entra tokens (RS256 against AAD signing keys with issuer/audience
// checks); it is a self-contained stand-in so the mock can model the
// Entra path end-to-end without a real directory.
//
// Steps: split header.payload.signature; recompute HMAC-SHA256 over
// "header.payload" with cfg.SASKey (constant-time compare); base64url-
// decode the payload and require exp > now. Returns nil on success.
// Like validateSAS, the error MUST NOT be echoed to the client.
func (s *Server) validateBearer(r *http.Request) error {
	if s.cfg.SkipAuth {
		return nil
	}
	tok := r.URL.Query().Get("sb-hc-token")
	if tok == "" {
		return fmt.Errorf("missing sb-hc-token")
	}
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		return fmt.Errorf("malformed JWT")
	}
	signingInput := parts[0] + "." + parts[1]
	expected := signHS256(signingInput, s.cfg.SASKey)
	if !hmac.Equal([]byte(expected), []byte(parts[2])) {
		return fmt.Errorf("signature mismatch")
	}
	payloadJSON, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return fmt.Errorf("decode payload: %w", err)
	}
	var claims struct {
		Exp int64 `json:"exp"`
	}
	if err := json.Unmarshal(payloadJSON, &claims); err != nil {
		return fmt.Errorf("parse claims: %w", err)
	}
	if claims.Exp == 0 {
		return fmt.Errorf("missing exp")
	}
	if time.Now().Unix() >= claims.Exp {
		return fmt.Errorf("token expired")
	}
	return nil
}

// signHS256 computes the base64url (unpadded) HMAC-SHA256 of
// signingInput under key, i.e. the signature segment of an HS256
// compact JWS.
func signHS256(signingInput, key string) string {
	mac := hmac.New(sha256.New, []byte(key))
	mac.Write([]byte(signingInput))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// MintFakeBearerToken produces a fake Entra bearer token that
// validateBearer accepts: an HS256 compact JWS signed with key, whose
// payload carries an exp ttl into the future. It is exported so the e2e
// mock's fake Entra credential can mint tokens the mock relay will
// validate as Entra (charging EntraValidate). The token is NOT a real
// AAD token and is meaningless outside this mock.
func MintFakeBearerToken(key string, ttl time.Duration) (string, error) {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`))
	exp := time.Now().Add(ttl).Unix()
	payloadJSON, err := json.Marshal(struct {
		Exp int64  `json:"exp"`
		Aud string `json:"aud"`
	}{Exp: exp, Aud: "https://relay.azure.net/.default"})
	if err != nil {
		return "", fmt.Errorf("marshal claims: %w", err)
	}
	payload := base64.RawURLEncoding.EncodeToString(payloadJSON)
	signingInput := header + "." + payload
	return signingInput + "." + signHS256(signingInput, key), nil
}

// authCost returns the relay-side token-validation delay to charge for
// this request, chosen by the inbound token's shape: EntraValidate on
// the Entra (JWT) path, AuthInternal on the SAS path. Exactly one
// applies per token-bearing leg (see DelayProfile.EntraValidate).
func (s *Server) authCost(r *http.Request) time.Duration {
	if authKind(r.URL.Query().Get("sb-hc-token")) == authEntra {
		return s.delayProfile.EntraValidate
	}
	return s.delayProfile.AuthInternal
}

// validateToken dispatches to the validator matching the inbound
// token's shape (validateBearer for Entra JWTs, validateSAS otherwise),
// so handleListen and handleConnect accept either auth method. As with
// the underlying validators, callers MUST emit a generic 401 reason and
// not echo the returned error.
func (s *Server) validateToken(r *http.Request) error {
	if authKind(r.URL.Query().Get("sb-hc-token")) == authEntra {
		return s.validateBearer(r)
	}
	return s.validateSAS(r)
}
