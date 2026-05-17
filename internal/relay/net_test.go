package relay

import (
	"testing"
)

func TestConnSemaphore_Unlimited(t *testing.T) {
	sem := newConnSemaphore(0)
	// Should always succeed with no limit.
	for range 100 {
		if !sem.tryAcquire() {
			t.Fatal("unlimited semaphore should always acquire")
		}
	}
	// Release should not panic on nil channel.
	sem.release()
}

func TestConnSemaphore_Limited(t *testing.T) {
	sem := newConnSemaphore(2)

	if !sem.tryAcquire() {
		t.Fatal("first acquire should succeed")
	}
	if !sem.tryAcquire() {
		t.Fatal("second acquire should succeed")
	}
	// Third should fail (non-blocking).
	if sem.tryAcquire() {
		t.Fatal("third acquire should fail at capacity")
	}
	// Release one and retry.
	sem.release()
	if !sem.tryAcquire() {
		t.Fatal("acquire after release should succeed")
	}
}
