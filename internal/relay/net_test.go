package relay

import (
	"context"
	"testing"
)

func TestConnSemaphore_Unlimited(t *testing.T) {
	sem := newConnSemaphore(0)
	// Should always succeed with no limit.
	for range 100 {
		if !sem.tryAcquire(context.Background()) {
			t.Fatal("unlimited semaphore should always acquire")
		}
	}
	// Release should not panic on nil channel.
	sem.release()
}

func TestConnSemaphore_Limited(t *testing.T) {
	sem := newConnSemaphore(2)
	ctx := context.Background()

	if !sem.tryAcquire(ctx) {
		t.Fatal("first acquire should succeed")
	}
	if !sem.tryAcquire(ctx) {
		t.Fatal("second acquire should succeed")
	}
	// Third should fail (non-blocking).
	if sem.tryAcquire(ctx) {
		t.Fatal("third acquire should fail at capacity")
	}
	// Release one and retry.
	sem.release()
	if !sem.tryAcquire(ctx) {
		t.Fatal("acquire after release should succeed")
	}
}

func TestConnSemaphore_ContextCancel(t *testing.T) {
	sem := newConnSemaphore(1)
	ctx := context.Background()

	sem.tryAcquire(ctx) // fill it

	cancelCtx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled
	if sem.tryAcquire(cancelCtx) {
		t.Fatal("acquire with cancelled context should fail")
	}
}
