//go:build !windows

package push

import (
	"os"
	"testing"
)

func assertPrivateSpoolPath(t *testing.T, path string) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0077 != 0 {
		t.Errorf("%s is accessible to other users: mode %04o", path, info.Mode().Perm())
	}
}

func makeSpoolPathPublic(t *testing.T, path string, mode os.FileMode) {
	t.Helper()
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}
