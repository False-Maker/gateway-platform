# 待办与进度（TODO）

> 本文是**唯一的任务清单**。证据和已执行记录在 `docs/EVIDENCE.md`，不要混写。
>
> **标签**
> - **`[no-cred]`** —— 不需要真实凭据 / 真实 provider / 生产环境即可完成并验证。
> - **`[live-gate]`** —— 必须有真实账号，统一留到项目结束验收阶段。
>
> **取任务规则**（另见 `AGENTS.md` §0）：
> 从 §A 开始按序取 `[no-cred]` 任务。遇到 `[live-gate]` 直接跳过，**不要因此停止工作**。
> 只要还有未完成的 `[no-cred]`，本项目就不处于"等待真实账号"状态。
>
> 当前状态：**A1-A5、B1、B2、B3 已完成，2026-09-06 审查修复、B2 多模态切片与 B3 quota 精度扩展已收口。** A5 的数据库实跑仍按可选 PostgreSQL 环境执行；缺少环境不等于缺少 provider 凭据，也不阻塞本地转换验证。
>
> **2026-09-10 复盘新增 A6、A7、A8（均 `[no-cred]`），并给 D1 定了解法。** 复盘发现设计文档要求但代码中不存在、且 TODO 从未列出的三个缺口：gateway 鉴权面/多 bucket、utls TLS profile、文档与代码漂移。D1、A6、A7、A8、A9、A10 已于 2026-09-10 完成。§A 全部收口。
>
> **2026-09-10 第二次复盘（结构分析）：**用与 A8 相同的口径把设计文档逐条对照代码，又找出四个"设计要求但从未进过任务清单"的缺口，已编号为
> **A11**（迁移 tenant 转正）、**A12**（请求明细存储）、**E1**（告警规则落点）、**E2**（wrapper 其余 executor 的 no-cred 部分）；
> 另把总览附录那条"每请求 PoW/turnstile 可能击穿热/慢拆分"从脚注提升为 **C6**——它的爆炸半径是契约级的，不该继续以附录形式存在。
> 同时把原来只有一行字的 B4 拆成 **B4.0–B4.5**，每项带 DoD。
>
> **下一步顺序（已定）**：~~`B4.0 定计费模型（不写代码）`~~（2026-09-10 完成，见 `docs/B4-BILLING-MODEL.md`）
> → ~~`A11 迁移 tenant 转正`~~（2026-09-11 完成）→ ~~`B4.1 计价表与钱包 schema`~~（2026-09-11 完成）
> → ~~`B4.2 扣费作业`~~（2026-09-11 完成）→ ~~`B4.3 UsageSource 可信度口径`~~（2026-09-11 完成）
> → ~~`B4.4 余额不足的执行点`~~（2026-09-11 完成）→ ~~`B4.5 对账`~~（2026-09-11 完成）。
> **§B4 到此闭合。**下一步在 `B5 控制台` 与并行项 A12 / E1 / E2 / C6(no-cred 部分) 之间取。
> A12/E1/E2 不阻塞 B4，可并行取。P4（B4）在 A11 之前不进入编码——迁移来的用户还停在 staging，没有可扣费主体。

---

## §A 端到端可运行性 —— 阻塞一切的结构性缺口

背景：截至 2026-09-03，本仓库有 10 个 provider 适配器、约 5,700 行测试、全绿的
`go test ./...`，但**没有一条端到端可运行的路径**。以下每条都不需要任何真实凭据。

### A1 `[no-cred]` control 缺少快照构建与发布回路 —— 最高优先级

**状态：已完成（2026-09-03）**

**完成前基线（历史）**
- `internal/snapshot/redis.go:74` 的 `Publisher.Publish` 实现完整、有测试。
- 其唯一非测试调用方是 `internal/control/quota_refresh.go:349`，位于
  `RedisQuotaSnapshotPublisher.PublishQuota` 内。该函数**先 `Load` 一个已存在的快照**，
  改掉其中一个账号的 quota，再发布；账号不在快照里就返回
  `account %s is missing from snapshot`（`quota_refresh.go:347`）。
- 全仓库没有任何 SQL 从 `accounts` 构建 `[]contracts.Account`。三处 accounts 查询分别是：
  待刷新 id 列表（`refresh_loop.go:49`）、按 id+epoch 取单账号（`refresh.go:61`）、
  quota 候选 id 列表（`quota_refresh.go:368`）。
- `internal/control/run.go:47` 的主循环只有：events 消费、trim、credential refresh、
  quota refresh/reconcile。**无快照发布 tick。**

**完成前结论（历史）**：快照发布器结构上依赖一个**没有任何代码去创建**的快照。
后果是 gateway 永远拿不到快照，按 fail-closed 设计永远返回 503。

**DoD**
- control 能从 PostgreSQL 读出某 bucket（platform+group）的全部可调度账号，
  组装为 `[]contracts.Account`（含 Credential / AccountLimits / QuotaInfo），
  经 `Publisher.Publish` 原子发布，并遵守既有 `expectedEpoch` CAS 语义。
- 加入 control 主循环的周期性 tick，并在启动时发布一次（否则冷启动仍无快照）。
- 回归测试：miniredis + 内存/可选 PG，覆盖首次发布、epoch CAS 冲突、账号增删、
  以及"快照存在后 `PublishQuota` 能成功"这一 A1↔quota 的衔接。

### A2 `[no-cred]` 账号无法进入系统：`ImportService` 没有调用入口

**状态：已完成（2026-09-03）**

**完成前基线（历史）**
- `internal/control/import.go:83` 的 `ImportService` 实现完整（加密落库、fence epoch 递增、
  auth-mode 解析），有 167 行测试。
- `grep ImportService` 在非测试代码中**只匹配到定义本身**，无任何调用方。
- `grep HandleFunc internal/control` 为空 —— control 角色没有任何 HTTP handler。
  它对外暴露的唯一端口是 metrics `:9091`（`run.go:98`）。
- `cmd/gwd/main.go` 没有 import 相关子命令。

**完成前结论（历史）**：账号只能由测试代码创建，运维无法把账号放进系统。

**DoD**
- 提供一个可用入口（二选一，建议 CLI 优先，避免过早引入鉴权面）：
  `gwd import --file=...` 子命令，或 control 的本地管理 HTTP 端点。
- 入口必须复用现有 `ImportService`，不复制其逻辑；凭据只经 AES-GCM envelope 落库。
- 端到端验证：导入 → PG 中出现加密 credential → A1 快照发布 → gateway 可选中该账号。

### A3 `[no-cred]` wrapper 生产入口跑的是 stub，且没有派单方

**状态：已完成（2026-09-04）**

**完成前基线（历史）**
- `internal/control/wrapper/run.go:28` 硬编码 `Executor: FixtureExecutor{...}`。
  `FixtureExecutor`（`worker.go:19`）自述为"P2 local stub"，只把密文原样复制回去。
- `Queue.Enqueue`（`queue.go:76`）**无任何非测试调用方** —— 没有代码会给 wrapper 派活。
- 真实的 PKCE / device / CLI / PoW / sentinel / turnstile executor 一个都没有实现。

**完成前结论（历史）**：wrapper 进程可以启动、可以 claim、可以 complete，但永远收不到活，
且即使收到也只会执行 stub。

**DoD**
- control 侧有代码在需要 OAuth 交互时 `Enqueue` job（至少打通一个 provider 的一种模式）。
- wrapper 侧按 job 类型分发到真实 executor；`FixtureExecutor` 降级为仅测试可选。
- 先做不需要账号的部分：PKCE 参数生成、回调处理、job 分发、错误归类，均可用本地 fixture
  HTTP server 验证。真实授权码交换属 §C。

### A4 `[no-cred]` 没有进程级端到端测试

**状态：已完成（2026-09-04）**

**完成前基线（历史）**：所有测试都是包内单元/fixture 测试。没有任何测试同时拉起 control + gateway
并让一个请求走完全程。A1/A2/A3 三个缺口能长期存在且不被发现，根因就是这一条。

**DoD**（建议作为 P3 真正的完成标志）
- 一个进程级 e2e：用一个**假的本地 provider**（httptest，非真实上游），走完
  `导入账号 → control 发布快照 → gateway 收到推理请求 → 选号 → 打"上游" → 返回响应
  → Release 事件 XADD → control 消费 → 写入 usage_ledger`。
- 断言：ledger 有且只有一条对应记录、usage 字段正确、attempt 幂等。
- 需要 PG + Redis，可用 miniredis + 可选 PG（环境变量缺失时 skip），但**必须**在
  有环境时真实跑通，不允许全程 mock 掉。

**实现**：`cmd/gwd/process_e2e_test.go` 在可选 PostgreSQL 下导入本地 Codex API-key fixture 账号，
把其 profile 指向 httptest provider，拉起独立 control/gateway 进程，验证快照、上游请求、
usage ledger 和重复 Release 事件幂等。Redis 使用进程内 miniredis，未配置
`GATEWAY_TEST_DATABASE_URL` 时严格 skip。

### A5 `[no-cred]` new-api 迁移脚本（策略 C）

**状态：已完成（2026-09-04）**

**现状**：`docs/AUDIT-CONTEXT.md` §1.3 已把迁移定得很细——三个易丢字段
（`setting.proxy`、codex `client_id`、多 key 数组→N 账号）、`source_system + source_id`
幂等键、`migration_runs` / `migration_records` staging 表。现已复用
`migrations/001_initial.sql` 中已有 staging 表，不重复新增 schema；实现位于
`internal/migration/newapi` 与 `cmd/new-api-migrate`。

**注意**：这项**不需要真实 OAuth 账号**。现网 new-api 库允许只读查询和 dry-run。

**DoD**
- 只读探测并读取 `channels/users/tokens/quota_data/groups`（缺失可选表/列进入 schema warning），一次性 CLI 默认 dry-run；`--apply` 才写目标库。
- 按 Codex OAuth、Claude/Gemini/Grok API key 分别映射；多 key 与多 group 展开为独立账号，`setting.proxy`、model mapping、能力和状态保留，无法映射的类型/状态/凭据进入 rejected 并记原因。
- Codex OAuth JSON 保留 access/refresh/id token、account id、email、过期原文；目标凭据只经 AES-GCM 加密，staging/raw summary 只存 digest，不存明文。
- users/tokens/quota/groups 写入现有 `migration_records` staging（目标当前无 tenant/principal 表，记录标为 `staging_only`）；quota/balance 只保留源单位/原始值，`contracts.QuotaInfo` 不补零。
- dry-run 输出 provider/status/unknown type/rejected/duplicate key/quota digest/schema warning 摘要；源库事务使用 PostgreSQL read-only，生产源库不写入。

### A6 `[no-cred]` gateway 没有鉴权面，单 provider / 单租户 / 单 bucket

**状态：已完成（2026-09-10）。** 实现摘要：`migrations/002_tenants.sql` 新增 tenants / principals /
tenant_tokens（只存 SHA-256）；control 每 15s 把活跃 token 哈希发布到 Redis `snap:auth:tokens:v1`
（原子 RENAME），gateway 只读该键派生 `AuthContext{TenantID, PrincipalID, TokenID, Group}`，三个推理路由
全部先鉴权，请求体中的 tenant/group 字段被忽略；`gwd tenant create` 生成 token 且只打印一次。gateway 改为
装载 `snap:bucket_registry` 中全部 bucket，`Lease.Provider` 由账号带出，按 bucket 独立维护 epoch/冷却/新鲜度。
`GATEWAY_TENANT_ID` 已删除；`GATEWAY_PROVIDER` 仅作无 provider lease 的兜底。
证据见 `docs/EVIDENCE.md`。**未做**：per-tenant 限流、粘滞会话按 tenant 隔离（`SessionKey` 仍未接线）、
A5 `staging_only` 记录转正为 tenants 行（迁移侧后续任务）。

**基线**
- 总览 §6.4.1 要求 gateway 由内部 `AuthContext` 决定 `TenantID`/用户/分组；代码中 `grep AuthContext` 为空。
- `internal/gateway/run.go` 三个路由（`/v1/chat/completions`、`/v1/messages`、`/v1/responses`）无任何鉴权。
- `TenantID`、`Provider`、`Group` 均来自环境变量，固定为单值；gateway 只装载 `provider:default` 一个 bucket
  （`run.go:25`、`run.go:39`）。快照分桶设计因此在 gateway 侧未被使用。
- `migrations/001_initial.sql` 没有 tenants / principals / tenant tokens 表；A5 迁移只能把 users/tokens 标为 `staging_only`。

**结论**：当前系统无法替换 new-api——它不能区分调用方，也不能同时服务多个 provider。这是 P4 计费的硬前置。

**DoD**
- 建表：tenants、principals、tenant API tokens（只存 hash）。A5 的 `staging_only` 记录可据此转正。
- gateway 入站校验 Bearer token → 派生 `AuthContext{TenantID, PrincipalID, Group}`；客户端请求体中的同名字段忽略。
- gateway 装载 `snap:buckets` 中全部 bucket，按 `AuthContext.Group` + 请求 model 选 provider/bucket；
  单 provider 环境变量模式保留为兼容默认。
- Release 的 `TenantID` 来自 `AuthContext`，不再来自环境变量。
- 回归：未授权 401、跨 tenant 粘滞会话隔离、多 bucket 选号；A4 进程级 e2e 改为带 token 调用。

### A7 `[no-cred]` utls TLS profile 注册表缺失

**状态：已完成（2026-09-10）。** `internal/gateway/tlsprofile.go` 引入 `refraction-networking/utls v1.6.7`，
注册表含 `codex_rustls`（Codex CLI rustls）与 `node24`（Claude Code / Node.js 24），均从 elucid-relay
`codex_tls.go` 搬入；未知名字返回 `ErrUnknownTLSProfile` 且不拨号。非流式与流式上游 client 按 lease 的
`TLSFingerprint` 建 transport；带 proxy 时自行发 CONNECT 隧道，指纹握手终止在源站。control 侧
`CodexChatGPTProfile` 标 `codex_rustls`、Claude OAuth profile 标 `node24`，API-key profile 留空走标准
crypto/tls。证据见 `docs/EVIDENCE.md`。**未做**：真实上游是否接受该指纹（C3/C5）、WebSocket 路径、
指纹随官方客户端版本更新（D4）。

**基线**
- 总览 §5 把 `UpstreamProfile.TLSFingerprint` 定为两角色唯一深耦合点；fluxgate §5 有未勾选的
  "TLS profile 注册表 + codex_rustls" 交付物。
- `go.mod` 无 `refraction-networking/utls` 依赖，代码无任何引用。`TLSFingerprint` 目前写而不读。

**DoD**
- 引入 utls，建立 profile 名 → `ClientHelloSpec` 注册表；未知名字 fail-closed 拒绝出站。
- 从 `elucid-relay/services/gateway-api/internal/httpserver/codex_tls.go` 搬入 `codex_rustls`。
- gateway 非流式与流式上游 client 按 lease 的 `TLSFingerprint` 选 transport；空值沿用标准 `crypto/tls`。
- 回归：本地 TLS fixture server 记录 ClientHello 的 cipher suite / extension 顺序并断言；
  真实上游是否接受该指纹属 §C（C3/C5）。

### A8 `[no-cred]` 设计文档与代码漂移

**状态：已完成（2026-09-10）。** 下列各条已在对应文档改为与代码一致；设计决策未动。`run.go` 的 reconcile
重叠 tick 仅记录，未改代码。仍未勾选的 checklist 项（control 侧 ErrorClass 权威层、平台级速率告警、
`UsageSource=missing` 告警规则）是真实缺口，不是文档漂移。

已核对的不一致（只改与代码不符的描述，不改设计决策）：
- `keyhive/docs/architecture.md` §2：目录写成每 provider 四文件（strategy/profile/quota/const）和
  `github_copilot/`，实际为单文件和 `copilot/`；§6 写 `internal/control/modules/*` 分层，实际扁平。
- `keyhive/docs/architecture.md` §3.3/§5：命令写 `keyhive wrapper run`，实际 `gwd --role=wrapper` 或 `gwd wrapper run`。
- `keyhive/docs/architecture.md` §4/§5：建表清单含 `import_templates`、`request_logs`，migration 中不存在；
  P1 checklist 多项未勾选但 `docs/EVIDENCE.md` 已记录实现。
- `docs/P2-ADMISSION.md` 末段仍写"仓库当前没有 AGENTS.md"。
- `internal/control/run.go:131` quota tick 内的 reconcile 与 `run.go:138` 独立 reconcile tick 重叠，无害但冗余；顺手记录，是否合并由 A6 之后决定。

### A9 `[no-cred]` control 侧 ErrorClass 权威层 + 平台级速率告警

**状态：已完成（2026-09-10）。** 对应总览 §6.3.1 "control 持久处理" 列与规则 2/3：

- `migrations/003_account_health.sql`：`accounts` 新增 `consecutive_failures`、`cooldown_until`、`excluded_models`。
- `internal/control/health.go`：单条 SQL 在 ledger 事务内按 ErrorClass 落权威判罚——`ok` 清零；`auth_invalid`
  禁用；`forbidden_capability` 按模型追加 `excluded_models`，不冷却整号；`rate_limited_unknown` 连续 3 次才
  5 分钟冷却；`blocked` 60 秒冷却；`forbidden_transport` / `upstream_5xx` / `network_error` / `auth_expired` /
  `rate_limited_known` 只记 last_error，不罚号。重复投递不重复判罚。
- `PlatformSignals`：60 秒窗口内同平台 ≥2 个不同账号出现 `forbidden_transport`/`blocked` 即置
  `control_platform_alert{provider,reason=transport_rejections}=1`，另有
  `control_platform_error_total{provider,class}`、`control_platform_transport_rejections_accounts{provider}`。
- 快照循环排除 `cooldown_until > now()` 的账号，并把 `excluded_models` 发布到快照；gateway chooser 对
  `ExcludedModels` 中的模型不选该号。
- 防饿死闸 `ReleaseStarvedCooldowns`：某平台冷却占比 >50% 时释放最早到期的一个冷却，每次快照 tick 执行，
  计数 `control_platform_starvation_release_total{platform}`。
- 所有 PG 测试改为 `migrations.Apply` 统一应用全部迁移。

证据见 `docs/EVIDENCE.md`。**未做**：告警规则本身（Prometheus alert / 通知）属部署侧；`auth_expired` 由
refresh loop 结果驱动的语义已存在于 `refresh.go`，本轮未改；冷却时长为文档定值，未经真实上游回调。

### A10 `[no-cred]` 粘滞会话按 tenant 接线 + per-tenant 限流

**状态：已完成（2026-09-10）。**

- **粘滞会话**：客户端可选带 `X-Session-Key` 头；gateway 以 `gateway:sticky:<tenant>:<sha256(session)[:16]>`
  在 Redis 记 pin，TTL 取账号 `AccountLimits.StickyTTL`，为 0 时默认 5 分钟。选号时若 pin 的账号仍可调度
  （未冷却、未排除、未被 failover 排除）则优先它，否则正常选号并重新 pin。键按 tenant 命名空间，同一
  session key 在另一 tenant 下无效；原始 session key 不落 Redis；超长或含控制字符的 key 返回 400。
  Redis 不可用时粘滞退化为普通选号，不影响正确性。
- **per-tenant 限流**：`tenants.max_concurrency` / `tenants.rpm`（`004_tenant_limits.sql`，0 = 不限）随
  token 快照下发到 `AuthContext.Limits`；gateway 在选号前用同一段原子 Lua 对
  `gateway:tenant:concurrency:<tenant>` / `gateway:tenant:rpm:<tenant>:<minute>` 计数，超限 429 且错误
  文案标明 tenant。tenant 限流始终 fail-closed（Redis 失联不放行）。`gwd tenant create` 新增
  `--max-concurrency` / `--rpm`，缺省不改既有值。
- `AuthContext` 经 request context 传入 handler，未改 `Handle*` 签名。测试 ACL 的 gateway 身份补 `+set`。

证据见 `docs/EVIDENCE.md`。**未做**：按 principal 或按 token 的更细粒度限流；粘滞 pin 在 gateway 多实例
间天然共享（Redis），但未做 pin 与账号删除的主动清理，靠 TTL 过期。

### A11 `[no-cred]` A5 迁移的 `staging_only` 记录转正为 tenants / principals 行

**状态：已完成（2026-09-11）。** `BuildPlan` 产出 `Plan.Tenants` / `Plan.TenantTokens`，`Apply` 在同一事务内 upsert
tenants / principals / tenant_tokens；摘要新增 `tenant_count` / `principal_count` / `tenant_token_count`。
两点需要知道的取舍：(1) token hash 取的是 **`sk-<key>`** 形式——new-api 库里存的是不带前缀的裸 key，客户端拿到的是
`sk-` 前缀形式，其鉴权中间件会剥掉前缀；只迁这一种形式，**不**复刻 new-api 同时接受裸 key 的宽松回退，否则一份凭据有两个
撤销点。(2) tenant / principal / token 的 id 是 `source_system + source_id` 的哈希，不是 `tenant-<user id>`，避免与
`gwd tenant create` 手工选的 id 撞车。证据见 `docs/EVIDENCE.md`。

**基线**
- A5 实现时目标库还没有 tenants 表，因此 `internal/migration/newapi/plan.go` 的 `addSourceRecords`
  把 users / tokens 一律写成 `migration_records` 里的 `staging_only` 行，没有任何目标实体。
- A6 的 `migrations/002_tenants.sql` 之后建了 tenants / principals / tenant_tokens，但**迁移侧没有回头补这一步**。
  A6 条目把它记在"未做"的小字里，从未升为任务。
- 总览 §6.1 的映射表明确要求：一个 new-api user → 一个 tenant + 一个默认 principal，`tokens` → tenant API token
  （只迁 hash / 过期 / 撤销状态）。

**结论**：迁移来的用户目前没有可扣费主体。B4 若在此之前编码，只能对着空的 tenants 表做计费。

**DoD**
- `BuildPlan` 为每个 source user 产出 planned tenant + 默认 principal，沿用 `source_system + source_id` 幂等键；
  `Apply` 在同一事务内 upsert 到 tenants / principals，重复执行不新建。
- `tokens` 只迁移 hash、过期、撤销状态到 `tenant_tokens`；**明文 key 不得进入 staging、日志或 records**
  （现有 `TestBuildPlanStagesUsersTokensQuotaWithoutSecrets` 的断言必须继续成立并扩展到新路径）。
- 无法映射的 user / token（缺 id、状态未知）进 rejected 并记原因，不猜测、不补默认租户。
- quota / balance 仍只保留 source unit 与原始值，本任务**不做任何换算或扣费**——那是 B4。
- 回归：dry-run 摘要新增 tenant / principal / token 计数；PG 下断言重复 apply 幂等、明文不落库。

### A12 `[no-cred]` 请求明细存储缺失（总览 §6.4 的 P0 级共享决策）

**状态：未开始。不阻塞 B4，可并行。**

**基线**
- 总览 §6.4 把"计量流水"与"高频请求明细日志"的**物理分离**定为 P0 级共享决策，理由是 control 的
  单写者路径不能随 RPM 增长；§6.5 又把"实时请求日志"列为控制台三个预留钩子之一。
- 全仓库 `clickhouse|request_logs|detail_log` 零命中，migration 中也没有对应表。
  A8 已确认 `request_logs` 在 `keyhive/docs/architecture.md` 的建表清单里但实际不存在。

**结论**：这是一条 P0 级决策，但从未进入过任务清单。当前所有 Release 都只落 `usage_ledger`，
一旦 RPM 上来，"control 负载只随账号数增长"这个立论就不成立。

**DoD**
- 先定形态再动代码：明细走独立可丢 / 可抽样存储，**不进控制状态 PG**、不经单写者路径。
  选型（ClickHouse / 抽样落文件 / 直接丢弃 + 仅保留指标）需先写一段决策记录，不默认引入新中间件。
- 决策为"引入存储"时：写入路径与 `usage_ledger` 事务解耦，明细写失败不得影响计量与 ACK。
- 决策为"暂不引入"时：显式在文档中记为已评估的取舍，并保留采样指标，**不留空白**。
- 凭据、token 明文、refresh token 不得进入明细。

---

## §B 功能扩展（不阻塞 §A，全部 `[no-cred]` 除非另注）

- **B1** `[no-cred]` 非 OpenAI 入站到其他协议的跨协议 streaming。当前只有 OpenAI Chat 入站
  可跨协议出 SSE；Anthropic/Responses 入站只保留同协议原生 SSE。
  **状态：已完成（2026-09-06）**。Anthropic Messages 与 OpenAI Responses 入站可在纯文本、
  无工具调用边界内转发到 OpenAI Chat、Anthropic Messages、OpenAI Responses 或 Gemini
  GenerateContent 上游；跨协议请求通过 canonical Chat 组合，客户端回程生成对应协议的文本、终态
  与 usage 事件，同协议仍原样透传。上游 usage 继续写入 terminal Release，工具调用和多模态仍由
  后续任务处理。
- **B2** `[no-cred]` 多模态内容块（图像等）。
  **状态：已完成（2026-09-06）**。非流式 canonical Chat 图像块支持 OpenAI
  `image_url`（data URL/绝对 http(s) URL）、Anthropic `image`（base64/url）、Responses
  `input_image` 与 Gemini `inlineData/fileData` 请求互转；Anthropic、Gemini 和 OpenAI Chat
  可表示的非流式图像响应也能回到 canonical Chat。严格拒绝非图像 MIME、非法 base64、空图像字段、
  流式图像事件和 Responses 无稳定 MIME 的图像生成输出，避免把不可无损表示的内容静默丢弃。
- **B3** `[no-cred]` `QuotaInfo` 表达能力：当前整数字段无法承载 Antigravity 的小数
  `remainingFraction` 与 Kiro 的精度字段，因此这些 quota 只能返回 missing。
  **状态：已完成（2026-09-06）**。契约保留旧整数字段，并新增不经 `float64`/整数转换的十进制
  `remaining_fraction`、`*_exact` 与 `precision` 字段；Antigravity `fetchAvailableModels` 和 Kiro
  `getUsageLimits` 仅映射已核实字段，未知 envelope/缺失字段保持 missing。快照 JSON、PG quota/outbox
  与 gateway 选号保留精度；选号只在明确余量为零或负数时排除账号。真实 provider schema 对账仍属 C2。
- **B4** P4 范围：真实用量扣减、调度深化、多租户计费。依赖 §A 打通后才有意义。
  **状态：B4.0–B4.5 全部完成（2026-09-11）。**§B4 到此闭合；`§B` 剩下的是 B5（P6 控制台）。

  - **B4.0** `[no-cred]` **定计费模型（只出文档，不写代码）**。
    **状态：已完成（2026-09-10）。决策记录见 `docs/B4-BILLING-MODEL.md`。**
    结论摘要：token 计价、四类分别单价；单价放 PG 表按 `(provider, model, effective_from)` 组织，
    改价只向未来生效且历史行不可变；预付余额钱包；余额不足**拒绝新请求**，余额状态经 A6 已有的
    `snap:auth:tokens:v1` 通道随 `TokenRecord` 下发，gateway 热路径零 PG、零 control 调用，
    接受"快照周期 + 扣费作业周期 + 在途请求"这一有界透支窗口，并要求预付租户 `rpm` 非 0 给它封顶；
    找不到单价的 ledger 行进 `unpriced`，不按 0 计费。被否决的选项与理由见记录正文，
    B4.1–B4.5 的 DoD 增补见其 §2。
    这一项的产物是一段可评审的决策记录，不是实现。必须回答清楚：计费单位（token / request / credit）与
    输入、输出、cache read、cache write 四类 token 的**分别单价**；单价的存放位置（配置 / PG 表 / 随
    provider 走）与生效时间语义（改价是否追溯）；钱包是预付余额还是后付账单；余额不足时的行为
    （拒绝新请求 / 允许透支到阈值 / 仅告警）——这条直接决定 gateway 是否需要在热路径读余额，
    影响 §1 的"热路径不调用 control"立论，**必须显式回答**。
    **DoD**：决策记录进 `docs/`，含被否决的选项与否决理由；B4.1–B4.5 的 DoD 据此细化。

  - **B4.1** `[no-cred]` 计价表与钱包 schema。
    **状态：已完成（2026-09-11）。** `migrations/005_billing.sql`（`model_prices` / `tenant_wallets` /
    `wallet_transactions`）+ `internal/control/billing.go`（`PGBillingRepository`）。
    单价不可变由**数据库 trigger** 承载，不只靠应用层——手工 psql 也改不动历史价；
    仓储不提供任何改价/删价入口，改价只能是一条 `effective_from` 更晚的新行。
    金额全程 `contracts.Decimal` ↔ `NUMERIC(38,12)`（SQL 里 `$n::numeric` / `col::text`），
    余额加减交给 PG 的 numeric 运算，Go 侧不做浮点算术。
    **DoD**：新增 migration（单价表 + 钱包/账单表），单价带生效时间且历史单价不可变；
    钱包余额为精确十进制，**不经 `float64`**（沿用 B3 已建立的 `contracts.Decimal` 口径）；
    重复应用 migration 幂等。
    **本轮未做**：未写入任何真实单价数值（业务定价，非工程决策）；
    new-api quota 整数单位 ↔ 货币金额的换算比例**仍未核对**（B4-BILLING-MODEL D4 的遗留项），
    充值入口留在 B5。

  - **B4.2** `[no-cred]` 从 `usage_ledger` 到扣费的慢路径作业。
    **状态：已完成（2026-09-11）。** `migrations/006_usage_billing.sql`（`usage_ledger` 加
    `billing_state`/`billed_amount`/`billing_run_id` + `billing_runs`）+
    `internal/control/billing_job.go`（`BillingJob`），挂在 control 的 select 循环上，5 分钟一次。
    每个 tenant 一个事务：取价 → 扣钱包 → 标记行已计费，一起提交或一起回滚；
    单个 tenant 失败（例如没有钱包）只让它自己的行留在 `pending`，不影响其他 tenant。
    取价按行的 `occurred_at`，不是作业运行时刻；取不到价的行进 `unpriced` 且**不**扣费。
    价格表为空时整轮 `skipped`——否则先上作业后配价会把全部用量永久打成 `unpriced`（D3 不允许补价）。
    **DoD**：control 慢路径按 tenant 聚合未计费的 ledger 行 → 乘单价 → 扣钱包，**热路径零参与**；
    扣费与"标记 ledger 行已计费"在同一 PG 事务；作业重跑不重复扣费（幂等键覆盖到 `event_id`）。
    **本轮未做**：只对 `usage_source='upstream' AND partial=false` 的行扣费，其余一律留 `pending`
    交给 B4.3，不做隐式默认；余额扣成负数不拦（B4.4）；三方对账未做（B4.5）；
    new-api quota 整数单位 ↔ 货币金额的换算比例**仍未核对**（B4-BILLING-MODEL D4 的遗留项）。

  - **B4.3** `[no-cred]` `UsageSource` 可信度口径。
    **状态：已完成（2026-09-11）。** 证据见 `docs/EVIDENCE.md` 同名小节。
    总览 §6.2 要求计费层用 `UsageSource` 区分可信度，但**没定不可信时怎么办**——这是 B4.0 的遗留问题。
    **DoD**：`upstream` / `estimated` / `missing` 与 `Partial=true` 四种组合各自的计费行为明确落地
    （计费 / 不计费 / 挂起待人工），**不得由代码隐式默认**；`missing` 的量单独可查询、单独告警，
    不被计入正常收入。
    结论摘要：处置表以 `(usage_source, partial, 是否成功)` 三元组为键、12 种组合逐条写死在
    `internal/control/billing_policy.go`，穷举性由测试断言；表外的组合一律 `held` 并单独计数，
    不猜、不按零补齐。用户拍板的三条口径：**流式截断但上游给了 usage → 照常计费**、
    **请求失败但上游给了 usage → 不计费**、**请求成功但 usage 丢失 → 挂起待人工并单独告警**。
    `billing_state` 增加 `not_billable`，与 `held` 分开：前者是"已判定为零"，后者是"等人判"，
    合并会把真实漏收埋进网络错误堆里；`unpriced`（我们的配置问题）与 `missing`（上游的数据问题）
    也保持两个桶不合并。
    **本轮未做**：`estimated` 仍无生产者，只留口径不造数据；`held` 的人工处理入口未做，
    只有 `control_billing_held_rows{usage_source}` 这个 gauge 和按 `usage_source` 的索引供查询；
    余额扣成负数仍不拦（B4.4）；三方对账仍未做（B4.5）；
    new-api quota 整数单位 ↔ 货币金额的换算比例**仍未核对**（B4-BILLING-MODEL D4 的遗留项）。

  - **B4.4** `[no-cred]` 余额不足的执行点。
    **状态：已完成（2026-09-11）。** 证据见 `docs/EVIDENCE.md` 同名小节。
    **DoD**：按 B4.0 的结论实现。若结论是"拒绝新请求"，则必须走**已有的 tenant 快照下发通道**
    （A6 的 `snap:auth:tokens:v1` 已经在下发 tenant 限额，余额状态同理），
    **不允许 gateway 在热路径同步查 PG 或调用 control**。
    结论摘要：`snapshot.TokenRecord` 增加 `BillingBlocked`，control 在 15s publish tick 上
    用 `LEFT JOIN tenant_wallets` 算出 `balance <= 0`（**无钱包行的租户永不被拦**）；
    gateway 在鉴权中间件里返回 **402**，文案带 tenant，计 `gateway_billing_rejected_total{tenant}`，
    因为拦在 handler 之前，所以结构性地**不打上游、不产生 Release 事件、不写 `usage_ledger`**。
    "未查 PG / 未调 control"由 import 闭包断言证明（覆盖所有代码路径，不止被测到的那些）。
    在途请求不杀；敞口上界 ≈ `峰值花费速率 × (快照周期 + 扣费周期 + 在途时长)`。
    **本轮未做**：`rpm = 0` 的预付租户敞口无上界，只上报
    `control_billing_uncapped_wallet_tenants` gauge + log 点名，**不自动处置**，
    告警规则本身留给 E1；三方对账未做（B4.5）；
    new-api quota 整数单位 ↔ 货币金额的换算比例**仍未核对**（B4-BILLING-MODEL D4），
    因此"余额 ≤ 0"的绝对刻度仍未验证。

  - **B4.5** `[no-cred]` 对账。
    **状态：已完成（2026-09-11）。** 证据见 `docs/EVIDENCE.md` 同名小节。
    `internal/control/billing_reconcile.go` 的 `Reconciliation.Run(start, end)` 在**一个
    `pgx.ReadOnly` + `RepeatableRead` 事务**里读三方：窗口内 `usage_ledger` 按 `billing_state`
    的行数与 `billed_amount` 求和、这些行所属 `billing_run_id` 的 `wallet_transactions` 扣款、
    `tenant_wallets.balance` 与该租户**全量**流水求和。
    窗口取 `occurred_at`（用量发生时刻），**不**取扣费时刻——两者是不同的钟，按扣费时刻切窗
    会在每个边界上造出并不存在的差额；窗口行改为顺着 `billing_run_id` 与该次运行的扣款比。
    `unpriced` / `held` / `pending` / `not_billable` 单独列进 `UnsettledRows` 并带 `event_id`，
    不计入收入也不被抹平。差额分六类（`run_debit_mismatch` / `wallet_balance_drift` /
    `billed_row_without_amount` / `billed_row_without_run` / `amount_on_unsettled_row` /
    `missing_wallet`），能归因到请求的都带 `EventIDs`。
    **DoD**：给定一段时间窗，能对齐 ledger 行数、扣费总额、钱包变动三者；差额可解释到具体
    `event_id`；对账本身只读，不修数据。
    **本轮未做**：只提供库内 API，没有 CLI/HTTP 入口，也没有定时运行与指标（告警仍属 E1）；
    只比较金额，不重算单价（不做"应收 vs 实收"的重新定价核对）；
    不校验 `wallet_transactions.balance_after` 的逐行链条（同 `created_at` 的顺序不唯一，
    会造假阳性），只用与顺序无关的"余额 == 流水求和"这一恒等式；
    `held` 的人工处理入口仍未做（B4.3 遗留）；`estimated` 仍无生产者；
    new-api quota 整数单位 ↔ 货币金额的换算比例**仍未核对**（B4-BILLING-MODEL D4 的遗留项）。

- **B5** P6 范围：控制台。

---

## §C 真实凭据门禁 `[live-gate]` —— 全部推迟到项目结束验收阶段

**遇到本节任务一律跳过，回到 §A。不要因本节而停止工作、不要索要账号。**

- **C1** 各 provider 真实 OAuth 授权、refresh token 生命周期与轮换行为。
- **C2** 真实 usage / quota schema 与本地 adapter 的一致性对账
  （Grok / Gemini / Copilot / Windsurf 账号级 quota endpoint 均未核实）。
- **C3** 真实服务端拒绝条件与 `ErrorClass` 分类校准、429 退避真实语义。
- **C4** Copilot device authorization 端到端流程。
- **C5** Antigravity / Kiro / Windsurf 的真实协议 harness。
- **C6** **codex CLI 的 API 端点是否需要每请求 PoW / turnstile**。
  **爆炸半径是契约级的，不是一个渠道的适配问题** —— 这是它区别于 C1–C5 的地方，取任务时优先核实。
  出处：总览"附：未决架构问题"与 `fluxgate/docs/architecture.md` §7。原文标注"P2 打通 codex 前必须核实"，
  但 P2、P3 都已过去，这条既未核实也未关闭，一直以附录脚注形式存在——2026-09-10 复盘将其提升为编号任务。
  **若结论为"需要每请求 challenge"**：`UpstreamProfile` 这个**静态**配方就覆盖不了，
  受影响的是两角色唯一深耦合点（总览 §5），要么给该 provider 开一条 gateway 可调用的 per-request minter 旁路，
  要么承认该渠道不适用无状态热路径模型。两条路都会改契约，**不要在核实前先写实现**。
  **可先做的 no-cred 部分**：读 `chatgpt2api/services/proxy_service.py` 与 `utils/pow.py`
  确认其 PoW 调用点究竟在 `chat-requirements` 还是仅在登录，把结论写进文档；
  真正的端到端确认需要真实 codex 账号，属 live-gate。

harness 契约（变量、约束）见 `docs/EVIDENCE.md` 中"项目结束真实 provider 验收"一节。
harness 默认严格 skipped，只接受已审计官方 base URL，不接受 fixture 或 loopback。

---

## §D 环境与部署门槛

- **D1** `[no-cred]` PostgreSQL + 真实 Redis 集成测试的复现。这些测试已存在但默认 skipped
  （`GATEWAY_TEST_DATABASE_URL` / `GATEWAY_TEST_REDIS_*`）。2026-09-02 有历史通过证据，
  之后未复现。**这需要的是本地容器，不是 provider 凭据**，因此是 `[no-cred]`。
  **状态：已完成（2026-09-10）。** Docker daemon 已可用；`configs/test-infra/` 提供 compose、ACL 与
  `test.env`。`source configs/test-infra/test.env && go test -count=1 -p 1 ./...` 23 个包全部通过，
  A4 进程级 e2e、PG ledger 幂等、crash 恢复、真实 Redis ACL 与 wrapper 进程用例首次在本机真实跑通，
  证据见 `docs/EVIDENCE.md`。**注意**：必须 `-p 1`（三个包共用 Redis DB 并 FLUSHDB），
  Redis 7.0.15 与 7.4.11 均已验证通过，进程用例的版本断言已放宽到 7.x；compose 默认仍 pin 7.0.15
  作为原始验收基线。
- **D2** 生产 AES 密钥的 KMS / secret 托管与轮换。属部署边界，非仓库内工程任务。
- **D3** Redis HA 拓扑与演练参数（部署侧）。冷启动拉不到快照即无法加入，无降级路径。
- **D4** utls JA3 指纹跟随上游客户端版本更新 —— 目前是人工流程，无自动化
  （检测已有，见总览 §6.3.1 规则 2；缺的是"更新"不是"发现"）。

---

## §E 可观测性与运维闭环（2026-09-10 复盘新增）

- **E1** `[no-cred]` **告警规则没有落点：指标埋了但没有消费方**。
  **状态：未开始。不阻塞 B4，可并行。**
  **基线**：A9 产出了 `control_platform_alert{provider,reason}`、`control_platform_error_total{provider,class}`、
  `control_platform_transport_rejections_accounts`、`control_platform_starvation_release_total`；
  总览 §4.1 另要求 `control_stream_pending`、`control_stream_dlq_total{reason}`、
  `usage_ledger_duplicate_total`、`request_attempt_open_age_seconds`、
  `request_attempt_recovered_total{source}`、`release_xadd_total{result}` 为"P0 必须暴露的可验收指标"，
  并明确"合成终态和 `UsageSource=missing` 的比例**单独告警**，不得被普通成功率掩盖"。
  这些指标都已存在，但 **A9 把告警本身记为"属部署侧"后就没有任何落点**，§D 也没有对应条目。
  **结论**：埋了没人看。总览 §6.3.1 规则 2 那条"多账号同时 transport 被拒 = 出口 IP / TLS 指纹被标记"
  的价值全在告警上——没有告警，它就退化回"一个号一个号烧着去发现"。
  **DoD**
  - 仓库内提供可直接加载的告警规则文件（Prometheus rule 形式即可），至少覆盖：
    平台级 transport 拒绝、DLQ 增长、stream pending 积压、`UsageSource=missing` 占比、防饿死闸触发。
  - 每条规则写明阈值来源。**总览 §6.3.1 已声明冷却时长"待线上回调"，因此阈值必须标注为"未经真实流量校准"**，
    不得写成已验证值。
  - 规则文件有语法校验（`promtool check rules` 或等价），进 CI 或至少进一条可执行的验证命令。
  - 通知通道（谁收、怎么收）属部署边界，**不在本任务范围**——本任务只交付规则与阈值。

- **E2** `[no-cred]` **wrapper 其余 executor 的 no-cred 部分**。
  **状态：未开始。**
  **基线**：A3 的 DoD 是"至少打通一个 provider 的一种模式"，PKCE 已达标，A3 的验收成立。
  但 device / CLI / PoW / turnstile executor 一个都没有实现，
  `internal/control/wrapper/run.go:43` 在无配置时仍回落到 `FixtureExecutor`
  （其自述为 "test-only"，注释已声明"Production configuration uses PKCEExecutor"）。
  **结论**：这与当初 A3 是同一性质的缺口——**参数生成、job 分发、错误归类这些不需要真实账号的部分
  现在不在任何清单上**，只有端到端授权属 live-gate。
  **DoD**
  - 按 A3 已建立的模式，为 device flow 补 executor 骨架：参数生成、回调 / 轮询处理、错误归类，
    用本地 fixture HTTP server 验证；真实授权码交换属 C4，不在本任务内。
  - `FixtureExecutor` 的生产回落路径收紧：生产配置下选不到真实 executor 应**显式失败**，
    而不是静默跑 stub（这正是 A3 基线里"永远收不到活、收到也只跑 stub"的成因）。
  - CLI / PoW / turnstile：先只确认各自需要哪些 no-cred 前置（总览 §9 标注 turnstile 保留 Python），
    写进文档，不在本任务内实现。
