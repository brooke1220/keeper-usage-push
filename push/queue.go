package push

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	_ "github.com/mattn/go-sqlite3"
	log "github.com/sirupsen/logrus"
)

var ErrQueueFull = errors.New("usage outbox capacity exhausted")

// ErrSpoolLocked reports that another process or plugin image owns the spool.
var ErrSpoolLocked = errors.New("usage spool already has a writer")

// codeEventTooLarge marks rows that can never fit an upload batch; replay would only re-quarantine them.
const codeEventTooLarge = "event_too_large"

// dropLogInterval is the shortest gap between persistence-failure logs after the first one.
const dropLogInterval = time.Minute

type Queue struct {
	cfg               Config
	db                *sql.DB
	client            *http.Client
	lock              *os.File
	wake              chan struct{}
	cancel            context.CancelFunc
	done              chan struct{}
	startOnce         sync.Once
	closeOnce         sync.Once
	dropped           atomic.Uint64
	enqueuedSinceWake atomic.Uint64
	// dropLog rate-limits persistence-failure logs; last keeps a monotonic clock reading.
	dropLog struct {
		sync.Mutex
		last     time.Time
		unlogged uint64
	}
	// lastStats is owned by the upload worker and only used to decide log levels.
	lastStats Stats
}

// QuarantinedEvent identifies a locally retained record without exposing its payload.
type QuarantinedEvent struct {
	ID   string `json:"delivery_event_id"`
	Code string `json:"error_code"`
}

type Stats struct {
	Pending     int64  `json:"pending"`
	Quarantined int64  `json:"quarantined"`
	Bytes       int64  `json:"bytes"`
	Dropped     uint64 `json:"dropped"`
	LastSuccess string `json:"last_success,omitempty"`
}

func Open(config Config) (*Queue, error) {
	cfg, err := ResolveConfig(config)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(cfg.SpoolDir, 0700); err != nil {
		return nil, fmt.Errorf("create usage spool: %w", err)
	}
	// MkdirAll leaves an existing directory's permissions unchanged.
	if err := os.Chmod(cfg.SpoolDir, 0700); err != nil {
		return nil, fmt.Errorf("secure usage spool directory: %w", err)
	}
	lock, err := lockSpool(filepath.Join(cfg.SpoolDir, "writer.lock"))
	if err != nil {
		return nil, err
	}
	path, err := filepath.Abs(filepath.Join(cfg.SpoolDir, "outbox.db"))
	if err != nil {
		_ = lock.Close()
		return nil, err
	}
	if err := secureSpoolFiles(path); err != nil {
		_ = lock.Close()
		return nil, err
	}
	u := url.URL{Scheme: "file", Path: path}
	v := url.Values{}
	v.Set("_journal_mode", "WAL")
	v.Set("_synchronous", "FULL")
	v.Set("_busy_timeout", "5000")
	u.RawQuery = v.Encode()
	db, err := sql.Open("sqlite3", u.String())
	if err != nil {
		_ = lock.Close()
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	q := &Queue{cfg: cfg, db: db, lock: lock, wake: make(chan struct{}, 1), done: make(chan struct{}), client: &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
	fail := func(err error) (*Queue, error) { _ = db.Close(); _ = lock.Close(); return nil, err }
	var mode string
	var synchronous int
	if err := db.QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil {
		return fail(err)
	}
	if err := db.QueryRow("PRAGMA synchronous").Scan(&synchronous); err != nil {
		return fail(err)
	}
	if mode != "wal" || synchronous != 2 {
		return fail(fmt.Errorf("usage spool requires WAL and synchronous=FULL"))
	}
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS outbox (id INTEGER PRIMARY KEY, event_id TEXT NOT NULL UNIQUE, payload BLOB NOT NULL, size INTEGER NOT NULL, status TEXT NOT NULL DEFAULT 'pending', error_code TEXT NOT NULL DEFAULT ''); CREATE INDEX IF NOT EXISTS idx_outbox_status_id ON outbox(status,id); CREATE TABLE IF NOT EXISTS state (id INTEGER PRIMARY KEY CHECK(id=1), source_id TEXT NOT NULL, endpoint TEXT NOT NULL, queued_bytes INTEGER NOT NULL DEFAULT 0, dropped INTEGER NOT NULL DEFAULT 0, last_success TEXT NOT NULL DEFAULT '');`); err != nil {
		return fail(err)
	}
	// The spool stays bound to one Keeper endpoint so pending rows cannot be redirected. The legacy
	// source_id column is kept for older spools but no longer checked: Keeper derives the source
	// from the token, and rotating the token must keep delivering the same pending records.
	if _, err := db.Exec("INSERT OR IGNORE INTO state(id,source_id,endpoint) VALUES(1,'',?)", cfg.Endpoint); err != nil {
		return fail(err)
	}
	var endpoint string
	var dropped uint64
	if err := db.QueryRow("SELECT endpoint,dropped FROM state WHERE id=1").Scan(&endpoint, &dropped); err != nil {
		return fail(err)
	}
	if endpoint != cfg.Endpoint {
		return fail(fmt.Errorf("spool belongs to another Keeper endpoint; use its original endpoint or a separate spool_dir"))
	}
	q.dropped.Store(dropped)
	return q, nil
}

func (q *Queue) Enqueue(r UsageRecord) error {
	if r.RequestID == "" {
		return q.drop(fmt.Errorf("usage record lacks an execution ID"))
	}
	if err := canonicalAccounting(&r); err != nil {
		return q.drop(err)
	}
	normalizeOutcome(&r)
	raw, err := payload(r)
	if err != nil {
		return q.drop(fmt.Errorf("encode usage record"))
	}
	id, err := newID()
	if err != nil {
		return q.drop(err)
	}
	tx, err := q.db.Begin()
	if err != nil {
		return q.drop(err)
	}
	defer func() { _ = tx.Rollback() }()
	var size int64
	if err := tx.QueryRow("SELECT queued_bytes FROM state WHERE id=1").Scan(&size); err != nil {
		return q.dropAfterRollback(tx, err)
	}
	if size+int64(len(raw)) > q.cfg.MaxSpoolBytes {
		return q.dropAfterRollback(tx, ErrQueueFull)
	}
	status, code := "pending", ""
	if len(raw) > MaxBatchBytes-4096 {
		status, code = "quarantined", codeEventTooLarge
	}
	if _, err := tx.Exec("INSERT INTO outbox(event_id,payload,size,status,error_code) VALUES(?,?,?,?,?)", id, raw, len(raw), status, code); err != nil {
		return q.dropAfterRollback(tx, err)
	}
	if _, err := tx.Exec("UPDATE state SET queued_bytes=queued_bytes+? WHERE id=1", len(raw)); err != nil {
		return q.dropAfterRollback(tx, err)
	}
	if err := tx.Commit(); err != nil {
		return q.drop(err)
	}
	if status == "pending" && q.enqueuedSinceWake.Add(1)%MaxBatchEvents == 0 {
		select {
		case q.wake <- struct{}{}:
		default:
		}
	}
	return nil
}

func (q *Queue) dropAfterRollback(tx *sql.Tx, err error) error { _ = tx.Rollback(); return q.drop(err) }
func (q *Queue) drop(err error) error {
	n := q.dropped.Add(1)
	// SQLite may be unwritable. The counter and CPA's log still expose new losses.
	_, _ = q.db.Exec("UPDATE state SET dropped=? WHERE id=1", n)
	// A full or failing disk drops every record. Logging each one would flood CPA's log, which
	// often shares that disk, so log the first drop and then at most once per interval.
	q.dropLog.Lock()
	defer q.dropLog.Unlock()
	q.dropLog.unlogged++
	if !q.dropLog.last.IsZero() && time.Since(q.dropLog.last) < dropLogInterval {
		return err
	}
	log.WithError(err).WithFields(log.Fields{"dropped": q.dropped.Load(), "dropped_since_last_log": q.dropLog.unlogged}).Error("keeper usage push: local persistence failed")
	q.dropLog.last, q.dropLog.unlogged = time.Now(), 0
	return err
}

func newID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = (b[6] & 15) | 64
	b[8] = (b[8] & 63) | 128
	return hex.EncodeToString(b[:]), nil
}

func (q *Queue) Stats() (Stats, error) {
	s := Stats{Dropped: q.dropped.Load()}
	err := q.db.QueryRow("SELECT (SELECT COUNT(*) FROM outbox WHERE status='pending'),(SELECT COUNT(*) FROM outbox WHERE status='quarantined'),queued_bytes,last_success FROM state WHERE id=1").Scan(&s.Pending, &s.Quarantined, &s.Bytes, &s.LastSuccess)
	return s, err
}

func (q *Queue) batch() ([]Event, error) {
	for {
		events, oversized, err := q.readBatch()
		if err != nil || oversized == "" {
			return events, err
		}
		// A row that fits no batch would otherwise end every flush early and stall the rows behind it.
		if err := q.quarantine([]Event{{ID: oversized}}, codeEventTooLarge); err != nil {
			return nil, err
		}
	}
}

// readBatch returns the next batch, or the ID of a leading row that cannot fit any batch.
// The single connection must be released before the caller writes.
func (q *Queue) readBatch() ([]Event, string, error) {
	rows, err := q.db.Query("SELECT event_id,payload FROM outbox WHERE status='pending' ORDER BY id LIMIT ?", MaxBatchEvents)
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = rows.Close() }()
	events := make([]Event, 0, MaxBatchEvents)
	// Envelope overhead estimate; it still over-counts so the request stays under Keeper's limit.
	size := 256
	for rows.Next() {
		var e Event
		if err := rows.Scan(&e.ID, &e.Usage); err != nil {
			return nil, "", err
		}
		size += len(e.Usage) + len(e.ID) + 64
		if size > MaxBatchBytes {
			if len(events) == 0 {
				return nil, e.ID, rows.Err()
			}
			break
		}
		events = append(events, e)
	}
	return events, "", rows.Err()
}

func (q *Queue) Start() {
	q.startOnce.Do(func() { ctx, cancel := context.WithCancel(context.Background()); q.cancel = cancel; go q.run(ctx) })
}
func (q *Queue) run(ctx context.Context) {
	defer close(q.done)
	tick := time.NewTicker(q.cfg.FlushInterval)
	defer tick.Stop()
	backoff := time.Second
	for {
		for {
			count, err := q.Flush(ctx)
			if ctx.Err() != nil {
				return
			}
			if err != nil {
				log.WithError(err).Warn("keeper usage push: upload deferred; durable records retained")
				jitter := make([]byte, 1)
				_, _ = rand.Read(jitter)
				delay := retryDelay(backoff, jitter[0])
				timer := time.NewTimer(delay)
				select {
				case <-ctx.Done():
					timer.Stop()
					return
				case <-timer.C:
				}
				backoff = min(backoff*2, 60*time.Second)
				continue
			}
			backoff = time.Second
			if count == 0 {
				break
			}
		}
		if s, err := q.Stats(); err == nil {
			q.logStats(s)
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		case <-q.wake:
		}
	}
}

// Flush sends one batch, and is also the deterministic recovery/test boundary.
func (q *Queue) Flush(ctx context.Context) (int, error) {
	events, err := q.batch()
	if err != nil || len(events) == 0 {
		return 0, err
	}
	return len(events), q.deliver(ctx, events)
}

func (q *Queue) Close() error {
	var err error
	q.closeOnce.Do(func() {
		q.startOnce.Do(func() { close(q.done) })
		if q.cancel != nil {
			q.cancel()
		}
		<-q.done
		err = errors.Join(q.db.Close(), q.lock.Close())
	})
	return err
}

// logStats keeps idle cycles quiet and warns when records are quarantined. Drops are reported
// by drop() at a limited rate; warning here too would repeat them on every flush.
func (q *Queue) logStats(s Stats) {
	entry := log.WithFields(log.Fields{"pending": s.Pending, "quarantined": s.Quarantined, "bytes": s.Bytes, "dropped": s.Dropped, "last_success": s.LastSuccess})
	if s.Quarantined > q.lastStats.Quarantined {
		entry.Warn("keeper usage push: records were quarantined; inspect the local spool")
	} else {
		entry.Debug("keeper usage push queue")
	}
	q.lastStats = s
}

// Quarantined lists the first retained records for the local operator. Never expose this via CPA management.
func (q *Queue) Quarantined() ([]QuarantinedEvent, error) {
	rows, err := q.db.Query("SELECT event_id,error_code FROM outbox WHERE status='quarantined' ORDER BY id LIMIT 100")
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var result []QuarantinedEvent
	for rows.Next() {
		var e QuarantinedEvent
		if err := rows.Scan(&e.ID, &e.Code); err != nil {
			return nil, err
		}
		result = append(result, e)
	}
	return result, rows.Err()
}

// QuarantineCodes counts every retained record by its rejection code.
func (q *Queue) QuarantineCodes() (map[string]int64, error) {
	rows, err := q.db.Query("SELECT error_code,COUNT(*) FROM outbox WHERE status='quarantined' GROUP BY error_code")
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	result := map[string]int64{}
	for rows.Next() {
		var code string
		var n int64
		if err := rows.Scan(&code, &n); err != nil {
			return nil, err
		}
		result[code] = n
	}
	return result, rows.Err()
}

func (q *Queue) Replay(id string) error {
	result, err := q.db.Exec("UPDATE outbox SET status='pending',error_code='' WHERE event_id=? AND status='quarantined' AND error_code<>?", id, codeEventTooLarge)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err == nil && n == 0 {
		return fmt.Errorf("quarantined event not found, or it is too large to upload")
	}
	return err
}

// ReplayCode requeues every record quarantined with code, e.g. after a receiver rollback was fixed.
func (q *Queue) ReplayCode(code string) (int64, error) {
	if code == codeEventTooLarge {
		return 0, fmt.Errorf("%s records exceed the upload batch limit and cannot be replayed", codeEventTooLarge)
	}
	result, err := q.db.Exec("UPDATE outbox SET status='pending',error_code='' WHERE status='quarantined' AND error_code=?", code)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

func (q *Queue) quarantine(events []Event, code string) error {
	tx, err := q.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, e := range events {
		if _, err := tx.Exec("UPDATE outbox SET status='quarantined',error_code=? WHERE event_id=?", code, e.ID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (q *Queue) acknowledge(events []Event) error {
	tx, err := q.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, e := range events {
		if _, err := tx.Exec("UPDATE state SET queued_bytes=queued_bytes-COALESCE((SELECT size FROM outbox WHERE event_id=?),0) WHERE id=1", e.ID); err != nil {
			return err
		}
		if _, err := tx.Exec("DELETE FROM outbox WHERE event_id=?", e.ID); err != nil {
			return err
		}
	}
	if _, err := tx.Exec("UPDATE state SET last_success=? WHERE id=1", time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		return err
	}
	return tx.Commit()
}

func (q *Queue) MarshalBatch(events []Event) ([]byte, error) {
	return json.Marshal(Batch{SchemaVersion: SchemaVersion, Events: events})
}

// retryDelay keeps jitter within the documented one-minute retry ceiling.
func retryDelay(backoff time.Duration, entropy byte) time.Duration {
	return min(backoff+time.Duration(entropy)*backoff/512, 60*time.Second)
}
