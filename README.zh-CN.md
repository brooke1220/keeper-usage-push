# Keeper 用量推送插件

[English](README.md) | 简体中文

这是一个原生 [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI)（简称 CPA）插件，用于将用量记录持久化到本地队列，再通过 HTTP 或 HTTPS 上传到统一的 [Keeper](https://github.com/brooke1220/cpa-usage-keeper) 服务。插件只负责采集用量，模型请求无需等待用量数据上传。

插件可直接运行在未经修改的官方 CPA v8 发行版上。它依赖官方 `github.com/router-for-me/CLIProxyAPI/v8` 模块，注册的插件 schema 版本为 6，所有 v8 宿主均可接受。已基于 v8.0.44 完成端到端验证。

## 安装

### 选择发布包

[Releases](https://github.com/brooke1220/keeper-usage-push/releases) 中有两类压缩包，分别是主插件和配套维护工具，并不是两个插件：

| 发布包 | 用途 | 是否需要安装 |
|---|---|---|
| `keeper-usage-push_<version>_<goos>_<goarch>.zip` | CPA 加载的主插件，负责采集用量、本地持久化、自动上传和失败重试 | 正常使用时需要安装 |
| `keeper-push-spool_<version>_<goos>_<goarch>.zip` | 独立运行的命令行维护工具，用于检查本地队列及被隔离的异常记录，并将指定记录重新入队 | 可选，排查问题时再下载 |

日常使用只需安装 `keeper-usage-push`，无需另行启动 `keeper-push-spool`。插件商店也只会安装主插件。维护工具的使用方法见[检查本地隔离记录](#检查本地隔离记录)。

### 插件商店安装

将本仓库的插件商店源添加到 CPA，然后在管理中心的插件商店中安装 **Keeper Usage Push**：

```yaml
plugins:
  enabled: true
  dir: plugins
  store-sources:
    - "https://raw.githubusercontent.com/brooke1220/keeper-usage-push/main/registry.json"
```

CPA 会从最新的 GitHub Release 下载 `keeper-usage-push_<version>_<goos>_<goarch>.zip`，使用 `checksums.txt` 校验后，将其安装为 `plugins/<goos>/<goarch>/keeper-usage-push-v<version>`，并使用对应平台的扩展名（`.so`、`.dylib` 或 `.dll`），随后启用插件。通过插件商店安装和升级可直接生效，无需重启 CPA。CPA 本身需要能够访问 `raw.githubusercontent.com` 和 GitHub Releases，可以直连，也可以使用其 `proxy-url` 代理配置。

安装完成后，打开插件设置，填写上传地址和令牌，详见[配置](#配置)。

### 手动安装

从 [Releases](https://github.com/brooke1220/keeper-usage-push/releases) 下载适合运行平台的压缩包，使用 `checksums.txt` 校验后，将 `keeper-usage-push.so`（Linux）、`keeper-usage-push.dylib`（macOS）或 `keeper-usage-push.dll`（Windows x64）解压到 CPA 的 `plugins.dir` 下对应的 `plugins/<goos>/<goarch>/` 目录，例如 `plugins/linux/amd64/` 或 `plugins/windows/amd64/`。官方 Docker 镜像已将插件目录挂载为数据卷。手动替换文件后，需要重启 CPA，或先关闭再启用插件，才能加载新文件。随后按下文完成配置。

Windows x64 请下载 `keeper-usage-push_<version>_windows_amd64.zip`。DLL 是供 CPA 加载的插件，请通过插件商店安装，或放入插件目录，无需双击运行。各平台都在同一个管理中心设置表单中配置上传地址和令牌。Windows 支持 `plugins/keeper-usage-push-spool` 这样的相对路径；在 YAML 中填写绝对路径时，可使用正斜杠（`C:/CPA/plugins/keeper-usage-push-spool`），或使用单引号包裹带反斜杠的路径（`'C:\CPA\plugins\keeper-usage-push-spool'`）。

## 宿主要求

- **原生插件支持。** 使用官方 Docker 镜像，或常规 Linux/macOS/FreeBSD 发行压缩包。Unix 平台名称中带有 `_no-plugin` 的压缩包不支持原生插件。CPA v8 也提供 Windows DLL 加载器；本项目构建 Windows x64 插件，并通过 Windows 文件锁保护本地队列。
- **Windows 存储。** 本地队列应使用支持 Windows 访问控制的本地 NTFS 目录。目录权限仅授予运行 CPA 的账户和 SYSTEM。运行维护工具或将 CPA 改为服务运行时，应继续使用同一账户；如果更换账户，可能需要管理员先调整目录权限。
- **C 运行时。** Linux 发布包在 manylinux2014 环境中构建，与官方 CPA Linux 压缩包一样，仅要求 glibc 2.17，因此可在官方 Debian 镜像和较旧的发行版中加载。不支持 musl（Alpine）宿主。

官方宿主不会发送规范化的 token 核算信息、入站端点、请求日志 ID 或会话树标记。插件会在可行范围内补充处理：

| 字段或行为 | 官方宿主 | 本插件的处理 |
|---|---|---|
| Token 明细（缓存、推理等分类） | 不发送 | 使用 CPA SDK 的 `EnsureTokenBreakdownForProvider`，根据平铺的 token 计数推导；宿主在执行器未提供明细时也使用该方法。部分上游响应只能归为 `unclassified`，试运行期间应与拉取通道核对费用 |
| 成功状态码 | `0` | 设为 `200`；没有状态码的失败记录设为 `500`，与 CPA 用量队列的处理一致 |
| `endpoint`、`request_id`（请求日志）、`node_kind`、`is_fork`、`is_compaction` | 不发送 | 保持为空，按 `endpoint` 匹配的计价规则或筛选条件不会命中 |
| `service_tier`、`reasoning_effort`、`stream` | 仅发送执行器记录的值 | 原样发送。CPA 用量队列还会从请求上下文补充这些字段，因此拉取通道中存在的值，在插件上报中可能为空 |
| 会话 ID | 客户端原始值 | 使用 CPA SDK 转换为规范 UUID，与 CPA 用量队列的处理一致 |
| 仅能通过 HTTP 响应状态识别的失败 | 记录为成功 | 插件无法恢复真实失败状态 |
| 卸载插件前排空队列 | 官方宿主在停止用量分发前先卸载插件 | 正常重启时，仍在 CPA 内存队列中的记录可能丢失 |

如果宿主已经发送规范的核算数据（accounting version 为 2，且包含 token 明细），插件会保留宿主提供的值，不会覆盖。

## 构建与打包

需要安装 Go（版本见 `go.mod`），启用 CGO，并准备 C 编译器；构建 Linux 发布包还需要 Docker。Windows x64 构建使用 MinGW-w64 GCC 和 Git Bash。如果无法访问默认的 Go 模块代理，可设置 `GOPROXY`，例如 `https://goproxy.cn,direct`。

```sh
go test ./...
go build -buildmode=c-shared -o keeper-usage-push.so .   # macOS 使用 .dylib，Windows 使用 .dll
go build -o keeper-push-spool ./cmd/spool
```

生成供插件商店使用的发布包：

```sh
./scripts/package-release.sh 0.4.0                           # linux/amd64 和 linux/arm64
./scripts/package-release.sh 0.4.0 linux/arm64 darwin/arm64  # 仅构建指定平台
./scripts/package-release.sh 0.4.0 windows/amd64            # 在 Windows 上使用 Git Bash + MinGW-w64
```

压缩包和 `checksums.txt` 会输出到 `dist/`：包括供插件商店使用的主插件压缩包，以及包含离线队列维护工具的 `keeper-push-spool_<version>_<goos>_<goarch>.zip`（Windows 中的工具名为 `keeper-push-spool.exe`）。插件商店只安装主插件压缩包。Linux 目标在 `quay.io/pypa/manylinux2014_*` 容器中构建，如果任一二进制文件要求高于 2.17 的 glibc，打包会失败。打包时还会实际加载一次插件库，以验证原生 ABI。`darwin` 目标必须在 macOS 上原生构建。Windows 目标可在 Windows 上使用 GCC 构建，也可使用 MinGW-w64 交叉编译器（可通过 `CC` 覆盖默认编译器）；在 Windows 上打包时，还会加载 DLL 并检查其生命周期。版本号通过 `-X main.Version` 写入，宿主会输出 `pluginhost: plugin registered ... version=<version>` 日志。

推送 `v*` 标签会触发 `.github/workflows/release.yml`，构建 linux/amd64、linux/arm64、darwin/arm64、darwin/amd64 和 windows/amd64，并将它们连同合并后的 `checksums.txt` 一起发布到 GitHub Release。Windows CI 会使用竞态检测器运行测试，并检查原生 DLL 加载。插件商店会安装最新发行版，因此标签去掉 `v` 后就是插件版本号。

如需基于其他官方 CPA 版本构建，可执行 `go get github.com/router-for-me/CLIProxyAPI/v8@vX.Y.Z && go mod tidy`。

### 升级与卸载

插件商店支持运行期间原地升级：CPA 先让旧实例静默，再加载新版本并退出旧实例。进入静默阶段时，插件会关闭本地队列并释放写入锁，新版本即可接管待上传记录，已持久化的记录不会丢失。已配置的新实例必须成功打开本地队列，才能完成注册。如果打开失败，包括已保存的设置无效等情况，CPA 会重新注册旧实例，并使用其上一次接受的设置重新打开队列。短暂交接期间产生的用量不会被保存。

0.5.0 之前的版本不会在静默阶段释放队列。从这些版本原地升级时，新版本会因无法取得队列锁而拒绝启动，CPA 会继续运行旧版本，并记录 `usage spool already has a writer`。重启一次 CPA 即可完成这次升级；之后的升级可原地生效。

从管理中心卸载插件会删除当前启用的插件文件及其设置，但会保留本地队列目录，因此尚未上传的记录在重新安装后仍可继续处理。升级遗留的旧版 `keeper-usage-push-v<version>` 文件不会被删除。只有确定不再需要待上传记录时，才手动删除 `plugins/keeper-usage-push-spool`。

## 配置

在 Keeper 中打开 **设置 → 推送接收**，启用接收功能，并为这台 CPA 创建令牌。Keeper 只展示一次令牌，同时会在同一卡片中显示上传地址。Keeper 根据令牌识别 CPA 来源，因此插件无需配置来源 ID。接收端部署方法和第 1 版协议见 Keeper 服务端的 `docs/usage-push.md` 和 `docs/usage-push.openapi.json`。

请先升级 Keeper，再升级本插件。Keeper 会拒绝包含未知字段的数据，因此新插件向旧 Keeper 上报时，受影响的事件可能在本地被隔离，错误码为 `http_422_invalid_usage_event`。升级 Keeper 后，可通过 `--retry-code` 将这些记录重新入队，详见[检查本地隔离记录](#检查本地隔离记录)。

必填项只有 `endpoint` 和 `token`。最方便的配置方式是在 CPA 管理中心打开 **插件**，点击 `keeper-usage-push` 的设置按钮，粘贴上传地址和令牌并保存。CPA 会立即重新配置插件，无需重启。

新安装的插件即使尚未填写 `endpoint` 和 `token`，也会完成注册，以便显示设置表单。此时只会记录一次 `keeper usage push: not configured`，不会打开本地队列，也不会上传数据；配置完成前产生的用量不会被保存，也不会计入丢弃数量。同时清空这两个字段会停止上报并释放队列。

管理中心保存设置后，不会显示插件是否接受了这些设置，因此保存后应检查 CPA 日志。只填写 `endpoint` 或 `token` 其中一项、令牌过短、时长未带单位（例如 `flush_interval: 5`），或 `spool_dir` 不可用等情况，会被拒绝，并以错误级别记录一次 `keeper usage push: settings rejected` 及具体原因。已经注册的插件会继续显示设置表单，并使用上一次接受的设置上传。尚未配置的插件会在设置修正前保持不记录用量。被拒绝后再次保存有效设置，会记录 `settings accepted`。

新加载的插件如果带有非空但无效的设置，或无法使用本地队列，就会注册失败，使热升级能够回滚，避免替换掉原本正常运行的实例。CPA 重启时没有旧实例可供恢复：此时需要修正 `config.yaml` 中保存的插件设置，再重新加载 CPA。空设置仍允许新安装的插件注册并显示设置表单。

也可以在 `config.yaml` 中配置相同的设置：

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

可选配置项包括：`token_file`（从挂载的密钥文件中读取令牌，用于替代 `token`，两者不能同时设置）、`spool_dir`、`max_spool_bytes`（默认 1 GiB）、`flush_interval`（默认 `5s`）和 `request_timeout`（默认 `15s`）。令牌至少需要 32 个字符，且不能包含空白字符。HTTP 和 HTTPS 地址均直接支持，包括 IP 地址和域名，无需额外开关。插件会验证 HTTPS 证书。HTTP 会明文传输推送令牌和用量数据，包括完整的客户端 API Key。插件不会跟随重定向。

插件接受配置后，会将读取到的令牌保存在内存中，即使之后 `token_file` 无法读取，也能在拒绝配置修改或升级回滚时恢复原来的令牌。该配置快照不会写入磁盘。成功重新加载配置时会再次读取文件；CPA 重启时也要求配置的令牌文件可读。

`spool_dir` 默认是 CPA 工作目录下的 `plugins/keeper-usage-push-spool`。官方 Docker Compose 文件已将 `plugins` 挂载为持久化数据卷，无需额外挂载。如果 `plugins.dir` 指向其他位置，或 CPA 从其他目录启动，请将 `spool_dir` 显式设置为持久化路径；未设置且默认路径不可用时，插件会拒绝启动，不会退回临时目录。

请为队列使用独立目录。在 Unix 上，插件会强制将目录权限设为 `0700`，将数据库、WAL 和 SHM 文件权限设为 `0600`，也会修正旧版本遗留文件的权限。在 Windows 上，插件会为目录设置受保护的 DACL，仅授权 CPA 进程账户和 SYSTEM，让新建的辅助文件继承这些权限，并修复现有数据库及辅助文件的权限。由于队列包含完整的客户端 API Key，权限处理会在 SQLite 打开文件之前完成。切换队列目录时，插件会先打开新队列，再关闭旧队列；如果新目录被拒绝，旧队列会继续运行。

队列与完整的 `endpoint` 地址绑定；修改 `endpoint` 时，应同时更换 `spool_dir`。轮换令牌时会保留队列和待上传记录的投递 ID，Keeper 应将新令牌映射到同一来源。不要将队列复制到另一套安装中：使用不同令牌重新上传的记录会计入该令牌对应的来源。克隆 CPA 时应清空队列。同一时间只能由一个 CPA 实例或一个队列维护工具占用队列。

## 数据投递行为

- 用量回调会将不可变、具有独立 ID 的事件写入 SQLite WAL，并使用 `synchronous=FULL`。上传工作线程等待 HTTP 响应时不会持有数据库事务。
- 每批上传最多包含 100 个事件，总大小最多为 1 MiB，默认通常在 5 秒内发送。每次上传请求的超时为 15 秒，只影响用量上报，不影响模型请求。出错时会保留数据，采用带随机抖动的指数退避重试，最长等待 60 秒。
- 只有收到完整且有效的 Keeper 确认响应，才会删除本地记录。进程重启、响应丢失或重新分批都不会改变投递 ID。使用同一投递 ID 发送不同内容属于协议冲突。
- 只有 Keeper 明确拒绝批次时，插件才会拆分批次，将异常记录放入持久化的本地隔离区：400 `invalid_batch`、409 `delivery_id_conflict`、413 `batch_too_large` 或 422 `invalid_usage_event`。如果只有相同 HTTP 状态码，却没有 Keeper 错误码（例如来自代理、WAF 或旧接收端），批次会继续保留等待重试；只有状态码的 413 会触发拆分，但不会隔离记录。认证失败、重定向、网络故障和服务端故障都会继续重试。
- 单条记录如果过大、无法放入任何批次，会被标记为 `event_too_large` 并隔离，避免阻塞后续记录；此类记录无法重新入队。
- 队列容量上限按保留的数据载荷字节数计算，包含隔离记录。请为 SQLite 数据页、元数据和 WAL 预留额外磁盘空间。空间耗尽时会保留现有记录，累计每次新记录持久化失败的次数，同时保持模型服务可用。首次失败会立即记录日志，此后最多每分钟记录一次，包含累计丢弃数量及自上次日志以来的新增丢弃数量，避免磁盘写满时刷屏。磁盘不可写时，新增丢弃计数本身也无法持久化，此时可通过 CPA 日志和内存中的计数查看。
- 异常终止可能导致仍在 CPA 内存中等待处理的记录丢失；官方宿主在正常重启时也可能丢失这些记录，因为它们会先卸载插件，再停止用量分发。插件关闭时不会等待远端投递完成。永久性磁盘丢失，以及中央备份回退后与本地状态的协调恢复，不在投递保证范围内。
- 日志通过宿主日志回调写入 CPA 日志。队列空闲状态使用 debug 级别，新增隔离记录使用 warning 级别，丢弃记录使用限频的 error 日志。上传错误会保留 DNS、TLS、连接被拒绝等传输层原因，但不会包含推送令牌。

上传数据仅包含白名单中的统计字段，不包含提示词、模型回复、失败响应体、上游令牌或任意请求头。上传数据会包含完整的客户端 API Key，以保留 Keeper 现有的 Key 分组和脱敏展示功能。HTTPS 会加密传输，HTTP 则明文传输。费用由 Keeper 统一计算。

## 检查本地隔离记录

从与主插件相同的发行版中下载 `keeper-push-spool_<version>_<goos>_<goarch>.zip`，并使用 `checksums.txt` 校验。先停止 CPA，再将插件自身的配置（`keeper-usage-push` 下的字段，不含 `enabled` 和外层结构）保存到仅授权人员可读的 YAML 文件中。检查操作不会上传数据，也不会输出 API Key 或数据载荷：

```sh
keeper-push-spool --config /etc/cpa/usage-push.yaml
keeper-push-spool --config /etc/cpa/usage-push.yaml --retry DELIVERY_EVENT_ID
keeper-push-spool --config /etc/cpa/usage-push.yaml --retry-code http_422_invalid_usage_event
```

Windows 下请在 PowerShell 中使用 `.\keeper-push-spool.exe --config 'C:\CPA\usage-push.yaml'`，并按上面的示例追加相同参数。请使用与 CPA 相同的 Windows 账户运行，以便访问受权限保护的队列。

输出会按错误码统计所有隔离记录，并列出前 100 条记录的 ID。`--retry-code` 会将指定错误码对应的全部记录重新入队，例如在修复接收端版本回退问题后使用。请先修复根本原因，再重试。操作完成后重启 CPA，恢复上传。Keeper 服务端另有处理隔离区，可在其管理员状态面板中查看。

## 验证

`go test ./...` 使用真实的本地 HTTPS 服务，覆盖重试、响应丢失、无效确认、超时、隔离、隐私、队列独占和连接持久性等场景，还会解析与官方宿主实际发送格式完全一致的记录。测试需要能够监听本机回环端口。

插件还曾加载到基于官方 v8.0.44 源码构建的 CPA 中，并通过兼容 OpenAI 接口的模拟上游完成验证：成功、失败和流式请求的用量均携带推导的核算数据到达 Keeper；Keeper 停机期间的记录得到保留并在恢复后补传；CPA 正常重启后可重新打开队列；上游密钥不会传到 Keeper。在同一宿主上，还通过本地插件商店源验证了安装、启用、通过管理 API 接受设置、热升级时将 3 条待上传记录交接给新版本并随后成功投递，以及卸载。也验证了设置被拒绝后，插件仍保持注册并使用此前的设置继续上报，以及从不支持静默释放队列的旧版本原地升级失败后，回滚到运行中的版本。

仓库还包含一个本地 Postman 模拟场景：接收端先返回 503，再对同一个投递 ID 返回确认响应：

```sh
# 在仓库根目录打开终端 1
postman simulation run test/postman/simulations/usage-push.sim.yaml --port 4907 --no-history
# 在仓库根目录打开终端 2
KEEPER_PUSH_SIMULATION_ENDPOINT=http://127.0.0.1:4907/keeper/api/v1/ingest/usage \
  go test ./push -run TestPostmanSimulationTransport -count=1 -v
```

重新运行此场景前，请先重启模拟服务。无需发布到云端，也无需账户凭据。

插件有意不上传 CPA 的旧版 `UsageRecord.Source` 字段，因为使用 API Key 凭据时，该字段可能包含上游密钥本身。上传令牌用于识别 CPA 安装实例；token 用量、计价及客户端 API Key 分组均不依赖旧版来源标签。

### 凭据来源名称

插件通过 CPA 的只读 `host.auth.get_runtime` 接口按 `AuthIndex` 查询凭据，仅上传 `identity.display_name` 和 `identity.provider_type`。名称依次取凭据标签（标签只是 provider 类型时跳过）、OAuth 邮箱、凭据文件名，因此 OAuth 凭据的显示名称通常就是账户邮箱，与 CPA 上报的 `Account` 相同。不转发整个查询结果、旧版 `Source`、上游密钥或凭据 JSON。文件和运行时 API Key 凭据正常查询名称；`config.yaml` 中的 API Key 凭据在 CPA 的运行时查询无法解析时不带名称，不再仅凭认证类型判断凭据来源。成功查询缓存五分钟，失败缓存三十秒；查询失败不影响用量保存和上传，刷新失败时保留上次缓存的名称。

先升级 Keeper 才能记录凭据名称。旧接收端不认识可选的 `identity` 字段，会返回 422；插件随即去掉该字段重发这些事件并记录一条警告，用量照常送达，只是没有名称。升级 Keeper 之前，这类上传每次会多发一个请求。排队中的记录保持原始字节和投递编号。Keeper 按 CPA 来源与凭据编号关联新旧事件名称，推送来源不再被当作本机已删除凭据。推理强度、请求／实际发出／响应速度档位和流式标记继续按 CPA 给出的值传递；官方 v8 usage 回调缺少的入口路径和请求日志编号继续留空。
