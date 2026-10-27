package push

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// DefaultSpoolDir sits inside CPA's default plugins directory, which the official
// Docker Compose file already mounts as a persistent volume.
const DefaultSpoolDir = "plugins/keeper-usage-push-spool"

// Config needs only endpoint and token. Keeper creates the token in its settings page and
// derives the CPA source from it, so pending records keep their identity across rotation.
type Config struct {
	Endpoint          string        `yaml:"endpoint"`
	Token             string        `yaml:"token"`
	TokenFile         string        `yaml:"token_file"`
	SpoolDir          string        `yaml:"spool_dir"`
	MaxSpoolBytes     int64         `yaml:"max_spool_bytes"`
	FlushInterval     time.Duration `yaml:"flush_interval"`
	RequestTimeout    time.Duration `yaml:"request_timeout"`
	AllowHTTPLoopback bool          `yaml:"allow_http_loopback"`
}

// identifier matches Keeper's error codes in rejection bodies.
var identifier = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,127}$`)

// Unconfigured reports a fresh install whose endpoint and token are not filled in yet. Such a
// plugin still registers so CPA's plugin page can show its settings form; any other partial
// configuration is a validation error.
func (c Config) Unconfigured() bool {
	return strings.TrimSpace(c.Endpoint) == "" && strings.TrimSpace(c.Token) == "" && strings.TrimSpace(c.TokenFile) == ""
}

func (c Config) normalized() (Config, error) {
	u, err := url.Parse(c.Endpoint)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return c, fmt.Errorf("endpoint must be an HTTPS URL without credentials, query or fragment")
	}
	loopback := net.ParseIP(u.Hostname())
	if u.Scheme != "https" && !(c.AllowHTTPLoopback && u.Scheme == "http" && loopback != nil && loopback.IsLoopback()) {
		return c, fmt.Errorf("endpoint must use HTTPS (explicit HTTP loopback is for local testing only)")
	}
	if c.TokenFile != "" {
		if c.Token != "" {
			return c, fmt.Errorf("configure token or token_file, not both")
		}
		raw, errRead := os.ReadFile(c.TokenFile)
		if errRead != nil {
			return c, fmt.Errorf("read token file: %w", errRead)
		}
		c.Token = strings.TrimSpace(string(raw))
		// Accepted settings are a self-contained snapshot; rollback must not re-read a secret.
		c.TokenFile = ""
	}
	if len(c.Token) < 32 || strings.ContainsAny(c.Token, " \t\r\n") {
		return c, fmt.Errorf("push token must contain at least 32 characters without whitespace")
	}
	if strings.TrimSpace(c.SpoolDir) == "" {
		dir, errDefault := defaultSpoolDir()
		if errDefault != nil {
			return c, errDefault
		}
		c.SpoolDir = dir
	}
	c.SpoolDir, err = filepath.Abs(c.SpoolDir)
	if err != nil {
		return c, fmt.Errorf("resolve spool_dir: %w", err)
	}
	if c.MaxSpoolBytes == 0 {
		c.MaxSpoolBytes = 1 << 30
	}
	if c.MaxSpoolBytes < 1 {
		return c, fmt.Errorf("max_spool_bytes must be positive")
	}
	if c.FlushInterval == 0 {
		c.FlushInterval = 5 * time.Second
	}
	if c.FlushInterval < 0 {
		return c, fmt.Errorf("flush_interval must be positive")
	}
	if c.RequestTimeout == 0 {
		c.RequestTimeout = 15 * time.Second
	}
	if c.RequestTimeout < 0 {
		return c, fmt.Errorf("request_timeout must be positive")
	}
	return c, nil
}

// ResolveConfig validates settings, reads token_file once and resolves defaults and paths.
// Its result can be reopened without consulting the token file again. Keep it in memory only.
func ResolveConfig(c Config) (Config, error) { return c.normalized() }

// ValidateConfig checks configuration without replacing a live queue.
func ValidateConfig(c Config) error { _, err := ResolveConfig(c); return err }

// defaultSpoolDir resolves DefaultSpoolDir against CPA's working directory. The host does
// not tell plugins where its plugins directory is, so the default only applies when that
// directory exists; it never falls back to a temporary location that loses pending records.
func defaultSpoolDir() (string, error) {
	pluginsDir := filepath.Dir(DefaultSpoolDir)
	info, err := os.Stat(pluginsDir)
	if err != nil || !info.IsDir() {
		return "", fmt.Errorf("spool_dir is not set and %s was not found in the CPA working directory; set spool_dir to a persistent directory", pluginsDir)
	}
	dir, err := filepath.Abs(DefaultSpoolDir)
	if err != nil {
		return "", fmt.Errorf("resolve default spool_dir: %w", err)
	}
	return dir, nil
}
