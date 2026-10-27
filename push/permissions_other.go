//go:build !windows

package push

import "os"

func secureSpoolPath(path string, mode os.FileMode) error {
	return os.Chmod(path, mode)
}
