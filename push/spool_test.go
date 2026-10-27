package push

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func assertPrivateSpool(t *testing.T, dir string) {
	t.Helper()
	for _, name := range []string{"", "outbox.db", "outbox.db-wal", "outbox.db-shm"} {
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm()&0077 != 0 {
			t.Errorf("%s is accessible to other users: mode %04o", name, info.Mode().Perm())
		}
	}
}

func TestSpoolFilesArePrivate(t *testing.T) {
	for _, preexisting := range []bool{false, true} {
		name := "new"
		if preexisting {
			name = "preexisting-public-directory"
		}
		t.Run(name, func(t *testing.T) {
			cfg := configFor(t, "https://keeper.invalid")
			cfg.SpoolDir = filepath.Join(cfg.SpoolDir, "spool")
			if preexisting {
				if err := os.Mkdir(cfg.SpoolDir, 0755); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(cfg.SpoolDir, 0755); err != nil {
					t.Fatal(err)
				}
			}
			q := openQueue(t, cfg)
			if err := q.Enqueue(record(t)); err != nil {
				t.Fatal(err)
			}
			assertPrivateSpool(t, cfg.SpoolDir)
		})
	}
}

func TestSpoolReopensLegacyFilesPrivately(t *testing.T) {
	const childSpoolEnv = "KEEPER_PUSH_TEST_CRASH_SPOOL"
	if dir := os.Getenv(childSpoolEnv); dir != "" {
		cfg := configFor(t, "https://keeper.invalid")
		cfg.SpoolDir = dir
		q, err := Open(cfg)
		if err != nil {
			t.Fatal(err)
		}
		if err := q.Enqueue(record(t)); err != nil {
			t.Fatal(err)
		}
		// Leave committed WAL pages on disk, as after a crash, without checkpointing on Close.
		os.Exit(0)
	}
	cfg := configFor(t, "https://keeper.invalid")
	child := exec.Command(os.Args[0], "-test.run=^TestSpoolReopensLegacyFilesPrivately$")
	child.Env = append(os.Environ(), childSpoolEnv+"="+cfg.SpoolDir)
	if output, err := child.CombinedOutput(); err != nil {
		t.Fatalf("create crash fixture: %v\n%s", err, output)
	}
	wal, err := os.ReadFile(filepath.Join(cfg.SpoolDir, "outbox.db-wal"))
	if err != nil || !bytes.Contains(wal, []byte("sk-full-private-key")) {
		t.Fatalf("fixture did not retain the sensitive record in WAL: %v", err)
	}
	// Reproduce permissions from an earlier version before reopening it with the fixed code.
	for _, name := range []string{"", "outbox.db", "outbox.db-wal", "outbox.db-shm"} {
		mode := os.FileMode(0644)
		if name == "" {
			mode = 0755
		}
		if err := os.Chmod(filepath.Join(cfg.SpoolDir, name), mode); err != nil {
			t.Fatal(err)
		}
	}
	q := openQueue(t, cfg)
	assertPrivateSpool(t, cfg.SpoolDir)
	if stats, err := q.Stats(); err != nil || stats.Pending != 1 {
		t.Fatalf("repairing permissions lost the committed record: %+v %v", stats, err)
	}
}
