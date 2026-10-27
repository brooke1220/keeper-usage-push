package main

/*
#include <stdint.h>
#include <stdlib.h>

typedef struct {
	void* ptr;
	size_t len;
} keeper_host_buffer;

typedef int (*keeper_host_call_fn)(void*, const char*, const uint8_t*, size_t, keeper_host_buffer*);
typedef void (*keeper_host_free_fn)(void*, size_t);

// Field order mirrors cliproxy_host_api. The host keeps every block alive for the
// process lifetime, including after the plugin is unloaded.
typedef struct {
	uint32_t abi_version;
	void* host_ctx;
	keeper_host_call_fn call;
	keeper_host_free_fn free_buffer;
} keeper_host_api;

static int keeper_host_call(const keeper_host_api* host, const char* method, const uint8_t* request, size_t request_len, keeper_host_buffer* response) {
	if (host == NULL || host->call == NULL) {
		return 1;
	}
	return host->call(host->host_ctx, method, request, request_len, response);
}

static void keeper_host_free(const keeper_host_api* host, void* ptr, size_t len) {
	if (host != NULL && host->free_buffer != NULL && ptr != NULL) {
		host->free_buffer(ptr, len);
	}
}
*/
import "C"

import (
	"encoding/json"
	"io"
	"os"
	"sync"
	"sync/atomic"
	"unsafe"

	log "github.com/sirupsen/logrus"
)

var (
	hostAPI       atomic.Pointer[C.keeper_host_api]
	hostLogMethod = C.CString("host.log") // Allocated once for the process lifetime.
	hostLogOnce   sync.Once
	// hostLogFallback receives entries the host did not accept.
	hostLogFallback io.Writer = os.Stderr
)

// hostLogHook forwards plugin logs to CPA's logger so they reach its configured log files.
// Entries fall back to stderr while no host API is available or when the host rejects them.
type hostLogHook struct{}

func installHostLogger(host unsafe.Pointer) {
	if host == nil {
		return
	}
	// The host re-runs init when the same image is enabled again; only the API pointer changes.
	hostAPI.Store((*C.keeper_host_api)(host))
	installHostLogHook()
}

func installHostLogHook() {
	hostLogOnce.Do(func() {
		log.SetOutput(io.Discard)
		log.SetLevel(log.TraceLevel)
		log.AddHook(hostLogHook{})
	})
}

func (hostLogHook) Levels() []log.Level { return log.AllLevels }

func (hostLogHook) Fire(entry *log.Entry) error {
	fields := make(map[string]any, len(entry.Data))
	for key, value := range entry.Data {
		if err, ok := value.(error); ok {
			value = err.Error()
		}
		fields[key] = value
	}
	fields["plugin"] = "keeper-usage-push"
	raw, err := json.Marshal(struct {
		Level   string         `json:"level"`
		Message string         `json:"message"`
		Fields  map[string]any `json:"fields,omitempty"`
	}{hostLogLevel(entry.Level), entry.Message, fields})
	if err != nil || !forwardHostLog(raw) {
		line, _ := entry.String()
		_, _ = io.WriteString(hostLogFallback, line)
	}
	return nil
}

func forwardHostLog(raw []byte) bool {
	host := hostAPI.Load()
	if host == nil {
		return false
	}
	request := C.CBytes(raw)
	defer C.free(request)
	var response C.keeper_host_buffer
	rc := C.keeper_host_call(host, hostLogMethod, (*C.uint8_t)(request), C.size_t(len(raw)), &response)
	var envelope []byte
	if response.ptr != nil {
		envelope = C.GoBytes(response.ptr, C.int(response.len))
		C.keeper_host_free(host, response.ptr, response.len)
	}
	return hostLogAccepted(int(rc), envelope)
}

// hostLogAccepted reports whether the host logged the entry. The host answers a rejected call,
// e.g. from an instance it already closed before plugin shutdown, with rc 0 and an error envelope.
func hostLogAccepted(rc int, envelope []byte) bool {
	var result struct {
		OK bool `json:"ok"`
	}
	return rc == 0 && json.Unmarshal(envelope, &result) == nil && result.OK
}

func hostLogLevel(level log.Level) string {
	switch level {
	case log.PanicLevel, log.FatalLevel, log.ErrorLevel:
		return "error"
	case log.WarnLevel:
		return "warn"
	case log.InfoLevel:
		return "info"
	case log.TraceLevel:
		return "trace"
	default:
		return "debug"
	}
}
