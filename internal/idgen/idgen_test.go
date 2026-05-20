package idgen

import (
	"testing"
)

// TestNewBridgeID_Format asserts NewBridgeID returns a stable-length
// base32 string drawn from the [A-Z2-7] charset. Log scrapers grep
// for this exact pattern, so the format is a public contract: any
// change here must come with an explicit version bump and downstream
// scraper updates.
func TestNewBridgeID_Format(t *testing.T) {
	const want = 16
	for i := 0; i < 32; i++ {
		id := NewBridgeID()
		if len(id) != want {
			t.Fatalf("NewBridgeID() len = %d, want %d (id=%q)", len(id), want, id)
		}
		for j, c := range id {
			ok := (c >= 'A' && c <= 'Z') || (c >= '2' && c <= '7')
			if !ok {
				t.Fatalf("NewBridgeID() id=%q: invalid char %q at %d", id, c, j)
			}
		}
	}
}

// TestNewBridgeID_NoCollisions defends the 80-bit entropy claim.
// 10_000 IDs gives a vanishingly small birthday-collision probability
// at 2^80; a collision here is a regression in either the RNG plumbing
// or the encoding (e.g. accidentally truncating).
func TestNewBridgeID_NoCollisions(t *testing.T) {
	const n = 10_000
	seen := make(map[string]struct{}, n)
	for i := 0; i < n; i++ {
		id := NewBridgeID()
		if _, dup := seen[id]; dup {
			t.Fatalf("collision after %d IDs: %q", i+1, id)
		}
		seen[id] = struct{}{}
	}
}

// TestNewBridgeID_NotEmpty is a defence against the zero-value
// regression — a caller that fails to call NewBridgeID and binds the
// zero value onto a logger would emit bridge_id="" which is exactly
// the mixed-version-compatibility signal P5's listener wiring uses.
// A non-empty contract here means "this function never produces that
// signal by accident".
func TestNewBridgeID_NotEmpty(t *testing.T) {
	if NewBridgeID() == "" {
		t.Fatalf("NewBridgeID() returned empty string")
	}
}

// TestNewControlSessionID_Format mirrors TestNewBridgeID_Format for
// the per-control-loop id. The format is a public contract because
// log scrapers grep for control_session_id=<value> across listener
// stderr; any drift here breaks operator queries.
func TestNewControlSessionID_Format(t *testing.T) {
	const want = 16
	for i := 0; i < 32; i++ {
		id := NewControlSessionID()
		if len(id) != want {
			t.Fatalf("NewControlSessionID() len = %d, want %d (id=%q)", len(id), want, id)
		}
		for j, c := range id {
			ok := (c >= 'A' && c <= 'Z') || (c >= '2' && c <= '7')
			if !ok {
				t.Fatalf("NewControlSessionID() id=%q: invalid char %q at %d", id, c, j)
			}
		}
	}
}

// TestNewControlSessionID_NoCollisions defends the 80-bit entropy
// claim for control-session ids. Each control-channel reconnect
// mints a fresh id, so collisions here would silently merge log
// streams that operators rely on being distinct.
func TestNewControlSessionID_NoCollisions(t *testing.T) {
	const n = 10_000
	seen := make(map[string]struct{}, n)
	for i := 0; i < n; i++ {
		id := NewControlSessionID()
		if _, dup := seen[id]; dup {
			t.Fatalf("collision after %d IDs: %q", i+1, id)
		}
		seen[id] = struct{}{}
	}
}

// TestControlSessionID_DistinctFromBridgeID asserts the two
// constructors mint independent values even though they share an
// internal helper. A regression that collapsed them onto one source
// (e.g. a memoised global) would defeat correlation by introducing
// matching ids across unrelated log scopes.
func TestControlSessionID_DistinctFromBridgeID(t *testing.T) {
	if NewBridgeID() == NewControlSessionID() {
		t.Fatalf("expected distinct ids from independent constructors")
	}
}
