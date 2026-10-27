package push

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

// UsageRecord decodes the host's usage.handle request. Hosts marshal pluginapi.UsageRecord
// without JSON tags, so keys are Go field names. The plugin owns this struct instead of
// importing pluginapi.UsageRecord: official v8 hosts omit the canonical accounting and
// request-log fields that a newer host adds, and unknown or missing keys decode as zero.
type UsageRecord struct {
	// Sent only by hosts with plugin accounting fields; zero on official v8 releases.
	AccountingVersion int
	TokenBreakdown    json.RawMessage
	RequestLogID      string
	Endpoint          string
	NodeKind          string
	IsFork            bool
	IsCompaction      bool

	RequestID            string
	TraceID              string
	Provider             string
	ExecutorType         string
	Model                string
	Alias                string
	APIKey               string
	SessionID            string
	ParentSessionID      string
	AuthIndex            string
	AuthType             string
	ReasoningEffort      string
	ServiceTier          string
	EffectiveServiceTier string
	ResponseServiceTier  string
	ResponseModel        string
	Generate             bool
	Stream               bool
	RequestedAt          time.Time
	Latency              time.Duration
	TTFT                 time.Duration
	Failed               bool
	Failure              UsageFailure
	Detail               UsageDetail

	// Decoded only so tests can prove they never reach the payload.
	Source          string
	ResponseHeaders http.Header
}

type UsageFailure struct {
	StatusCode int
	Body       string
}

type UsageDetail struct {
	InputTokens         int64
	OutputTokens        int64
	ReasoningTokens     int64
	CachedTokens        int64
	CacheReadTokens     int64
	CacheCreationTokens int64
	TotalTokens         int64
}

// canonicalAccounting returns the host's v2 token breakdown, or derives one from the flat
// token counts with the same CPA SDK function hosts use when an executor supplies none.
// Derived breakdowns can be less specific than an executor's parser, e.g. unclassified.
func canonicalAccounting(r *UsageRecord) error {
	if r.AccountingVersion != 0 || len(r.TokenBreakdown) != 0 {
		if r.AccountingVersion != coreusage.TokenAccountingSchemaVersion || len(r.TokenBreakdown) == 0 {
			return fmt.Errorf("unsupported usage accounting version %d", r.AccountingVersion)
		}
		return nil
	}
	detail := coreusage.EnsureTokenBreakdownForProvider(coreusage.Detail{
		InputTokens:         r.Detail.InputTokens,
		OutputTokens:        r.Detail.OutputTokens,
		ReasoningTokens:     r.Detail.ReasoningTokens,
		CachedTokens:        r.Detail.CachedTokens,
		CacheReadTokens:     r.Detail.CacheReadTokens,
		CacheCreationTokens: r.Detail.CacheCreationTokens,
		TotalTokens:         r.Detail.TotalTokens,
	}, r.Provider, r.ExecutorType)
	breakdown, err := json.Marshal(detail.TokenBreakdown)
	if err != nil {
		return err
	}
	r.AccountingVersion, r.TokenBreakdown = coreusage.TokenAccountingSchemaVersion, breakdown
	r.Detail.TotalTokens = detail.TotalTokens
	return nil
}

// normalizeOutcome mirrors the newer host's sink normalization. Official hosts deliver
// successes with a zero status code; Keeper's pull channel records them as 200.
func normalizeOutcome(r *UsageRecord) {
	if !r.Failed {
		r.Failure.StatusCode = http.StatusOK
	} else if r.Failure.StatusCode <= 0 {
		r.Failure.StatusCode = http.StatusInternalServerError
	}
	if r.RequestedAt.IsZero() {
		r.RequestedAt = time.Now()
	}
}
