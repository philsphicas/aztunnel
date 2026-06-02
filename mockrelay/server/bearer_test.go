package server

import (
	"context"
	"encoding/base64"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/philsphicas/aztunnel/internal/relay"
)

// TestAuthKind covers the token-shape classifier the relay uses to pick
// the validation cost + validator. SAS-prefixed tokens are SAS;
// JWT-shaped tokens are Entra; everything else (missing, truncated,
// garbage) falls back to SAS so it lands on the SAS 401 path.
func TestAuthKind(t *testing.T) {
	jwt, err := MintFakeBearerToken(DefaultSASKey, time.Minute)
	if err != nil {
		t.Fatalf("MintFakeBearerToken: %v", err)
	}
	cases := []struct {
		name string
		tok  string
		want authMethod
	}{
		{"sas prefix", "SharedAccessSignature sr=x&sig=y&se=1&skn=dev", authSAS},
		{"real jwt", jwt, authEntra},
		{"minimal jwt shape", "eyJ.a.b", authEntra},
		{"empty", "", authSAS},
		{"garbage", "not-a-token", authSAS},
		{"bearer prefixed jwt", "Bearer " + jwt, authSAS},
		{"jwt two parts", "eyJ.a", authSAS},
		{"jwt empty segment", "eyJ..b", authSAS},
		{"jwt wrong magic", "abc.def.ghi", authSAS},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := authKind(c.tok); got != c.want {
				t.Errorf("authKind(%q) = %v, want %v", c.tok, got, c.want)
			}
		})
	}
}

// TestValidateBearer covers the fake-Entra JWT validator directly:
// a freshly minted token validates; a token signed with the wrong key,
// an expired token, and a structurally malformed token are all
// rejected.
func TestValidateBearer(t *testing.T) {
	s := makeServer(t, Config{})

	good, err := MintFakeBearerToken(DefaultSASKey, time.Minute)
	if err != nil {
		t.Fatalf("mint good: %v", err)
	}
	wrongKey, err := MintFakeBearerToken("some-other-key", time.Minute)
	if err != nil {
		t.Fatalf("mint wrongKey: %v", err)
	}
	expired, err := MintFakeBearerToken(DefaultSASKey, -time.Minute)
	if err != nil {
		t.Fatalf("mint expired: %v", err)
	}
	// A JWT whose payload has no exp claim must be rejected.
	hdr := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`))
	noExpPayload := base64.RawURLEncoding.EncodeToString([]byte(`{"aud":"x"}`))
	noExp := hdr + "." + noExpPayload + "." + signHS256(hdr+"."+noExpPayload, DefaultSASKey)

	cases := []struct {
		name    string
		tok     string
		wantErr bool
	}{
		{"valid", good, false},
		{"wrong key", wrongKey, true},
		{"expired", expired, true},
		{"missing exp", noExp, true},
		{"two parts", "eyJ.a", true},
		{"empty", "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet,
				"http://relay/$hc/foo?sb-hc-token="+url.QueryEscape(c.tok), nil)
			err := s.validateBearer(r)
			if (err != nil) != c.wantErr {
				t.Errorf("validateBearer(%q) err = %v, wantErr = %v", c.tok, err, c.wantErr)
			}
		})
	}
}

// TestValidateToken_AcceptsEntraJWT confirms an end-to-end request
// carrying a valid Entra JWT gets past auth on the connect path (404
// "no listener" rather than 401), proving validateToken dispatched to
// the bearer validator.
func TestValidateToken_AcceptsEntraJWT(t *testing.T) {
	s := makeServer(t, Config{})
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	tok, err := MintFakeBearerToken(DefaultSASKey, time.Minute)
	if err != nil {
		t.Fatalf("MintFakeBearerToken: %v", err)
	}
	resp, err := http.Get(srv.URL + "/$hc/foo?sb-hc-action=connect&sb-hc-token=" + url.QueryEscape(tok))
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404 (entra auth passed; no listener)", resp.StatusCode)
	}
}

// TestValidateToken_EntraEdgeCases pins the sniff-heuristic boundaries
// at the HTTP layer: a JWT-shaped token with a bad signature is routed
// to the bearer validator and rejected (401), and a "Bearer <jwt>"
// token (space prefix, not JWT-shaped) falls back to the SAS validator
// and is also rejected (401). Neither must slip through.
func TestValidateToken_EntraEdgeCases(t *testing.T) {
	s := makeServer(t, Config{})
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	jwt, err := MintFakeBearerToken(DefaultSASKey, time.Minute)
	if err != nil {
		t.Fatalf("MintFakeBearerToken: %v", err)
	}
	for _, tok := range []string{
		"eyJ.a.b",       // JWT-shaped, bad signature -> bearer path -> 401
		"Bearer " + jwt, // not JWT-shaped -> SAS fallback -> 401
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

// TestDelayProfile_EntraPathPaysEntraValidate asserts the relay charges
// EntraValidate (not AuthInternal) when the inbound token is an Entra
// JWT, on BOTH the listener (handleListen) and sender (handleConnect)
// legs — and that a SAS token on the same handler pays only the much
// smaller AuthInternal. The profile sets EntraValidate >> AuthInternal
// so the two paths are wall-clock distinguishable.
func TestDelayProfile_EntraPathPaysEntraValidate(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping wall-clock DelayProfile test in -short mode")
	}
	p := DelayProfile{
		SLatency:          5 * time.Millisecond,
		LLatency:          5 * time.Millisecond,
		DNSLookup:         5 * time.Millisecond,
		AuthInternal:      5 * time.Millisecond,
		EntraValidate:     200 * time.Millisecond,
		MatchMakeInternal: 5 * time.Millisecond,
	}
	s, err := NewServerForTesting(Config{
		Logger:            slog.New(slog.NewTextHandler(io.Discard, nil)),
		SkipAuth:          false,
		RendezvousTimeout: 30 * time.Second,
	}, WithDelayProfile(p))
	if err != nil {
		t.Fatalf("NewServerForTesting: %v", err)
	}
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	jwt, err := MintFakeBearerToken(DefaultSASKey, time.Minute)
	if err != nil {
		t.Fatalf("MintFakeBearerToken: %v", err)
	}
	sas, err := relay.GenerateSASToken(
		relay.ResourceURI("relay.example.com", "foo"),
		DefaultSASKeyName, DefaultSASKey, time.Minute,
	)
	if err != nil {
		t.Fatalf("GenerateSASToken: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Listener leg: a valid JWT listen reaches a 101 upgrade, so it pays
	// the full listener lane plus EntraValidate.
	listenEntra := p.DNSLookup +
		(hopsHandshake+hopsWSGet+hopsResponse)*p.LLatency + p.EntraValidate
	start := time.Now()
	lws, _, err := websocket.Dial(ctx,
		wsURLOf(srv.URL)+"/$hc/foo?sb-hc-action=listen&sb-hc-token="+url.QueryEscape(jwt), nil)
	listenElapsed := time.Since(start)
	if err != nil {
		t.Fatalf("listen with JWT: %v", err)
	}
	_ = lws.CloseNow()
	withinTolerance(t, "entra listen lane", listenElapsed, listenEntra, listenEntra+500*time.Millisecond)

	// Sender leg (no listener registered): a valid JWT connect pays
	// EntraValidate + matchmake before the 404, so its lower bound
	// includes EntraValidate.
	connectEntraLower := p.DNSLookup + (hopsHandshake+hopsWSGet)*p.SLatency + p.EntraValidate
	start = time.Now()
	resp, err := http.Get(srv.URL + "/$hc/bar?sb-hc-action=connect&sb-hc-token=" + url.QueryEscape(jwt))
	connectElapsed := time.Since(start)
	if err != nil {
		t.Fatalf("connect with JWT: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("connect status = %d, want 404", resp.StatusCode)
	}
	if connectElapsed < connectEntraLower {
		t.Errorf("entra connect elapsed %v < lower bound %v (EntraValidate not charged)",
			connectElapsed, connectEntraLower)
	}

	// SAS leg on the same handler must NOT pay EntraValidate: a valid
	// SAS connect returns 404 well under the EntraValidate floor.
	start = time.Now()
	resp, err = http.Get(srv.URL + "/$hc/baz?sb-hc-action=connect&sb-hc-token=" + url.QueryEscape(sas))
	sasElapsed := time.Since(start)
	if err != nil {
		t.Fatalf("connect with SAS: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("sas connect status = %d, want 404", resp.StatusCode)
	}
	if sasElapsed >= p.EntraValidate {
		t.Errorf("sas connect elapsed %v >= EntraValidate %v (SAS path wrongly charged EntraValidate)",
			sasElapsed, p.EntraValidate)
	}
}
