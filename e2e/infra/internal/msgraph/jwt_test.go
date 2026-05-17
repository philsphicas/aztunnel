package msgraph

import (
	"encoding/base64"
	"encoding/json"
	"testing"
)

func TestParseTenantIDFromJWT(t *testing.T) {
	payload := map[string]string{"tid": "11111111-2222-3333-4444-555555555555"}
	b, _ := json.Marshal(payload)
	token := "header." + base64.RawURLEncoding.EncodeToString(b) + ".sig"
	tid, err := parseTenantIDFromJWT(token)
	if err != nil {
		t.Fatalf("parseTenantIDFromJWT: %v", err)
	}
	if tid != "11111111-2222-3333-4444-555555555555" {
		t.Errorf("got tid %q", tid)
	}
}

func TestParseTenantIDFromJWT_NoTid(t *testing.T) {
	b, _ := json.Marshal(map[string]string{"sub": "x"})
	token := "h." + base64.RawURLEncoding.EncodeToString(b) + ".s"
	if _, err := parseTenantIDFromJWT(token); err == nil {
		t.Fatal("expected error for missing tid claim")
	}
}

func TestParseTenantIDFromJWT_Malformed(t *testing.T) {
	if _, err := parseTenantIDFromJWT("not-a-jwt"); err == nil {
		t.Fatal("expected error for malformed JWT")
	}
}

func TestEscapeODataString(t *testing.T) {
	cases := map[string]string{
		"plain":         "plain",
		"o'reilly":      "o''reilly",
		"a'b'c":         "a''b''c",
		"":              "",
		"no apostrophe": "no apostrophe",
	}
	for in, want := range cases {
		got, err := escapeODataString(in)
		if err != nil {
			t.Errorf("escapeODataString(%q) returned unexpected error: %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("escapeODataString(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestEscapeODataStringRejectsNonPrintable(t *testing.T) {
	cases := []string{
		"with\x00null",
		"control\x01char",
		"newline\nin\nname",
		"tab\there",
		"del\x7fchar",
	}
	for _, in := range cases {
		if _, err := escapeODataString(in); err == nil {
			t.Errorf("escapeODataString(%q) returned no error; want one", in)
		}
	}
}
