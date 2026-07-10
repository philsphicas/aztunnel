package aadssh

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/gofrs/flock"
)

const lockRetryDelay = 100 * time.Millisecond

// writeFileAtomic writes data to path by first writing a sibling temp file and
// then replacing the destination, so readers do not observe an in-place partial
// write.
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	// Best-effort cleanup if we bail before the rename; a no-op once renamed.
	defer func() { _ = os.Remove(tmpName) }()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, perm); err != nil {
		return err
	}
	return replaceFile(tmpName, path)
}

// acquireDirLock takes an advisory inter-process lock for dir. The OS releases
// the lock when its process exits, so a slow interactive sign-in cannot be
// mistaken for a stale lock and a crashed process cannot strand one.
func acquireDirLock(ctx context.Context, dir string) (func(), error) {
	return acquireFileLock(ctx, filepath.Join(dir, ".lock"))
}

func acquireFileLock(ctx context.Context, path string) (func(), error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}

	l := flock.New(path, flock.SetPermissions(0o600))
	locked, err := l.TryLockContext(ctx, lockRetryDelay)
	if err != nil {
		return nil, err
	}
	if !locked {
		return nil, fmt.Errorf("lock %s was not acquired", path)
	}
	return func() { _ = l.Unlock() }, nil
}
