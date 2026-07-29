//go:build !windows

package database

import (
	"fmt"
	"os"
	"path/filepath"
)

// AcquireInstanceLock acquires an exclusive, advisory file lock at lockPath.
// serve holds this lock for its entire lifetime; db:reset must acquire it
// exclusively before performing any destructive operation, and fails fast
// with a clear error if another instance is already running. The actual
// lock/unlock primitive is platform-specific (flock on unix, LockFileEx on
// windows) and lives in lock_unix.go / lock_windows.go.
func AcquireInstanceLock(lockPath string) (func() error, error) {
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o755); err != nil {
		return nil, fmt.Errorf("create lock file parent dir: %w", err)
	}

	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open lock file: %w", err)
	}

	if err := lockFileExclusiveNonBlocking(f); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("another yolorouter instance appears to be running (lock held on %s)", lockPath)
	}

	unlock := func() error {
		defer func() { _ = f.Close() }()
		return unlockFile(f)
	}
	return unlock, nil
}
