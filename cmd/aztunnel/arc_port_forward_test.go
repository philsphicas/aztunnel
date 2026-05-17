package main

import (
	"sync"
	"sync/atomic"
	"testing"
)

// TestConsumeExplainOnFirstDial guards the UX guarantee that the
// first-connection explanatory logging is consumed exactly once even
// when many inbound TCP connections arrive concurrently against
// `arc port-forward`. A regression here (e.g. swapping CompareAndSwap
// for a non-atomic load/store) would silently re-emit the setup INFO
// for every inbound connection.
func TestConsumeExplainOnFirstDial(t *testing.T) {
	t.Run("unset flag never returns true", func(t *testing.T) {
		var flag atomic.Bool
		if consumeExplainOnFirstDial(&flag) {
			t.Fatal("expected false when flag was never set")
		}
		if consumeExplainOnFirstDial(&flag) {
			t.Fatal("expected false on repeated call when flag was never set")
		}
	})

	t.Run("sequential calls return true once, then false", func(t *testing.T) {
		var flag atomic.Bool
		flag.Store(true)
		if !consumeExplainOnFirstDial(&flag) {
			t.Fatal("first call should return true")
		}
		if consumeExplainOnFirstDial(&flag) {
			t.Fatal("subsequent call should return false")
		}
		if consumeExplainOnFirstDial(&flag) {
			t.Fatal("further calls should keep returning false")
		}
	})

	t.Run("concurrent callers see exactly one true", func(t *testing.T) {
		const callers = 256
		var flag atomic.Bool
		flag.Store(true)

		var trues atomic.Int32
		var wg sync.WaitGroup
		start := make(chan struct{})
		wg.Add(callers)
		for range callers {
			go func() {
				defer wg.Done()
				<-start
				if consumeExplainOnFirstDial(&flag) {
					trues.Add(1)
				}
			}()
		}
		close(start)
		wg.Wait()

		if got := trues.Load(); got != 1 {
			t.Errorf("expected exactly one caller to observe true, got %d", got)
		}
		if consumeExplainOnFirstDial(&flag) {
			t.Error("flag should be cleared after the consuming call")
		}
	})
}
