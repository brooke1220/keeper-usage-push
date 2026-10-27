//go:build linux || darwin || freebsd

package push

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

func lockSpool(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, ErrSpoolLocked
		}
		return nil, fmt.Errorf("lock usage spool: %w", err)
	}
	return f, nil
}
