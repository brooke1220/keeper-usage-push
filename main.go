package main

/*
#include <stdint.h>
#include <stdlib.h>

typedef struct {
	void* ptr;
	size_t len;
} cliproxy_buffer;

typedef struct {
	uint32_t abi_version;
	void* host_ctx;
	void* call;
	void* free_buffer;
} cliproxy_host_api;

typedef int (*cliproxy_plugin_call_fn)(char*, uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_plugin_free_fn)(void*, size_t);
typedef void (*cliproxy_plugin_shutdown_fn)(void);

typedef struct {
	uint32_t abi_version;
	cliproxy_plugin_call_fn call;
	cliproxy_plugin_free_fn free_buffer;
	cliproxy_plugin_shutdown_fn shutdown;
} cliproxy_plugin_api;

extern int cliproxyPluginCall(char*, uint8_t*, size_t, cliproxy_buffer*);
extern void cliproxyPluginFree(void*, size_t);
extern void cliproxyPluginShutdown(void);
*/
import "C"

import (
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"unsafe"

	"github.com/brooke1220/keeper-usage-push/push"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	log "github.com/sirupsen/logrus"
	"gopkg.in/yaml.v3"
)

var lifecycle sync.RWMutex
var queue *push.Queue

// currentConfig holds resolved, accepted settings, including the token read from token_file.
// Rejected settings never replace this in-memory snapshot.
var currentConfig push.Config
var unconfiguredWarned bool

// quiesced marks a spool released for a replacement instance; currentConfig is kept so a
// rollback can reopen it.
var quiesced bool

// rejectedSettings is the last logged rejection, so repeated reconfigures log it once.
var rejectedSettings string

// Version is injected at release build time with -ldflags "-X main.Version=x.y.z".
var Version = "0.0.0-dev"

// configFields lets CPA's plugin page render the settings form. Only endpoint and token are
// needed to start uploading; both come from the push receiving card in Keeper's settings page.
var configFields = []pluginapi.ConfigField{
	{Name: "endpoint", Type: pluginapi.ConfigFieldTypeString, Description: "Required to upload. Upload URL shown in Keeper Settings > Push receiving, e.g. https://keeper.example.com/api/v1/ingest/usage. HTTP and HTTPS are supported without extra settings. HTTP sends the token and usage without encryption."},
	{Name: "token", Type: pluginapi.ConfigFieldTypeString, Description: "Required to upload. Token created for this CPA in Keeper Settings > Push receiving. Use token_file instead to read it from a mounted secret."},
	{Name: "token_file", Type: pluginapi.ConfigFieldTypeString, Description: "Optional. File containing the token; set either token or token_file."},
	{Name: "spool_dir", Type: pluginapi.ConfigFieldTypeString, Default: push.DefaultSpoolDir, Description: "Optional. Persistent local queue directory, relative to the CPA working directory. Defaults to the plugins directory, which the official Docker setup already persists."},
	{Name: "max_spool_bytes", Type: pluginapi.ConfigFieldTypeInteger, Default: 1 << 30, Description: "Optional. Local queue size limit in bytes; new records are counted as dropped once it is full."},
	{Name: "flush_interval", Type: pluginapi.ConfigFieldTypeString, Default: "5s", Description: "Optional. Longest wait before uploading a partial batch, as a duration with a unit such as 5s."},
	{Name: "request_timeout", Type: pluginapi.ConfigFieldTypeString, Default: "15s", Description: "Optional. Upload request timeout as a duration with a unit such as 15s; this telemetry-only timeout never affects model traffic."},
}

// registeredSchemaVersion is the oldest contract every official v8 host accepts; hosts reject
// a plugin whose schema is newer than their own. Usage observation is unchanged across these
// versions, and the plugin uses no stream, interceptor or management features they gate.
const registeredSchemaVersion = 6

type envelope struct {
	OK     bool `json:"ok"`
	Result any  `json:"result,omitempty"`
	Error  any  `json:"error,omitempty"`
}

func main() {}

//export cliproxy_plugin_init
func cliproxy_plugin_init(host *C.cliproxy_host_api, plugin *C.cliproxy_plugin_api) C.int {
	if plugin == nil {
		return 1
	}
	installHostLogger(unsafe.Pointer(host))
	usageIdentities.reset()
	plugin.abi_version = C.uint32_t(pluginabi.ABIVersion)
	plugin.call = C.cliproxy_plugin_call_fn(C.cliproxyPluginCall)
	plugin.free_buffer = C.cliproxy_plugin_free_fn(C.cliproxyPluginFree)
	plugin.shutdown = C.cliproxy_plugin_shutdown_fn(C.cliproxyPluginShutdown)
	return 0
}

//export cliproxyPluginCall
func cliproxyPluginCall(method *C.char, request *C.uint8_t, requestLen C.size_t, response *C.cliproxy_buffer) (result C.int) {
	if response != nil {
		response.ptr = nil
		response.len = 0
	}
	defer func() {
		if recover() != nil {
			writeResponse(response, envelope{Error: map[string]string{"code": "plugin_error", "message": "usage plugin call failed"}})
			result = 1
		}
	}()
	if method == nil {
		return 1
	}
	var raw []byte
	if request != nil && requestLen > 0 {
		raw = C.GoBytes(unsafe.Pointer(request), C.int(requestLen))
	}
	value, err := handle(C.GoString(method), raw)
	if err != nil {
		writeResponse(response, envelope{Error: map[string]string{"code": "plugin_error", "message": err.Error()}})
		return 1
	}
	writeResponse(response, envelope{OK: true, Result: value})
	return 0
}

//export cliproxyPluginFree
func cliproxyPluginFree(ptr unsafe.Pointer, _ C.size_t) {
	if ptr != nil {
		C.free(ptr)
	}
}

//export cliproxyPluginShutdown
func cliproxyPluginShutdown() {
	lifecycle.Lock()
	defer lifecycle.Unlock()
	if queue != nil {
		if err := queue.Close(); err != nil {
			log.Error("keeper usage push: spool close failed")
		}
		queue = nil
	}
	// The host re-runs init when the same image is enabled again; start that instance afresh.
	currentConfig, quiesced, rejectedSettings, unconfiguredWarned = push.Config{}, false, "", false
	usageIdentities.reset()
}

func handle(method string, raw []byte) (any, error) {
	switch method {
	case pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure:
		return configure(method == pluginabi.MethodPluginRegister, raw)
	case pluginabi.MethodPluginQuiesce:
		quiesce()
		return struct{}{}, nil
	case pluginabi.MethodUsageHandle:
		var r push.UsageRecord
		if err := json.Unmarshal(raw, &r); err != nil {
			return nil, fmt.Errorf("invalid usage record")
		}
		// Resolved before taking the lifecycle lock, so a host round-trip never holds up a
		// pending configure or quiesce.
		r.Identity = usageIdentities.metadata(r)
		lifecycle.RLock()
		defer lifecycle.RUnlock()
		if queue == nil {
			if quiesced {
				return nil, fmt.Errorf("usage push is handing its spool to a replacement instance")
			}
			if rejectedSettings != "" {
				return nil, fmt.Errorf("usage push settings were rejected; see CPA's log")
			}
			return nil, fmt.Errorf("usage push is not configured")
		}
		if err := queue.Enqueue(r); err != nil {
			return nil, fmt.Errorf("usage record could not be persisted; see local drop counter")
		}
		return struct{}{}, nil
	default:
		return nil, fmt.Errorf("unsupported usage plugin method")
	}
}

// configure keeps a running instance on its last accepted settings after a rejected edit.
// A fresh, configured image must fail registration if it cannot record usage, so the host
// rolls a hot upgrade back. An empty fresh install still registers to expose its settings form.
func configure(fresh bool, raw []byte) (any, error) {
	cfg, err := lifecycleConfig(raw)
	if err == nil && cfg.Unconfigured() {
		return registerUnconfigured(), nil
	}
	if err == nil {
		cfg, err = push.ResolveConfig(cfg)
	}
	lifecycle.Lock()
	defer lifecycle.Unlock()
	if err == nil {
		err = openLocked(cfg)
	}
	if err != nil {
		rejectSettingsLocked(err)
		if fresh {
			return nil, err
		}
	}
	return registration(), nil
}

func lifecycleConfig(raw []byte) (push.Config, error) {
	var req struct {
		ConfigYAML []byte `json:"config_yaml"`
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		return push.Config{}, fmt.Errorf("invalid lifecycle request")
	}
	var cfg push.Config
	if err := yaml.Unmarshal(req.ConfigYAML, &cfg); err != nil {
		// yaml.v3 shortens offending values to a few characters; the token is a string and decodes from any scalar.
		return push.Config{}, fmt.Errorf("invalid settings: %w", err)
	}
	return cfg, nil
}

// openLocked replaces the live spool with resolved cfg. A different spool is opened before
// closing the old one; a same-spool failure is restored from currentConfig by rejectSettingsLocked.
func openLocked(cfg push.Config) error {
	sameSpool := sameSpoolDirectory(cfg.SpoolDir, currentConfig.SpoolDir)
	// A quiesced spool is released but still owned by this instance until a rollback reopens it.
	if (queue != nil || quiesced) && sameSpool && cfg.Endpoint != currentConfig.Endpoint {
		return fmt.Errorf("endpoint cannot change for a live spool; use a separate spool_dir")
	}
	var next *push.Queue
	var err error
	if queue != nil && !sameSpool {
		next, err = push.Open(cfg)
		if err != nil {
			return err
		}
	}
	if queue != nil {
		errClose := queue.Close()
		queue = nil
		if errClose != nil {
			if next != nil {
				_ = next.Close()
			}
			return errClose
		}
	}
	if next == nil {
		next, err = push.Open(cfg)
		if err != nil {
			return err
		}
	}
	if rejectedSettings != "" {
		log.Info("keeper usage push: settings accepted; uploading with the new settings")
	}
	queue, currentConfig = next, cfg
	unconfiguredWarned, quiesced, rejectedSettings = false, false, ""
	queue.Start()
	return nil
}

func sameSpoolDirectory(a, b string) bool {
	if a == b {
		return true
	}
	left, errLeft := os.Stat(a)
	right, errRight := os.Stat(b)
	return errLeft == nil && errRight == nil && os.SameFile(left, right)
}

// rejectSettingsLocked keeps the last accepted settings uploading, reopening their spool after a
// failed replacement or a quiesce that is being rolled back, and logs each new rejection once.
func rejectSettingsLocked(err error) {
	if queue == nil && !currentConfig.Unconfigured() {
		restored, errOpen := push.Open(currentConfig)
		if errOpen != nil {
			log.WithError(errOpen).Error("keeper usage push: previous spool could not be reopened")
		} else {
			queue, quiesced = restored, false
			queue.Start()
		}
	}
	if err.Error() == rejectedSettings {
		return
	}
	rejectedSettings = err.Error()
	if queue != nil {
		log.WithError(err).Error("keeper usage push: settings rejected; still uploading with the previous settings")
		return
	}
	log.WithError(err).Error("keeper usage push: settings rejected; usage is not recorded until they are fixed")
}

// quiesce hands the spool to a replacement image. During a hot upgrade the host quiesces this
// instance, loads the new image in the same process and only then retires this one, so the
// spool's writer lock must already be released. Pending rows stay on disk and the new instance
// uploads them. If the replacement fails, the host re-registers this instance, which reopens it.
func quiesce() {
	lifecycle.Lock()
	defer lifecycle.Unlock()
	if queue == nil {
		return
	}
	if err := queue.Close(); err != nil {
		log.WithError(err).Error("keeper usage push: spool close failed during quiesce")
	}
	queue, quiesced = nil, true
}

func registration() map[string]any {
	return map[string]any{"schema_version": registeredSchemaVersion, "metadata": pluginapi.Metadata{Name: "keeper-usage-push", Version: Version, Author: "brooke1220", GitHubRepository: "https://github.com/brooke1220/keeper-usage-push", ConfigFields: configFields}, "capabilities": map[string]bool{"usage_plugin": true}}
}

// registerUnconfigured keeps a fresh install registered so CPA's plugin page shows the
// settings form. No spool is opened, so usage seen before setup is not counted as dropped.
func registerUnconfigured() map[string]any {
	lifecycle.Lock()
	defer lifecycle.Unlock()
	if queue != nil {
		if err := queue.Close(); err != nil {
			log.WithError(err).Error("keeper usage push: spool close failed")
		}
		queue = nil
	}
	currentConfig, quiesced, rejectedSettings = push.Config{}, false, ""
	// The host reconfigures several times while starting; warn once per unconfigured period.
	if !unconfiguredWarned {
		unconfiguredWarned = true
		log.Warn("keeper usage push: not configured; set endpoint and token in CPA's plugin settings to start uploading")
	}
	return registration()
}

func writeResponse(response *C.cliproxy_buffer, v envelope) {
	if response == nil {
		return
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return
	}
	response.ptr = C.CBytes(raw)
	response.len = C.size_t(len(raw))
}
