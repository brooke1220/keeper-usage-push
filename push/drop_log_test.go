package push

import (
	"io"
	"testing"
	"time"

	log "github.com/sirupsen/logrus"
	logtest "github.com/sirupsen/logrus/hooks/test"
)

func TestPersistenceFailuresAreLoggedAtLimitedRate(t *testing.T) {
	logger := log.StandardLogger()
	hook := new(logtest.Hook)
	previousHooks := logger.ReplaceHooks(log.LevelHooks{})
	previousOut, previousLevel := logger.Out, logger.GetLevel()
	logger.AddHook(hook)
	logger.SetOutput(io.Discard)
	logger.SetLevel(log.TraceLevel)
	t.Cleanup(func() {
		logger.ReplaceHooks(previousHooks)
		logger.SetOutput(previousOut)
		logger.SetLevel(previousLevel)
	})

	cfg := configFor(t, "https://keeper.invalid")
	cfg.MaxSpoolBytes = 1
	q := openQueue(t, cfg)
	failures := func() []*log.Entry {
		var entries []*log.Entry
		for _, e := range hook.AllEntries() {
			if e.Message == "keeper usage push: local persistence failed" {
				entries = append(entries, e)
			}
		}
		return entries
	}
	for range 5 {
		if err := q.Enqueue(record(t)); err != ErrQueueFull {
			t.Fatalf("full spool accepted a record: %v", err)
		}
	}
	if got := failures(); len(got) != 1 || got[0].Data["dropped"] != uint64(1) {
		t.Fatalf("a burst of drops must log once: %v", got)
	}
	// Once the interval has passed, the next drop reports every drop since the last log.
	q.dropLog.last = time.Now().Add(-2 * dropLogInterval)
	if err := q.Enqueue(record(t)); err != ErrQueueFull {
		t.Fatalf("full spool accepted a record: %v", err)
	}
	got := failures()
	if len(got) != 2 || got[1].Data["dropped"] != uint64(6) || got[1].Data["dropped_since_last_log"] != uint64(5) {
		t.Fatalf("drop summary is wrong: %v", got)
	}
	if s, err := q.Stats(); err != nil || s.Dropped != 6 {
		t.Fatalf("every drop must still be counted: %+v %v", s, err)
	}
}
