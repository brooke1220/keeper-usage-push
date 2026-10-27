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

func TestPackShipsSpoolToolSeparately(t *testing.T) {
	dir := t.TempDir()
	library := filepath.Join(dir, "keeper-usage-push.so")
	tool := filepath.Join(dir, "keeper-push-spool")
	for _, path := range []string{library, tool} {
		if err := os.WriteFile(path, []byte("elf"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	out := filepath.Join(dir, "dist")
	names, err := packAll(library, tool, "1.2.3", "linux", "amd64", out)
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 2 || names[0] != "keeper-usage-push_1.2.3_linux_amd64.zip" || names[1] != "keeper-push-spool_1.2.3_linux_amd64.zip" {
		t.Fatalf("unexpected archives: %v", names)
	}
	reader, err := zip.OpenReader(filepath.Join(out, names[1]))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reader.Close() }()
	if len(reader.File) != 1 || reader.File[0].Name != "keeper-push-spool" || reader.File[0].Mode().Perm()&0o111 == 0 {
		t.Fatalf("tool archive must hold only the executable at its root: %+v", reader.File)
	}
	sums, err := os.ReadFile(filepath.Join(out, "checksums.txt"))
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range names {
		if !strings.Contains(string(sums), "  "+name+"\n") {
			t.Fatalf("checksums.txt lacks %s:\n%s", name, sums)
		}
	}
	if _, err := packAll(library, library, "1.2.3", "linux", "amd64", out); err == nil {
		t.Fatal("accepted the plugin library as the spool tool")
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

func TestPackWindowsDLLAndSpoolEXE(t *testing.T) {
	dir := t.TempDir()
	library := filepath.Join(dir, "keeper-usage-push.dll")
	tool := filepath.Join(dir, "keeper-push-spool.exe")
	for _, path := range []string{library, tool} {
		if err := os.WriteFile(path, []byte("MZ"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	out := filepath.Join(dir, "dist")
	names, err := packAll(library, tool, "1.2.3", "windows", "amd64", out)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"keeper-usage-push_1.2.3_windows_amd64.zip": "keeper-usage-push.dll",
		"keeper-push-spool_1.2.3_windows_amd64.zip": "keeper-push-spool.exe",
	}
	if len(names) != len(want) {
		t.Fatalf("unexpected Windows assets: %v", names)
	}
	for _, name := range names {
		reader, err := zip.OpenReader(filepath.Join(out, name))
		if err != nil {
			t.Fatal(err)
		}
		if len(reader.File) != 1 || reader.File[0].Name != want[name] {
			t.Errorf("unexpected contents of %s: %+v", name, reader.File)
		}
		_ = reader.Close()
	}
	if _, err := packTool(strings.TrimSuffix(tool, ".exe"), "1.2.3", "windows", "amd64", out); err == nil {
		t.Fatal("Windows spool tool must retain its .exe extension")
	}
}
