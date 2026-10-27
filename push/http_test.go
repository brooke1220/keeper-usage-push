package push

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
)

func TestHTTPUploadWithoutExtraSettings(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/ingest/usage" || r.TLS != nil {
			t.Errorf("unexpected HTTP upload: %s %s TLS=%v", r.Method, r.URL.Path, r.TLS != nil)
		}
		if r.Header.Get("Authorization") != "Bearer "+strings.Repeat("t", 32) {
			t.Error("HTTP upload did not authenticate")
		}
		var batch Batch
		if err := json.NewDecoder(r.Body).Decode(&batch); err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if batch.SchemaVersion != SchemaVersion || len(batch.Events) != 1 {
			t.Errorf("unexpected HTTP batch: version=%d events=%d", batch.SchemaVersion, len(batch.Events))
		}
		ack(w, batch)
	}))
	defer server.Close()
	q := openQueue(t, configFor(t, server.URL+"/api/v1/ingest/usage"))
	if err := q.Enqueue(record(t)); err != nil {
		t.Fatal(err)
	}
	if count, err := q.Flush(context.Background()); err != nil || count != 1 {
		t.Fatalf("HTTP upload failed: count=%d error=%v", count, err)
	}
	stats, err := q.Stats()
	if err != nil || stats.Pending != 0 || stats.Bytes != 0 || stats.LastSuccess == "" || calls.Load() != 1 {
		t.Fatalf("HTTP upload was not acknowledged: stats=%+v calls=%d error=%v", stats, calls.Load(), err)
	}
}

func TestAuthenticationFailureKeepsDurableRecord(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusUnauthorized) }))
	defer server.Close()
	q := openQueue(t, configFor(t, server.URL))
	q.client.Transport = server.Client().Transport
	if err := q.Enqueue(record(t)); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if _, err := q.Flush(context.Background()); err == nil {
			t.Fatal("authentication failure was acknowledged")
		}
	}
	stats, err := q.Stats()
	if err != nil || stats.Pending != 1 || stats.Quarantined != 0 || stats.Dropped != 0 {
		t.Fatalf("authentication failure discarded data: %+v %v", stats, err)
	}
}

func TestRedirectNeverForwardsPushCredential(t *testing.T) {
	var followed atomic.Int32
	target := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { followed.Add(1); w.WriteHeader(200) }))
	defer target.Close()
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer server.Close()
	q := openQueue(t, configFor(t, server.URL))
	q.client.Transport = server.Client().Transport
	if err := q.Enqueue(record(t)); err != nil {
		t.Fatal(err)
	}
	if _, err := q.Flush(context.Background()); err == nil {
		t.Fatal("redirect was acknowledged")
	}
	if followed.Load() != 0 {
		t.Fatal("redirect forwarded the upload credential")
	}
	stats, err := q.Stats()
	if err != nil || stats.Pending != 1 {
		t.Fatalf("redirect dropped record: %+v %v", stats, err)
	}
}

func TestUntrustedTLSCertificateKeepsDurableRecord(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("untrusted HTTPS server received usage") }))
	defer server.Close()
	q := openQueue(t, configFor(t, server.URL))
	if err := q.Enqueue(record(t)); err != nil {
		t.Fatal(err)
	}
	if _, err := q.Flush(context.Background()); err == nil {
		t.Fatal("untrusted certificate was accepted")
	}
	stats, err := q.Stats()
	if err != nil || stats.Pending != 1 || stats.Quarantined != 0 {
		t.Fatalf("TLS failure dropped record: %+v %v", stats, err)
	}
}

func TestForeignRejectionKeepsRecordsPending(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"proxy-400", http.StatusBadRequest, "<html>blocked by proxy</html>"},
		{"waf-422", http.StatusUnprocessableEntity, `{"message":"rejected"}`},
		{"mismatched-code", http.StatusUnprocessableEntity, `{"error":"delivery_id_conflict"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer server.Close()
			q := openQueue(t, configFor(t, server.URL))
			q.client.Transport = server.Client().Transport
			for i := 0; i < 3; i++ {
				if err := q.Enqueue(record(t)); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := q.Flush(context.Background()); err == nil {
				t.Fatal("foreign rejection was acknowledged")
			}
			stats, err := q.Stats()
			if err != nil || stats.Pending != 3 || stats.Quarantined != 0 {
				t.Fatalf("foreign rejection quarantined data: %+v %v", stats, err)
			}
			if calls.Load() != 1 {
				t.Fatalf("foreign rejection split the batch: %d requests", calls.Load())
			}
		})
	}
}

func TestProxyBodyLimitSplitsWithoutQuarantine(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b Batch
		_ = json.NewDecoder(r.Body).Decode(&b)
		if len(b.Events) > 1 {
			w.WriteHeader(http.StatusRequestEntityTooLarge)
			return
		}
		ack(w, b)
	}))
	defer server.Close()
	q := openQueue(t, configFor(t, server.URL))
	q.client.Transport = server.Client().Transport
	for i := 0; i < 3; i++ {
		if err := q.Enqueue(record(t)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := q.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	stats, err := q.Stats()
	if err != nil || stats.Pending != 0 || stats.Quarantined != 0 {
		t.Fatalf("proxy limit was not resolved by splitting: %+v %v", stats, err)
	}
}

func TestBareSingleEvent413StaysPending(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusRequestEntityTooLarge)
	}))
	defer server.Close()
	q := openQueue(t, configFor(t, server.URL))
	q.client.Transport = server.Client().Transport
	if err := q.Enqueue(record(t)); err != nil {
		t.Fatal(err)
	}
	if _, err := q.Flush(context.Background()); err == nil {
		t.Fatal("bare 413 was acknowledged")
	}
	stats, err := q.Stats()
	if err != nil || stats.Pending != 1 || stats.Quarantined != 0 {
		t.Fatalf("a proxy limit quarantined an event Keeper never saw: %+v %v", stats, err)
	}
}

func TestTransportErrorKeepsCause(t *testing.T) {
	q := openQueue(t, configFor(t, "https://127.0.0.1:1/api/v1/ingest/usage"))
	if err := q.Enqueue(record(t)); err != nil {
		t.Fatal(err)
	}
	_, err := q.Flush(context.Background())
	var opErr *net.OpError
	// Windows reports Winsock errors rather than Unix ECONNREFUSED. Both must
	// retain the network operation and its underlying OS error for diagnosis.
	var errno syscall.Errno
	if err == nil || !errors.As(err, &opErr) || !errors.As(err, &errno) {
		t.Fatalf("transport error lost its cause: %v", err)
	}
	if strings.Contains(err.Error(), strings.Repeat("t", 32)) {
		t.Fatal("transport error leaked the push token")
	}
}
