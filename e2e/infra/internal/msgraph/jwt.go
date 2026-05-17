package msgraph

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
)

// parseTenantIDFromJWT extracts the "tid" claim from a JWT access token.
// Tokens issued by Microsoft identity platform always include this claim.
// We intentionally do not validate the signature — the token was obtained
// from a trusted credential and is only being inspected to extract the
// tenant ID for outbound configuration; we never act on the token's
// authorization claims.
func parseTenantIDFromJWT(token string) (string, error) {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return "", errors.New("invalid JWT: fewer than 2 segments")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		// Try standard encoding as a fallback for tokens with padding.
		payload, err = base64.StdEncoding.DecodeString(addPadding(parts[1]))
		if err != nil {
			return "", err
		}
	}
	var claims struct {
		TID string `json:"tid"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return "", err
	}
	if claims.TID == "" {
		return "", errors.New("JWT did not include a tid claim")
	}
	return claims.TID, nil
}

// addPadding appends '=' chars so a base64 string has length divisible by 4.
func addPadding(s string) string {
	if rem := len(s) % 4; rem != 0 {
		s += strings.Repeat("=", 4-rem)
	}
	return s
}
