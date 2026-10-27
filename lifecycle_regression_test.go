package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brooke1220/keeper-usage-push/push"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
)

func TestFreshConfiguredImageRejectsInvalidSettings(t *testing.T) {
	defer cliproxyPluginShutdown()
	captureLogs(t)
	for _, raw := range [][]byte{
		lifecyclePayload(t, push.Config{Endpoint: "https://keeper.invalid", Token: "short"}),
		lifecyclePayload(t, push.Config{Endpoint: "https://keeper.invalid"}),
		rawLifecyclePayload(t, "endpoint: [unterminated\n"),
		[]byte("{"),
	} {
		cliproxyPluginShutdown()
		result, err := handle(pluginabi.MethodPluginRegister, raw)
		if err == nil || result != nil || queue != nil {
			t.Fatalf("invalid configured image reported ready: result=%v error=%v", result, err)
		}
	}
}

func TestTokenFileSnapshotRestoresQuiescedSpool(t *testing.T) {
	defer cliproxyPluginShutdown()
	captureLogs(t)
	secret := filepath.Join(t.TempDir(), "token")
	token := strings.Repeat("s", 32)
	if err := os.WriteFile(secret, []byte(token), 0600); err != nil {
		t.Fatal(err)
	}
	cfg := push.Config{Endpoint: "https://keeper.invalid", TokenFile: secret, SpoolDir: t.TempDir()}
	mustRegister(t, pluginabi.MethodPluginRegister, lifecyclePayload(t, cfg))
	if currentConfig.Token != token || currentConfig.TokenFile != "" || !filepath.IsAbs(currentConfig.SpoolDir) {
		t.Fatal("accepted settings are not an independent, resolved snapshot")
	}
	if _, err := handle(pluginabi.MethodUsageHandle, []byte(`{"RequestID":"before-quiesce"}`)); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(secret); err != nil {
		t.Fatal(err)
	}
	quiesce()
	// The persisted token_file is now unreadable, but rollback has the accepted token in memory.
	mustRegister(t, pluginabi.MethodPluginReconfigure, lifecyclePayload(t, cfg))
	if queue == nil || quiesced {
		t.Fatal("rollback could not restore the previously accepted token")
	}
	if stats, err := queue.Stats(); err != nil || stats.Pending != 1 {
		t.Fatalf("rollback lost pending usage: %+v %v", stats, err)
	}
	if _, err := handle(pluginabi.MethodUsageHandle, []byte(`{"RequestID":"after-rollback"}`)); err != nil {
		t.Fatal(err)
	}
}

func TestFailedSpoolSwitchKeepsTokenFileQueue(t *testing.T) {
	defer cliproxyPluginShutdown()
	captureLogs(t)
	secret := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(secret, []byte(strings.Repeat("s", 32)), 0600); err != nil {
		t.Fatal(err)
	}
	cfg := push.Config{Endpoint: "https://keeper.invalid", TokenFile: secret, SpoolDir: t.TempDir()}
	mustRegister(t, pluginabi.MethodPluginRegister, lifecyclePayload(t, cfg))
	original := queue
	if err := os.Remove(secret); err != nil {
		t.Fatal(err)
	}
	blocker := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(blocker, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	next := cfg
	next.TokenFile, next.Token = "", strings.Repeat("r", 32)
	next.SpoolDir = filepath.Join(blocker, "queue")
	mustRegister(t, pluginabi.MethodPluginReconfigure, lifecyclePayload(t, next))
	if queue != original {
		t.Fatal("failed replacement interrupted the original queue")
	}
	if _, err := handle(pluginabi.MethodUsageHandle, []byte(`{"RequestID":"after-rejected-switch"}`)); err != nil {
		t.Fatal(err)
	}
}

func TestSpoolSwitchPreservesOldPendingRecords(t *testing.T) {
	defer cliproxyPluginShutdown()
	captureLogs(t)
	cfg := push.Config{Endpoint: "https://keeper.invalid", Token: strings.Repeat("s", 32), SpoolDir: t.TempDir()}
	mustRegister(t, pluginabi.MethodPluginRegister, lifecyclePayload(t, cfg))
	if _, err := handle(pluginabi.MethodUsageHandle, []byte(`{"RequestID":"old-spool"}`)); err != nil {
		t.Fatal(err)
	}
	next := cfg
	next.Endpoint, next.SpoolDir = "https://other-keeper.invalid", t.TempDir()
	mustRegister(t, pluginabi.MethodPluginReconfigure, lifecyclePayload(t, next))
	if currentConfig.Endpoint != next.Endpoint {
		t.Fatal("replacement settings were not accepted")
	}
	old, err := push.Open(cfg)
	if err != nil {
		t.Fatalf("old spool lock was not released: %v", err)
	}
	defer func() { _ = old.Close() }()
	if stats, err := old.Stats(); err != nil || stats.Pending != 1 {
		t.Fatalf("switch lost the old spool's pending records: %+v %v", stats, err)
	}
}

func TestSpoolAliasPreservesEndpointBindingAndAllowsTokenRotation(t *testing.T) {
	defer cliproxyPluginShutdown()
	captureLogs(t)
	work := t.TempDir()
	t.Chdir(work)
	cfg := push.Config{Endpoint: "https://keeper.invalid", Token: strings.Repeat("s", 32), SpoolDir: "spool"}
	mustRegister(t, pluginabi.MethodPluginRegister, lifecyclePayload(t, cfg))
	if currentConfig.SpoolDir != filepath.Join(work, "spool") {
		t.Fatal("relative spool path was not resolved")
	}
	if err := os.Symlink(filepath.Join(work, "spool"), filepath.Join(work, "alias")); err != nil {
		t.Fatal(err)
	}
	alias := cfg
	alias.SpoolDir, alias.Endpoint = "alias", "https://other-keeper.invalid"
	original := queue
	mustRegister(t, pluginabi.MethodPluginReconfigure, lifecyclePayload(t, alias))
	if queue != original || currentConfig.Endpoint != cfg.Endpoint {
		t.Fatal("a spool alias bypassed endpoint binding or interrupted the queue")
	}
	alias.Endpoint, alias.Token = cfg.Endpoint, strings.Repeat("r", 32)
	mustRegister(t, pluginabi.MethodPluginReconfigure, lifecyclePayload(t, alias))
	if currentConfig.Token != alias.Token {
		t.Fatal("a spool alias prevented valid token rotation")
	}
}

func TestConfiguredReplacementFailureRollsBack(t *testing.T) {
	defer cliproxyPluginShutdown()
	captureLogs(t)
	const childSpoolEnv = "KEEPER_PUSH_TEST_REPLACEMENT_SPOOL"
	if spool := os.Getenv(childSpoolEnv); spool != "" {
		cfg := push.Config{Endpoint: "https://keeper.invalid", Token: "short", SpoolDir: spool}
		if result, err := handle(pluginabi.MethodPluginRegister, lifecyclePayload(t, cfg)); err == nil || result != nil {
			t.Fatal("a fresh replacement accepted unusable persisted settings")
		}
		return
	}
	cfg := push.Config{Endpoint: "https://keeper.invalid", Token: strings.Repeat("s", 32), SpoolDir: t.TempDir()}
	mustRegister(t, pluginabi.MethodPluginRegister, lifecyclePayload(t, cfg))
	if _, err := handle(pluginabi.MethodUsageHandle, []byte(`{"RequestID":"before-upgrade"}`)); err != nil {
		t.Fatal(err)
	}
	bad := cfg
	bad.Token = "short"
	mustRegister(t, pluginabi.MethodPluginReconfigure, lifecyclePayload(t, bad))
	quiesce()
	// A separate process gives the replacement fresh globals and the same released spool.
	child := exec.Command(os.Args[0], "-test.run=^TestConfiguredReplacementFailureRollsBack$")
	child.Env = append(os.Environ(), childSpoolEnv+"="+cfg.SpoolDir)
	if output, err := child.CombinedOutput(); err != nil {
		t.Fatalf("replacement did not reject registration: %v\n%s", err, output)
	}
	// The host now reconfigures the old instance with its persisted, rejected settings.
	mustRegister(t, pluginabi.MethodPluginReconfigure, lifecyclePayload(t, bad))
	if queue == nil || quiesced || currentConfig.Token != cfg.Token {
		t.Fatal("old instance did not resume with its accepted settings")
	}
	if stats, err := queue.Stats(); err != nil || stats.Pending != 1 {
		t.Fatalf("upgrade rollback lost pending records: %+v %v", stats, err)
	}
}
