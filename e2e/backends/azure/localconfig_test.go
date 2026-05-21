package azure

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolveTestConfig_EnvWins(t *testing.T) {
	dir := t.TempDir()
	writeLocalConfigJSON(t, dir, LocalConfig{
		Subscription:  "file-sub",
		ResourceGroup: "file-rg",
		RelayName:     "file-ns",
	})
	env := mapEnv(map[string]string{
		"AZURE_SUBSCRIPTION_ID": "env-sub",
		"E2E_RESOURCE_GROUP":    "env-rg",
		"E2E_RELAY_NAME":        "env-ns",
	})
	got, err := resolveTestConfig(env, dir)
	if err != nil {
		t.Fatalf("resolveTestConfig: %v", err)
	}
	if got.Source != "env" {
		t.Errorf("Source = %q, want \"env\"", got.Source)
	}
	if got.Subscription != "env-sub" || got.ResourceGroup != "env-rg" || got.RelayName != "env-ns" {
		t.Errorf("env values not used: %+v", got)
	}
	if got.Local != nil {
		t.Errorf("Local should be nil on env path, got %+v", got.Local)
	}
}

func TestResolveTestConfig_EnvMissingFallsBackToFile(t *testing.T) {
	dir := t.TempDir()
	writeLocalConfigJSON(t, dir, LocalConfig{
		Subscription:  "file-sub",
		Tenant:        "file-tenant",
		UserObjectID:  "file-oid",
		ResourceGroup: "file-rg",
		RelayName:     "file-ns",
		SASOnly:       true,
		CreatedAt:     "2026-05-20T00:00:00Z",
	})
	// Only E2E_RELAY_NAME is set (a single env var should not be
	// enough to short-circuit the file path).
	env := mapEnv(map[string]string{"E2E_RELAY_NAME": "env-ns"})
	got, err := resolveTestConfig(env, dir)
	if err != nil {
		t.Fatalf("resolveTestConfig: %v", err)
	}
	if got.Source != "local-config" {
		t.Errorf("Source = %q, want \"local-config\"", got.Source)
	}
	if got.RelayName != "file-ns" {
		t.Errorf("RelayName = %q, want \"file-ns\" (file should win when env path incomplete)", got.RelayName)
	}
	if got.Local == nil || !got.Local.SASOnly || got.Local.UserObjectID != "file-oid" {
		t.Errorf("Local fields not populated: %+v", got.Local)
	}
}

func TestResolveTestConfig_NoConfig(t *testing.T) {
	dir := t.TempDir()
	env := mapEnv(nil)
	_, err := resolveTestConfig(env, dir)
	if !errors.Is(err, errNoConfig) {
		t.Fatalf("err = %v, want errNoConfig", err)
	}
}

func TestResolveTestConfig_FileMalformed(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, LocalConfigFilename), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	env := mapEnv(nil)
	_, err := resolveTestConfig(env, dir)
	if err == nil || errors.Is(err, errNoConfig) {
		t.Fatalf("expected parse error, got %v", err)
	}
}

func TestResolveTestConfig_FileMissingRequiredField(t *testing.T) {
	dir := t.TempDir()
	writeLocalConfigJSON(t, dir, LocalConfig{
		Subscription:  "file-sub",
		ResourceGroup: "file-rg",
		// RelayName intentionally omitted.
	})
	_, err := resolveTestConfig(mapEnv(nil), dir)
	if err == nil {
		t.Fatal("expected error for missing relayName")
	}
	if !strings.Contains(err.Error(), "make e2e-setup") {
		t.Errorf("error should point at `make e2e-setup`: %v", err)
	}
}

func TestCheckIdentityDrift_NilLocal(t *testing.T) {
	if err := checkIdentityDrift(context.Background(), nil, func(context.Context) (string, error) {
		t.Fatal("currentUserOIDFn must not be called when local is nil")
		return "", nil
	}); err != nil {
		t.Errorf("nil local should be a no-op, got %v", err)
	}
}

func TestCheckIdentityDrift_EmptyPersistedOID(t *testing.T) {
	local := &LocalConfig{Subscription: "x"}
	if err := checkIdentityDrift(context.Background(), local, func(context.Context) (string, error) {
		t.Fatal("currentUserOIDFn must not be called when persisted OID is empty")
		return "", nil
	}); err != nil {
		t.Errorf("empty persisted OID should be a no-op, got %v", err)
	}
}

func TestCheckIdentityDrift_Match(t *testing.T) {
	local := &LocalConfig{UserObjectID: "11111111-1111-1111-1111-111111111111"}
	live := func(context.Context) (string, error) {
		return "11111111-1111-1111-1111-111111111111", nil
	}
	if err := checkIdentityDrift(context.Background(), local, live); err != nil {
		t.Errorf("match should be no-op, got %v", err)
	}
}

func TestCheckIdentityDrift_MatchCaseInsensitive(t *testing.T) {
	local := &LocalConfig{UserObjectID: "AAAAAAAA-AAAA-AAAA-AAAA-AAAAAAAAAAAA"}
	live := func(context.Context) (string, error) {
		return "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa", nil
	}
	if err := checkIdentityDrift(context.Background(), local, live); err != nil {
		t.Errorf("case-insensitive UUID compare should match, got %v", err)
	}
}

func TestCheckIdentityDrift_Mismatch(t *testing.T) {
	local := &LocalConfig{UserObjectID: "11111111-1111-1111-1111-111111111111"}
	live := func(context.Context) (string, error) {
		return "00000000-0000-0000-0000-000000000000", nil
	}
	err := checkIdentityDrift(context.Background(), local, live)
	if err == nil {
		t.Fatal("expected mismatch error")
	}
	msg := err.Error()
	for _, want := range []string{"persisted", "11111111-1111-1111-1111-111111111111", "00000000-0000-0000-0000-000000000000", "make e2e-setup"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error missing %q: %s", want, msg)
		}
	}
}

func TestCheckIdentityDrift_FetchError(t *testing.T) {
	local := &LocalConfig{UserObjectID: "x"}
	live := func(context.Context) (string, error) {
		return "", errors.New("boom")
	}
	err := checkIdentityDrift(context.Background(), local, live)
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("error should wrap fetch failure, got %v", err)
	}
}

func TestParseOIDFromJWT(t *testing.T) {
	want := "11111111-1111-1111-1111-111111111111"
	b, _ := json.Marshal(map[string]string{"oid": want})
	token := "header." + base64.RawURLEncoding.EncodeToString(b) + ".sig"
	got, err := parseOIDFromJWT(token)
	if err != nil {
		t.Fatalf("parseOIDFromJWT: %v", err)
	}
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestParseOIDFromJWT_PaddedBase64(t *testing.T) {
	want := "00000000-0000-0000-0000-000000000000"
	b, _ := json.Marshal(map[string]string{"oid": want})
	// Standard base64url with padding (URLEncoding emits = padding;
	// the parser strips it).
	encoded := base64.URLEncoding.EncodeToString(b)
	token := "h." + encoded + ".s"
	got, err := parseOIDFromJWT(token)
	if err != nil {
		t.Fatalf("parseOIDFromJWT: %v", err)
	}
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestParseOIDFromJWT_NoOID(t *testing.T) {
	b, _ := json.Marshal(map[string]string{"sub": "x"})
	token := "h." + base64.RawURLEncoding.EncodeToString(b) + ".s"
	if _, err := parseOIDFromJWT(token); err == nil {
		t.Fatal("expected error for missing oid claim")
	}
}

func TestParseOIDFromJWT_Malformed(t *testing.T) {
	if _, err := parseOIDFromJWT("not-a-jwt"); err == nil {
		t.Fatal("expected error for malformed JWT")
	}
}

func mapEnv(m map[string]string) envLookup {
	return func(key string) (string, bool) {
		v, ok := m[key]
		return v, ok
	}
}

func writeLocalConfigJSON(t *testing.T, dir string, cfg LocalConfig) {
	t.Helper()
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, LocalConfigFilename), data, 0o600); err != nil {
		t.Fatal(err)
	}
}
