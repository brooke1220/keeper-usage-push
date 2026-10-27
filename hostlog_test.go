package main

import (
	"bytes"
	"os"
	"strings"
	"sync"
	"testing"

	log "github.com/sirupsen/logrus"
)

func TestHostLogAcceptance(t *testing.T) {
	for _, tc := range []struct {
		name     string
		rc       int
		envelope string
		want     bool
	}{
		{"accepted", 0, `{"ok":true,"result":{}}`, true},
		{"closed-instance", 0, `{"ok":false,"error":{"code":"host_call_failed","message":"host plugin callback instance is closed"}}`, false},
		{"call-failed", 1, `{"ok":true}`, false},
		{"no-envelope", 0, ``, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := hostLogAccepted(tc.rc, []byte(tc.envelope)); got != tc.want {
				t.Fatalf("hostLogAccepted = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestHostLogHookInstallsOnceAndFallsBack(t *testing.T) {
	logger := log.StandardLogger()
	previousHooks := logger.ReplaceHooks(make(log.LevelHooks))
	previousOut, previousLevel := logger.Out, logger.GetLevel()
	var fallback bytes.Buffer
	hostLogFallback = &fallback
	t.Cleanup(func() {
		logger.ReplaceHooks(previousHooks)
		logger.SetOutput(previousOut)
		logger.SetLevel(previousLevel)
		hostLogFallback = os.Stderr
		hostLogOnce = sync.Once{}
	})
	installHostLogHook()
	installHostLogHook()
	if n := len(logger.Hooks[log.ErrorLevel]); n != 1 {
		t.Fatalf("re-initialization installed %d log hooks", n)
	}
	log.Error("keeper usage push: spool close failed")
	if got := fallback.String(); strings.Count(got, "spool close failed") != 1 {
		t.Fatalf("rejected host log was not written to the fallback exactly once: %q", got)
	}
}
