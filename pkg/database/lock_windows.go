//go:build windows

package database

import (
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/windows"
)

func AcquireInstanceLock(lockPath string) (func() error, error) {
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o755); err != nil {
		return nil, fmt.Errorf("create lock file parent dir: %w", err)
	}

	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open lock file: %w", err)
	}

	ol := &windows.Overlapped{}
	if err := windows.LockFileEx(windows.Handle(f.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY,
		0, 1, 0, ol); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("another yolorouter instance appears to be running (lock held on %s)", lockPath)
	}

	unlock := func() error {
		defer func() { _ = f.Close() }()
		return windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, ol)
	}
	return unlock, nil
}

// lockFileExclusiveNonBlocking takes an exclusive lock on the first byte of f
// without blocking. LOCKFILE_FAIL_IMMEDIATELY mirrors flock's LOCK_NB so a
// second instance fails fast instead of waiting.
func lockFileExclusiveNonBlocking(f *os.File) error {
	h := windows.Handle(f.Fd())
	return windows.LockFileEx(
		h,
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY,
		0, 1, 0,
		new(windows.Overlapped),
	)
}

func unlockFile(f *os.File) error {
	h := windows.Handle(f.Fd())
	return windows.UnlockFileEx(h, 0, 1, 0, new(windows.Overlapped))
}
