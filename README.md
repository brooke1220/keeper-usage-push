# Keeper usage push plugin

English | [简体中文](README.zh-CN.md)

A native [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) (CPA) plugin that durably buffers usage and uploads it to a central [Keeper](https://github.com/brooke1220/cpa-usage-keeper) over HTTPS. It only observes usage; model requests never wait for network delivery.

The plugin runs on unmodified official CPA v8 releases. It is built against the official `github.com/router-for-me/CLIProxyAPI/v8` module and registers plugin schema 6, which every v8 host accepts. Verified end to end against v8.0.44.

## Install

### Plugin store

Add this repository's store source to CPA, then install **Keeper Usage Push** from the management center's plugin store:

```yaml
plugins:
  enabled: true
  dir: plugins
  store-sources:
    - "https://raw.githubusercontent.com/brooke1220/keeper-usage-push/main/registry.json"
```

CPA downloads `keeper-usage-push_<version>_<goos>_<goarch>.zip` from the latest GitHub Release, verifies it against `checksums.txt`, installs it as `plugins/<goos>/<goarch>/keeper-usage-push-v<version>` with the platform extension (`.so`, `.dylib` or `.dll`) and enables it. Store installs and upgrades apply without restarting CPA. CPA itself must reach `raw.githubusercontent.com` and GitHub Releases, directly or through its `proxy-url`.

After installing, open the plugin's settings and enter the upload URL and token (see [Configure](#configure)).

### Manual install

Download the archive for your platform from [Releases](https://github.com/brooke1220/keeper-usage-push/releases), check it against `checksums.txt`, and extract `keeper-usage-push.so` (Linux), `keeper-usage-push.dylib` (macOS) or `keeper-usage-push.dll` (Windows x64) into `plugins/<goos>/<goarch>/` under CPA's `plugins.dir`, for example `plugins/linux/amd64/` or `plugins/windows/amd64/`. With the official Docker image, the plugins directory is already a mounted volume. Restart CPA, or toggle the plugin off and on, to load a manually replaced file. Then configure it as below.

On Windows x64, use `keeper-usage-push_<version>_windows_amd64.zip`. The DLL is a CPA plugin, so install it through the plugin store or place it in the plugin directory; it is not an executable to double-click. The upload URL and token are configured in the same management-center form on every platform. Relative paths such as `plugins/keeper-usage-push-spool` work on Windows; for an absolute path in YAML, use forward slashes (`C:/CPA/plugins/keeper-usage-push-spool`) or single quotes around backslashes (`'C:\CPA\plugins\keeper-usage-push-spool'`).

## Host requirements

- **Native plugin support.** Use the official Docker image or a regular Linux/macOS/FreeBSD release archive. Unix `_no-plugin` archives are built without native plugin support. CPA v8 also has a Windows DLL loader; this project builds Windows x64 plugins and protects the spool with a Windows file lock.
- **Windows storage.** Use a local NTFS directory with Windows access-control support for the spool. Its permissions are restricted to the account running CPA and SYSTEM. Keep the same account when operating the spool tool or moving CPA to a service; changing accounts may require an administrator to adjust the directory permissions first.
- **C runtime.** Release builds for Linux are made on manylinux2014 and need only glibc 2.17, like official CPA Linux archives, so they load in the official Debian image and on older distributions. musl (Alpine) hosts are not supported.

Official hosts do not send canonical token accounting, the inbound endpoint or request-log ID, or session-tree flags. The plugin compensates where it can:

| Field | Official host | This plugin |
|---|---|---|
| Token breakdown (cache/reasoning buckets) | Not sent | Derived from the flat token counts with the CPA SDK's `EnsureTokenBreakdownForProvider`, which hosts use when an executor supplies none. Some upstream response shapes can only be classified as `unclassified`; reconcile costs against the pull channel during the pilot |
| Success status code | `0` | Set to `200`; failures without a code become `500`, as in CPA's usage queue |
| `endpoint`, `request_id` (request log), `node_kind`, `is_fork`, `is_compaction` | Not sent | Empty. Price rules or filters keyed on `endpoint` do not match |
| `service_tier`, `reasoning_effort`, `stream` | Only the values the executor recorded | Sent as received. CPA's usage queue also falls back to request context, so these can be empty here when the pull channel has them |
| Session IDs | Raw client values | Normalized to canonical UUIDs with the CPA SDK, as in CPA's usage queue |
| Failure detected only from the HTTP response status | Recorded as success | Not recoverable in the plugin |
| Drain before plugin unload | Official hosts unload plugins before stopping usage dispatch | Records still queued in CPA memory at shutdown can be lost on a normal restart |

A host that does send canonical accounting (accounting version 2 and a token breakdown) keeps its own values; the plugin never overwrites them.

## Build and package

Requirements: Go (version in `go.mod`) with CGO and a C compiler; Docker for Linux release builds. Windows x64 builds use MinGW-w64 GCC and Git Bash. Set `GOPROXY` (for example `https://goproxy.cn,direct`) when the default module proxy is unreachable.

```sh
go test ./...
go build -buildmode=c-shared -o keeper-usage-push.so .   # .dylib on macOS, .dll on Windows
go build -o keeper-push-spool ./cmd/spool
```

Release archives for the plugin store:

```sh
./scripts/package-release.sh 0.4.0                           # linux/amd64 and linux/arm64
./scripts/package-release.sh 0.4.0 linux/arm64 darwin/arm64  # selected targets
./scripts/package-release.sh 0.4.0 windows/amd64            # Git Bash + MinGW-w64 on Windows
```

Archives and `checksums.txt` are written to `dist/`: the plugin archive for the store and `keeper-push-spool_<version>_<goos>_<goarch>.zip` with the offline spool tool (`keeper-push-spool.exe` on Windows). The store installs only the plugin archive. Linux targets are built in `quay.io/pypa/manylinux2014_*` containers, and both binaries are rejected if they need glibc newer than 2.17. The plugin library is loaded once to validate the native ABI. `darwin` targets build natively and must run on macOS. Windows targets build with GCC on Windows or a MinGW-w64 cross-compiler (`CC` can override it); the DLL is loaded and its lifecycle checked when packaging on Windows. The version is embedded with `-X main.Version`; the host logs `pluginhost: plugin registered ... version=<version>`.

Pushing a `v*` tag runs `.github/workflows/release.yml`, which builds linux/amd64, linux/arm64, darwin/arm64, darwin/amd64 and windows/amd64 and publishes them with one combined `checksums.txt` as a GitHub Release. Windows CI runs the tests with the race detector and checks native DLL loading. The store installs the latest release, so the tag (without `v`) is the plugin version.

To build against a different official CPA release, run `go get github.com/router-for-me/CLIProxyAPI/v8@vX.Y.Z && go mod tidy`.

### Upgrades and removal

The plugin store upgrades a running plugin in place: CPA quiesces the old instance, loads the new version and retires the old one. On quiesce the plugin closes its spool and releases the writer lock, so the new version takes over pending records without loss. A configured replacement must open its spool successfully before registration succeeds. If it cannot, including when the saved settings are invalid, CPA re-registers the old instance and it reopens the spool using its last accepted settings. Usage recorded during the brief handoff is not stored.

Versions before 0.5.0 do not release the spool on quiesce. Upgrading from them in place fails safely: the new version refuses to start without the spool lock, CPA keeps the old version running and logs `usage spool already has a writer`. Restart CPA once to finish that upgrade; later upgrades apply in place.

Uninstalling from the management center removes the active plugin file and its settings but keeps the spool directory, so records not yet uploaded survive a reinstall. Older `keeper-usage-push-v<version>` files left by upgrades are not removed. Delete `plugins/keeper-usage-push-spool` yourself only when its pending records are no longer needed.

## Configure

In Keeper, open **Settings → Push receiving**, turn on receiving, and create a token for this CPA. Keeper shows the token once and the upload URL on the same card. Keeper derives the CPA source from the token, so the plugin needs no source ID. The server's `docs/usage-push.md` and `docs/usage-push.openapi.json` describe receiver deployment and protocol version 1.

Upgrade Keeper before this plugin. Keeper rejects unknown payload fields, so a newer plugin sending to an older Keeper gets its affected events quarantined locally as `http_422_invalid_usage_event`. After upgrading Keeper, requeue them with `--retry-code` (see [Inspect local quarantine](#inspect-local-quarantine)).

Only `endpoint` and `token` are required. The easiest way to set them is CPA's management center: open **Plugins**, choose the settings button on `keeper-usage-push`, paste the upload URL and token, and save. CPA reconfigures the plugin immediately; no restart is needed.

A freshly installed plugin without `endpoint` and `token` still registers, so the settings form is available. It logs `keeper usage push: not configured` once, opens no local queue and uploads nothing; usage seen before setup is not recorded and not counted as dropped. Clearing both stops uploading and releases the queue.

The management center saves settings without showing whether the plugin accepted them; check CPA's log after saving. Rejected edits, such as only one of `endpoint` and `token`, a short token, a duration without a unit (`flush_interval: 5`) or an unusable `spool_dir`, are logged once at error level as `keeper usage push: settings rejected`, with the reason. An already registered plugin keeps its form available and keeps uploading with its last accepted settings. An unconfigured installation records nothing until the settings are fixed. Saving valid settings after a rejection logs `settings accepted`.

A newly loaded image with nonempty, invalid settings or an unusable spool fails registration, so a hot upgrade can roll back instead of replacing a working instance with one that records nothing. On a CPA restart there is no previous instance to restore: fix the saved plugin settings in `config.yaml` and reload CPA. Empty settings still allow a fresh installation to register and show its form.

The same settings in `config.yaml`:

```yaml
plugins:
  enabled: true
  dir: plugins
  configs:
    keeper-usage-push:
      enabled: true
      endpoint: https://keeper.example.com/api/v1/ingest/usage
      token: paste-the-token-from-keeper
```

Optional fields: `token_file` (read the token from a mounted secret instead of `token`; never set both), `spool_dir`, `max_spool_bytes` (default 1 GiB), `flush_interval` (5s) and `request_timeout` (15s). A token must contain at least 32 characters without whitespace. HTTPS certificates are verified. HTTP is only supported for literal loopback addresses when `allow_http_loopback: true` is explicitly set for local testing. Redirects are not followed.

An accepted configuration keeps the resolved token in memory, so a rejected edit or upgrade rollback can restore it even if `token_file` has become unavailable. The snapshot is not written to disk. A successful configuration reload reads the file again; a CPA restart also requires the configured token file to be readable.

`spool_dir` defaults to `plugins/keeper-usage-push-spool` under the CPA working directory. The official Docker Compose file already mounts `plugins` as a persistent volume, so no extra mount is needed. If `plugins.dir` points elsewhere or CPA runs from another directory, set `spool_dir` to a persistent path; without it the plugin refuses to start instead of falling back to a temporary location.

Use a dedicated spool directory: on Unix the plugin enforces directory mode `0700` and database, WAL and SHM modes `0600`, including files left by older versions. On Windows it sets a protected directory DACL for the CPA process account and SYSTEM, lets new sidecars inherit that access, and repairs existing database and sidecar permissions. Files are secured before SQLite opens them because the spool contains full client Keys. When switching to a different spool directory, the replacement is opened before the old queue is closed; a rejected replacement leaves the old queue running.

The spool is bound to its exact endpoint; change `spool_dir` together with `endpoint`. Token rotation keeps the spool and pending delivery IDs, and Keeper maps the new token to the same source. Do not copy a spool to another installation: records uploaded again with a different token are counted under that token's source. Clear the spool when cloning a CPA. Only one CPA or spool operator may own the queue at once.

## Delivery behavior

- A callback writes an immutable, independently identified event to SQLite WAL with `synchronous=FULL`. The upload worker never holds a transaction while waiting for HTTP.
- Uploads contain at most 100 events / 1 MiB and normally flush within five seconds. Each upload request has a 15-second timeout; it applies only to this telemetry upload, never to model traffic. Errors retain data and retry with exponential backoff and jitter, capped at 60 seconds.
- Only a complete, valid Keeper acknowledgement deletes rows. Process restarts, lost responses and re-batching preserve delivery IDs. Reusing a delivery ID with changed content is a protocol conflict.
- A batch is split to isolate bad records in persistent local quarantine only when Keeper itself rejects it: 400 `invalid_batch`, 409 `delivery_id_conflict`, 413 `batch_too_large` or 422 `invalid_usage_event`. The same status without Keeper's error code (a proxy, WAF or older receiver) keeps the batch pending; a bare 413 is split but never quarantined. Authentication, redirects, network and server failures keep retrying.
- A row that can never fit a batch is quarantined as `event_too_large` instead of blocking the rows behind it, and cannot be replayed.
- The spool limit counts retained payload bytes, including quarantine. Allocate extra disk for SQLite pages, metadata and WAL. Exhaustion preserves existing records, counts every new persistence failure and keeps model service available. The first failure is logged at once, then at most one log per minute with the total and the number dropped since the last log, so a full disk does not flood CPA's log. An unwritable disk cannot persist its own new drop counter; CPA's log and the in-memory count expose the problem.
- Records still waiting in CPA memory can be lost on abrupt termination, and on official hosts also during a normal restart, because they unload plugins before stopping usage dispatch. The plugin never waits for remote delivery at shutdown. Permanent disk loss and coordinated restoration of old central backups are outside the delivery guarantee.
- Logs go to CPA's logger through the host log callback. Idle queue status is logged at debug level; new quarantines are logged as warnings, and drops as rate-limited errors. Upload errors keep their transport cause (DNS, TLS, refused connection) and never contain the push token.

The payload whitelists statistical fields. It excludes prompts, responses, failure bodies, upstream tokens and arbitrary headers. Full client Keys are intentionally transmitted over HTTPS so Keeper can retain its existing Key grouping and masked display behavior. Fees are calculated centrally.

## Inspect local quarantine

Download `keeper-push-spool_<version>_<goos>_<goarch>.zip` from the same release as the plugin and check it against `checksums.txt`. Stop CPA. Save the plugin-owned configuration (the fields below `keeper-usage-push`, without `enabled` or the wrapper) as a private YAML file. Inspection never uploads data or prints Keys/payloads:

```sh
keeper-push-spool --config /etc/cpa/usage-push.yaml
keeper-push-spool --config /etc/cpa/usage-push.yaml --retry DELIVERY_EVENT_ID
keeper-push-spool --config /etc/cpa/usage-push.yaml --retry-code http_422_invalid_usage_event
```

On Windows, run the same commands with `.\keeper-push-spool.exe --config 'C:\CPA\usage-push.yaml'` from PowerShell. Run it as the same Windows account as CPA so it can access the private spool.

The output counts every quarantined record by error code and lists the first 100 IDs. `--retry-code` requeues all records with one code, for example after fixing a receiver rollback. Fix the cause before retrying. Restart CPA to resume uploads. Keeper's separate processing quarantine is reviewed through its administrator status panel.

## Verification

`go test ./...` uses real local HTTPS servers for retry, lost response, bad ACK, timeout, quarantine, privacy, queue ownership and connection durability cases. It also decodes a record in the exact shape an official host sends. Tests require permission to listen on loopback ports.

The plugin was also loaded into a CPA built from the official v8.0.44 source and exercised through an OpenAI-compatible mock upstream: successful, failed and streaming requests reached Keeper with derived accounting, a Keeper outage was retained and replayed, a graceful CPA restart reopened the spool, and no upstream key reached Keeper. Through a local plugin-store source, the same host installed the plugin, enabled it, accepted settings from the management API, hot-upgraded it with three pending records handed to the new version and later delivered, and uninstalled it. On the same host, rejected settings left the plugin registered and uploading with its previous settings, and an in-place upgrade from a version without quiesce rolled back to the running version.

The repository also includes a local Postman simulation for a receiver that first responds with 503 and then acknowledges the same delivery ID:

```sh
# Repository root, terminal 1
postman simulation run test/postman/simulations/usage-push.sim.yaml --port 4907 --no-history
# Repository root, terminal 2
KEEPER_PUSH_SIMULATION_ENDPOINT=http://127.0.0.1:4907/keeper/api/v1/ingest/usage \
  go test ./push -run TestPostmanSimulationTransport -count=1 -v
```

Restart the simulation before rerunning this scenario. No cloud publication or account credentials are needed.

The plugin intentionally omits CPA's legacy `UsageRecord.Source`: for API Key credentials that field can contain the upstream key itself. The upload token identifies the CPA installation; tokens, pricing and client API Key grouping do not depend on the legacy source label.
