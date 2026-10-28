package main

import (
	"encoding/json"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/brooke1220/keeper-usage-push/push"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
)

const identityCacheLimit = 4096

// hostIdentity is a display-only projection. In particular, Account may contain
// an upstream API key and must never be decoded into the outgoing metadata.
type hostIdentity struct {
	AuthIndex string `json:"auth_index"`
	Name      string `json:"name"`
	Label     string `json:"label"`
	Email     string `json:"email"`
	Provider  string `json:"provider"`
}

type identityCacheEntry struct {
	identity hostIdentity
	expires  time.Time
}

type identityCache struct {
	mu      sync.Mutex
	entries map[string]identityCacheEntry
	lookup  func(string) (hostIdentity, bool)
	now     func() time.Time
}

var usageIdentities = identityCache{lookup: lookupHostIdentity, now: time.Now}

func lookupHostIdentity(index string) (hostIdentity, bool) {
	request, _ := json.Marshal(struct {
		AuthIndex string `json:"auth_index"`
	}{index})
	raw, rc := invokeHostMethod(hostAuthGetRuntimeMethod, request)
	if rc != 0 {
		return hostIdentity{}, false
	}
	return decodeRuntimeIdentity(raw, index)
}

// decodeRuntimeIdentity accepts only a successful host.auth.get_runtime answer for index.
func decodeRuntimeIdentity(raw []byte, index string) (hostIdentity, bool) {
	var envelope pluginabi.Envelope
	var result struct {
		Auth hostIdentity `json:"auth"`
	}
	if json.Unmarshal(raw, &envelope) != nil || !envelope.OK || json.Unmarshal(envelope.Result, &result) != nil || result.Auth.AuthIndex != index {
		return hostIdentity{}, false
	}
	return result.Auth, true
}

// evictLocked makes room for one entry. Expired entries go first, and fresh ones only when
// none expired, so a full cache never makes every index refetch at once.
func (c *identityCache) evictLocked(now time.Time) {
	for key, entry := range c.entries {
		if !now.Before(entry.expires) {
			delete(c.entries, key)
		}
	}
	for key := range c.entries {
		if len(c.entries) < identityCacheLimit {
			return
		}
		delete(c.entries, key)
	}
}

func (c *identityCache) reset() {
	c.mu.Lock()
	c.entries = nil
	c.mu.Unlock()
}

func (c *identityCache) metadata(record push.UsageRecord) *push.IdentityMetadata {
	index := strings.TrimSpace(record.AuthIndex)
	authType := strings.ToLower(strings.TrimSpace(record.AuthType))
	// Authentication kind does not identify the source: file-backed and runtime-only
	// credentials may use API keys too. Cache failed lookups for config-only credentials.
	if index == "" || len(record.AuthIndex) > 128 {
		return nil
	}
	now := c.now()
	c.mu.Lock()
	entry, cached := c.entries[index]
	c.mu.Unlock()
	if !cached || !now.Before(entry.expires) {
		// CPA hands usage to plugins one record at a time on a single goroutine, so a miss
		// delays every usage plugin. This is a local read-only lookup, never an HTTP request.
		identity, ok := c.lookup(index)
		ttl := 30 * time.Second
		if ok {
			ttl = 5 * time.Minute
		}
		if !ok && cached {
			identity = entry.identity
		}
		entry = identityCacheEntry{identity: identity, expires: now.Add(ttl)}
		c.mu.Lock()
		if c.entries == nil {
			c.entries = make(map[string]identityCacheEntry)
		}
		if _, ok := c.entries[index]; !ok && len(c.entries) >= identityCacheLimit {
			c.evictLocked(now)
		}
		c.entries[index] = entry
		c.mu.Unlock()
	}
	identity, oauth := entry.identity, authType == "oauth"
	var candidates []string
	// CPA labels a credential with its provider type when it has no email; that names no credential.
	if !strings.EqualFold(strings.TrimSpace(identity.Label), strings.TrimSpace(identity.Provider)) {
		candidates = append(candidates, identity.Label)
	}
	if oauth {
		candidates = append(candidates, identity.Email)
	}
	candidates = append(candidates, identity.Name)
	// Some hosts use the upstream secret as Source. Never recover it as a
	// display label, including when an accidental label contains that value.
	secret := strings.TrimSpace(record.Source)
	for _, candidate := range candidates {
		name := displayMetadataText(candidate, 256)
		if name == "" || (!oauth && len(secret) >= 6 && strings.Contains(name, secret)) {
			continue
		}
		return &push.IdentityMetadata{DisplayName: name, ProviderType: displayMetadataText(identity.Provider, 64)}
	}
	return nil
}

func displayMetadataText(value string, limit int) string {
	value = strings.TrimSpace(value)
	if !utf8.ValidString(value) || utf8.RuneCountInString(value) > limit || strings.ContainsFunc(value, unicode.IsControl) {
		return ""
	}
	return value
}
