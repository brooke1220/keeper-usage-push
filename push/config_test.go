package push

import (
	"strings"
	"testing"
)

func TestEndpointAcceptsHTTPAndHTTPSWithoutExtraSettings(t *testing.T) {
	for _, endpoint := range []string{
		"http://8.217.203.4:8080/api/v1/ingest/usage",
		"http://10.0.0.2:8080/api/v1/ingest/usage",
		"http://keeper.example.com/api/v1/ingest/usage",
		"http://127.0.0.1:8080/api/v1/ingest/usage",
		"http://[::1]:8080/api/v1/ingest/usage",
		"https://keeper.example.com/api/v1/ingest/usage",
	} {
		t.Run(endpoint, func(t *testing.T) {
			cfg := Config{Endpoint: endpoint, Token: strings.Repeat("t", 32), SpoolDir: t.TempDir()}
			resolved, err := ResolveConfig(cfg)
			if err != nil {
				t.Fatalf("valid endpoint was rejected: %v", err)
			}
			if resolved.Endpoint != endpoint {
				t.Fatalf("endpoint changed to %q, want %q", resolved.Endpoint, endpoint)
			}
		})
	}
}

func TestEndpointRejectsUnsupportedSchemesAndURLSecrets(t *testing.T) {
	for _, endpoint := range []string{
		"ftp://keeper.example.com/api/v1/ingest/usage",
		"ws://keeper.example.com/api/v1/ingest/usage",
		"//keeper.example.com/api/v1/ingest/usage",
		"http:///api/v1/ingest/usage",
		"http://keeper.example.com:bad/api/v1/ingest/usage",
		"http://user:password@keeper.example.com/api/v1/ingest/usage",
		"https://user:password@keeper.example.com/api/v1/ingest/usage",
		"http://keeper.example.com/api/v1/ingest/usage?token=secret",
		"https://keeper.example.com/api/v1/ingest/usage#fragment",
	} {
		t.Run(endpoint, func(t *testing.T) {
			cfg := Config{Endpoint: endpoint, Token: strings.Repeat("t", 32), SpoolDir: t.TempDir()}
			if err := ValidateConfig(cfg); err == nil {
				t.Fatal("invalid endpoint was accepted")
			}
		})
	}
}
