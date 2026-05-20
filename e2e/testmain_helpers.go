// Package e2e — config-resolution helpers used by TestMain.
//
// This file is intentionally untagged so the pure-Go helpers can be
// unit-tested without the e2e build tag (which would otherwise drag in
// every Azure-dependent test fixture). Anything that actually calls
// Azure SDKs lives in testmain_test.go (//go:build e2e).
package e2e

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// LocalConfigFilename is the basename of the per-developer config file
// produced by `make e2e-setup` / `make e2e-attach` and consumed by
// TestMain. Lives at e2e/.local.json relative to the repo root.
const LocalConfigFilename = ".local.json"

// LocalConfig is the on-disk shape of e2e/.local.json. Field tags use
// camelCase; missing/zero fields are tolerated by load but checked by
// the consumers that need them.
type LocalConfig struct {
	Subscription  string `json:"subscription"`
	Tenant        string `json:"tenant"`
	UserObjectID  string `json:"userObjectId"`
	ResourceGroup string `json:"resourceGroup"`
	RelayName     string `json:"relayName"`
	SASOnly       bool   `json:"sasOnly"`
	CreatedAt     string `json:"createdAt"`
}

// loadLocalConfig reads <dir>/.local.json. Returns the wrapped
// os.ErrNotExist when the file is absent (the env-vars-only path);
// returns a parse error otherwise.
func loadLocalConfig(dir string) (*LocalConfig, error) {
	path := filepath.Join(dir, LocalConfigFilename)
	data, err := os.ReadFile(path) //nolint:gosec // path is composed from a test-controlled dir + a fixed basename.
	if err != nil {
		return nil, err
	}
	var cfg LocalConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return &cfg, nil
}

// resolvedConfig is the union of fields TestMain needs regardless of
// where it found them.
type resolvedConfig struct {
	Subscription  string
	ResourceGroup string
	RelayName     string
	// Source describes where the values came from. Values: "env"
	// (CI / explicit override) or "local-config" (developer flow).
	Source string
	// Local is the parsed .local.json when Source == "local-config".
	// nil otherwise.
	Local *LocalConfig
}

// envLookup mirrors os.LookupEnv so resolveTestConfig is unit-testable
// without process-wide env mutation.
type envLookup func(key string) (string, bool)

// errNoConfig is the sentinel returned when neither env vars nor a
// local config file are present. Its Error() text is the message
// TestMain prints to stderr before exiting.
var errNoConfig = errors.New("e2e configuration not found")

// resolveTestConfig applies the precedence:
//
//  1. AZURE_SUBSCRIPTION_ID + E2E_RESOURCE_GROUP both set → env path
//     (CI / explicit override). E2E_RELAY_NAME also read from env.
//  2. <dir>/.local.json present → file path (developer flow).
//  3. Otherwise → errNoConfig.
//
// The env path takes precedence even when a local config file is
// present, which is what lets CI shadow a developer's persisted
// config and what lets a developer override the file ad-hoc.
func resolveTestConfig(env envLookup, dir string) (*resolvedConfig, error) {
	subEnv, _ := env("AZURE_SUBSCRIPTION_ID")
	rgEnv, _ := env("E2E_RESOURCE_GROUP")
	if subEnv != "" && rgEnv != "" {
		nsEnv, _ := env("E2E_RELAY_NAME")
		return &resolvedConfig{
			Subscription:  subEnv,
			ResourceGroup: rgEnv,
			RelayName:     nsEnv,
			Source:        "env",
		}, nil
	}
	cfg, err := loadLocalConfig(dir)
	if err == nil {
		if cfg.Subscription == "" || cfg.ResourceGroup == "" || cfg.RelayName == "" {
			return nil, fmt.Errorf("e2e: %s/%s is missing required fields (subscription/resourceGroup/relayName); re-run `make e2e-setup`",
				dir, LocalConfigFilename)
		}
		return &resolvedConfig{
			Subscription:  cfg.Subscription,
			ResourceGroup: cfg.ResourceGroup,
			RelayName:     cfg.RelayName,
			Source:        "local-config",
			Local:         cfg,
		}, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	return nil, errNoConfig
}

// currentUserOIDFn returns the AAD object ID of the current credential.
// Injected as a function so checkIdentityDrift is testable without an
// Azure dependency.
type currentUserOIDFn func(ctx context.Context) (string, error)

// checkIdentityDrift returns nil if local is nil, has no
// UserObjectID, or the live OID matches the persisted one. Otherwise
// returns a formatted error matching the message documented in the
// spec.
func checkIdentityDrift(ctx context.Context, local *LocalConfig, current currentUserOIDFn) error {
	if local == nil || local.UserObjectID == "" {
		return nil
	}
	got, err := current(ctx)
	if err != nil {
		return fmt.Errorf("resolve current Azure identity: %w", err)
	}
	if !strings.EqualFold(got, local.UserObjectID) {
		return &identityDriftError{
			expectedOID: local.UserObjectID,
			currentOID:  got,
		}
	}
	return nil
}

type identityDriftError struct {
	expectedOID string
	currentOID  string
}

func (e *identityDriftError) Error() string {
	return fmt.Sprintf("==> e2e: persisted e2e/%s is for user %s, but the current Azure\n"+
		"    credential resolves to %s. Either:\n"+
		"      - `az login` with the original user, OR\n"+
		"      - `rm e2e/%s && make e2e-setup` to re-provision under your\n"+
		"        current identity (your new RG will be aztunnel-e2e-<your-alias>).",
		LocalConfigFilename, e.expectedOID, e.currentOID, LocalConfigFilename)
}

// parseOIDFromJWT extracts the "oid" claim from a JWT access token.
// Used by the e2e-tagged TestMain to derive the current AAD object id
// from a token issued by DefaultAzureCredential. The token's signature
// is not validated — the credential is trusted and we only inspect a
// claim to compare against persisted config; no authorization decision
// is being made.
func parseOIDFromJWT(token string) (string, error) {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return "", errors.New("invalid JWT: fewer than 2 segments")
	}
	// JWT payloads are spec'd as base64url without padding, but some
	// issuers pad anyway; tolerate both by stripping trailing '=' and
	// using RawURLEncoding (which handles the -/_ alphabet).
	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return "", fmt.Errorf("decode JWT payload: %w", err)
	}
	var claims struct {
		OID string `json:"oid"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return "", err
	}
	if claims.OID == "" {
		return "", errors.New("JWT did not include an oid claim")
	}
	return claims.OID, nil
}
