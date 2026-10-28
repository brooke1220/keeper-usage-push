package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/brooke1220/keeper-usage-push/push"
)

func TestIdentityMetadataUsesDisplayFieldsAndCachesLookups(t *testing.T) {
	now := time.Unix(100, 0)
	calls := 0
	// A runtime-only channel, e.g. an AI Studio websocket: CPA labels it with the channel ID.
	cache := identityCache{now: func() time.Time { return now }, lookup: func(index string) (hostIdentity, bool) {
		calls++
		return hostIdentity{AuthIndex: index, Label: "Channel A", Name: "channel-a", Email: "Channel A", Provider: "aistudio"}, true
	}}
	r := push.UsageRecord{AuthIndex: "same-index", AuthType: "oauth", Source: "Channel A"}
	for range 2 {
		got := cache.metadata(r)
		if got == nil || got.DisplayName != "Channel A" || got.ProviderType != "aistudio" {
			t.Fatalf("metadata = %+v", got)
		}
	}
	if calls != 1 {
		t.Fatalf("queries = %d", calls)
	}
	now = now.Add(6 * time.Minute)
	cache.metadata(r)
	if calls != 2 {
		t.Fatalf("expired cache queries = %d", calls)
	}
	cache.reset()
	cache.metadata(r)
	if calls != 3 {
		t.Fatalf("reset cache queries = %d", calls)
	}
}

// The cases mirror what CPA's synthesizers store: a file credential's label is its email, or
// its provider type when it has none.
func TestIdentityMetadataFallbacksAndLookupFailures(t *testing.T) {
	for _, tc := range []struct{ name, authType, label, email, file, want string }{
		{"oauth-email-label", "oauth", "user@example.com", "user@example.com", "codex-user@example.com.json", "user@example.com"},
		{"oauth-provider-label", "oauth", "codex", "", "codex-a.json", "codex-a.json"},
		{"oauth-provider-label-case", "OAuth ", "Codex", "", "codex-b.json", "codex-b.json"},
		{"oauth-email", "oauth", "", "user@example.com", "auth.json", "user@example.com"},
		{"unknown-kind-skips-email", "", "", "irrelevant@example.com", "credential-id", "credential-id"},
		{"secret-label", "", "key: sk-upstream-secret", "", "credential-id", "credential-id"},
		{"apikey-secret-label", "apikey", "key: sk-upstream-secret", "", "credential-id", "credential-id"},
		{"apikey-secret-name", "apikey", "", "", "sk-upstream-secret.json", ""},
		{"apikey-skips-email", "apikey", "", "irrelevant@example.com", "credential-id", "credential-id"},
		{"invalid-label", "oauth", "bad\nlabel", "user@example.com", "auth.json", "user@example.com"},
		{"missing-name", "oauth", "", "", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cache := identityCache{now: time.Now, lookup: func(string) (hostIdentity, bool) {
				return hostIdentity{Label: tc.label, Email: tc.email, Name: tc.file, Provider: "codex"}, true
			}}
			got := cache.metadata(push.UsageRecord{AuthIndex: "idx", AuthType: tc.authType, Source: "sk-upstream-secret"})
			if tc.want == "" {
				if got != nil {
					t.Fatalf("unexpected metadata: %+v", got)
				}
				return
			}
			if got == nil || got.DisplayName != tc.want {
				t.Fatalf("metadata = %+v, want %s", got, tc.want)
			}
		})
	}
	now := time.Unix(100, 0)
	calls := 0
	cache := identityCache{now: func() time.Time { return now }, lookup: func(string) (hostIdentity, bool) { calls++; return hostIdentity{}, false }}
	for range 2 {
		if cache.metadata(push.UsageRecord{AuthIndex: "idx"}) != nil {
			t.Fatal("failed lookup invented a name")
		}
	}
	if calls != 1 {
		t.Fatalf("negative cache queries = %d", calls)
	}
	now = now.Add(31 * time.Second)
	cache.metadata(push.UsageRecord{AuthIndex: "idx"})
	if calls != 2 {
		t.Fatal("failed lookup never retried")
	}
}

// Authentication kind does not identify the source: file-backed and runtime-only
// credentials can use API keys and still be resolved by the host.
func TestIdentityMetadataResolvesAPIKeyCredentials(t *testing.T) {
	for _, tc := range []struct{ name, authType, label, file, want string }{
		{"file-backed", "apikey", "", "codex-service.json", "codex-service.json"},
		{"runtime-only", " APIKey ", "Runtime Channel", "runtime-channel", "Runtime Channel"},
		{"provider-label", "apikey", "codex", "codex-service.json", "codex-service.json"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			cache := identityCache{now: time.Now, lookup: func(index string) (hostIdentity, bool) {
				calls++
				return hostIdentity{AuthIndex: index, Label: tc.label, Name: tc.file, Provider: "codex"}, true
			}}
			for range 2 {
				got := cache.metadata(push.UsageRecord{AuthIndex: "idx", AuthType: tc.authType, Source: "sk-upstream-secret"})
				if got == nil || got.DisplayName != tc.want || got.ProviderType != "codex" {
					t.Fatalf("metadata = %+v, want %q with provider codex", got, tc.want)
				}
			}
			if calls != 1 {
				t.Fatalf("API key lookup was skipped or not cached: queries = %d", calls)
			}
		})
	}
}

// Config-only credentials still produce no identity when the host cannot resolve them.
// Cache the failure, then allow a later successful lookup for the same index.
func TestIdentityMetadataCachesUnavailableAPIKeyLookups(t *testing.T) {
	now := time.Unix(100, 0)
	calls := 0
	available := false
	cache := identityCache{now: func() time.Time { return now }, lookup: func(index string) (hostIdentity, bool) {
		calls++
		if !available {
			return hostIdentity{}, false
		}
		return hostIdentity{AuthIndex: index, Label: "Available credential", Provider: "codex"}, true
	}}
	r := push.UsageRecord{AuthIndex: "idx", AuthType: "apikey", Source: "sk-upstream-secret"}
	for range 2 {
		if got := cache.metadata(r); got != nil {
			t.Fatalf("unavailable API key credential produced metadata: %+v", got)
		}
	}
	if calls != 1 {
		t.Fatalf("failed API key lookup was skipped or not cached: queries = %d", calls)
	}
	available = true
	now = now.Add(29 * time.Second)
	if got := cache.metadata(r); got != nil || calls != 1 {
		t.Fatalf("failed lookup retried before expiry: metadata = %+v, queries = %d", got, calls)
	}
	now = now.Add(2 * time.Second)
	if got := cache.metadata(r); got == nil || got.DisplayName != "Available credential" || calls != 2 {
		t.Fatalf("expired failure did not resolve the credential: metadata = %+v, queries = %d", got, calls)
	}
}

func TestIdentityMetadataProjectionExcludesAccountAndSecrets(t *testing.T) {
	var identity hostIdentity
	if err := json.Unmarshal([]byte(`{"auth_index":"idx","label":"Display","provider":"codex","account":"sk-upstream-secret","source":"sk-upstream-secret","json":{"refresh_token":"secret"}}`), &identity); err != nil {
		t.Fatal(err)
	}
	cache := identityCache{now: time.Now, lookup: func(string) (hostIdentity, bool) { return identity, true }}
	metadata := cache.metadata(push.UsageRecord{AuthIndex: "idx", AuthType: "oauth"})
	raw, _ := json.Marshal(metadata)
	if strings.Contains(string(raw), "secret") || strings.Contains(string(raw), "account") || string(raw) != `{"display_name":"Display","provider_type":"codex"}` {
		t.Fatalf("unsafe metadata: %s", raw)
	}
}

func TestDecodeRuntimeIdentityAcceptsOnlyTheRequestedCredential(t *testing.T) {
	const ok = `{"ok":true,"result":{"auth":{"auth_index":"idx","name":"codex-a.json","label":"codex","provider":"codex","account":"sk-upstream-secret"}}}`
	got, found := decodeRuntimeIdentity([]byte(ok), "idx")
	if !found || got != (hostIdentity{AuthIndex: "idx", Name: "codex-a.json", Label: "codex", Provider: "codex"}) {
		t.Fatalf("decoded identity = %+v, %v", got, found)
	}
	for name, raw := range map[string]string{
		"other-index": strings.Replace(ok, `"auth_index":"idx"`, `"auth_index":"other"`, 1),
		"host-error":  `{"ok":false,"error":{"code":"not_found","message":"auth runtime info not found"}}`,
		"no-result":   `{"ok":true}`,
		"bad-result":  `{"ok":true,"result":"auth"}`,
		"malformed":   `{"ok":`,
		"empty":       ``,
	} {
		if got, found := decodeRuntimeIdentity([]byte(raw), "idx"); found || got != (hostIdentity{}) {
			t.Errorf("%s: accepted %+v", name, got)
		}
	}
}

func TestIdentityCacheConcurrentLookups(t *testing.T) {
	var calls atomic.Int32
	cache := identityCache{now: time.Now, lookup: func(string) (hostIdentity, bool) {
		calls.Add(1)
		return hostIdentity{Label: "Concurrent"}, true
	}}
	var workers sync.WaitGroup
	var wrong atomic.Int32
	for range 16 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for range 50 {
				if got := cache.metadata(push.UsageRecord{AuthIndex: "idx"}); got == nil || got.DisplayName != "Concurrent" {
					wrong.Add(1)
				}
			}
		}()
	}
	workers.Wait()
	// Concurrent first misses may each query the host; afterwards every record hits the cache.
	if wrong.Load() != 0 || calls.Load() < 1 || calls.Load() > 16 {
		t.Fatalf("wrong=%d queries=%d", wrong.Load(), calls.Load())
	}
}

func TestIdentityRefreshFailureKeepsLastNameAndSkipsOversizedIndex(t *testing.T) {
	now := time.Unix(100, 0)
	available := true
	cache := identityCache{now: func() time.Time { return now }, lookup: func(string) (hostIdentity, bool) { return hostIdentity{Label: "Last known"}, available }}
	r := push.UsageRecord{AuthIndex: "idx"}
	if got := cache.metadata(r); got == nil || got.DisplayName != "Last known" {
		t.Fatal("initial metadata missing")
	}
	available = false
	now = now.Add(6 * time.Minute)
	if got := cache.metadata(r); got == nil || got.DisplayName != "Last known" {
		t.Fatal("refresh failure removed a previously known name")
	}
	if got := cache.metadata(push.UsageRecord{AuthIndex: strings.Repeat("x", 129)}); got != nil {
		t.Fatal("oversized auth index produced incompatible metadata")
	}
}

func TestFullIdentityCacheEvictsExpiredEntriesFirst(t *testing.T) {
	now := time.Unix(100, 0)
	calls := map[string]int{}
	cache := identityCache{now: func() time.Time { return now }, lookup: func(index string) (hostIdentity, bool) {
		calls[index]++
		return hostIdentity{Label: "name-" + index}, true
	}}
	cache.metadata(push.UsageRecord{AuthIndex: "stale"})
	now = now.Add(6 * time.Minute)
	for i := range identityCacheLimit - 1 {
		cache.metadata(push.UsageRecord{AuthIndex: fmt.Sprintf("fresh-%d", i)})
	}
	cache.metadata(push.UsageRecord{AuthIndex: "newest"})
	if len(cache.entries) != identityCacheLimit {
		t.Fatalf("cache size = %d", len(cache.entries))
	}
	if _, ok := cache.entries["stale"]; ok {
		t.Fatal("expired entry survived while the cache was full")
	}
	for i := range identityCacheLimit - 1 {
		cache.metadata(push.UsageRecord{AuthIndex: fmt.Sprintf("fresh-%d", i)})
	}
	for index, n := range calls {
		if n != 1 {
			t.Fatalf("%s was queried %d times; a full cache discarded fresh entries", index, n)
		}
	}
}
