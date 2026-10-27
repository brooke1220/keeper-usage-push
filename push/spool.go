package push

import (
	"errors"
	"fmt"
	"os"
)

// secureSpoolFiles runs before SQLite opens the database. Unix sidecars derive
// permissions from the database; Windows sidecars inherit the directory DACL.
// Crash leftovers may already exist and must be secured on both platforms.
func secureSpoolFiles(path string) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return fmt.Errorf("prepare usage spool database: %w", err)
	}
	if err := errors.Join(secureSpoolPath(path, 0600), f.Close()); err != nil {
		return fmt.Errorf("secure usage spool database: %w", err)
	}
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		if err := secureSpoolPath(path+suffix, 0600); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("secure usage spool %s file: %w", suffix, err)
		}
	}
	return nil
}
