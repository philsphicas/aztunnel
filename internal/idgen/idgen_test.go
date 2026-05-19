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
