package server

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/philsphicas/aztunnel/internal/relay"
)

// makeServer is a small helper to construct a Server with the given
// SAS configuration and logger silenced.
func makeServer(t *testing.T, cfg Config) *Server {
	t.Helper()
	cfg.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	s, err := NewServer(cfg)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	return s
}

// readBodyTrim reads and trims the response body — used by
// TestValidateSAS_DoesNotLeakDetails.
func readBodyTrim(t *testing.T, resp *http.Response) string {
	t.Helper()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return strings.TrimSpace(string(b))
}

func TestValidateSAS_RejectsMissingToken(t *testing.T) {
	s := makeServer(t, Config{})
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/$hc/foo?sb-hc-action=connect")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
}

func TestValidateSAS_RejectsBadSignature(t *testing.T) {
	s := makeServer(t, Config{})
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	tok, err := relay.GenerateSASToken(
		relay.ResourceURI("relay.example.com", "foo"),
		DefaultSASKeyName,
		"WRONG-KEY", // signed with the wrong secret
		1*time.Minute,
	)
	if err != nil {
		t.Fatalf("GenerateSASToken: %v", err)
	}
	resp, err := http.Get(srv.URL + "/$hc/foo?sb-hc-action=connect&sb-hc-token=" + url.QueryEscape(tok))
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
}

func TestValidateSAS_RejectsWrongKeyName(t *testing.T) {
	s := makeServer(t, Config{})
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	tok, err := relay.GenerateSASToken(
		relay.ResourceURI("relay.example.com", "foo"),
		"someone-else",
		DefaultSASKey,
		1*time.Minute,
	)
	if err != nil {
		t.Fatalf("GenerateSASToken: %v", err)
	}
	resp, err := http.Get(srv.URL + "/$hc/foo?sb-hc-action=connect&sb-hc-token=" + url.QueryEscape(tok))
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
}

func TestValidateSAS_RejectsExpired(t *testing.T) {
	s := makeServer(t, Config{})
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	tok, err := relay.GenerateSASToken(
		relay.ResourceURI("relay.example.com", "foo"),
		DefaultSASKeyName,
		DefaultSASKey,
		-1*time.Minute, // already expired
	)
	if err != nil {
		t.Fatalf("GenerateSASToken: %v", err)
	}
	resp, err := http.Get(srv.URL + "/$hc/foo?sb-hc-action=connect&sb-hc-token=" + url.QueryEscape(tok))
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
}

func TestValidateSAS_AcceptsValidToken(t *testing.T) {
	s := makeServer(t, Config{})
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	tok, err := relay.GenerateSASToken(
		relay.ResourceURI("relay.example.com", "foo"),
		DefaultSASKeyName,
		DefaultSASKey,
		1*time.Minute,
	)
	if err != nil {
		t.Fatalf("GenerateSASToken: %v", err)
	}
	// Use connect (no listener registered) — a valid token gets us past
	// validateSAS and we expect 404 ("no active listener") rather than
	// 401. That confirms auth passed without needing a real WS upgrade.
	resp, err := http.Get(srv.URL + "/$hc/foo?sb-hc-action=connect&sb-hc-token=" + url.QueryEscape(tok))
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404 (auth passed; no listener)", resp.StatusCode)
	}
}

func TestValidateSAS_SkipAuthLetsAnyRequestThrough(t *testing.T) {
	s := makeServer(t, Config{SkipAuth: true})
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/$hc/foo?sb-hc-action=connect")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404 (auth skipped; no listener)", resp.StatusCode)
	}
}

func TestValidateSAS_AppliesToListenAction(t *testing.T) {
	s := makeServer(t, Config{})
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/$hc/foo?sb-hc-action=listen")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
}

func TestValidateSAS_RejectsMalformedPrefix(t *testing.T) {
	s := makeServer(t, Config{})
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	for _, tok := range []string{
		"Bearer abc",
		"sr=foo&sig=bar&se=1&skn=dev",
		"",
		"SharedAccessSignature",
	} {
		resp, err := http.Get(srv.URL + "/$hc/foo?sb-hc-action=connect&sb-hc-token=" + url.QueryEscape(tok))
		if err != nil {
			t.Fatalf("GET %q: %v", tok, err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("token=%q status=%d, want 401", tok, resp.StatusCode)
		}
	}
}

// Ensure validateSAS does not echo any internal failure mode to the
// HTTP response body. The body should be a generic "unauthorized".
func TestValidateSAS_DoesNotLeakDetails(t *testing.T) {
	s := makeServer(t, Config{})
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	for _, tok := range []string{
		"",
		"SharedAccessSignature ;;;;",
		"SharedAccessSignature sr=&sig=&se=&skn=",
	} {
		resp, err := http.Get(srv.URL + "/$hc/foo?sb-hc-action=connect&sb-hc-token=" + url.QueryEscape(tok))
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		body := readBodyTrim(t, resp)
		_ = resp.Body.Close()
		if !strings.EqualFold(body, "unauthorized") {
			t.Errorf("token=%q body=%q want %q", tok, body, "unauthorized")
		}
	}
}
