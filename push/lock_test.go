//go:build linux || darwin || freebsd || windows

package push

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestSpoolLockAcrossProcesses(t *testing.T) {
	const pathEnv = "KEEPER_PUSH_TEST_LOCK_PATH"
	const stateEnv = "KEEPER_PUSH_TEST_LOCK_STATE"
	if path := os.Getenv(pathEnv); path != "" {
		f, err := lockSpool(path)
		if os.Getenv(stateEnv) == "locked" {
			if !errors.Is(err, ErrSpoolLocked) {
				t.Fatalf("second process acquired a held lock: %v", err)
			}
		} else if err != nil {
			t.Fatal(err)
		}
		// Intentionally leave the handle open, as after an abrupt process exit.
		_ = f
		os.Exit(0)
	}
	path := filepath.Join(t.TempDir(), "writer.lock")
	f, err := lockSpool(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	checkChild := func(state string) {
		t.Helper()
		child := exec.Command(os.Args[0], "-test.run=^TestSpoolLockAcrossProcesses$")
		child.Env = append(os.Environ(), pathEnv+"="+path, stateEnv+"="+state)
		if output, err := child.CombinedOutput(); err != nil {
			t.Fatalf("child lock check (%s): %v\n%s", state, err, output)
		}
	}
	checkChild("locked")
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	checkChild("available")
	reopened, err := lockSpool(path)
	if err != nil {
		t.Fatalf("exiting process did not release its lock: %v", err)
	}
	_ = reopened.Close()
}
