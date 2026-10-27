package push

import (
	"errors"
	"fmt"
	"os"
)

// secureSpoolFiles runs before SQLite opens the database. SQLite derives the permissions
// of newly created WAL/SHM files from the database, and crash leftovers may already exist.
func secureSpoolFiles(path string) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return fmt.Errorf("prepare usage spool database: %w", err)
	}
	if err := errors.Join(f.Chmod(0600), f.Close()); err != nil {
		return fmt.Errorf("secure usage spool database: %w", err)
	}
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		if err := os.Chmod(path+suffix, 0600); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("secure usage spool %s file: %w", suffix, err)
		}
	}
	return nil
}
