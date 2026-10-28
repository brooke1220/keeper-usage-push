// Package push implements durable, asynchronous Keeper usage delivery.
package push

import (
	"encoding/json"
	"strings"
	"time"

	coresession "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/session"
)

const SchemaVersion = 1
const MaxBatchBytes = 1 << 20
const MaxBatchEvents = 100

type Event struct {
	ID    string          `json:"delivery_event_id"`
	Usage json.RawMessage `json:"usage"`
}

// Batch carries no source identity: Keeper derives the source from the bearer token.
type Batch struct {
	SchemaVersion int     `json:"schema_version"`
	Events        []Event `json:"events"`
}

type Acknowledgement struct {
	SchemaVersion int      `json:"schema_version"`
	AcceptedIDs   []string `json:"accepted_event_ids"`
}

type tokenStats struct {
	Input            int64 `json:"input_tokens"`
	Output           int64 `json:"output_tokens"`
	Reasoning        int64 `json:"reasoning_tokens"`
	Cached           int64 `json:"cached_tokens"`
	CacheRead        int64 `json:"cache_read_tokens"`
	CacheReadPresent bool  `json:"cache_read_tokens_present"`
	CacheCreation    int64 `json:"cache_creation_tokens"`
	Total            int64 `json:"total_tokens"`
}

// usagePayload is a whitelist: no response bodies, arbitrary headers or upstream secrets.
// Legacy UsageRecord.Source can be an upstream API key and is never copied into this payload.
type usagePayload struct {
	Timestamp            time.Time         `json:"timestamp"`
	APIKey               string            `json:"api_key"`
	Provider             string            `json:"provider"`
	Model                string            `json:"model"`
	Alias                string            `json:"alias,omitempty"`
	ResponseModel        string            `json:"response_model,omitempty"`
	ExecutorType         string            `json:"executor_type"`
	Endpoint             string            `json:"endpoint,omitempty"`
	AuthType             string            `json:"auth_type"`
	AuthIndex            string            `json:"auth_index,omitempty"`
	Identity             *IdentityMetadata `json:"identity,omitempty"`
	RequestID            string            `json:"request_id,omitempty"`
	ExecutionID          string            `json:"execution_id"`
	TraceID              string            `json:"trace_id,omitempty"`
	SessionID            string            `json:"session_id,omitempty"`
	ParentSessionID      string            `json:"parent_session_id,omitempty"`
	NodeKind             string            `json:"node_kind,omitempty"`
	IsFork               bool              `json:"is_fork,omitempty"`
	IsCompaction         bool              `json:"is_compaction,omitempty"`
	ReasoningEffort      string            `json:"reasoning_effort,omitempty"`
	ServiceTier          string            `json:"service_tier,omitempty"`
	EffectiveServiceTier string            `json:"effective_service_tier,omitempty"`
	ResponseServiceTier  string            `json:"response_service_tier,omitempty"`
	Generate             bool              `json:"generate"`
	Stream               bool              `json:"stream"`
	Failed               bool              `json:"failed"`
	Fail                 struct {
		StatusCode int `json:"status_code"`
	} `json:"fail"`
	LatencyMS         int64           `json:"latency_ms"`
	TTFTMS            *int64          `json:"ttft_ms,omitempty"`
	Tokens            tokenStats      `json:"tokens"`
	AccountingVersion int             `json:"accounting_version"`
	TokenBreakdown    json.RawMessage `json:"token_breakdown"`
}

func payload(r UsageRecord) ([]byte, error) {
	var ttft *int64
	if r.TTFT > 0 {
		v := r.TTFT.Milliseconds()
		ttft = &v
	}
	// Match the Redis usage queue defaults so both channels count the same records.
	model := orUnknown(r.Model)
	alias := strings.TrimSpace(r.Alias)
	if alias == "" {
		alias = model
	}
	// The host hands plugins raw session IDs; the Redis queue stores them as canonical UUIDs.
	sessionID := coresession.NormalizeToCanonicalUUID(r.SessionID)
	parentSessionID := coresession.NormalizeToCanonicalUUID(r.ParentSessionID)
	if sessionID == "" || sessionID == parentSessionID {
		parentSessionID = ""
	}
	v := usagePayload{
		Timestamp: r.RequestedAt, APIKey: r.APIKey, Provider: orUnknown(r.Provider), Model: model,
		Alias: alias, ResponseModel: r.ResponseModel, ExecutorType: orUnknown(r.ExecutorType),
		Endpoint: r.Endpoint, AuthType: orUnknown(r.AuthType), AuthIndex: r.AuthIndex,
		Identity:  r.Identity,
		RequestID: r.RequestLogID, ExecutionID: r.RequestID, TraceID: r.TraceID,
		SessionID: sessionID, ParentSessionID: parentSessionID,
		NodeKind: r.NodeKind, IsFork: r.IsFork, IsCompaction: r.IsCompaction,
		ReasoningEffort: r.ReasoningEffort, ServiceTier: r.ServiceTier,
		EffectiveServiceTier: r.EffectiveServiceTier, ResponseServiceTier: r.ResponseServiceTier,
		Generate: r.Generate, Stream: r.Stream, Failed: r.Failed,
		LatencyMS: max(r.Latency.Milliseconds(), 0), TTFTMS: ttft,
		AccountingVersion: r.AccountingVersion, TokenBreakdown: r.TokenBreakdown,
		Tokens: tokenStats{r.Detail.InputTokens, r.Detail.OutputTokens, r.Detail.ReasoningTokens,
			r.Detail.CachedTokens, r.Detail.CacheReadTokens, true, r.Detail.CacheCreationTokens, r.Detail.TotalTokens},
	}
	v.Fail.StatusCode = r.Failure.StatusCode
	return json.Marshal(v)
}

// withoutIdentity returns a copy of events with the optional identity removed, and whether any
// event carried one. The result depends only on the input bytes, so every retry sends the same
// content; other fields keep their encoded values and rows that do not decode stay unchanged.
func withoutIdentity(events []Event) ([]Event, bool) {
	stripped := append([]Event(nil), events...)
	changed := false
	for i, e := range stripped {
		var fields map[string]json.RawMessage
		if json.Unmarshal(e.Usage, &fields) != nil {
			continue
		}
		if _, ok := fields["identity"]; !ok {
			continue
		}
		delete(fields, "identity")
		raw, err := json.Marshal(fields)
		if err != nil {
			continue
		}
		stripped[i].Usage, changed = raw, true
	}
	return stripped, changed
}

func orUnknown(value string) string {
	if value = strings.TrimSpace(value); value != "" {
		return value
	}
	return "unknown"
}
