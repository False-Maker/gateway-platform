# 执行证据记录（EVIDENCE）

> 本文只记录**已执行的命令、结果、验收证据和免责边界**。
> **待办、计划、下一步一律写在 `docs/TODO.md`，不写在本文。**
> 本文原名 `docs/P3-STATUS.md`，2026-09-03 拆分为证据（本文）与进度（`docs/TODO.md`）两份。
>
> 阅读规则：本文的每一条都是**对已发生事实的描述**。不要从本文推断"项目是否可以继续开发"——
> 那个问题由 `docs/TODO.md` 和 `AGENTS.md` §0 回答。

## A9 control 侧 ErrorClass 权威层 + 平台级速率告警（2026-09-10）

实现见 `docs/TODO.md` A9 摘要。本轮实际执行并通过：

- 无基础设施 `go test -count=1 ./...`：全部通过。
- `source configs/test-infra/test.env && go test -count=1 -p 1 ./...`（Docker PG16 + Redis 7.0.15）：连续两轮
  全部通过。其中新增 `TestPGReleaseAuthorityCooldownsAndExclusions` 在真实 PG 上断言：两次无头 429 不冷却、
  第三次进入 4–6 分钟冷却且从快照消失、`ok` 清零并回到快照；`forbidden_capability` 两模型去重追加且不冷却，
  快照携带 `ExcludedModels`；`forbidden_transport`/`upstream_5xx` 三连不计失败不冷却；第二个账号 `blocked`
  后 `PlatformSignals.Alerting()` 返回该平台且账号获得 50–70 秒冷却；4 账号冷却 3 个时防饿死闸释放最早到期的
  `a9-4`，降到 2/4 后不再释放。`TestPlatformSignalsAlertOnlyWhenSeveralAccountsFailTogether` 覆盖单账号重复
  不告警、非 transport 类不计、5xx 不计、平台独立、窗口过期清除、nil 接收器安全。
  `TestChooserHonoursControlExcludedModels` 覆盖 gateway 侧只排除指定模型。
- `go vet ./...`、`go test -race` 覆盖 control/gateway/contracts、`git diff --check` 通过。

本轮排查的两个测试自身问题（非实现缺陷）：新测试一处 SQL 把字面量与 `$1` 混用（SQLSTATE 42P18），已改；
新测试用独立密钥写入的账号在后续 `cmd/gwd` 导入测试遍历全部 bucket 时无法解密，已加 `defer DELETE`
清理。两者修正后全量两轮稳定。

未执行真实 provider 请求、生产写入或部署。冷却阈值与时长为总览 §6.3.1 定值，真实 429/403 语义校准属 C3。

## A8 设计文档与代码对齐（2026-09-10，仅文档）

对照仓库当前代码修正描述性内容，未改任何 `.go` 文件、设计决策或阶段计划：

- `keyhive/docs/architecture.md`：§2 目录树改为实际布局（`profile.go` 集中魔数、`builtin.RegisterAll` 显式
  注册、每 provider 单文件 + 可选 `quota.go`、`copilot/` 而非 `github_copilot/`），补充可选能力接口；§3.3
  `keyhive wrapper run` → `gwd --role=wrapper`；§4 建表清单标注 `quota_snapshot_outbox`、tenants 三表已建，
  `import_templates`、`request_logs` 未建；§5 checklist 依 2026-09-10 Docker 真实通过的用例勾选 ledger 幂等/
  崩溃恢复与八项 P0 指标，建表项改为"部分"；§6 明确当前为扁平包而非 `modules/*` 分层。
- `docs/fluxgate-keyhive-overview.md` §8：目录树补全 `cmd/new-api-migrate`、`pkg/protokit`、
  `internal/{events,migration,control/wrapper,control/credentials}`，去掉 `modules/*` 描述，补 wrapper 第三角色说明。
- `fluxgate/docs/architecture.md` §5：P1 验收项勾选并指向测试文件，新增入站鉴权交付项；§8 utls 版本与
  `SessionKey` 未接线说明。
- `docs/P2-ADMISSION.md`："仓库当前没有 AGENTS.md" 改为带日期的历史注记。

核对方法：`ls internal/control/provider/`、`grep` 八个指标名、`grep forbidden_transport internal/control/*.go`
（无非测试命中，平台级告警确未实现）。本轮无命令执行结果需要记录。

## A7 utls TLS 指纹注册表（2026-09-10）

本轮实现 `docs/TODO.md` A7。`go.mod` 新增 `github.com/refraction-networking/utls v1.6.7`（间接引入
brotli、circl、klauspost/compress）。`internal/gateway/tlsprofile.go`：

- 注册表 `codex_rustls`、`node24`，`ClientHelloSpec` 逐字段从
  `elucid-relay/services/gateway-api/internal/httpserver/codex_tls.go` 搬入；`RegisterTLSProfile`/
  `TLSProfiles` 可扩展。
- `clientForTLSProfile`：空名返回原 client；未知名返回 `ErrUnknownTLSProfile`，不建 transport 不拨号；
  已知名克隆 transport，设置 `DialTLSContext` 用 utls `HelloCustom` + `ApplyPreset` 握手，关闭 h2 协商
  （两个 profile 均只宣告 http/1.1 或无 ALPN）。transport 带 proxy 时改为自建 CONNECT 隧道再在隧道内
  做指纹握手，保证指纹终止在源站而非代理。
- `server.go` 非流式 `callUpstream` 与流式路径均在 `clientForProxy` 之后接 `clientForTLSProfile`。
- control：`EndpointProfile.TLSFingerprint` 字段；`CodexChatGPTProfile` = `codex_rustls`，Claude
  Console/AI OAuth profile = `node24`；Codex/Claude `Profile()` 透传到 `UpstreamProfile`。API-key
  profile 不设指纹。

本轮实际执行并通过：

- `go test -count=1 ./internal/gateway -run 'TLS|Profile'`：本地 `httptest` TLS 服务器通过
  `GetConfigForClient` 记录真实 ClientHello 并断言——`codex_rustls` 的 31 个 cipher suite 顺序、10 条
  curve（含 x448 0x001e）、3 个 point format、20 个 signature scheme、无 ALPN、TLS1.3 优先；`node24`
  的 18 个 cipher suite、ALPN 仅 `http/1.1`、3 curve、9 signature scheme；两者与 Go 默认 hello 可区分。
  未知 profile 在 `clientForTLSProfile` 与 `callUpstream` 两层均在拨号前失败（记录器零 hello）。
  本地 CONNECT 代理 fixture 证明带 proxy 时代理只见 CONNECT、源站收到 rustls hello。
- `go test -count=1 ./...`（无基础设施）与 `source configs/test-infra/test.env && go test -count=1 -p 1 ./...`
  （Docker PG16 + Redis 7.0.15）全部通过；`go vet ./...`、`go test -race` 覆盖 gateway/contracts/provider、
  `go build ./cmd/gwd`、`git diff --check` 通过。

边界：JA3 哈希值（`d39e1be3…`、`44f88fca…`）来自参考实现注释，本轮未独立计算 JA3，也未向任何真实上游发起
TLS 握手；真实 OpenAI/Anthropic 边缘是否接受该 hello 属 C3/C5。WebSocket 上游（`UpstreamProfile.WS`）
尚无调用方，本轮未接指纹。

## A6 gateway 鉴权面与多 bucket（2026-09-10）

本轮实现 `docs/TODO.md` A6。改动：

- `migrations/002_tenants.sql`：tenants、principals、tenant_tokens（`token_hash` UNIQUE，无明文列）；
  `migrations/migrations.go` 以 `embed` 提供有序 `Apply`，测试与运维共用同一 schema 来源。
- `internal/snapshot/auth.go`：`snap:auth:tokens:v1` Hash（field = SHA-256(token)，value = `TokenRecord`
  JSON），`PublishTokens` 在 staging key 上构建后 `RENAME` 原子替换，空集合也发布以传播撤销；键前缀落在
  `snap:*` 使 gateway 只读 ACL 即可覆盖。
- `internal/control/tenants.go`：`PGTenantRepository.ListActiveTokens` 过滤 revoked/expired/suspended，
  `TokenLoop` 每 15s 发布，`CreateTenantToken` 单事务建 tenant/principal/token 并校验 principal 归属。
- `internal/gateway/auth.go`：`Authenticator` 从 Redis 快照校验 Bearer 或 `x-api-key`，派生
  `AuthContext`；空/未知/过期 token 与非 Bearer scheme 均 401；刷新失败保留 last-known-good。
- `internal/gateway/run.go`：`NewRouter` 三个推理路由先鉴权，TenantID/Group 只来自 `AuthContext`；
  `BucketLoader.RefreshAll` 按 `snap:bucket_registry` 装载全部 bucket 并移除已注销 bucket。
- `internal/gateway/chooser.go`：`ReplaceBucket`/`RemoveBucket`/`Buckets`，每 bucket 独立 epoch、
  冷却清除与 `validUntil` 新鲜度门；`Criteria.Platform` 为空时跨全部 platform 选号；`Lease.Provider`
  由账号填充，`server.go` 的 provider 相关整形逻辑全部改读 lease。
- `cmd/gwd`：新增 `tenant create` 子命令；`GATEWAY_TENANT_ID` 删除。A4 进程级 e2e 改为先断言无 token
  401 且上游零命中，再以 `CreateTenantToken` 生成的真实 token 走通全链路。

本轮实际执行并通过：

- 无基础设施：`go test -count=1 ./...` 23 个包全部通过。
- `source configs/test-infra/test.env && go test -count=1 -p 1 ./...`：全部通过，其中
  `TestProcessE2EControlGatewayRelease`（含 401 断言与真实 token）、`TestPGTenantTokensPublishAndRevoke`
  （PG 落库只存哈希、过期不发布、撤销与租户 suspended 后下一轮消失、跨租户 principal 拒绝）真实执行通过。
- `go vet ./...`、`go test -race` 覆盖 contracts/control/gateway/snapshot、`git diff --check` 通过。
- 新增本地回归：`TestChooserServesMultipleBucketsAndIsolatesGroups`、`TestBucketLoaderTracksRegistry`、
  `TestRouterAuthenticatesAndDerivesTenantFromToken`（两租户两 bucket，请求体伪造 tenant/group 被忽略，
  Release 的 TenantID/AccountID 跟随 token）、`TestAuthenticator*`、`TestPublishAndLoadTokens*`。

Redis ACL 边界在 Docker Redis 7.0.15 上以 `redis-cli` 实测：`test-control` 可 HSET staging 键并
`RENAME` 到 `snap:auth:tokens:v1`（ACL 已补 `+rename`）；`test-gateway` 可 `HGETALL` 该键、`HSET` 被
NOPERM 拒绝；`test-wrapper` `HGETALL` 被 NOPERM 拒绝。

`go fmt ./...` 顺带改动了 `cmd/new-api-migrate/main_test.go` 的一处格式，已还原，不计入本轮。
本轮未执行真实 provider 请求、生产写入、部署或切流。最高剩余风险：token 传播依赖 control 15s tick 与
gateway 5s 轮询，撤销最坏延迟约 20s；per-tenant 限流与粘滞会话隔离尚未实现。

## D1 Docker PostgreSQL + Redis 7.0.15 全量集成验收（2026-09-10）

本轮 Docker daemon 已可连接（`docker info` 正常）。新增 `configs/test-infra/` 三个文件：
`docker-compose.yml`（`postgres:16-alpine` 映射 `127.0.0.1:15432`，`redis:7.0.15-alpine` 映射
`127.0.0.1:16389`，与既有 `new-api-dev-*` 容器互不干扰）、`redis-test.acl`（default 关闭；
`test-admin` 全权；`test-control` 全键 `+@all -@dangerous +flushdb`；`test-gateway` 只读 `snap:*`、
读写 `gateway:*`、仅限 limiter/事件所需命令；`test-wrapper` 与 `configs/redis-wrapper.acl` 同命令集、
仅 `control:wrapper:*`）、`test.env`（对应的 `GATEWAY_TEST_*` 变量，全部为测试专用口令）。

本轮实际执行并通过（`source configs/test-infra/test.env` 后）：

- `/home/elucid/go/pkg/mod/golang.org/toolchain@v0.0.1-go1.25.0.linux-amd64/bin/go test -count=1 -p 1 ./...`：
  23 个包全部 `ok`，退出码 0。
- 同环境 `-v` 聚焦 `./cmd/gwd ./internal/control ./internal/control/wrapper`：以下此前只有历史证据或
  从未在本机执行的用例本轮**首次真实通过，无一 SKIP**：
  `TestProcessE2EControlGatewayRelease`（独立 control/gateway 进程 + PG ledger + 重复 Release 幂等）、
  `TestImportCommandPublishesSnapshotAndGatewayCanChooseAccount`、`TestP2PostgresImportAndRefresh`、
  `TestPostgresLedgerIdempotencyAndRecovery`、`TestPGSnapshotRepositoryReadsSchedulableAccounts`、
  `TestAcceptanceRealRedisACLAndReliability`、`TestAcceptanceControlCrashAfterCommitBeforeAck`、
  `TestAcceptanceGatewayCrashAfterAttemptStartedRecovery`、`TestP2RealRedisWrapperProcessLifecycle`、
  `TestP2RealRedisQueueLifecycleAndGatewayIsolation`。
- `go vet ./...`、`git diff --check`：通过。

本轮排查并处理的两个环境问题（均非业务代码缺陷，未改任何 `.go` 文件）：

1. Redis aclfile 不接受 `#` 注释行，首次启动失败；已把角色说明移到 compose 文件。
2. 首次用 `redis:7-alpine`（实为 7.4.10）时 `TestP2RealRedisWrapperProcessLifecycle` 因硬编码断言
   `redis_version:7.0.15` 失败，`TestP2RealRedisQueueLifecycleAndGatewayIsolation` 在默认并行 `go test`
   下也失败（`cmd/gwd`、`internal/control`、`internal/control/wrapper` 三个包共用同一 Redis DB 并各自
   `FLUSHDB`，互相清空对方的 pending）。改为 pin `redis:7.0.15-alpine` 并以 `-p 1` 串行后全部通过。
   随后单独验证：临时起 `redis:7.4-alpine`（7.4.11），串行执行
   `TestP2RealRedisQueueLifecycleAndGatewayIsolation`、三个 `TestAcceptance*` 各三轮，以及放宽断言后的
   `TestP2RealRedisWrapperProcessLifecycle`，全部通过；7.0.15 同组用例同样通过。结论：7.4 上的失败
   是并行 FLUSHDB 干扰，不是 XCLAIM 语义差异。`process_integration_test.go` 的版本断言已从
   `7.0.15` 放宽到 `7.x`（唯一改动的 Go 文件，仅测试）。`-p 1` 保留为文档约束：wrapper 子进程经
   环境变量连 Redis 且不支持 DB 序号，按包拆 DB 会扩散到生产配置。临时 7.4 容器已删除。

本轮未执行真实 provider 请求、生产写入、部署或切流。

## D1 PostgreSQL 与真实 Redis 集成复核（2026-09-07）

本轮检查了现有 PostgreSQL、真实 Redis ACL 和进程级集成测试入口。环境中未配置
`GATEWAY_TEST_DATABASE_URL` 或 `GATEWAY_TEST_REDIS_*`；PATH 中没有 `psql`、`redis-cli` 或
`pg_isready`，也没有监听本地 PostgreSQL `5432` 或 Redis `6379` 的服务。Docker CLI 路径可见，
但 `docker info`、`docker ps` 和 `docker context ls` 均因当前 WSL 发行版未启用 Docker Desktop
WSL 集成而失败，无法启动临时测试容器。

本轮实际执行并通过（可选外部服务用例按既有约定 skipped）：

- `/home/elucid/go/pkg/mod/golang.org/toolchain@v0.0.1-go1.25.0.linux-amd64/bin/go test -count=1 -v ./internal/control ./internal/control/wrapper ./cmd/gwd -run 'Integration|Acceptance|ProcessE2E|QuotaReconcile|Ledger|Snapshot'`

该命令中内存 quota、snapshot、ledger 和 control 回归通过；PG ledger/snapshot、真实 Redis ACL、
导入到快照以及 control/gateway 进程级 PG 用例分别因 `GATEWAY_TEST_DATABASE_URL` 或真实 Redis
环境未配置而严格 skipped。此前本轮 B3 完成后执行的全量
`/home/elucid/go/pkg/mod/golang.org/toolchain@v0.0.1-go1.25.0.linux-amd64/bin/go test -count=1 ./...`
也通过。

本轮未连接 PostgreSQL 或 Redis，未执行任何生产写入、部署、真实 provider 请求或真实计费；本轮
没有形成真实基础设施验收证据，skipped 结果未被表述为验收通过。

## B3 Quota 精度契约（2026-09-06）

本轮扩展 `contracts.QuotaInfo`：保留既有 `*int64` 字段以兼容旧快照，同时增加 JSON 数字形式的
`Decimal`、`remaining_fraction`、`limit_exact`、`remaining_exact` 和 `precision` 字段。Decimal 以
严格十进制字符串校验并原样序列化，不经过 `float64` 或整数截断；未知/缺失值不补零。gateway chooser
只在明确的精确余量、fraction 或 precision remaining 为零/负数时排除账号，未知余量继续放行。

Antigravity quota adapter 调用 `fetchAvailableModels`，只映射带 `quotaInfo.remainingFraction` 的模型项
及 RFC3339 `resetTime`；Kiro quota adapter 调用 `getUsageLimits`，只映射带
`currentUsagePrecise`/`usageLimitPrecise`/`remainingPrecise` 的 usage breakdown，并从 used/limit 精确
相减得到 remaining。没有可靠字段的条目和未知 envelope 返回 `Items == nil`，不会变成耗尽额度。
短期 credential snapshot 只携带 quota 所需的非秘密 metadata（Antigravity project、Kiro profile/machine/region），
不把 client secret 放入 gateway 快照。

本轮实际执行并通过：

- `/home/elucid/go/pkg/mod/golang.org/toolchain@v0.0.1-go1.25.0.linux-amd64/bin/go test -count=1 ./pkg/contracts ./internal/control/provider/... ./internal/control ./internal/snapshot ./internal/gateway`
- `/home/elucid/go/pkg/mod/golang.org/toolchain@v0.0.1-go1.25.0.linux-amd64/bin/go vet ./...`
- `/home/elucid/go/pkg/mod/golang.org/toolchain@v0.0.1-go1.25.0.linux-amd64/bin/go build -o /tmp/gwd-b3 ./cmd/gwd`
- `/home/elucid/go/pkg/mod/golang.org/toolchain@v0.0.1-go1.25.0.linux-amd64/bin/go test -race -count=1 ./pkg/contracts ./internal/control/... ./internal/gateway`
- `git diff --check`

针对性回归覆盖十进制精度/非法值、Antigravity/Kiro fixture 请求与 parser、missing、Redis snapshot round-trip
以及 fraction/precision chooser 边界。真实 Antigravity/Kiro provider 请求、生产写入、部署和 live schema 对账未执行；
后者按 C2/C5 留到项目结束真实验收。

本轮提交尝试：`git add -A` 与 `git commit -m 'extend quota contracts for decimal provider precision'` 均因项目
`.git` 挂载为只读而失败，Git 无法创建 `.git/index.lock`，返回 `Read-only file system`；工作区改动未回滚。

## B1 非 OpenAI 入站跨协议 streaming（2026-09-06）

本轮在纯文本、无工具调用边界内补齐 Anthropic Messages 与 OpenAI Responses 入站的跨协议
streaming。请求先移除入站 `stream` 字段；目标协议不是 canonical Chat 时，经 Chat 请求模型组合
转换，再按目标协议恢复流式标志。支持的上游协议为 OpenAI Chat、Anthropic Messages、OpenAI
Responses 与 Gemini GenerateContent；转到 Chat 时强制 `stream_options.include_usage=true`，同协议
SSE 仍逐块原样转发。

跨协议回程现在按入站协议生成完整事件生命周期：Anthropic 输出
`message_start`、文本块、`message_delta` 和 `message_stop`；Responses 输出 response/item/content
创建、文本 delta/done 和 `response.completed`。`stop`、`max_tokens`/`max_output_tokens` 终态按协议
映射；上游 input/output/cache usage 继续由独立累加器写入 terminal Release，客户端断开后的 drain
语义未改。回归覆盖六种非 OpenAI 入站请求目标组合、Chat → Anthropic、Anthropic → Responses、
Responses → Anthropic 和 Gemini → Responses 的事件与 usage 映射。

本轮实际执行并通过：

- `/home/elucid/go/pkg/mod/golang.org/toolchain@v0.0.1-go1.25.0.linux-amd64/bin/go test -count=1 ./internal/gateway`
- `/home/elucid/go/pkg/mod/golang.org/toolchain@v0.0.1-go1.25.0.linux-amd64/bin/go test -count=1 ./pkg/protokit ./internal/gateway/...`
- `/home/elucid/go/pkg/mod/golang.org/toolchain@v0.0.1-go1.25.0.linux-amd64/bin/go test -count=1 ./...`
- `/home/elucid/go/pkg/mod/golang.org/toolchain@v0.0.1-go1.25.0.linux-amd64/bin/go vet ./...`
- `/home/elucid/go/pkg/mod/golang.org/toolchain@v0.0.1-go1.25.0.linux-amd64/bin/go build -o /tmp/gwd-b1 ./cmd/gwd`
- `/home/elucid/go/pkg/mod/golang.org/toolchain@v0.0.1-go1.25.0.linux-amd64/bin/go test -race -count=1 ./internal/gateway ./pkg/contracts ./internal/control/...`
- `git diff --check`

本轮限制仍是流式工具调用、多模态与未知内容事件不做跨协议转换；请求侧的工具和多模态继续在调用
上游前拒绝，上游返回这些事件时终止转换，并以 `forbidden_capability`、`partial=true` 写 Release，
避免静默丢内容。证据来自本地 `httptest`/miniredis fixture，不代表真实 provider 或各官方 SDK 已验收。
本轮未设置可选 PostgreSQL、真实 Redis 或 live provider 环境变量；相应测试按约定 skipped。

本轮提交尝试仍因项目 `.git` 只读失败：Git 无法创建 `.git/index.lock`，返回
`Read-only file system`。B1 与此前未提交改动均保留在工作区，未产生 commit。

## B2 非流式图像内容块（2026-09-06）

本轮补齐 `pkg/protokit` 的非流式图像内容块转换。canonical Chat 保留 OpenAI
`image_url` 的 data URL 与绝对 http(s) URL，并映射到 Anthropic `image` 的 base64/url source、
Responses `input_image`、Gemini `inlineData`/`fileData`。响应方向保留 Anthropic 与 Gemini 的文本+图像
块，并把 OpenAI Chat 可表示的图像内容转换回这两种协议；纯文本仍按原有字符串形态输出，混合内容才使用
数组，避免旧客户端纯文本回归。

边界校验覆盖：data URL 必须是非空标准 base64 且 MIME 为 `image/*`；远程地址必须是绝对
`http(s)` URL；图像 `detail` 只接受 `auto/low/high`；`image_url: null`、非法 base64、音频 MIME、空
Gemini 图像 part 均在调用上游前拒绝。
gateway 非流式入口允许合法图像数组，流式入口仍返回 `unsupported_capability`，不会把图像事件塞进文本
SSE。Responses 输出侧只接受标准 `output_text`；图像生成 call 没有稳定 MIME，继续按不可无损表示拒绝。

本轮实际执行并通过：

- `/home/elucid/go/pkg/mod/golang.org/toolchain@v0.0.1-go1.25.0.linux-amd64/bin/go test -count=1 -v ./pkg/protokit ./internal/gateway`
- `/home/elucid/go/pkg/mod/golang.org/toolchain@v0.0.1-go1.25.0.linux-amd64/bin/go test -count=1 ./...`
- `/home/elucid/go/pkg/mod/golang.org/toolchain@v0.0.1-go1.25.0.linux-amd64/bin/go vet ./...`
- `/home/elucid/go/pkg/mod/golang.org/toolchain@v0.0.1-go1.25.0.linux-amd64/bin/go build -o /tmp/gwd-b2-20260906 ./cmd/gwd`
- `/home/elucid/go/pkg/mod/golang.org/toolchain@v0.0.1-go1.25.0.linux-amd64/bin/go test -race -count=1 ./pkg/contracts ./pkg/protokit ./internal/gateway ./internal/control/...`
- `git diff --check`

回归覆盖 OpenAI Chat → Anthropic/Responses/Gemini 请求、Anthropic/Gemini → OpenAI Chat 响应、
混合文本+图像 round-trip、非法 MIME/base64/空字段，以及 gateway 非流式放行与流式拒绝。证据来自本地
Go fixture、httptest 和 miniredis，不代表真实 provider 或真实图像模型准入；本轮未执行生产写入、部署、
真实 provider 请求或可选 PostgreSQL/Redis 环境测试。

提交尝试因项目 `.git` 挂载为只读失败：Git 无法创建 `.git/index.lock`，返回 `Read-only file system`。
本轮未产生 commit，工作区改动保持未暂存状态。

## 审查修复：快照删除与账号代理（2026-09-06）

本轮修复两项代码审查确认的问题。control 现在把已发布 bucket 的
`platform + group` 身份保存在 Redis 注册表；当 PostgreSQL 中整个 bucket 消失时，下一轮先发布
空快照，再清理 `snap:buckets` 与注册表记录，避免最后一条账号被物理删除后旧账号继续留在 active
快照。回归覆盖账号列表清空、bucket 行消失、空快照版本推进和注册表清理。

`accounts.proxy` 现在进入 `UpstreamProfile` 和短期 `Credential` 快照，并用于 gateway 的流式与
非流式上游 HTTP client。control 的 OAuth authorization-code exchange、refresh、revoke、Codex/Claude
quota，以及 Copilot/Antigravity/Kiro refresh 均使用账号代理；wrapper PKCE job 通过加密输入携带代理。
代理只接受带 host 的 `http`/`https` URL，按请求复制 `http.Client`/`http.Transport`，不修改共享 client。

本轮实际执行并通过：

- `/home/elucid/go/pkg/mod/golang.org/toolchain@v0.0.1-go1.25.0.linux-amd64/bin/go test -count=1 ./internal/snapshot ./internal/control ./internal/control/provider/... ./internal/control/wrapper ./internal/gateway ./internal/migration/newapi ./cmd/gwd`
- `/home/elucid/go/pkg/mod/golang.org/toolchain@v0.0.1-go1.25.0.linux-amd64/bin/go build -o /tmp/gwd-review-fix ./cmd/gwd`
- `/home/elucid/go/pkg/mod/golang.org/toolchain@v0.0.1-go1.25.0.linux-amd64/bin/go vet ./...`
- `/home/elucid/go/pkg/mod/golang.org/toolchain@v0.0.1-go1.25.0.linux-amd64/bin/go test -count=1 ./...`
- `/home/elucid/go/pkg/mod/golang.org/toolchain@v0.0.1-go1.25.0.linux-amd64/bin/go test -race -count=1 ./pkg/contracts ./internal/control/...`
- `/home/elucid/go/pkg/mod/golang.org/toolchain@v0.0.1-go1.25.0.linux-amd64/bin/go test -count=1 ./cmd/gwd ./internal/control/wrapper`
- `git diff --check`

本轮 `GATEWAY_TEST_DATABASE_URL`、`GATEWAY_TEST_REDIS_ADDR` 和
`GATEWAY_RUN_LIVE_PROVIDER_TESTS` 均未设置，因此可选 PostgreSQL、真实 Redis ACL 和真实 provider
测试按既有约定 skipped；没有执行生产写入、部署、切流或真实计费。最高剩余风险是账号代理尚未在
真实 provider/企业代理环境验收，当前证据来自本地 HTTP proxy fixture；这不阻塞下一条 `[no-cred]`
任务。

提交尝试因项目 `.git` 挂载为只读失败：Git 无法创建 `.git/index.lock`，返回
`Read-only file system`。本轮未产生 commit，工作区改动保持未暂存状态。

## A5 new-api 迁移（2026-09-04）

本轮新增 `cmd/new-api-migrate` 与 `internal/migration/newapi`。命令默认只读读取
PostgreSQL `information_schema` 和 `channels/users/tokens/quota_data/groups`，按已核实的
new-api channel type 分别转换 Codex OAuth、Claude/Gemini/Grok API key；多 key、多 group
展开为独立账号。`setting.proxy`、model mapping、能力、状态和 Codex OAuth token bundle
字段保留；未知 provider/status/凭据进入 `rejected`，不会猜测。users/tokens/quota/groups
进入已有 `migration_records` staging，token 只存 SHA-256，quota/balance 只保留源单位和原始
字符串；`contracts.QuotaInfo` 不补零。目标 apply 使用现有 AES-GCM credential cipher，accounts、
credentials、staging records 和 `migration_runs` 在单个事务内写入，source transaction 使用
PostgreSQL `READ ONLY`。

本轮实际执行并通过：

- `/home/elucid/go/pkg/mod/golang.org/toolchain@v0.0.1-go1.25.0.linux-amd64/bin/go test -count=1 ./internal/migration/newapi ./cmd/new-api-migrate`：provider 分流、多 key/group、per-key status、proxy、unknown/rejected、model manual review、重复 key、token 脱敏和 CLI 配置保护通过。
- `/home/elucid/go/pkg/mod/golang.org/toolchain@v0.0.1-go1.25.0.linux-amd64/bin/go build ./cmd/new-api-migrate`：通过；生成的二进制已移至 `/tmp/new-api-migrate-a5-build-20260904`。
- `/home/elucid/go/pkg/mod/golang.org/toolchain@v0.0.1-go1.25.0.linux-amd64/bin/go build ./cmd/gwd`：通过；生成的二进制已移至 `/tmp/gwd-a5-build-20260904`，未留在仓库根目录。
- `/home/elucid/go/pkg/mod/golang.org/toolchain@v0.0.1-go1.25.0.linux-amd64/bin/go vet ./...`：通过。
- `/home/elucid/go/pkg/mod/golang.org/toolchain@v0.0.1-go1.25.0.linux-amd64/bin/go test -count=1 ./...`：全部通过。
- `/home/elucid/go/pkg/mod/golang.org/toolchain@v0.0.1-go1.25.0.linux-amd64/bin/go test -race -count=1 ./pkg/contracts ./internal/control/...`：全部通过。

安全边界验证：`/home/elucid/go/pkg/mod/golang.org/toolchain@v0.0.1-go1.25.0.linux-amd64/bin/go run ./cmd/new-api-migrate` 在无
`GATEWAY_NEW_API_DATABASE_URL` 时按设计拒绝并提示 source database URL required，退出码为 1，
未建立连接或写入。

本轮没有设置 `GATEWAY_NEW_API_DATABASE_URL` 或 `GATEWAY_TEST_DATABASE_URL`，因此没有执行真实
new-api PostgreSQL schema 查询，也没有执行 target `--apply`。没有执行生产写入、部署或真实
provider 请求；缺少 provider 账号不是本轮阻塞。真实 PG 运行时仍需先应用现有
`migrations/001_initial.sql`，并由运维提供只读 source DSN、独立 target DSN 与 32-byte base64
credential key。

本轮最高剩余风险是不同部署的 new-api PostgreSQL schema 可能存在未覆盖的列类型或自定义表名；
实现会把可选列/表缺失记录为 warning，必需 channels 列缺失则停止，不能替代一次实际 source
database dry-run。provider 服务端行为与 quota schema 仍按 §C live-gate 保持未验证。

## A1 快照发布回路（2026-09-03）

本轮实现了 control 从 PostgreSQL 读取 active 账号、解密 credential、解析 profile/limits/quota/capabilities，按 `platform + group` 经既有 Redis `snapshot.Publisher` 发布；control 启动时执行一次，之后每分钟执行一次。发布前读取 Redis bucket epoch，仍由既有 Lua publisher 执行 expected-epoch CAS。账号变更与 quota 发布衔接均使用 miniredis 回归覆盖。

本轮实际执行并通过：

- `/home/elucid/go/pkg/mod/golang.org/toolchain@v0.0.1-go1.25.0.linux-amd64/bin/go test -count=1 ./internal/control ./internal/snapshot`
- `/home/elucid/go/pkg/mod/golang.org/toolchain@v0.0.1-go1.25.0.linux-amd64/bin/go test -count=1 -v ./internal/control -run 'TestSnapshotLoop|TestPGSnapshotRepository'`：快照首次发布、账号增删、最终账号删除后的空快照、quota 发布衔接通过；PG 测试因 `GATEWAY_TEST_DATABASE_URL` 未设置而严格 skipped。
- `/home/elucid/go/pkg/mod/golang.org/toolchain@v0.0.1-go1.25.0.linux-amd64/bin/go build ./cmd/gwd`：通过；随后删除本次生成的根目录 `gwd` 二进制。
- `/home/elucid/go/pkg/mod/golang.org/toolchain@v0.0.1-go1.25.0.linux-amd64/bin/go vet ./...`：通过。
- `/home/elucid/go/pkg/mod/golang.org/toolchain@v0.0.1-go1.25.0.linux-amd64/bin/go test -count=1 ./...`：全部通过。
- `/home/elucid/go/pkg/mod/golang.org/toolchain@v0.0.1-go1.25.0.linux-amd64/bin/go test -race -count=1 ./pkg/contracts ./internal/control/...`：全部通过。

本轮未配置 PostgreSQL，因此 PG SQL 查询与真实数据库连接未在本轮执行；未执行真实 provider、生产写入或部署。miniredis 与内存 repository 证据不等同于真实 Redis/PostgreSQL 验收。

提交尝试：`git add docs/TODO.md docs/EVIDENCE.md internal/control/run.go internal/control/snapshot_loop.go internal/control/snapshot_loop_test.go internal/control/snapshot_loop_integration_test.go` 因项目 `.git` 独立挂载为只读（`Read-only file system`，无法创建 `.git/index.lock`）失败；本轮未产生 commit。

## A2 导入入口（2026-09-03）

本轮新增 `gwd import --file=... [--dry-run]` CLI。JSON 文件直接解码为既有 `contracts.ImportRequest`，调用既有 `ImportService`、`PGImportRepository` 和 AES-GCM `credentials.Cipher`；dry-run 不连接或写入 PostgreSQL。CLI 只输出账号摘要（ID、provider、bucket、credential kind/version、fence epoch、dry-run 状态），不输出 access token、refresh token 或 static key。

本轮实际执行并通过：

- `/home/elucid/go/pkg/mod/golang.org/toolchain@v0.0.1-go1.25.0.linux-amd64/bin/go test -count=1 -v ./cmd/gwd -run 'TestRunImportCommand|TestImportCommandPublishesSnapshot'`：dry-run provider import 与凭据脱敏通过；PG + miniredis 导入→密文→快照→gateway chooser 测试因 `GATEWAY_TEST_DATABASE_URL` 未设置而严格 skipped。
- `/home/elucid/go/pkg/mod/golang.org/toolchain@v0.0.1-go1.25.0.linux-amd64/bin/go run ./cmd/gwd import --file=/tmp/gwd-a2-import.json --dry-run`：手工 dry-run 通过，输出为账号摘要且不含 static key。
- `/home/elucid/go/pkg/mod/golang.org/toolchain@v0.0.1-go1.25.0.linux-amd64/bin/go build ./cmd/gwd`：通过；随后删除本次生成的根目录 `gwd` 二进制。
- `/home/elucid/go/pkg/mod/golang.org/toolchain@v0.0.1-go1.25.0.linux-amd64/bin/go vet ./...`：通过。
- `/home/elucid/go/pkg/mod/golang.org/toolchain@v0.0.1-go1.25.0.linux-amd64/bin/go test -count=1 ./...`：全部通过。
- `/home/elucid/go/pkg/mod/golang.org/toolchain@v0.0.1-go1.25.0.linux-amd64/bin/go test -race -count=1 ./pkg/contracts ./internal/control/...`：全部通过。

本轮未执行真实 PostgreSQL CLI 导入；未执行真实 provider 请求或生产写入。dry-run 只证明 CLI 到既有 `ImportService` 的本地调用路径，不能替代 PG 集成证据。

提交尝试：`git add cmd/gwd/main.go cmd/gwd/main_test.go cmd/gwd/import_integration_test.go docs/TODO.md docs/EVIDENCE.md` 因项目 `.git` 独立挂载为只读（`Read-only file system`，无法创建 `.git/index.lock`）失败；本轮未产生 commit。

## A3 OAuth wrapper 派单与 PKCE executor（2026-09-04）

本轮把 `gwd import` 的 `auth_mode=pkce` 接到既有 wrapper Redis Stream：control 使用独立的 `GATEWAY_WRAPPER_ENVELOPE_KEY` 加密 job，等待 terminal 后解密 `TokenBundle`，再交给既有 `ImportService`。wrapper 生产入口默认使用 Codex PKCE executor，按 provider + operation 分发；`FixtureExecutor` 不再是隐式 fallback，只能通过 `GATEWAY_WRAPPER_EXECUTOR=fixture` 显式选择。PKCE executor 使用 64 字节随机 verifier、S256 challenge、32 字节随机 state 和 `localhost` 临时回调，校验 method/state/code，复用既有 provider HTTP client 交换授权码，并将失败归为稳定的 code/retryable 结果。默认 callback 预算为 5 分钟、lease 为 10 分钟，启动时拒绝 callback + exchange 预算不短于 lease 的配置。

公共协议参数通过 OpenAI 官方 Codex 仓库当前源码核对：`codex-rs/login/src/pkce.rs` 与 `codex-rs/login/src/server.rs`。实际执行 `curl -fsSL https://raw.githubusercontent.com/openai/codex/main/codex-rs/login/src/pkce.rs` 和 `curl -fsSL https://raw.githubusercontent.com/openai/codex/main/codex-rs/login/src/server.rs`，只读取公开源码，确认 verifier/challenge 算法、`http://localhost:<port>/auth/callback`、scope、state、`S256` 和标准 token form；未访问 OAuth authorize/token 服务，也未使用真实账号。

本轮实际执行并通过：

- 绝对路径 `gofmt -w`：A3 Go 文件格式化通过。首次直接执行裸 `gofmt` 因当前 `PATH` 中无该命令失败，随后使用项目指定 Go toolchain 的 `gofmt` 成功。
- `go test -count=1 -v ./internal/control/wrapper ./cmd/gwd ./internal/control`：Codex PKCE 本地 HTTP fixture、错误 state 拒绝、成功 callback/token exchange、拒绝授权分类、unsupported dispatch、worker failure 分类、CLI enqueue/terminal/import dry-run 全部通过；可选 PostgreSQL 与真实 Redis 测试因环境变量未配置按设计 skipped。
- `go test -race -count=1 ./internal/control/wrapper ./cmd/gwd`：通过。
- `go build ./cmd/gwd`：通过。
- `go vet ./...`：通过。
- `go test -count=1 ./...`：21 个包全部通过。
- `go test -race -count=1 ./pkg/contracts ./internal/control/...`：全部通过。
- `git diff --check`：通过。

`go build` 生成的仓库根目录 `gwd` 已确认是本轮产物。直接 `rm -f` 被执行环境安全策略拒绝，随后将其移动到可恢复路径 `/tmp/gwd-a3-build-20260904`；仓库未残留该二进制。

本轮未执行真实 Codex OAuth、未使用默认 `xdg-open`/系统浏览器完成登录、未连接 PostgreSQL 或真实 Redis，也未执行生产写入、部署或切流。本地 miniredis + httptest 证据只覆盖 job 派发、密文 envelope、PKCE 参数/回调、fixture token exchange 和错误分类。用户明确要求跳过当前 git 问题继续后续任务；项目 `.git` 仍为只读挂载，本轮未尝试 commit，也未产生 commit。

## A4 control→gateway 进程级端到端回路（2026-09-04）

新增 `cmd/gwd/process_e2e_test.go`。测试在可选 PostgreSQL 上执行真实导入和 SQL profile 配置，
启动独立 `gwd --role=control` 与 `gwd --role=gateway` 进程，共用 miniredis；本地
httptest provider 校验请求路径、Bearer fixture key 和 model，返回带 usage 的 OpenAI Chat
响应。测试随后轮询 PostgreSQL，断言对应 `request_attempts` 只有一条且为 `terminal`，
`usage_ledger` 只有一条且 `status_code=200`、`error_class=ok`、`tokens_in=3`、
`tokens_out=5`、`usage_source=upstream`、`partial=false`；再向 Redis Stream 注入相同
Release 事件，确认 control 消费后 ledger/attempt 仍各一条，且上游只收到一次请求。

本轮实际执行并通过：

- `/home/elucid/go/pkg/mod/golang.org/toolchain@v0.0.1-go1.25.0.linux-amd64/bin/go test ./cmd/gwd`：A4 测试编译通过；PG 用例因环境未配置而 skip。
- `/home/elucid/go/pkg/mod/golang.org/toolchain@v0.0.1-go1.25.0.linux-amd64/bin/go test -count=1 -v ./cmd/gwd -run '^TestProcessE2EControlGatewayRelease$'`：严格 skip（`GATEWAY_TEST_DATABASE_URL` 未设置），命令退出码 0。
- `/home/elucid/go/pkg/mod/golang.org/toolchain@v0.0.1-go1.25.0.linux-amd64/bin/go build ./cmd/gwd`：通过；生成的临时二进制已移至 `/tmp/gwd-a4-build-20260904`，仓库未残留构建产物。
- `/home/elucid/go/pkg/mod/golang.org/toolchain@v0.0.1-go1.25.0.linux-amd64/bin/go vet ./...`：通过。
- `/home/elucid/go/pkg/mod/golang.org/toolchain@v0.0.1-go1.25.0.linux-amd64/bin/go test -count=1 ./...`：全部通过。
- `/home/elucid/go/pkg/mod/golang.org/toolchain@v0.0.1-go1.25.0.linux-amd64/bin/go test -race -count=1 ./pkg/contracts ./internal/control/...`：全部通过。
- `git diff --check`：通过。

本轮未配置 PostgreSQL，因此 control/gateway 子进程、真实 SQL ledger 写入和 A4 的真实进程
断言未在本轮执行；也未执行真实 provider、生产写入、部署或切流。A4 代码路径已由编译、
静态检查和默认无凭据保护验证，但最高剩余风险是尚未在本地 PostgreSQL 上复现一次完整
进程回路。项目 `.git` 仍为只读挂载，按用户要求跳过提交问题，本轮未产生 commit。

## 独立审核轮（2026-09-03，仅工程基线与文档，未改动代码）

本轮不写业务代码，只处理版本控制缺失和文档结构问题。已执行并确认：

- **`git init` 建立仓库**：此前项目**无有效版本控制**——上层 `d1/.git` 只含 `info/exclude`，
  没有 HEAD/objects/refs，`git rev-parse` 在项目内报 `not a repository`。约 14,000 行代码
  在无版本控制下迭代过，无历史可保留。已在项目根目录 `git init -b main` 并做基线 commit
  （98 个文件），新增 `.gitignore`。未触碰上层坏壳。
- **`-buildvcs=false` 不再必需**：仓库建立后 `go build ./cmd/gwd` 直接通过（退出码 0）。
  本文下方历史记录中的该写法反映当时状态，现已过时。
- **提交前基线验证**：`go vet ./...`、`go build ./cmd/gwd`、`go test -count=1 ./...`
  全部退出码 0，21 个包通过。
- **新增 `AGENTS.md`**：此前 P2/P3 两轮均记录"仓库当前没有 AGENTS.md"但未创建。
- **文档拆分**：`docs/P3-STATUS.md` → 本文（证据）+ `docs/TODO.md`（进度）。

本轮代码审核发现的结构性缺口已全部写入 `docs/TODO.md` §A，均为 `[no-cred]`。
本轮没有执行真实 provider、PostgreSQL、Redis 或生产操作。

## 本轮复核与扩展（2026-09-03）

本轮继续限定在 P3 本地 admission，不进入 P4，不执行生产部署、切流、真实计费或真实 provider 请求。独立复核没有发现 wrapper claim/terminal、quota fence/outbox/reconcile 或 gateway quota/快照准入的新增正确性回归；现有针对性测试和本轮全量回归均通过。

本轮补齐了四个有本地协议依据的 provider 传输切片，并保持真实 provider 请求关闭：

- GitHub Copilot OAuth 导入、短期 Copilot token 刷新、官方 Chat Completions headers、request identity 和 usage/Release fixture；交互式 GitHub device authorization仍由 wrapper 负责。
- Antigravity OAuth token refresh、`/v1internal:generateContent` 与 `:streamGenerateContent?alt=sse` 的 Code Assist envelope、Gemini 文本响应组合和 project header fixture。`project_id` 与 OAuth `client_secret` 均由导入元数据显式提供并加密保存，不内置真实 secret。
- Kiro OAuth refresh、`/generateAssistantResponse` conversationState、原始 JSON event stream 文本解析、`messageMetadataEvent.tokenUsage` 精确 input/cache/output usage 和断流 drain。部分 tokenUsage 字段现在严格保持 missing，不会按零补齐。
- Windsurf API-key 的显式兼容 endpoint passthrough；必须由导入元数据提供 `protocol=openai_chat` 和绝对路径，拒绝远程 URL 或猜测 `server.codeium.com` 的私有 endpoint。

上述 provider 的本地 fixture 覆盖请求路径、鉴权/客户端 headers、文本回程、usage、终态 Release、OAuth refresh 或导入加密边界。quota 仍按已知 schema 处理：Antigravity 的 `remainingFraction`、Kiro 的精度字段、Copilot/Windsurf 未核实的账号 quota 不强行映射到当前整数 `QuotaInfo`，统一返回 missing。

本轮发现并修复一个高严重度 admission blocker：Gemini 静态 API-key provider 虽已注册，但 `ImportService` 仍只接受 OAuth/PKCE/device/CLI，拒绝 `auth_mode=api_key`，且把所有导入账号的 credential kind 固定写成 `oauth`；原 registry 又只按 provider kind 保存单一实现，不能同时解析 Codex/Claude 的 OAuth 与 API-key profile。这使静态 provider 无法可靠进入正常导入、quota refresh、快照和 gateway 调度链路。现已允许 `api_key`，只在该模式下生成 `static` credential，并按 `provider + auth_mode` 解析已注册实现；provider 入口拒绝 profile 与请求 auth mode 不一致。密钥仍以既有 AES-GCM envelope 保存，返回账号只携带 gateway 所需 access token。内存 repository 回归覆盖 Codex/Claude/Gemini/Grok/Windsurf 静态导入、密文解密、profile、credential kind 和错误模式拒绝。

在已有本地参考实现明确给出 `https://api.x.ai/v1/chat/completions`、Bearer API key 和 OpenAI Chat 请求/响应语义的边界内，新增 Grok 静态 API-key provider、注册项、官方 profile、provider HTTP fixture、gateway fixture 和项目结束 live harness 入口。成功响应必须携带 usage，否则沿用既有 failover/missing 策略；未核实账号级 quota endpoint，因此 `Quota()` 明确返回 missing，不伪造额度。未实现或声称 Grok OAuth、真实 quota 或真实服务端 admission。

非流式协议组合另补齐 Anthropic Messages 入站到 OpenAI Responses/Gemini 上游的响应回程：Responses → Chat → Anthropic 保留标准 function tool 与 input/output/cache usage，Gemini → Chat → Anthropic 保留单文本、终态和 usage。gateway fixture 同时覆盖请求转换、上游鉴权头和 terminal Release；没有扩展非 OpenAI 入站的跨协议 streaming、多模态或未知内容块。

上一轮文档核对还发现 `keyhive/docs/architecture.md` 的 wrapper checklist 仍写“真实 Redis ACL/进程验收 pending”，与 2026-09-02 的临时 Redis 7.0.15 历史证据不一致；已改为明确“历史临时验收通过、本轮未复现”，避免把历史证据误写成本轮执行结果。

## 上一轮收口（2026-09-02）

上一轮只做 P3 admission/验收收口，不进入 P4，不执行生产部署、切流或真实计费。仓库根目录没有 `AGENTS.md`；`.git` 目录只有空元数据，构建统一使用 `-buildvcs=false`，没有修复或删除 `.git`。

上一轮独立审核发现并修复了三个最小 P3 admission 问题：wrapper claim 的并发占有窗口、live harness 可指向 fixture base URL、以及 Claude Messages 缺少官方要求的 `anthropic-version: 2023-06-01` 请求头。修复分别位于 `internal/control/wrapper/queue.go`、`internal/control/provider/live_admission_test.go`、`internal/control/provider/profile.go`/`internal/gateway/server.go`，并有针对性回归测试；没有改动 P0/P1 数据结构、真实 provider endpoint 或 quota schema。此前发现并修正的文档口径问题是：`docs/AUDIT-CONTEXT.md` 原来仍写“纯设计阶段、尚无一行代码”，已改为说明当前已有本地实现切片，但不代表 P3 全量完成。

已补充显式 opt-in 的真实 provider smoke-test harness：`internal/control/provider/live_admission_test.go` 默认不联网。真实凭据测试固定为**项目结束验收阶段**执行，不作为当前 P3 本地实现的阻塞项；当前只验证 harness 的默认 skipped、凭据缺失保护和 fixture 拒绝。到项目结束时，只有同时设置 `GATEWAY_RUN_LIVE_PROVIDER_TESTS=1`、`GATEWAY_LIVE_CONFIRM=provider-admission` 及 provider/access token/model/输出预算后才执行；未提供 refresh token 或 quota path 时，仅对应 OAuth refresh/quota 子项 skipped。harness 只允许代码中已审计的官方 API base URL，不接受自定义或 loopback fixture；inference 还必须返回单一非空文本、终态和正向 usage，quota 必须返回带 `remaining` 的条目，不把 fixture、缺失字段或错误响应转成通过。

项目结束真实 provider 验收时的 harness 变量契约：必填 `GATEWAY_RUN_LIVE_PROVIDER_TESTS=1`、
`GATEWAY_LIVE_CONFIRM=provider-admission`、`GATEWAY_LIVE_PROVIDER=codex|claude|gemini|grok|copilot`、
`GATEWAY_LIVE_AUTH_MODE=oauth|api_key`（Gemini/Grok 仅 `api_key`，Copilot 仅 `oauth`）、
`GATEWAY_LIVE_ACCESS_TOKEN`、`GATEWAY_LIVE_MODEL`、`GATEWAY_LIVE_MAX_OUTPUT_TOKENS`；
可选 `GATEWAY_LIVE_REFRESH_TOKEN`、`GATEWAY_LIVE_QUOTA_PATH`、
`GATEWAY_LIVE_API_BASE_URL`（若设置，必须与该 provider 的已审计官方 base URL 完全一致；不能用于代理、fixture 或自定义 endpoint）。测试不会输出 token 或响应原文。

## 累计独立审核 findings

- 高：`internal/control/wrapper/queue.go` 原先在 `XCLAIM` 后才写 lease marker，并发 worker 可同时拿到同一 job；已增加 claim marker、pending 最小空闲时间和 `TestQueueConcurrentClaimsAreSingleOwner`，并将 terminal 写入/ack/lease owner 校验收敛到原子 Redis 脚本。
- 高：`internal/control/provider/live_admission_test.go` 原先接受任意 `GATEWAY_LIVE_API_BASE_URL`，本地 fixture 加假 token 即可让 inference/quota smoke test PASS；已限制为审计 profile 的官方 base URL，并补充拒绝自定义 base 的回归测试。没有真实凭据，本轮没有执行 live provider。
- 高（2026-09-03）：静态 API-key provider 已存在，但 `ImportService` 拒绝 `auth_mode=api_key`、把 credential kind 固定为 `oauth`，且 registry 无法同时保存同一 provider 的 OAuth/API-key profile；已改为 auth-mode-aware 解析，并增加四个静态 provider 的加密导入和模式错配回归。
- 中（2026-09-03）：OpenAI Responses 入站到 Anthropic 上游的请求转换已存在，但成功响应没有 Anthropic → Chat → Responses 组合，导致上游 2xx 后返回 502；已补最小组合转换和 gateway fixture，usage/Release 保持上游值。
- 中：Claude 官方 Messages 请求需要 `anthropic-version: 2023-06-01`；原实现和 fixture 均未覆盖，已在 provider headers、gateway 非流式/流式路径补齐，并由 fixture 断言。真实 Claude 服务端仍 pending。
- 文档证据缺口：`internal/control/quota_refresh.go` 的 quota outbox/reconcile 和 `internal/gateway` 的 Responses/Gemini/SSE 切片已有本地测试，但 `keyhive/docs/architecture.md` 的 P1 checklist 仍保留未勾选项；这些 checklist 不能作为当前实现状态的唯一依据。

当前 P3 已交付一组可运行的本地切片：`pkg/protokit` 的非流式纯文本协议互转、Codex/Claude quota 的 fixture adapter、quota control-plane 刷新闭环、quota snapshot 可恢复发布、gateway quota 快照准入、OpenAI Responses 非流式文本互转、OpenAI/Anthropic/Responses 原生 SSE streaming、标准 function tool 互转、Gemini GenerateContent 文本 fixture/provider，以及 Grok OpenAI-compatible 静态 API-key provider。

已完成并有回归测试：

- Anthropic Messages request ↔ OpenAI Chat Completions request；支持 system、max_tokens、temperature、top_p、stop_sequences。
- Anthropic message response ↔ OpenAI chat.completion response；usage input/output、cache read/write 字段保留。
- gateway 新增 `/v1/messages`，并按 `UpstreamProfile.Protocol` 或 provider 选择上游协议；Claude 上游使用 `/v1/messages`、`anthropic-version: 2023-06-01`，静态凭据使用 `x-api-key`。
- 非流式协议转换支持标准 function tool 与 Anthropic `tool_use/tool_result` 双向互转；仍拒绝 thinking、图像/未知内容块和无法无损表示的多候选响应。OpenAI Chat、Anthropic Messages、OpenAI Responses 已增加同协议 SSE streaming。
- OpenAI → Anthropic 请求要求显式正数 `max_tokens`，缺失时在调用上游前返回 400，避免猜测模型上限。
- 现有 OpenAI-compatible 默认路径保持不变；`UpstreamProfile` 只增加可选 `protocol` 与 `inference_path` 字段。
- Codex 与 Claude provider 增加可选 `QuotaPath`；通过独立 quota HTTP 请求复用各自鉴权头，响应严格限制为 1 MiB 并归一化为既有 `QuotaInfo`。
- Codex Bearer、Claude OAuth/API key 的 httptest fixture 已验证请求路径、鉴权头、额度缺失、非法项拒绝和响应大小边界；Redis snapshot round-trip 保留 quota 字段。
- fixture 使用语言无关的 `{"items":[...]}` JSON envelope，证明 quota 结构不依赖 Go 私有类型；真实 provider quota schema 尚未宣称已验证。
- `QuotaService` 复用账号 advisory lock 与 `fence_epoch`，只通过条件更新写入 PostgreSQL `accounts.quota`；失去 fence 时拒绝提交。
- 成功 quota 刷新可在完整 Redis snapshot 上只更新目标账号并原子发布新 epoch；缺失 quota（adapter 返回 `items: null`）保留已有值，避免未配置被误判为耗尽。
- control role 在配置凭据密钥时每 5 分钟按最多 100 个 active Codex/Claude 账号执行一批刷新；fixture 已覆盖成功、鉴权失败、缺失 quota、fence/snapshot 边界。
- quota 的 PostgreSQL 条件写入与 `quota_snapshot_outbox` 入队位于同一事务。即时 Redis 发布失败时，outbox 保留待发布 quota；control 每分钟 reconcile，按 1、2、4 分钟指数退避，最大 15 分钟。
- reconcile 在账号 advisory lock 下再次检查 `fence_epoch`：旧 epoch 仅删除 outbox，不发布 Redis。重复 quota 发布不会推进 snapshot epoch；outbox 只保存有限错误状态码，不保存原始错误文本。
- `quota_snapshot_reconcile_total{result}` 记录 published/retry/stale/busy/canceled 结果；内存/miniredis 回归覆盖失败恢复、旧 fence、取消、幂等发布与指标输出。可选 PostgreSQL + miniredis fixture 已加入，但需要显式测试数据库才会执行。
- gateway 选择账号时排除 `Remaining <= 0` 的账号级 quota，以及同请求 model 的模型级 quota；未知/缺失余量和不匹配 model 不作耗尽推断。全部可选账号因 quota 被排除时返回 429，且不调用上游、不写 `attempt_started` 或 `Release`。
- gateway 仅在 Redis active `snap:data` 版本的 TTL 内使用本地快照；Redis 刷新短暂失败可以保留 last-known-good，但该版本 TTL 到期后请求 fail-closed 为 503，不会继续使用内存中的凭据。gateway 不按 `reset_at` 猜测额度复位，也不在本地扣减 quota；权威更新仍由 control/provider 刷新完成。
- `openai_responses` 已加入 `protokit`：`/v1/responses` 入站可转换为 canonical Chat，再按 profile 转发到 Responses、Chat、Anthropic Messages 或 Gemini GenerateContent endpoint，并把当前受支持的单一 assistant 文本、function_call/function_call_output、`max_output_tokens`、completed/incomplete(max_output_tokens) 和 input/output/cache usage 保留回来；Anthropic 与 Gemini 的非流式单文本响应均通过 canonical Chat 组合转换为 OpenAI Responses，并有 gateway fixture 覆盖。Responses streaming 已支持同协议原生事件转发与 `response.completed` usage。continuation、图像和多候选转换仍拒绝。
- Anthropic Messages 非流式入站可经 canonical Chat 转发到 OpenAI Chat、OpenAI Responses、Anthropic Messages 或 Gemini GenerateContent；Responses 回程保留文本/标准 function tool/cache usage，Gemini 回程限定单文本。对应 gateway fixture 覆盖 upstream headers、终态响应和 Release。
- OpenAI Chat streaming 强制向上游发送 `stream_options.include_usage=true`；Anthropic Messages 从 `message_start/message_delta` 累加 usage，Responses 从 `response.completed` 读取 usage。三者均逐行转发 SSE，上游终态 usage 写入既有 `Release`，客户端断开后仍继续 drain，并标记 `partial`；未收到终态或 usage 时标记 missing。
- OpenAI Chat 入站在纯文本、无工具调用边界内可转换到 Anthropic Messages、OpenAI Responses 或 Gemini GenerateContent SSE；事件、usage、`[DONE]` 和 terminal Release 均有 fixture 覆盖。Anthropic/Responses 入站保留同协议原生 SSE，跨协议输出暂不扩展到任意入站组合，避免伪造无法无损表示的事件语义。
- Gemini 静态 API-key provider 已注册，使用 `gemini_generate`、`x-goog-api-key` 和带 `{model}` 的 GenerateContent 路径模板；文本 request/response、generation controls 与 `usageMetadata` 已由 fixture 验证。未配置或未核实的 Gemini quota 合法返回 missing，不伪造额度。
- Codex/Claude 的 OAuth 与 API-key profile 可同时注册；import、quota refresh 和 OAuth refresh 按请求或存储 credential kind 选取实现。Codex/Claude/Gemini/Grok 静态账号均可经 `ImportService` 加密导入并正确标记为 `static`。
- Grok 静态 API-key provider 已注册，使用已审计的 xAI official base、`/v1/chat/completions`、Bearer header 和 `openai_chat` profile；provider/gateway fixture 验证文本和 usage/Release。Grok 账号级 quota endpoint、OAuth 与真实响应仍未验证，保持 missing/pending。
- GitHub Copilot、Antigravity、Kiro 和 Windsurf 的本地 provider/adapter 切片已完成并有 fixture 回归；真实 OAuth、真实 inference 和真实 usage/quota schema 仍只在项目结束阶段验收。Windsurf 仅支持显式配置的兼容 endpoint，不宣称 `server.codeium.com` 存在公开 OpenAI endpoint。
- wrapper 本地队列/worker/进程入口和 Python JSON interop 回归通过：enqueue 幂等、claim、lease reclaim、旧 lease 拒绝、terminal 幂等、fixture execute/fail、SIGTERM 退出和跨语言密文 envelope 均有本地测试证据。

## 本轮实际执行并通过（2026-09-03）

以下命令均使用 `/home/elucid/go/pkg/mod/golang.org/toolchain@v0.0.1-go1.25.0.linux-amd64/bin/go`：

- `go test -count=1 ./internal/control/provider ./internal/control/wrapper`
- `go test -count=1 ./pkg/protokit ./internal/gateway`
- `go test -count=1 ./internal/control/provider/... ./internal/control`
- `go test -count=1 ./internal/control/provider/kiro ./internal/control ./internal/gateway`（本轮新增 Kiro usage 完整性、事件碎片和加密元数据回归）
- `go test -count=1 ./...`
- `go vet ./...`
- `go test -race -count=1 ./pkg/contracts ./internal/control/...`
- `go build -buildvcs=false ./cmd/gwd`

另以 `go test -count=1 -v ./internal/control/provider -run '^TestLiveProvider'` 显式核对 harness；执行前清空全部 `GATEWAY_RUN_LIVE_PROVIDER_TESTS` / `GATEWAY_LIVE_*` 变量，inference、OAuth refresh、quota 三项均按设计 skipped。这不是 provider 通过证据，也没有发出真实 provider 请求。

另以聚焦 `-run` 命令显式核对可选基础设施测试；在清空全部 `GATEWAY_TEST_DATABASE_URL` / `GATEWAY_TEST_REDIS_*` 变量后，PostgreSQL quota outbox reconcile、Redis wrapper 进程、Redis wrapper ACL/队列三项均因环境未配置而按设计 skipped。这些结果只证明默认保护有效，不是 PostgreSQL 或真实 Redis 的本轮通过证据。

上述命令全部退出码为 0；本轮未配置真实 provider、PostgreSQL 或通用 Redis 验收环境。build 前确认仓库根目录不存在既有 `gwd`，build 后已删除本次生成的二进制。

## 上一轮临时 Redis 验收（2026-09-02，非本轮复现）

上一轮只为验证修改后的 wrapper Lua/ACL 行为创建临时 Redis `7.0.15` 容器（映射 `127.0.0.1:16389`），配置独立 admin/control/wrapper/gateway ACL 身份；测试结束后已清理容器。没有创建或触碰 PostgreSQL 容器，也没有触碰现有 `new-api-dev-*` 容器。本轮没有创建或复用该环境。

以下临时 Redis 验收通过：

- Redis 7.0.15 ACL/进程生命周期：control enqueue；wrapper 子进程 claim、fixture execute、complete；重复 complete 幂等；SIGKILL 后 lease reclaim；旧 worker completion 被拒绝；gateway 无法读取 wrapper job、lease、terminal；Redis 版本断言。
- Redis 队列生命周期：enqueue 幂等、lease 到期接管、旧 worker 写入拒绝、terminal 幂等和 pending 清理。

上一轮临时 Redis 聚焦命令：

- 带临时 Redis ACL 环境变量的 `go test -count=1 -v ./internal/control/wrapper -run 'TestP2RealRedisWrapperProcessLifecycle|TestP2RealRedisQueueLifecycleAndGatewayIsolation'`

上述命令在上一轮通过。`docs/P2-ADMISSION.md` 中更广的 PostgreSQL/Redis/crash 结果仍保留为此前历史证据，本轮没有重新执行，不计入本轮新证据。

## 本轮 skipped

- live provider inference/OAuth refresh/quota：按项目流程留到结束验收阶段；当前未设置 `GATEWAY_RUN_LIVE_PROVIDER_TESTS`，默认严格 skipped。没有真实 OAuth/API 凭据，也没有伪造通过；这不阻塞当前 P3 本地可验证结论。Copilot 仅加入了 opt-in harness，Antigravity/Kiro/Windsurf 因协议 envelope 或 endpoint 需专用 harness，当前不联网。
- PostgreSQL + Redis quota/crash integration：未设置 `GATEWAY_TEST_DATABASE_URL` 及对应验收环境变量，严格 skipped；本轮未宣称 PostgreSQL 通过。
- wrapper Redis 7.0.15 ACL/进程验收：本轮未创建临时 Redis，未重新执行；仅保留 2026-09-02 历史证据。
- 其他真实 provider、未知 quota schema、多模态和非 OpenAI 入站任意跨协议 streaming：现有通用 profile 无法无损承载，或仍需要真实凭据才能定性，保持 pending。

> 未完成项清单已迁出本文，见 `docs/TODO.md`。本文不再维护待办。

## 本轮结论（逐项，不做全局封顶）

已由本轮命令证实：

- 本轮全量本地回归、`-race`、`go vet` 和 build 全部通过，退出码为 0。
- 上述"已完成并有回归测试"各条，在本地 fixture 边界内成立。

明确**未**由本轮证实（不等于未完成，只等于本轮无证据）：

- 任何真实 provider 的服务端行为：OAuth、refresh token 生命周期、inference、服务端拒绝条件、
  quota reset 语义、usage 字段。本地代码只对已审计 fixture 字段做严格转换。
- PostgreSQL 与真实 Redis 的本轮通过：均因环境变量未配置而严格 skipped。
- 临时 Redis 7.0.15 wrapper/ACL：仅有 2026-09-02 历史证据，本轮未复现。

以上任一条都**不构成**真实 provider 或生产准入结论。

## 本轮最高剩余风险

真实 provider 服务端行为仍未验证，尤其无法确认真实额度/usage schema 与本地 adapter 一致。
没有账号时，无法证明 refresh token 生命周期、服务端拒绝条件、quota reset 语义和 usage 字段
在真实服务端与 fixture 一致。

**注意**：该风险的处置方式是把相关任务标为 `[live-gate]` 并推迟到结束验收阶段
（见 `docs/TODO.md` §C），**不是**暂停其余开发。仓库内仍有大量不需要凭据的未完成工作，
见 `docs/TODO.md` §A —— 其中包含若干使系统当前无法端到端运行的结构性缺口。
