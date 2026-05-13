package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
)

func TestParseEntity(t *testing.T) {
	cases := []struct {
		path    string
		want    string
		wantErr bool
	}{
		{"/$hc/myentity", "myentity", false},
		{"/$hc/path-with-dashes", "path-with-dashes", false},
		{"/$hc/with%2Fslash", "with/slash", false},
		{"/$hc/", "", true},
		{"/hc/foo", "", true},
		{"/$hc/foo/bar", "foo", false},
		{"/$hc/foo%ZZ", "", true},
	}
	for _, tc := range cases {
		got, err := parseEntity(tc.path)
		if tc.wantErr {
			if err == nil {
				t.Errorf("parseEntity(%q) = %q, want error", tc.path, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseEntity(%q) error: %v", tc.path, err)
			continue
		}
		if got != tc.want {
			t.Errorf("parseEntity(%q) = %q, want %q", tc.path, got, tc.want)
		}
	}
}

func TestValidatePublicURL(t *testing.T) {
	cases := []struct {
		s       string
		wantErr bool
	}{
		{"http://relay.example.com", false},
		{"http://relay.example.com/", false},
		{"https://relay.example.com:8443", false},
		{"ws://localhost:8080", false},
		{"wss://relay.example.com", false},
		{"ftp://relay.example.com", true},
		{"https://", true},
		{"not-a-url", true},
		{"://nohost", true},
		// path/query/fragment must be rejected — publicSchemeHost
		// only honors scheme+host, so anything else is a silent
		// misconfiguration.
		{"https://relay.example.com/base", true},
		{"https://relay.example.com/base/", true},
		{"https://relay.example.com/?foo=bar", true},
		{"https://relay.example.com/#frag", true},
		// userinfo is silently dropped by publicSchemeHost, so reject.
		{"https://user@relay.example.com", true},
		{"https://user:pass@relay.example.com", true},
		// invalid ports — url.Parse rejects non-numeric ports, but
		// empty / out-of-range / zero numerics pass and would mint
		// rendezvous URLs that can never be dialed.
		{"https://relay.example.com:", true},
		{"https://relay.example.com:0", true},
		{"https://relay.example.com:99999", true},
	}
	for _, tc := range cases {
		err := validatePublicURL(tc.s)
		if (err != nil) != tc.wantErr {
			t.Errorf("validatePublicURL(%q) err=%v wantErr=%v", tc.s, err, tc.wantErr)
		}
	}
}

func TestRedactURL(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{
			"wss://r/foo?sb-hc-action=listen&sb-hc-token=AAA&extra=1",
			"wss://r/foo?sb-hc-action=listen&sb-hc-token=REDACTED&extra=1",
		},
		{
			"wss://r/foo?sb-hc-token=SECRET",
			"wss://r/foo?sb-hc-token=REDACTED",
		},
		{
			"wss://r/foo?other=1",
			"wss://r/foo?other=1",
		},
	}
	for _, tc := range cases {
		got := redactURL(tc.in)
		if got != tc.want {
			t.Errorf("redactURL(%q) = %q, want %q", tc.in, got, tc.want)
		}
		if strings.Contains(got, "SECRET") || strings.Contains(got, "AAA") {
			t.Errorf("redactURL(%q) leaked token: %q", tc.in, got)
		}
	}
}

func TestNewRendezvousID_UniqueAndLength(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 1000; i++ {
		id, err := newRendezvousID()
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if len(id) != 32 {
			t.Fatalf("len(id)=%d, want 32 (16 bytes hex)", len(id))
		}
		if seen[id] {
			t.Fatalf("duplicate ID after %d iterations: %s", i, id)
		}
		seen[id] = true
	}
}

func TestHub_RoundRobin(t *testing.T) {
	h := newHub()
	a := &controlSession{}
	b := &controlSession{}
	c := &controlSession{}
	h.addControl("e", a)
	h.addControl("e", b)
	h.addControl("e", c)

	// Successive snapshots should rotate the starting cursor by 1.
	first := h.snapshotControls("e")
	second := h.snapshotControls("e")
	third := h.snapshotControls("e")

	if len(first) != 3 || len(second) != 3 || len(third) != 3 {
		t.Fatalf("snapshot lengths: %d %d %d", len(first), len(second), len(third))
	}
	if first[0] == second[0] && second[0] == third[0] {
		t.Errorf("round-robin not rotating: starts %v %v %v", first[0], second[0], third[0])
	}
	// Every snapshot must contain all three sessions exactly once.
	for _, snap := range [][]*controlSession{first, second, third} {
		set := map[*controlSession]bool{}
		for _, s := range snap {
			set[s] = true
		}
		if len(set) != 3 {
			t.Errorf("snapshot has duplicates: %v", snap)
		}
	}
}

func TestHub_RemoveControl(t *testing.T) {
	h := newHub()
	a := &controlSession{}
	b := &controlSession{}
	h.addControl("e", a)
	h.addControl("e", b)
	if !h.hasControls("e") {
		t.Fatalf("hasControls=false, want true")
	}
	h.removeControl("e", a)
	snap := h.snapshotControls("e")
	if len(snap) != 1 || snap[0] != b {
		t.Errorf("snapshot after remove=%v, want [b]", snap)
	}
	h.removeControl("e", b)
	if h.hasControls("e") {
		t.Errorf("hasControls=true after removing all")
	}
}

func TestHub_TakePending_OnlyOnce(t *testing.T) {
	h := newHub()
	p := &pendingRendezvous{ready: make(chan struct{}), bridgeDone: make(chan struct{})}
	h.addPending("e", "id1", p)

	if got := h.takePending("e", "id1"); got != p {
		t.Errorf("first take=%v, want %v", got, p)
	}
	if got := h.takePending("e", "id1"); got != nil {
		t.Errorf("second take=%v, want nil", got)
	}
}

func TestHub_ConcurrentAccess(t *testing.T) {
	// Smoke-test for races; run with -race.
	h := newHub()
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c := &controlSession{}
			h.addControl("e", c)
			_ = h.hasControls("e")
			_ = h.snapshotControls("e")
			h.removeControl("e", c)
		}()
	}
	wg.Wait()
}

func TestServer_PublicURL_DerivedFromRequest(t *testing.T) {
	s, err := NewServer(Config{})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	r := httptest.NewRequest("GET", "http://relay.example.com/$hc/foo?sb-hc-action=connect", nil)
	url, err := s.rendezvousURL(r, "foo", "abc123")
	if err != nil {
		t.Fatalf("rendezvousURL: %v", err)
	}
	want := "ws://relay.example.com/$hc/foo?sb-hc-action=accept&id=abc123"
	if url != want {
		t.Errorf("rendezvousURL = %q, want %q", url, want)
	}
}

func TestServer_PublicURL_Configured(t *testing.T) {
	s, err := NewServer(Config{PublicURL: "https://relay.example.com:8443"})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	r := httptest.NewRequest("GET", "http://internal-host/$hc/foo?sb-hc-action=connect", nil)
	url, err := s.rendezvousURL(r, "foo", "abc123")
	if err != nil {
		t.Fatalf("rendezvousURL: %v", err)
	}
	want := "wss://relay.example.com:8443/$hc/foo?sb-hc-action=accept&id=abc123"
	if url != want {
		t.Errorf("rendezvousURL = %q, want %q", url, want)
	}
}

func TestNewServer_BadPublicURL(t *testing.T) {
	_, err := NewServer(Config{PublicURL: "ftp://nope"})
	if err == nil {
		t.Fatalf("NewServer with ftp scheme: want error, got nil")
	}
}

func TestSelfSignedTLS(t *testing.T) {
	opts, err := SelfSignedTLS()
	if err != nil {
		t.Fatalf("SelfSignedTLS: %v", err)
	}
	defer opts.Cleanup() //nolint:errcheck
	if opts.CertFile == "" || opts.KeyFile == "" {
		t.Fatalf("empty paths: cert=%q key=%q", opts.CertFile, opts.KeyFile)
	}
	if opts.Config == nil || len(opts.Config.Certificates) == 0 {
		t.Fatalf("no cert in TLS config")
	}
	if !strings.Contains(opts.Fingerprint, ":") {
		t.Errorf("fingerprint not formatted with colons: %q", opts.Fingerprint)
	}
}

func TestPendingRendezvous_ClaimAndAbort(t *testing.T) {
	t.Run("claim wins, abort no-op", func(t *testing.T) {
		p := &pendingRendezvous{ready: make(chan struct{})}
		if !p.claim(nil) { // listenerWS=nil is fine for this state test
			t.Fatalf("first claim returned false")
		}
		if p.claim(nil) {
			t.Errorf("second claim returned true; want false (once enforced)")
		}
		select {
		case <-p.ready:
		default:
			t.Errorf("ready not closed after claim")
		}
		p.abort() // must be a no-op; the once already fired
	})

	t.Run("abort wins, claim fails", func(t *testing.T) {
		p := &pendingRendezvous{ready: make(chan struct{})}
		p.abort()
		if p.claim(nil) {
			t.Errorf("claim after abort returned true; want false")
		}
		if p.listenerWS != nil {
			t.Errorf("listenerWS=%v after abort, want nil", p.listenerWS)
		}
		select {
		case <-p.ready:
		default:
			t.Errorf("ready not closed after abort")
		}
	})

	t.Run("concurrent claim/abort: exactly one wins", func(t *testing.T) {
		// Run many iterations to catch races under -race.
		for i := 0; i < 200; i++ {
			p := &pendingRendezvous{ready: make(chan struct{})}
			var wg sync.WaitGroup
			var claimOK, abortRan int32
			wg.Add(2)
			go func() {
				defer wg.Done()
				if p.claim(nil) {
					claimOK = 1
				}
			}()
			go func() {
				defer wg.Done()
				p.abort()
				abortRan = 1
			}()
			wg.Wait()
			if claimOK == 1 && p.listenerWS != nil {
				t.Errorf("iter %d: claim won but listenerWS != nil", i)
			}
			if abortRan == 0 {
				t.Errorf("iter %d: abort goroutine did not run", i)
			}
			select {
			case <-p.ready:
			default:
				t.Errorf("iter %d: ready not closed", i)
			}
		}
	})
}

func TestRendezvousURL_NoDoubleEscape(t *testing.T) {
	// Regression: rendezvousURL must not %-escape the entity again;
	// the request URL already carries it in encoded form. Double-escape
	// turns "with/slash" → "with%2Fslash" → "with%252Fslash".
	s, err := NewServer(Config{})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	r := httptest.NewRequest("GET", "http://relay.example.com/$hc/with%2Fslash?sb-hc-action=connect", nil)
	got, err := s.rendezvousURL(r, "with/slash", "abc123")
	if err != nil {
		t.Fatalf("rendezvousURL: %v", err)
	}
	if strings.Contains(got, "%25") {
		t.Errorf("rendezvousURL = %q: contains %%25 (double-escape)", got)
	}
	if !strings.Contains(got, "with%2Fslash") {
		t.Errorf("rendezvousURL = %q: expected entity in %%2F form", got)
	}
}

func TestHub_TryReserveCap(t *testing.T) {
	h := newHub()
	// max=2: first two succeed, third fails until a release.
	if !h.tryReserve("e", 2) {
		t.Fatalf("first reserve failed")
	}
	if !h.tryReserve("e", 2) {
		t.Fatalf("second reserve failed")
	}
	if h.tryReserve("e", 2) {
		t.Fatalf("third reserve should have failed (cap=2)")
	}
	h.release("e")
	if !h.tryReserve("e", 2) {
		t.Fatalf("reserve after release failed")
	}
	// max=0 means unlimited.
	for i := 0; i < 50; i++ {
		if !h.tryReserve("u", 0) {
			t.Fatalf("unlimited reserve %d failed", i)
		}
	}
}

func TestHub_TryReserve_PerEntity(t *testing.T) {
	// Caps are per-entity: reserving e1 to its cap must not block e2.
	h := newHub()
	if !h.tryReserve("e1", 1) {
		t.Fatal("e1 first reserve failed")
	}
	if h.tryReserve("e1", 1) {
		t.Fatal("e1 over cap should fail")
	}
	if !h.tryReserve("e2", 1) {
		t.Fatal("e2 reserve should succeed even when e1 is at cap")
	}
}

func TestServe_RejectsEmptyTLSCertFile(t *testing.T) {
	s, err := NewServer(Config{})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	cases := []*TLSOptions{
		{CertFile: "", KeyFile: "k"},
		{CertFile: "c", KeyFile: ""},
		{},
	}
	for i, opts := range cases {
		err := s.Serve(context.Background(), nil, opts)
		if err == nil {
			t.Errorf("case %d: Serve with empty TLS paths returned nil error", i)
			continue
		}
		if !strings.Contains(err.Error(), "CertFile") && !strings.Contains(err.Error(), "KeyFile") {
			t.Errorf("case %d: error %q does not mention CertFile/KeyFile", i, err)
		}
	}
}

func TestHandler_EntityWithSlash(t *testing.T) {
	// Encoded slashes in the entity path must survive routing and reach
	// parseEntity intact. A `with%2Fslash` entity should produce a 400
	// "unknown sb-hc-action" (because no action was sent), proving the
	// path was parsed without 404'ing on routing.
	s, err := NewServer(Config{})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	// Use Path with the encoded form so it round-trips through URL.EscapedPath.
	u, err := url.Parse(srv.URL + "/$hc/with%2Fslash")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	resp, err := srv.Client().Get(u.String())
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close() //nolint:errcheck
	// We expect 400 ("unknown sb-hc-action") proving the path parsed
	// without 400'ing on "bad path".
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status=%d, want 400 (with body confirming action parse)", resp.StatusCode)
	}
}
