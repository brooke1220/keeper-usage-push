package push

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	coresession "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/session"
	coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

// officialHostRecord is what an official v8 host sends: pluginapi.UsageRecord marshaled without
// JSON tags, with no accounting fields and a zero status code on success.
const officialHostRecord = `{"RequestID":"exec-official","TraceID":"a1b2c3d4","SelectionID":"sel","Provider":"claude","BaseURL":"https://upstream.invalid","ExecutorType":"ClaudeExecutor","Model":"claude-test","Alias":"","APIKey":"sk-client","IsNativeKey":true,"AccessProvider":"config-inline","SessionID":"","ParentSessionID":"","AuthID":"auth","AuthIndex":"0123456789abcdef","AuthType":"apikey","Source":"sk-upstream-secret","ReasoningEffort":"","ServiceTier":"","EffectiveServiceTier":"","ResponseServiceTier":"","ResponseModel":"","Generate":true,"Stream":true,"RequestedAt":"2026-10-27T01:00:00Z","Latency":1500000000,"TTFT":300000000,"Failed":false,"Failure":{"StatusCode":0,"Body":""},"Detail":{"InputTokens":100,"OutputTokens":40,"ReasoningTokens":0,"CachedTokens":30,"CacheReadTokens":30,"CacheCreationTokens":0,"TotalTokens":170},"ResponseHeaders":{"Authorization":["upstream-secret"]}}`

func TestOfficialHostRecordIsAccepted(t *testing.T) {
	q := openQueue(t, configFor(t, "https://keeper.invalid/api/v1/ingest/usage"))
	var r UsageRecord
	if err := json.Unmarshal([]byte(officialHostRecord), &r); err != nil {
		t.Fatal(err)
	}
	if err := q.Enqueue(r); err != nil {
		t.Fatalf("official host record was dropped: %v", err)
	}
	var raw []byte
	if err := q.db.QueryRow("SELECT payload FROM outbox").Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var got usagePayload
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	var breakdown coreusage.TokenBreakdown
	if err := json.Unmarshal(got.TokenBreakdown, &breakdown); err != nil {
		t.Fatal(err)
	}
	want := coreusage.EnsureTokenBreakdownForProvider(coreusage.Detail{InputTokens: 100, OutputTokens: 40, CachedTokens: 30, CacheReadTokens: 30, TotalTokens: 170}, "claude", "ClaudeExecutor").TokenBreakdown
	if got.AccountingVersion != 2 || breakdown != want || !breakdown.Valid() || breakdown.Quality != coreusage.TokenAccountingQualityComplete {
		t.Fatalf("derived accounting differs from the CPA SDK: %+v", breakdown)
	}
	if got.Fail.StatusCode != 200 || got.ExecutionID != "exec-official" || got.LatencyMS != 1500 || got.TTFTMS == nil || *got.TTFTMS != 300 || got.Timestamp != time.Date(2026, 10, 27, 1, 0, 0, 0, time.UTC) {
		t.Fatalf("official fields were not mapped: %+v", got)
	}
	for _, forbidden := range []string{"sk-upstream-secret", "upstream-secret", "upstream.invalid"} {
		if strings.Contains(string(raw), forbidden) {
			t.Fatalf("payload leaked %q", forbidden)
		}
	}
}

func TestHostAccountingIsPreservedAndValidated(t *testing.T) {
	r := record(t)
	original := string(r.TokenBreakdown)
	if err := canonicalAccounting(&r); err != nil || string(r.TokenBreakdown) != original {
		t.Fatalf("host accounting was replaced: %v %s", err, r.TokenBreakdown)
	}
	r.AccountingVersion = 3
	if err := canonicalAccounting(&r); err == nil {
		t.Fatal("unknown accounting version accepted")
	}
}

func TestSessionIDsMatchRedisQueue(t *testing.T) {
	const uuid = "0f8fad5b-d9cb-469f-a165-70867728950e"
	for _, tc := range []struct{ session, parent, wantSession, wantParent string }{
		{" " + strings.ToUpper(uuid) + " ", "", uuid, ""},
		{"claude:" + uuid, "opaque-parent", uuid, coresession.NormalizeToCanonicalUUID("opaque-parent")},
		{uuid, uuid, uuid, ""},
		{"", uuid, "", ""},
	} {
		r := record(t)
		r.SessionID, r.ParentSessionID = tc.session, tc.parent
		raw, err := payload(r)
		if err != nil {
			t.Fatal(err)
		}
		var got usagePayload
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatal(err)
		}
		if got.SessionID != tc.wantSession || got.ParentSessionID != tc.wantParent {
			t.Fatalf("session %q parent %q: got %q %q, want %q %q", tc.session, tc.parent, got.SessionID, got.ParentSessionID, tc.wantSession, tc.wantParent)
		}
	}
}

func TestOutcomeDefaultsMatchRedisQueue(t *testing.T) {
	ok := UsageRecord{RequestedAt: time.Now()}
	normalizeOutcome(&ok)
	failed := UsageRecord{Failed: true}
	normalizeOutcome(&failed)
	if ok.Failure.StatusCode != 200 || failed.Failure.StatusCode != 500 || failed.RequestedAt.IsZero() {
		t.Fatalf("outcome defaults differ: ok=%+v failed=%+v", ok.Failure, failed)
	}
}
