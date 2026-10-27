package main

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brooke1220/keeper-usage-push/push"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	log "github.com/sirupsen/logrus"
	logtest "github.com/sirupsen/logrus/hooks/test"
	"gopkg.in/yaml.v3"
)

func lifecyclePayload(t *testing.T, config push.Config) []byte {
	t.Helper()
	raw, err := yaml.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(struct {
		ConfigYAML []byte `json:"config_yaml"`
	}{raw})
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func rawLifecyclePayload(t *testing.T, configYAML string) []byte {
	t.Helper()
	encoded, err := json.Marshal(struct {
		ConfigYAML []byte `json:"config_yaml"`
	}{[]byte(configYAML)})
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

// captureLogs records plugin log entries for the test instead of writing them.
func captureLogs(t *testing.T) *logtest.Hook {
	t.Helper()
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
	return hook
}

func countLogs(hook *logtest.Hook, level log.Level, message string) int {
	n := 0
	for _, entry := range hook.AllEntries() {
		if entry.Level == level && strings.Contains(entry.Message, message) {
			n++
		}
	}
	return n
}

// mustRegister asserts a reply the host accepts: an error, or a reply without metadata and
// capabilities, drops the plugin from the runtime.
func mustRegister(t *testing.T, method string, payload []byte) {
	t.Helper()
	result, err := handle(method, payload)
	if err != nil {
		t.Fatalf("lifecycle call failed, so the host would drop the plugin: %v", err)
	}
	raw, errMarshal := json.Marshal(result)
	if errMarshal != nil {
		t.Fatal(errMarshal)
	}
	var reg struct {
		Metadata struct {
			Name, Version, Author, GitHubRepository string
		} `json:"metadata"`
		Capabilities map[string]bool `json:"capabilities"`
	}
	if errDecode := json.Unmarshal(raw, &reg); errDecode != nil || reg.Metadata.Name == "" || reg.Metadata.Version == "" || reg.Metadata.Author == "" || reg.Metadata.GitHubRepository == "" || !reg.Capabilities["usage_plugin"] {
		t.Fatalf("registration the host would reject: %s %v", raw, errDecode)
	}
}

func TestRejectedSettingsKeepPreviousSpoolRegistered(t *testing.T) {
	defer cliproxyPluginShutdown()
	hook := captureLogs(t)
	config := push.Config{Endpoint: "https://keeper.invalid/api/v1/ingest/usage", Token: strings.Repeat("s", 32), SpoolDir: t.TempDir()}
	if _, err := handle(pluginabi.MethodPluginRegister, lifecyclePayload(t, config)); err != nil {
		t.Fatal(err)
	}
	original := queue
	short := config
	short.Token = "short"
	moved := config
	moved.Endpoint = "https://other-keeper.invalid/api/v1/ingest/usage"
	blocker := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(blocker, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	unwritable := config
	unwritable.SpoolDir = filepath.Join(blocker, "queue")
	for _, tc := range []struct {
		name    string
		payload []byte
		same    bool
	}{
		{"short token", lifecyclePayload(t, short), true},
		{"bare number duration", rawLifecyclePayload(t, "endpoint: "+config.Endpoint+"\ntoken: "+config.Token+"\nflush_interval: 5\n"), true},
		{"endpoint change for live spool", lifecyclePayload(t, moved), true},
		// An unusable replacement directory must not interrupt the previous spool.
		{"unwritable spool", lifecyclePayload(t, unwritable), true},
	} {
		mustRegister(t, pluginabi.MethodPluginReconfigure, tc.payload)
		if queue == nil || (tc.same && queue != original) {
			t.Fatalf("%s: previous spool is no longer live", tc.name)
		}
		if _, err := handle(pluginabi.MethodUsageHandle, []byte(`{"RequestID":"exec-after-rejection"}`)); err != nil {
			t.Fatalf("%s: usage stopped being recorded: %v", tc.name, err)
		}
	}
	if stats, err := queue.Stats(); err != nil || stats.Pending != 4 {
		t.Fatalf("usage recorded with previous settings: %+v %v", stats, err)
	}
	if n := countLogs(hook, log.ErrorLevel, "still uploading with the previous settings"); n != 4 {
		t.Fatalf("expected one error log per rejection, got %d", n)
	}
	for _, entry := range hook.AllEntries() {
		if line, _ := entry.String(); strings.Contains(line, config.Token) {
			t.Fatalf("log leaked the token: %s", line)
		}
	}
	rotated := config
	rotated.Token = strings.Repeat("r", 32)
	mustRegister(t, pluginabi.MethodPluginReconfigure, lifecyclePayload(t, rotated))
	if currentConfig.Token != rotated.Token || rejectedSettings != "" {
		t.Fatal("accepted settings did not replace the previous ones")
	}
	if countLogs(hook, log.InfoLevel, "settings accepted") != 1 {
		t.Fatal("recovery from rejected settings was not logged")
	}
}

func TestRepeatedRejectionLogsOnce(t *testing.T) {
	defer cliproxyPluginShutdown()
	hook := captureLogs(t)
	config := push.Config{Endpoint: "https://keeper.invalid/api/v1/ingest/usage", Token: strings.Repeat("s", 32), SpoolDir: t.TempDir()}
	if _, err := handle(pluginabi.MethodPluginRegister, lifecyclePayload(t, config)); err != nil {
		t.Fatal(err)
	}
	invalid := config
	invalid.Token = "short"
	// The host reconfigures every plugin on each CPA config reload.
	for i := 0; i < 3; i++ {
		mustRegister(t, pluginabi.MethodPluginReconfigure, lifecyclePayload(t, invalid))
	}
	if n := countLogs(hook, log.ErrorLevel, "settings rejected"); n != 1 {
		t.Fatalf("repeated rejection logged %d times", n)
	}
}

func TestRejectedEditsWithoutPreviousSettingsStayRegistered(t *testing.T) {
	defer cliproxyPluginShutdown()
	hook := captureLogs(t)
	for _, tc := range []struct {
		name    string
		payload []byte
	}{
		{"endpoint only", lifecyclePayload(t, push.Config{Endpoint: "https://keeper.invalid/api/v1/ingest/usage"})},
		{"token only", lifecyclePayload(t, push.Config{Token: strings.Repeat("s", 32)})},
		{"malformed yaml", rawLifecyclePayload(t, "endpoint: [unterminated\n")},
	} {
		cliproxyPluginShutdown()
		mustRegister(t, pluginabi.MethodPluginRegister, lifecyclePayload(t, push.Config{}))
		// A rejected edit to an unconfigured installation keeps its settings form available.
		mustRegister(t, pluginabi.MethodPluginReconfigure, tc.payload)
		if queue != nil {
			t.Fatalf("%s: rejected settings opened a spool", tc.name)
		}
		_, err := handle(pluginabi.MethodUsageHandle, []byte(`{"RequestID":"exec-1"}`))
		if err == nil || !strings.Contains(err.Error(), "rejected") {
			t.Fatalf("%s: usage error does not point to the rejection: %v", tc.name, err)
		}
	}
	if n := countLogs(hook, log.ErrorLevel, "usage is not recorded until they are fixed"); n != 3 {
		t.Fatalf("expected one error log per rejected first setting, got %d", n)
	}
	configured := push.Config{Endpoint: "https://keeper.invalid/api/v1/ingest/usage", Token: strings.Repeat("s", 32), SpoolDir: t.TempDir()}
	mustRegister(t, pluginabi.MethodPluginReconfigure, lifecyclePayload(t, configured))
	if queue == nil {
		t.Fatal("fixed settings did not open the spool")
	}
}

func TestFreshConfiguredImageRejectsUnusableSpool(t *testing.T) {
	defer cliproxyPluginShutdown()
	captureLogs(t)
	blocker := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(blocker, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	config := push.Config{Endpoint: "https://keeper.invalid/api/v1/ingest/usage", Token: strings.Repeat("s", 32), SpoolDir: filepath.Join(blocker, "queue")}
	if result, err := handle(pluginabi.MethodPluginRegister, lifecyclePayload(t, config)); err == nil || result != nil {
		t.Fatalf("unusable replacement registered successfully: result=%v error=%v", result, err)
	}
	if queue != nil {
		t.Fatal("unusable spool opened")
	}
	if _, err := handle(pluginabi.MethodUsageHandle, []byte(`{"RequestID":"exec-1"}`)); err == nil || !strings.Contains(err.Error(), "rejected") {
		t.Fatalf("usage error does not point to the rejection: %v", err)
	}
}

func TestFreshImageThatCannotOpenSpoolFailsRegister(t *testing.T) {
	defer cliproxyPluginShutdown()
	config := push.Config{Endpoint: "https://keeper.invalid/api/v1/ingest/usage", Token: strings.Repeat("s", 32), SpoolDir: t.TempDir()}
	// An old image without quiesce still holds the writer lock during a hot upgrade.
	holder, err := push.Open(config)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := handle(pluginabi.MethodPluginRegister, lifecyclePayload(t, config)); !errors.Is(err, push.ErrSpoolLocked) {
		t.Fatalf("register did not fail on the held lock, so the host would not roll back to the image holding it: %v", err)
	}
	if queue != nil {
		t.Fatal("failed register left a spool open")
	}
	if err := holder.Close(); err != nil {
		t.Fatal(err)
	}
	// After rollback and a later restart, the same image registers normally.
	mustRegister(t, pluginabi.MethodPluginRegister, lifecyclePayload(t, config))
	if queue == nil {
		t.Fatal("register after the lock was released did not open the spool")
	}
}

func TestUnconfiguredInstallRegistersWithSettingsForm(t *testing.T) {
	defer cliproxyPluginShutdown()
	// A fresh store install has neither endpoint nor token, and may run outside a CPA working
	// directory without ./plugins; registration must still succeed so the form is shown.
	t.Chdir(t.TempDir())
	result, err := handle(pluginabi.MethodPluginRegister, lifecyclePayload(t, push.Config{}))
	if err != nil {
		t.Fatalf("unconfigured install did not register: %v", err)
	}
	raw, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	var reg struct {
		Metadata struct {
			Version      string
			ConfigFields []struct{ Name string }
		} `json:"metadata"`
		Capabilities map[string]bool `json:"capabilities"`
	}
	if err := json.Unmarshal(raw, &reg); err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, f := range reg.Metadata.ConfigFields {
		names[f.Name] = true
	}
	if !names["endpoint"] || !names["token"] || !reg.Capabilities["usage_plugin"] || reg.Metadata.Version == "" {
		t.Fatalf("registration lacks the settings form: %s", raw)
	}
	if queue != nil {
		t.Fatal("unconfigured install opened a spool")
	}
	if _, err := handle(pluginabi.MethodUsageHandle, []byte(`{"RequestID":"exec-1"}`)); err == nil {
		t.Fatal("usage accepted without a configured spool")
	}
}

func TestSavingSettingsStartsAndClearingStopsUpload(t *testing.T) {
	defer cliproxyPluginShutdown()
	if _, err := handle(pluginabi.MethodPluginRegister, lifecyclePayload(t, push.Config{})); err != nil {
		t.Fatal(err)
	}
	configured := push.Config{Endpoint: "https://keeper.invalid/api/v1/ingest/usage", Token: strings.Repeat("s", 32), SpoolDir: t.TempDir()}
	if _, err := handle(pluginabi.MethodPluginReconfigure, lifecyclePayload(t, configured)); err != nil {
		t.Fatalf("saving settings failed: %v", err)
	}
	if queue == nil {
		t.Fatal("saving settings did not open the spool")
	}
	if _, err := handle(pluginabi.MethodPluginReconfigure, lifecyclePayload(t, push.Config{})); err != nil {
		t.Fatalf("clearing settings failed: %v", err)
	}
	if queue != nil {
		t.Fatal("clearing settings left the spool open")
	}
	// The released lock lets the same spool be configured again.
	if _, err := handle(pluginabi.MethodPluginReconfigure, lifecyclePayload(t, configured)); err != nil || queue == nil {
		t.Fatalf("reconfiguring after clearing failed: %v", err)
	}
}

func TestQuiesceHandsSpoolToReplacementAndRollbackReopens(t *testing.T) {
	defer cliproxyPluginShutdown()
	config := push.Config{Endpoint: "https://keeper.invalid/api/v1/ingest/usage", Token: strings.Repeat("s", 32), SpoolDir: t.TempDir()}
	if _, err := handle(pluginabi.MethodPluginRegister, lifecyclePayload(t, config)); err != nil {
		t.Fatal(err)
	}
	if _, err := handle(pluginabi.MethodUsageHandle, []byte(`{"RequestID":"exec-before-upgrade","Provider":"codex","ExecutorType":"CodexExecutor","Model":"m"}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := handle(pluginabi.MethodPluginQuiesce, []byte(`{}`)); err != nil {
		t.Fatalf("quiesce failed: %v", err)
	}
	if queue != nil {
		t.Fatal("quiesce kept the spool open")
	}
	// The replacement image opens the same spool while this instance is still loaded.
	replacement, err := push.Open(config)
	if err != nil {
		t.Fatalf("replacement could not take over the spool: %v", err)
	}
	stats, err := replacement.Stats()
	if err != nil || stats.Pending != 1 {
		t.Fatalf("pending record was not handed over: %+v %v", stats, err)
	}
	if _, err := handle(pluginabi.MethodUsageHandle, []byte(`{"RequestID":"exec-during-handoff"}`)); err == nil {
		t.Fatal("quiesced instance accepted usage")
	}
	if err := replacement.Close(); err != nil {
		t.Fatal(err)
	}
	// A failed replacement makes the host re-register this instance with its previous config.
	if _, err := handle(pluginabi.MethodPluginReconfigure, lifecyclePayload(t, config)); err != nil || queue == nil {
		t.Fatalf("rollback did not reopen the spool: %v", err)
	}
	if stats, err := queue.Stats(); err != nil || stats.Pending != 1 {
		t.Fatalf("rollback lost the pending record: %+v %v", stats, err)
	}
}

func TestQuiesceWithoutSpoolIsHarmless(t *testing.T) {
	defer cliproxyPluginShutdown()
	if _, err := handle(pluginabi.MethodPluginRegister, lifecyclePayload(t, push.Config{})); err != nil {
		t.Fatal(err)
	}
	if _, err := handle(pluginabi.MethodPluginQuiesce, nil); err != nil {
		t.Fatalf("quiesce of an unconfigured instance failed: %v", err)
	}
	if _, err := handle(pluginabi.MethodUsageHandle, []byte(`{"RequestID":"x"}`)); err == nil || !strings.Contains(err.Error(), "not configured") {
		t.Fatalf("unconfigured quiesce changed the usage error: %v", err)
	}
}

func TestRegistrationReportsInjectedVersion(t *testing.T) {
	previous := Version
	Version = "1.2.3"
	defer func() { Version = previous }()
	raw, err := json.Marshal(registration())
	if err != nil || !strings.Contains(string(raw), `"Version":"1.2.3"`) || !strings.Contains(string(raw), "brooke1220/keeper-usage-push") {
		t.Fatalf("registration metadata: %s %v", raw, err)
	}
}
