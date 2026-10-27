package push

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/windows"
)

func lockSpool(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	// A byte-range lock also excludes other plugin images in the same process.
	// Windows releases it when the file handle closes, including after a crash.
	var overlapped windows.Overlapped
	if err := windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &overlapped); err != nil {
		_ = f.Close()
		if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
			return nil, ErrSpoolLocked
		}
		return nil, fmt.Errorf("lock usage spool: %w", err)
	}
	return f, nil
}
