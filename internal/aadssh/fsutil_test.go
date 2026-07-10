package aadssh

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestAcquireFileLockSerializesAndReleases(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.lock")
	release, err := acquireFileLock(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}

	waitCtx, cancel := context.WithTimeout(t.Context(), 250*time.Millisecond)
	defer cancel()
	if _, err := acquireFileLock(waitCtx, path); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("second lock error = %v, want context deadline exceeded", err)
	}

	release()
	releaseAgain, err := acquireFileLock(t.Context(), path)
	if err != nil {
		t.Fatalf("acquire after release: %v", err)
	}
	releaseAgain()
}
