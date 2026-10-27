// Command packager builds a CPA plugin-store release archive: a zip holding the shared library
// at its root, named <id>_<version>_<goos>_<goarch>.zip, plus a checksums.txt line. With -tool it
// also packages the offline spool operator as a separate archive; the store skips that asset.
package main

import (
	"archive/zip"
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

const pluginID = "keeper-usage-push"

// toolName is the offline spool operator built from ./cmd/spool.
const toolName = "keeper-push-spool"

// versionPattern matches CPA's plugin version rule: no leading "v".
var versionPattern = regexp.MustCompile(`^[0-9][0-9A-Za-z.+-]*$`)

func main() {
	library := flag.String("lib", "", "built shared library (.so, .dylib or .dll)")
	version := flag.String("version", "", "release version without a leading v, e.g. 1.2.3")
	goos := flag.String("goos", "", "target GOOS")
	goarch := flag.String("goarch", "", "target GOARCH")
	outDir := flag.String("out", "dist", "output directory")
	tool := flag.String("tool", "", "optional keeper-push-spool executable to package for the same target")
	flag.Parse()
	names, err := packAll(*library, *tool, *version, *goos, *goarch, *outDir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "packager:", err)
		os.Exit(1)
	}
	for _, name := range names {
		fmt.Println(name)
	}
}

func packAll(library, tool, version, goos, goarch, outDir string) ([]string, error) {
	name, err := pack(library, version, goos, goarch, outDir)
	if err != nil || tool == "" {
		return []string{name}, err
	}
	toolArchive, err := packTool(tool, version, goos, goarch, outDir)
	return []string{name, toolArchive}, err
}

// packTool writes <tool>_<version>_<goos>_<goarch>.zip holding the executable at its root.
// The name does not match the plugin's, so the store never selects it as the plugin archive.
func packTool(tool, version, goos, goarch, outDir string) (string, error) {
	version, err := releaseVersion(version)
	if err != nil {
		return "", err
	}
	if goos == "" || goarch == "" {
		return "", fmt.Errorf("goos and goarch are required")
	}
	entryName := toolName
	if goos == "windows" {
		entryName += ".exe"
	}
	if filepath.Base(tool) != entryName {
		return "", fmt.Errorf("tool must be the %s executable, got %s", entryName, tool)
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return "", err
	}
	name := fmt.Sprintf("%s_%s_%s_%s.zip", toolName, version, goos, goarch)
	archive := filepath.Join(outDir, name)
	if err := writeZip(archive, tool, entryName); err != nil {
		return "", err
	}
	sum, err := fileSHA256(archive)
	if err != nil {
		return "", err
	}
	return name, appendChecksum(filepath.Join(outDir, "checksums.txt"), sum, name)
}

func releaseVersion(version string) (string, error) {
	version = strings.TrimPrefix(strings.TrimSpace(version), "v")
	if !versionPattern.MatchString(version) {
		return "", fmt.Errorf("invalid version %q", version)
	}
	return version, nil
}

// pack writes the archive and appends its checksum, replacing a stale line for the same archive.
func pack(library, version, goos, goarch, outDir string) (string, error) {
	version, err := releaseVersion(version)
	if err != nil {
		return "", err
	}
	if goos == "" || goarch == "" {
		return "", fmt.Errorf("goos and goarch are required")
	}
	extension := map[string]string{"darwin": ".dylib", "windows": ".dll"}[goos]
	if extension == "" {
		extension = ".so"
	}
	if filepath.Ext(library) != extension {
		return "", fmt.Errorf("%s needs a %s library, got %s", goos, extension, library)
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return "", err
	}
	name := fmt.Sprintf("%s_%s_%s_%s.zip", pluginID, version, goos, goarch)
	archive := filepath.Join(outDir, name)
	if err := writeZip(archive, library, pluginID+extension); err != nil {
		return "", err
	}
	sum, err := fileSHA256(archive)
	if err != nil {
		return "", err
	}
	return name, appendChecksum(filepath.Join(outDir, "checksums.txt"), sum, name)
}

func writeZip(archive, library, entryName string) error {
	src, err := os.Open(library)
	if err != nil {
		return err
	}
	defer func() { _ = src.Close() }()
	info, err := src.Stat()
	if err != nil {
		return err
	}
	out, err := os.Create(archive)
	if err != nil {
		return err
	}
	w := zip.NewWriter(out)
	header, err := zip.FileInfoHeader(info)
	if err != nil {
		_ = out.Close()
		return err
	}
	header.Name, header.Method = entryName, zip.Deflate
	header.SetMode(0o755)
	entry, err := w.CreateHeader(header)
	if err == nil {
		_, err = io.Copy(entry, src)
	}
	if errClose := w.Close(); err == nil {
		err = errClose
	}
	if errClose := out.Close(); err == nil {
		err = errClose
	}
	return err
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func appendChecksum(path, sum, name string) error {
	raw, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	var lines []string
	for _, line := range strings.Split(string(raw), "\n") {
		if fields := strings.Fields(line); len(fields) == 2 && fields[1] != name {
			lines = append(lines, line)
		}
	}
	lines = append(lines, sum+"  "+name)
	return os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644)
}
