package main

import (
	"archive/zip"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPackMatchesPluginStoreLayout(t *testing.T) {
	dir := t.TempDir()
	library := filepath.Join(dir, "keeper-usage-push.so")
	if err := os.WriteFile(library, []byte("elf"), 0o755); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "dist")
	for _, arch := range []string{"amd64", "arm64", "amd64"} {
		if _, err := pack(library, "v1.2.3", "linux", arch, out); err != nil {
			t.Fatal(err)
		}
	}
	reader, err := zip.OpenReader(filepath.Join(out, "keeper-usage-push_1.2.3_linux_amd64.zip"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reader.Close() }()
	if len(reader.File) != 1 || reader.File[0].Name != "keeper-usage-push.so" {
		t.Fatalf("archive must hold only the library at its root: %+v", reader.File)
	}
	sums, err := os.ReadFile(filepath.Join(out, "checksums.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if lines := strings.Split(strings.TrimSpace(string(sums)), "\n"); len(lines) != 2 {
		t.Fatalf("repacking must replace, not duplicate, checksum lines:\n%s", sums)
	}
}

func TestPackRejectsMismatchedInput(t *testing.T) {
	dir := t.TempDir()
	library := filepath.Join(dir, "keeper-usage-push.so")
	if err := os.WriteFile(library, []byte("elf"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ version, goos string }{{"latest", "linux"}, {"1.0.0", "darwin"}} {
		if _, err := pack(library, tc.version, tc.goos, "amd64", dir); err == nil {
			t.Fatalf("accepted version=%s goos=%s", tc.version, tc.goos)
		}
	}
}
