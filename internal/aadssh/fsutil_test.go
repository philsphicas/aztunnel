package aadssh

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestWriteFileAtomicCreatesAndReplaces(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state")
	for _, want := range []string{"first", "second"} {
		if err := writeFileAtomic(path, []byte(want), 0o600); err != nil {
			t.Fatalf("writeFileAtomic(%q): %v", want, err)
		}
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read state: %v", err)
		}
		if string(got) != want {
			t.Fatalf("state = %q, want %q", got, want)
		}
	}
}

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
