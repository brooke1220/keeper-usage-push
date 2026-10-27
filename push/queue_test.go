package push

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func record(t *testing.T) UsageRecord {
	t.Helper()
	breakdown := json.RawMessage(`{"schema_version":2,"quality":"complete","total_tokens":140,"unclassified_tokens":0,"input":{"total_tokens":100,"uncached_tokens":80,"cache_read_tokens":20,"cache_write_tokens":0},"output":{"total_tokens":40,"non_reasoning_tokens":30,"reasoning_tokens":10}}`)
	return UsageRecord{RequestID: "execution-1", RequestedAt: time.Date(2026, 10, 26, 1, 0, 0, 0, time.UTC), Provider: "codex", Model: "gpt-test", APIKey: "sk-full-private-key", ExecutorType: "CodexExecutor", AccountingVersion: 2, TokenBreakdown: breakdown, Generate: true, Detail: UsageDetail{InputTokens: 100, OutputTokens: 40, CacheReadTokens: 20, ReasoningTokens: 10, TotalTokens: 140}}
}
func configFor(t *testing.T, url string) Config {
	return Config{Endpoint: url, Token: strings.Repeat("t", 32), SpoolDir: t.TempDir()}
}
func openQueue(t *testing.T, c Config) *Queue {
	t.Helper()
	q, err := Open(c)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = q.Close() })
	return q
}
func ack(w http.ResponseWriter, b Batch) {
	_ = json.NewEncoder(w).Encode(Acknowledgement{SchemaVersion: 1, AcceptedIDs: ids(b.Events)})
}
func ids(e []Event) []string {
	r := make([]string, 0, len(e))
	for _, v := range e {
		r = append(r, v.ID)
	}
	return r
}

func TestDurableRetryPreservesDeliveryIdentity(t *testing.T) {
	var calls atomic.Int32
	var firstID string
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b Batch
		if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
			t.Error(err)
			return
		}
		if r.Header.Get("Authorization") != "Bearer "+strings.Repeat("t", 32) {
			t.Error("missing machine auth")
		}
		if calls.Add(1) == 1 {
			firstID = b.Events[0].ID
			http.Error(w, "temporarily unavailable", 503)
			return
		}
		if b.Events[0].ID != firstID {
			t.Error("retry changed event identity")
		}
		ack(w, b)
	}))
	defer server.Close()
	cfg := configFor(t, server.URL+"/api/v1/ingest/usage")
	q := openQueue(t, cfg)
	q.client = server.Client()
	if err := q.Enqueue(record(t)); err != nil {
		t.Fatal(err)
	}
	if _, err := q.Flush(context.Background()); err == nil {
		t.Fatal("failed upload acknowledged")
	}
	if err := q.Close(); err != nil {
		t.Fatal(err)
	}
	q = openQueue(t, cfg)
	q.client = server.Client()
	if _, err := q.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	s, err := q.Stats()
	if err != nil || s.Pending != 0 || s.Bytes != 0 || s.LastSuccess == "" {
		t.Fatalf("incorrect durable state: %+v %v", s, err)
	}
}

func TestLostResponseRetriesSameEvent(t *testing.T) {
	seen := map[string]int{}
	var first bool
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b Batch
		_ = json.NewDecoder(r.Body).Decode(&b)
		seen[b.Events[0].ID]++
		if !first {
			first = true
			conn, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			_ = conn.Close()
			return
		}
		ack(w, b)
	}))
	defer server.Close()
	q := openQueue(t, configFor(t, server.URL))
	q.client = server.Client()
	if err := q.Enqueue(record(t)); err != nil {
		t.Fatal(err)
	}
	if _, err := q.Flush(context.Background()); err == nil {
		t.Fatal("lost response should retain data")
	}
	if _, err := q.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 1 {
		t.Fatal("lost response created a new event")
	}
	for _, count := range seen {
		if count != 2 {
			t.Fatal("not replayed")
		}
	}
}

func TestPermanentFailureQuarantinesOnlyBadEvent(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b Batch
		_ = json.NewDecoder(r.Body).Decode(&b)
		for _, e := range b.Events {
			if strings.Contains(string(e.Usage), "bad-model") {
				w.WriteHeader(422)
				_, _ = io.WriteString(w, `{"error":"invalid_usage_event"}`)
				return
			}
		}
		ack(w, b)
	}))
	defer server.Close()
	q := openQueue(t, configFor(t, server.URL))
	q.client = server.Client()
	for _, model := range []string{"good-model", "bad-model", "another-good-model"} {
		r := record(t)
		r.Model = model
		if err := q.Enqueue(r); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := q.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	stats, _ := q.Stats()
	if stats.Pending != 0 || stats.Quarantined != 1 {
		t.Fatalf("wrong isolation: %+v", stats)
	}
	quarantined, err := q.Quarantined()
	if err != nil || len(quarantined) != 1 || quarantined[0].Code != "http_422_invalid_usage_event" {
		t.Fatalf("wrong quarantine listing: %+v %v", quarantined, err)
	}
	if err := q.Replay(quarantined[0].ID); err != nil {
		t.Fatal(err)
	}
	stats, _ = q.Stats()
	if stats.Pending != 1 {
		t.Fatal("operator replay lost")
	}
}

func TestUnknownAcknowledgementNeverDeletes(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(Acknowledgement{1, []string{"unknown"}})
	}))
	defer server.Close()
	q := openQueue(t, configFor(t, server.URL))
	q.client = server.Client()
	_ = q.Enqueue(record(t))
	if _, err := q.Flush(context.Background()); err == nil {
		t.Fatal("accepted unknown ack")
	}
	s, _ := q.Stats()
	if s.Pending != 1 {
		t.Fatal("deleted unacknowledged usage")
	}
}

func TestQueueCapacityOwnershipAndDurability(t *testing.T) {
	cfg := configFor(t, "https://keeper.invalid/api/v1/ingest/usage")
	q := openQueue(t, cfg)
	if other, err := Open(cfg); !errors.Is(err, ErrSpoolLocked) {
		if err == nil {
			_ = other.Close()
		}
		t.Fatalf("second writer was not reported as a held lock: %v", err)
	}
	if err := q.Enqueue(record(t)); err != nil {
		t.Fatal(err)
	}
	s, _ := q.Stats()
	q.cfg.MaxSpoolBytes = s.Bytes
	if err := q.Enqueue(record(t)); !errors.Is(err, ErrQueueFull) {
		t.Fatalf("expected queue full: %v", err)
	}
	s, _ = q.Stats()
	if s.Pending != 1 || s.Dropped != 1 {
		t.Fatalf("capacity policy wrong: %+v", s)
	}
	q.db.SetMaxIdleConns(0)
	var syncMode int
	if err := q.db.QueryRow("PRAGMA synchronous").Scan(&syncMode); err != nil || syncMode != 2 {
		t.Fatalf("replacement connection lost FULL: %d %v", syncMode, err)
	}
	_ = q.Close()
	moved := cfg
	moved.Endpoint = "https://other-keeper.invalid/api/v1/ingest/usage"
	if other, err := Open(moved); err == nil {
		_ = other.Close()
		t.Fatal("pending records were redirected to another Keeper")
	}
	// A rotated token keeps the same spool and pending records; Keeper maps it to the same source.
	rotated := cfg
	rotated.Token = strings.Repeat("r", 32)
	reopened, err := Open(rotated)
	if err != nil {
		t.Fatalf("token rotation lost the spool: %v", err)
	}
	if s, err := reopened.Stats(); err != nil || s.Pending != 1 {
		t.Fatalf("pending records lost after rotation: %+v %v", s, err)
	}
	_ = reopened.Close()
}

func TestBatchCarriesNoSourceIdentity(t *testing.T) {
	var body map[string]json.RawMessage
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Error(err)
		}
		var b Batch
		_ = json.Unmarshal(raw, &b)
		ack(w, b)
	}))
	defer server.Close()
	q := openQueue(t, configFor(t, server.URL+"/api/v1/ingest/usage"))
	q.client = server.Client()
	if err := q.Enqueue(record(t)); err != nil {
		t.Fatal(err)
	}
	if _, err := q.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, ok := body["source_id"]; ok {
		t.Fatalf("envelope still carries source_id: %v", body)
	}
	if _, ok := body["events"]; !ok {
		t.Fatalf("envelope lost events: %v", body)
	}
}

func TestDefaultSpoolDirUsesPersistentPluginsDirectory(t *testing.T) {
	work := t.TempDir()
	t.Chdir(work)
	cfg := Config{Endpoint: "https://keeper.invalid/api/v1/ingest/usage", Token: strings.Repeat("t", 32)}
	// Without CPA's plugins directory there is no persistent default: refuse instead of using a temp dir.
	if err := ValidateConfig(cfg); err == nil || !strings.Contains(err.Error(), "spool_dir") {
		t.Fatalf("missing plugins directory accepted: %v", err)
	}
	if err := os.Mkdir(filepath.Join(work, "plugins"), 0o755); err != nil {
		t.Fatal(err)
	}
	q, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = q.Close() }()
	if _, err := os.Stat(filepath.Join(work, DefaultSpoolDir, "outbox.db")); err != nil {
		t.Fatalf("default spool not created under plugins: %v", err)
	}
}

func TestTelemetryTimeoutRetainsRecord(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.ReadAll(r.Body); <-r.Context().Done() }))
	defer server.Close()
	cfg := configFor(t, server.URL)
	cfg.RequestTimeout = 20 * time.Millisecond
	q := openQueue(t, cfg)
	q.client = server.Client()
	_ = q.Enqueue(record(t))
	if _, err := q.Flush(context.Background()); err == nil {
		t.Fatal("hanging upload did not time out")
	}
	s, _ := q.Stats()
	if s.Pending != 1 {
		t.Fatal("timeout deleted event")
	}
}

func TestPayloadExcludesSecretsAndBodies(t *testing.T) {
	r := record(t)
	r.ResponseHeaders = http.Header{"Authorization": []string{"upstream-secret"}}
	r.Failure.Body = "private prompt in failure body"
	r.Source = "upstream-key-from-legacy-source"
	raw, err := payload(r)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"upstream-secret", "upstream-key-from-legacy-source", "private prompt", "response_headers", "Body"} {
		if strings.Contains(string(raw), forbidden) {
			t.Fatalf("payload leaked %q", forbidden)
		}
	}
}

func TestPostmanSimulationTransport(t *testing.T) {
	endpoint := os.Getenv("KEEPER_PUSH_SIMULATION_ENDPOINT")
	if endpoint == "" {
		t.Skip("set KEEPER_PUSH_SIMULATION_ENDPOINT to run real HTTP against the saved Postman simulation")
	}
	cfg := configFor(t, endpoint)
	q := openQueue(t, cfg)
	if err := q.Enqueue(record(t)); err != nil {
		t.Fatal(err)
	}
	if _, err := q.Flush(context.Background()); err == nil {
		t.Fatal("simulation's first 503 was not propagated")
	}
	if s, err := q.Stats(); err != nil || s.Pending != 1 {
		t.Fatalf("failed HTTP upload was not retained: %+v %v", s, err)
	}
	if _, err := q.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	s, _ := q.Stats()
	if s.Pending != 0 {
		t.Fatal("simulation retry did not complete")
	}
}

func TestRetryScheduleNeverExceedsOneMinute(t *testing.T) {
	for _, backoff := range []time.Duration{time.Second, 30 * time.Second, 60 * time.Second} {
		for entropy := 0; entropy < 256; entropy++ {
			delay := retryDelay(backoff, byte(entropy))
			if delay < backoff || delay > 60*time.Second {
				t.Fatalf("retry delay outside schedule: backoff=%v entropy=%d delay=%v", backoff, entropy, delay)
			}
		}
	}
}

func TestOversizedPendingRowDoesNotStallQueue(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b Batch
		_ = json.NewDecoder(r.Body).Decode(&b)
		ack(w, b)
	}))
	defer server.Close()
	q := openQueue(t, configFor(t, server.URL))
	q.client = server.Client()
	// Simulates a row persisted under a larger limit, e.g. before an upgrade lowered it.
	big := []byte(strings.Repeat("x", MaxBatchBytes))
	if _, err := q.db.Exec("INSERT INTO outbox(event_id,payload,size,status) VALUES('oversized',?,?,'pending')", big, len(big)); err != nil {
		t.Fatal(err)
	}
	if err := q.Enqueue(record(t)); err != nil {
		t.Fatal(err)
	}
	if n, err := q.Flush(context.Background()); err != nil || n != 1 {
		t.Fatalf("oversized row stalled delivery: %d %v", n, err)
	}
	stats, _ := q.Stats()
	if stats.Pending != 0 || stats.Quarantined != 1 {
		t.Fatalf("oversized row was not isolated: %+v", stats)
	}
	if err := q.Replay("oversized"); err == nil {
		t.Fatal("replayed a record that can never be uploaded")
	}
	if _, err := q.ReplayCode(codeEventTooLarge); err == nil {
		t.Fatal("bulk-replayed records that can never be uploaded")
	}
}

func TestReplayCodeRequeuesAfterReceiverFix(t *testing.T) {
	var reject atomic.Bool
	reject.Store(true)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b Batch
		_ = json.NewDecoder(r.Body).Decode(&b)
		if reject.Load() {
			w.WriteHeader(http.StatusUnprocessableEntity)
			_, _ = io.WriteString(w, `{"error":"invalid_usage_event"}`)
			return
		}
		ack(w, b)
	}))
	defer server.Close()
	q := openQueue(t, configFor(t, server.URL))
	q.client = server.Client()
	for i := 0; i < 3; i++ {
		if err := q.Enqueue(record(t)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := q.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	codes, err := q.QuarantineCodes()
	if err != nil || codes["http_422_invalid_usage_event"] != 3 {
		t.Fatalf("quarantine codes wrong: %v %v", codes, err)
	}
	reject.Store(false)
	if n, err := q.ReplayCode("http_422_invalid_usage_event"); err != nil || n != 3 {
		t.Fatalf("bulk replay: %d %v", n, err)
	}
	if _, err := q.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	stats, _ := q.Stats()
	if stats.Pending != 0 || stats.Quarantined != 0 || stats.Bytes != 0 {
		t.Fatalf("replayed records were not delivered: %+v", stats)
	}
}

func TestPayloadMatchesQueueDefaultsAndSessionTree(t *testing.T) {
	r := record(t)
	r.Model, r.Alias, r.Provider, r.ExecutorType, r.AuthType = "", "", "", " ", ""
	r.NodeKind, r.IsFork, r.IsCompaction = "subagent", true, true
	raw, err := payload(r)
	if err != nil {
		t.Fatal(err)
	}
	var got usagePayload
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got.Model != "unknown" || got.Alias != "unknown" || got.Provider != "unknown" || got.ExecutorType != "unknown" || got.AuthType != "unknown" {
		t.Fatalf("empty identity fields differ from the Redis queue defaults: %+v", got)
	}
	if got.NodeKind != "subagent" || !got.IsFork || !got.IsCompaction {
		t.Fatalf("session tree fields lost: %+v", got)
	}
}
