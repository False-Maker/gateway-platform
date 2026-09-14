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
> **§B4 到此闭合。**~~下一步在 `B5 控制台` 与并行项 A12 / E1 / E2 / C6(no-cred 部分) 之间取。~~
> `A12 请求明细存储` 2026-09-11 完成（见 `docs/A12-REQUEST-DETAIL.md`），**§A 到此闭合**。
> 下一步在 `B5 控制台` 与并行项 E1 / E2 / C6(no-cred 部分) 之间取。
> `E1 告警规则落点` 2026-09-11 完成。
> `E2 wrapper 其余 executor（no-cred 部分）` 2026-09-11 完成。
> ~~下一步在 `B5 控制台` 与 `C6(no-cred 部分)` 之间取。~~
>
> `B5 控制台（后端 API）` 2026-09-11 完成，见 `docs/B5-CONSOLE.md`。**§B 到此闭合。**
> `held` 人工处理入口（B4.3/B4.5/E1 三处遗留）、充值入口与单价录入入口（B4.1 遗留）一并闭合。
>
> ~~下一步：`§A`、`§B`、`§E` 的全部 `[no-cred]` 条目已清空~~ —— **该判断由第三次复盘推翻，见下。**
>
> **2026-09-13 第三次复盘（结构分析）**：用与 A8 / 二次复盘相同的口径再对照一遍设计文档与代码，
> 起因正是上一轮"`[no-cred]` 已清空"这个结论——它值得被验证而不是被接受。结果找出**一个**
> 设计文档明确要求、代码中零实现、且从未进过任务清单的缺口，编号 **A13**（每上游
> `usage_integrity` 策略）。它是总览 §6.2 的 P0 级契约要求，`fluxgate/docs/architecture.md` §4
> 列为硬约束，`docs/AUDIT-CONTEXT.md` 两处提及，而全仓库 `usage_integrity` 零命中。
>
> 本次复盘**核对通过**（设计与代码一致，不再重复列为缺口）的项：§4.1 七个 P0 指标全部有发射点、
> §3.1 快照七个键族齐全、§6.2 强注 `include_usage` 与断连 drain、§6.3.1 的 ±20% 抖动与
> 「新凭据版本/新 epoch 清本地冷却」、`AccountLimits.DegradePolicy` 的 fail_open 分支、
> §4.1 的 7 天 trim 与「`attempt_started` 写入失败即 503」、fluxgate/keyhive 两份交付物清单
> 已无未勾选项。
>
> `A13 每上游 usage_integrity 策略` 2026-09-13 完成（含一处基线更正，见该条目开头的引用块）。
>
> ~~**下一步**：§A/§B/§E 的 `[no-cred]` 条目再次全部清空~~ —— **该判断由第四次复盘再次推翻。**
>
> **2026-09-14 第四次复盘（结构分析）**：这次换了重点——第三次扫的是总览，两份**角色文档**
> 只查了 checkbox，`AUDIT-CONTEXT.md` / `P2-ADMISSION.md` 基本没碰，所以这次主扫这三份；
> 并吸取第三次"读片段导致误判"的教训，对每个候选缺口都**读完整函数**再下结论。
> 结果：找出**一个**设计要求、代码零实现、从未进清单的缺口 —— **A14**
> （`forbidden_transport` 的出口路径短路只做了一半）。
>
> 同时修正**六处文档漂移**（文档说"未实现/未做"，而 A12/E1/C6 早已交付；另有一处文档自相矛盾），
> 详见 EVIDENCE 同名小节。
>
> `A14 出口路径短路` 2026-09-14 完成（含一处实现时的自我更正，见该条目开头的引用块）。
>
> **下一步**：§A/§B/§E 的 `[no-cred]` 再次清空；§C 全部 `[live-gate]`，§D 为部署边界（D1 已完成）。
> 连续两次复盘都在"清单已空"之后找出 P0 级缺口，因此"清单空了"**不应**被当作开发完成的证据。
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

**状态：已完成（2026-09-11）。** 形态决策与取舍见 `docs/A12-REQUEST-DETAIL.md`。

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

**实现摘要**
- 形态：**引入 ClickHouse**（用户拍板，覆盖"不默认引入新中间件"这条默认；决策记录已按 DoD 先写）。
  走 HTTP 接口 + `JSONEachRow`，**不引入 ClickHouse Go 驱动依赖**。
- `internal/detail`：`Record`（Release 全字段 + 路由信息）、`FromRelease` 投影、`DDL`
  （`ReplacingMergeTree(ingested_at)` / `PARTITION BY toYYYYMMDD(occurred_at)` /
  `ORDER BY (tenant_id, occurred_at, event_id)` / TTL 30 天）、`Sink`（有界、异步、可丢）、
  `ClickHouseWriter`。
- 解耦靠形状而非自觉：`Sink.Observe` **无 error 返回**、非阻塞，只在 `Ledger.HandleRelease` 的
  `tx.Commit()` **之后**被调用；`*Sink` 为 nil 即"明细关闭"。
- 凭据不入明细靠结构：`Record` 无 map / 无 interface / 无 header 袋子 / 无 body 字段，
  `TestRecordCarriesNoFreeFormFields` 反射守门。ClickHouse 口令从 URL 移到 Basic-Auth header。
- 新增可选 `contracts.Release.InboundProtocol`（不进 `Validate()`，缺失不得使 release 不可计费）。
- 生产接线：`GATEWAY_DETAIL_CLICKHOUSE_URL` 为空即关闭；schema 不可用只 log 并降级为无明细，
  绝不因明细故障拒绝启动。
- **无新增 PG migration**——明细刻意不进控制状态库。

**本轮未做**
- `BatchSize` / `FlushInterval` / `BufferSize` / TTL 未经真实流量校准。
- 无 ClickHouse 集群、副本、备份与保留策略；测试与本地只有单实例。
- 控制台查询界面属于 B5。
- 明细与 `usage_ledger` 的交叉核对未做（B4.5 对账只覆盖账侧三方）。

### A13 `[no-cred]` 每上游 `usage_integrity` 策略缺失（总览 §6.2 的 P0 级契约要求）

**状态：已完成（2026-09-13）。** 证据见 `docs/EVIDENCE.md` 同名小节。

> **基线更正（2026-09-13，实现时发现）**：下方「当前实际行为」原写
> 「上游 200 但无 usage → 直接成功返回客户端，不换号、不重试」，**这对非流式路径是错的**。
> `server.go` 的非流式循环在 `status >= 400` 分支**之后**另有一段 `if usage == nil`，
> 已经在做换号与 502（`ErrUsageUnavailable`），行为上就是 §6.2 表第一行。
> 复盘时只读到 `status >= 400` 那段就下了结论，漏看了后面十行。
> **真实缺口是另外两条**，A13 的价值也在这两条上：
> 1. 该 failover 是**硬编码**的，不是 §6.2 要求的「逐 provider 显式配置 + 未配置不得进快照」；
> 2. 被放弃的那次尝试的 Release 记成 `ErrorClass=ok` / `status=200` / `UsageSource=missing`，
>    经 B4.3 判为 **`held`**——于是**每一次 failover 都产出一行待人工裁定的账目**，
>    正是 §6.2 设计 failover 想避免的结果。这条是真正花钱的缺陷。

**基线**
- 总览 §6.2 写明：「**P0 为每个已注册 provider 必须显式配置 `usage_integrity`（没有配置不得进入可调度快照）**」，
  并给出首批取值表（P1 非流式静态 apikey / Anthropic+Gemini / Grok / Codex+Kiro 四行，**首批全部为 `failover`**），
  末句「策略结果必须写入 `Release.UsageSource` 和 `Partial`，**不能由计费层猜测**」。
- `fluxgate/docs/architecture.md` §4 把它列为**硬约束**：「如 Grok 成功但无 usage → 转 failover 报错，**不静默计 0**」。
- `docs/AUDIT-CONTEXT.md` 两处（v2 新增、P0 具体契约）同样列入。
- **代码中 `grep -ri "usage_integrity\|UsageIntegrity"` 在 `.go` 与 `.sql` 全仓库零命中。**
  `provider.Provider` 接口（`internal/control/provider/provider.go:16`）无对应方法，registry 无该配置，
  `snapshot_loop.go:75` 的可调度过滤只看 `status='active'` 与 `cooldown_until`，**没有任何 usage_integrity 闸门**。

**当前实际行为（与设计不符的地方）**
- 非流式：`internal/gateway/server.go:1295` 把 `UsageSource` 初始化为 `missing`，
  仅当上游给了 usage 才改写为 `upstream`；`server.go:1323` 的 failover 条件是 `status >= 400`。
  因此**上游 200 但无 usage → 直接成功返回客户端，不换号、不重试**，
  正是 §6.2 Grok 行明令禁止的那条路径的另一半（没有静默计 0，但也没有 failover）。
- 流式：`server.go:1215` 同构。
- 三个不同 provider（OpenAI-compatible / Anthropic / Grok）走的是**同一条隐式路径**，
  而设计要求逐 provider 显式配置。

**结论**
后果不是丢数据，而是**把收入问题转嫁给人工**：B4.3 的处置表把
`(missing, partial=false, 成功)` 判为 `held`（`billing_policy.go:69`），
于是每一次「上游成功但没给 usage」都变成一行待人工裁定的 held 账目，
靠 B5 刚建的控制台托盘一条条捞。§6.2 设计 `failover` 的用意正是让这类请求
**在网关侧就换号重来**，根本不产生 held 行。B4.3/B5 是在为一个本应更早拦掉的缺口做兜底。
这条与 A11/A12/E1/E2 同性质：设计文档明确要求、代码中不存在、且从未列入任务清单。

**DoD**
- `usage_integrity` 成为 provider 的**显式**配置（取值 `failover` / `zero` / `estimated`），
  未配置的 provider **不得进入可调度快照**——闸门加在 `snapshot_loop.go` 的可调度过滤里，
  与 `status` / `cooldown_until` 同层，fail-closed 而非默认放行。
- 首批取值按总览 §6.2 表落 `failover`，**不得**为图省事给一个全局默认值；
  `zero` 仅在评审后对具体 provider 显式选择（§6.2 原文），`estimated` 本轮不启用
  （需 tokenizer 版本与校准报告，且 B4.3 已把 `estimated` 判为 `held`，无生产者）。
- 网关侧按策略执行，并区分流式与非流式——这是 §6.2 表里两行的差别，不能合并：
  - 非流式：完整响应已缓冲但无 usage → 换号重试，用尽预算后返回 502 且 `UsageSource=missing`。
  - 流式：**首字节前**缺 usage 可换号；**首字节后不得重放**，保留响应并标记 `missing` / `partial`。
- 复用既有的 failover 预算（`attemptNo < 2`，fluxgate §5 已定「最多两次、仅首字节前」），
  **不新开一套重试计数**，否则两套预算会互相叠乘。
- 回归：本地 fixture 上游返回「200 + 无 usage」，断言换号重试发生、
  用尽后终态为 `missing`；断言未配置 `usage_integrity` 的账号不出现在快照里。
- **本任务不改 B4.3 的处置表**——它对 `missing` 的判定仍然正确，
  A13 要减少的是 `missing` 的**产生量**，不是改变它的账务含义。

**实现摘要**
- **契约**：`contracts.UsageIntegrity`（`failover` / `zero` / `estimated` + `Valid()`）；
  `Account.UsageIntegrity` 与 `Lease.UsageIntegrity`（热路径零查询即可拿到策略）；
  新增 `ErrorClass` 值 **`usage_missing`**。
- **显式性靠编译器**：`provider.Provider` 接口新增**必选**方法 `UsageIntegrity()`，
  不是可选接口——新 provider 忘了声明是**编译失败**，而不是上线后账号静默消失。
  八个 provider 各自声明并在注释里引用 §6.2 的对应行；antigravity 与 copilot **不在该表内**，
  已在各自注释中写明取 `failover` 的推理与其局限。
- **fail-closed 闸门**：`SnapshotLoop.stampUsageIntegrity` 在发布前按 registry 打标，
  provider 未注册或取值非法的账号**不进快照**，按 provider 计
  `control_snapshot_accounts_dropped_total{provider,reason}` 并 log。
  放在发布路径而非某个 repository 实现里，因此任何 repository（含测试 fake）都被同一道闸门管。
- **不产生 held 行**：网关在「完整响应 + 无 usage + 策略非 zero/estimated」时把该次尝试记为
  `ErrorClass=usage_missing`。B4.3 对 `(missing, !partial, 失败)` 判 `not_billable`，
  于是 failover 与用尽预算两种结局都**不再**落进 `held`。`usage_missing` 不在 health.go
  的受罚 class 列表里，落到其 `ELSE` 分支（只记 last_error，不罚号）——上游不给 usage
  是上游的性质，不是被选中那个账号的健康问题。
- **复用既有预算**：仍是 `attemptNo < 2`，未新增重试计数。
- 指标：`gateway_usage_integrity_failover_total{provider}`。

**本轮未做**
- **流式路径未改，仍会产生 `held` 行。** §6.2 要求「首字节后不得重放」，而流式 usage 只在
  流末才知道，必然已过首字节——所以流式在设计上就无法 failover。这是 §6.2 的取舍，不是遗漏，
  但意味着 **A13 只减少非流式来源的 held，不能宣称 held 归零**。
- **`zero` 与 `estimated` 未接入热路径**：无 provider 选它们，且 §6.2 分别设了「评审」与
  「tokenizer 校准报告」前置。`zero` 目前的行为是「照常返回响应 + `UsageSource=missing`」，
  **并非真正的「计 0」**；真要启用需按 §6.2 先评审。已有测试钉住该分支不被静默改掉。
- 未新增告警规则（E1 已闭合，A13 的 DoD 未要求）；新指标目前无消费方。
- 未起 PG / Redis / ClickHouse，相关集成用例本轮为 skip。

**已知取舍（记录，避免被当成遗漏）**
- 真实 provider 到底会不会「成功但不给 usage」属 `[live-gate]`（C2/C3）。
  本任务只交付策略机制与本地 fixture 验证，**不声称**任何 provider 的真实行为已核实。

### A14 `[no-cred]` `forbidden_transport` 的出口路径短路未实现（只做了一半）

**状态：已完成（2026-09-14）。** 证据见 `docs/EVIDENCE.md` 同名小节。

> **实现时的一处自我更正**：第一版按字面实现成「收到 403-HTML 立即短路该出口」，
> 结果打破了既有测试 `TestForbiddenTransportFailoverUsesDifferentAccount`。
> 原因是我只读了 §6.3.1 那一行的「出口路径短路」，**漏看了同一行的 `failover = 是`**
> ——与第三次复盘同一种错法（只读了证据的一部分）。
> §6.3.1 规则 2 自己给了判据：**单个账号的 HTML 403 不是结论，同平台多个账号才是**。
> 最终实现按「60s 窗口内 ≥2 个不同账号」触发，与 A9 的 `PlatformSignals` 同一口径：
> 第一次拒绝照常 failover（设计保留的重试不被删掉），第二个不同账号再失败才短路。

**实现摘要**
- `Chooser` 新增两张表，与账号冷却表**分开**：`egress`（出口 → 短路截止）与
  `egressSeen`（出口 → 最近被拒的不同账号）。§6.3.1 明确 `forbidden_transport` 不得进
  账号冷却表（「混进账号冷却表会误杀健康号」）。
- 出口身份 = `lease.Profile.Proxy`（即 `clientForProxy` 实际拨号用的值，不会与现实漂移），
  空串即「直连」这一个桶。**不是账号 id**——用账号 id 当键就又变回账号冷却了。
- 触发条件：60s 窗口内 ≥2 个**不同**账号在同一出口被拒（同一账号重复不计）。
  短路 7s ± 20% 抖动 ∈ [5.6s, 8.4s]，落在文档的 5–10s 内。
- 选号时跳过短路中的出口；若候选全在短路中，返回新错误 `ErrEgressBlocked` → 503，
  **fail-closed，不绕过短路硬发**。这同时解决了 DoD 里「同请求内换号后仍走同一出口」——
  过滤发生在选号阶段，第二次尝试不会再出同一扇门。
- 指标 `gateway_egress_short_circuit_total{egress}`，标签走 `redactEgress` **脱敏**：
  proxy URL 可能带 `user:password@`，原串只作内存键，绝不进日志或指标。
- `markTransportOrAccount` 把「按出口记」与「按账号记」的分派收在一个函数里，
  避免将来有人把 transport 拒绝悄悄折回账号冷却表。

**本轮未做**
- 5–10s 与 60s/2 这几个数字**未经真实流量校准**（与 E1 阈值同一处理）；
  真实 WAF 的拦截特征与恢复时间属 C3 `[live-gate]`。
- 未新增告警规则（E1 已闭合）；新指标暂无消费方。
- 短路是**进程内**的：多个 gateway 实例各自独立判断，不共享证据。
  A9 的 control 侧聚合仍是跨实例的那一半。

**基线**
- 总览 §6.3.1 的 `ErrorClass` 表，`forbidden_transport` 行的「gateway 本地冷却（桥接）」列写的是：
  **「不罚账号；改为出口路径 5–10s 短路」**——这是**两个**要求，不是一个。
- `fluxgate/docs/architecture.md` §2.1 重复同一条：「`forbidden_transport`（403 HTML）走**出口路径**
  5–10s 短路，不碰账号」，并给出理由：「混进账号冷却表会误杀健康号」。
- 代码只实现了**后一半**。`internal/gateway/chooser.go:287`：
  ```go
  if class == contracts.ErrorForbiddenTransport || class == contracts.ErrorNetwork {
      return
  }
  ```
  直接早退——账号确实没被冷却（正确），但**出口路径也没有任何短路**。
  全仓库 `egress` / `circuit` / 代理级冷却零命中；`ForbiddenTransport` 在 gateway 侧只有两处出现：
  上面这个早退，和 `server.go:1750` 的分类。

**结论**
后果不是误杀账号，而是**对着已经在拦你的 WAF 持续全速重试**。出口 IP 或 TLS 指纹被标记时，
每个请求都会：选账号 A → 403 HTML → failover → 账号 B → 403 HTML → 失败。两次上游往返、
两个账号的尝试被烧掉，且没有任何退避——而这正是让 IP 被封得更死的行为。

A9 的 `PlatformSignals` 让这件事**可见**（同平台 60s 内 ≥2 个账号 transport 被拒即告警），
E1 也接上了规则。但 §6.3.1 规则 2 的价值是「不要一个号一个号烧着去发现」——
**检测有了，处置没有**。这条与 A11/A12/E1/E2/A13 同性质：设计文档明确要求、代码中不存在、
且从未列入任务清单。

**DoD**
- gateway 侧新增**按出口路径**（而非按账号）的短路表。键是出口身份——
  `UpstreamProfile.Proxy` 为空时退化为「直连」这一个桶，不要用账号 id 当键，
  否则又变回账号冷却。
- 命中 `forbidden_transport` 时短路该出口 5–10s（±20% 抖动，与既有本地冷却同一口径）；
  短路期间**不再从该出口发起请求**。若全部出口都在短路中，按 fail-closed 返回 503，
  **不得**绕过短路硬发——那等于没做。
- 与 failover 的关系必须写清：同一请求内换号后若仍走**同一出口**，不应算作一次有效重试
  （当前实现会白白烧掉第二个账号）。
- 不改 `ErrorClass` 表、不改 control 侧判罚（§6.3.1 明确 `forbidden_transport` **不罚账号**）。
- 回归：fixture 上游返回 403 + HTML body，断言同出口在短路窗口内不再被使用、
  账号冷却表**不受影响**、短路到期后自动恢复。
- 5–10s 这个数字来自设计文档，**标注为未经真实流量校准**（与 E1 阈值同一处理）。

**已知取舍**
- 真实 WAF 的拦截特征与恢复时间属 `[live-gate]`（C3）。本任务只交付机制与本地 fixture 验证，
  **不声称**任何真实上游的拦截行为已核实。

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
    new-api quota 整数单位 ↔ 货币金额的换算比例**已由 B4.6 读取**（2026-09-14），但真实部署的取值仍未读到（`[live-gate]`）。
    充值入口与单价录入入口**已由 B5 闭合**（2026-09-11）。

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
    new-api quota 整数单位 ↔ 货币金额的换算比例**已由 B4.6 读取**（2026-09-14），但真实部署的取值仍未读到（`[live-gate]`）。

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
    new-api quota 整数单位 ↔ 货币金额的换算比例**已由 B4.6 读取**（2026-09-14），但真实部署的取值仍未读到（`[live-gate]`）。

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
    new-api quota 整数单位 ↔ 货币金额的换算比例**已由 B4.6 读取**（2026-09-14，真实取值仍属 `[live-gate]`），
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
    **本轮未做**：~~没有定时运行与指标（告警仍属 E1）~~ —— **已由 B4.7 闭合（2026-09-14）**；
    按需查询的入口**已由 B5 闭合**（`GET /v1/billing/reconcile`，2026-09-11）；
    只比较金额，不重算单价（不做"应收 vs 实收"的重新定价核对）；
    不校验 `wallet_transactions.balance_after` 的逐行链条（同 `created_at` 的顺序不唯一，
    会造假阳性），只用与顺序无关的"余额 == 流水求和"这一恒等式；
    `held` 的人工处理入口**已由 B5 闭合**（2026-09-11）；`estimated` 仍无生产者；
    new-api quota 整数单位 ↔ 货币金额的换算比例**已由 B4.6 读取**（2026-09-14），但真实部署的取值仍未读到（`[live-gate]`）。

- **B4.6** `[no-cred]` **new-api `QuotaPerUnit` 换算比例的读取**。
    **状态：no-cred 部分已完成（2026-09-14）。** 证据见 `docs/EVIDENCE.md` 同名小节。
    **基线**：这条缺口在 `docs/B4-BILLING-MODEL.md` D4 与 §"未决"里写明是 **B4.1 的硬输入**，
    却在 B4.1–B4.5 五条的"本轮未做"里**重复出现五次而从未编号**，因此扫编号清单的 agent 看不见它。
    这是它能一直活到第四次复盘之后的原因，本轮据此补编号。
    **为什么它比其他遗留项更该先做**：换算比例错了，B4.1 单价、B4.2 扣费、B4.5 对账会**一起错**，
    而且是"平衡地错"——对账用的恒等式（余额 == 流水求和）在错误比例下依然成立，查不出来。
    **DoD**
    - 迁移读取路径从**源部署本身**读出该比例，不使用 new-api 编译期默认值。
      （核实结论：`common/constants.go:22` 的 `500000` 只是初值，
      `model/option.go:597` 会在启动时用 `options` 表的 `QuotaPerUnit` 行覆盖它，
      所以**默认值不是任何部署的可靠答案**，只有那一行是。）
    - 读不到时按 `missing` 上报并给出原因，**不得**按默认值补齐、不得按零补齐（AGENTS.md §2）。
    - 原始值进迁移审计记录，即使它无法解析。
    **实现摘要**
    - `SourceQuotaPerUnit{Raw, Value, Found, Reason}` 进 `SourceSnapshot` 与 `Summary`（JSON 键
      `quota_per_unit`），dry-run 摘要里因此能看到"这次迁移观察到的比例是多少 / 或者为什么没有"。
    - `Value` 是 `contracts.Decimal` 而非 float64，与 B4.1 「money is exact decimal everywhere」同一口径。
    - 六种"没有"各自有稳定的 reason 串：`options_table_absent` / `options_table_missing_key_or_value_column` /
      `quota_per_unit_option_absent` / `quota_per_unit_option_empty` / `quota_per_unit_option_unparsable` /
      `quota_per_unit_option_not_positive`（`0` 单列，否则下游是除零）。
    **本轮未做**
    - **没有任何换算被执行**：本轮只把比例读出来并记录，A11 的"只保留 source unit 与原始值、
      不做换算"**仍然成立**，`PlannedTenant` 依旧不带 quota / balance。
      要把 balance 变成钱包余额，还需要一条把 `Found==true` 的比例真正用起来的迁移路径——
      本轮**有意**不做，因为那要求先有真实取值。
    - **真实 new-api 部署的 `QuotaPerUnit` 取值仍未读到**：本轮全部由本地合成 schema 驱动，
      `GATEWAY_NEW_API_DATABASE_URL` 未指向任何真实库。**这部分是 `[live-gate]`**——
      它要的不是 provider 凭据，而是生产 new-api 库的只读访问。

- **B4.7** `[no-cred]` **对账的定时运行与指标**。
    **状态：已完成（2026-09-14）。** 证据见 `docs/EVIDENCE.md` 同名小节。
    **基线**：B4.5 交付了算法，B5 交付了按需查询端点（`GET /v1/billing/reconcile`），
    但**没有任何东西周期性地跑它**——"账目对不平"是一个需要有人想起来去问的问题。
    E1 自己记录了这点（"B4.5 对账作业不发射任何指标，因此'对账发现不一致'目前无法告警"）
    并明说不在其 DoD 内、本轮不改。于是它成了又一条**被记录过但从未编号**的缺口，
    与 B4.6 同一种漏网机制。
    **DoD**
    - 对账进 control 主循环周期性运行，且**保持只读**（不得让审计者能修它审计的东西）。
    - 发射指标，使 E1 能为"对账不平衡"写出告警。
    - 窗口口径必须避开扣费作业尚未处理的区间，否则每次都报不平，等于永远在响的告警。
    **实现摘要**
    - `internal/control/billing_reconcile_loop.go`：`ReconciliationLoop`，15 分钟一跑。
    - **窗口滞后 15 分钟且宽 1 小时**，两个数字各有理由：
      滞后必须**大于** `billingRunInterval`（5m），否则窗口里全是扣费作业还没碰的 `pending` 行，
      每次都报不平；窗口必须**大于**运行间隔，否则两次运行之间落地的行会被整段跳过，
      而"对一段没读过的区间报 balanced"比不对账更糟。两条都有测试守着，
      滞后不合规时 `RunOnce` **拒绝运行**而不是发一个无意义的信号。
    - 四个指标：`control_billing_reconcile_balanced`、
      `control_billing_reconcile_discrepancies{kind}`、`control_billing_reconcile_unsettled_rows`、
      `control_billing_reconcile_runs_total{result}` + `..._last_success_seconds`。
    - **每轮把六个 kind 全部写一遍（含 0）**。只写非零的会让 gauge 永不复位：
      某类出现过一次再消失，告警会一直挂着。
    - 三条告警规则：`BillingReconciliationUnbalanced`、`BillingReconciliationDiscrepancyKind`、
      `BillingReconciliationNotRunning`。第三条是要点——**对账停摆与对账没发现问题，
      在所有差额 gauge 上长得一模一样（都是 0）**，没有它，一个死掉的循环会被读成账目健康。
    **本轮未做**
    - 15m / 1h / 15m 三个数字**未经真实流量校准**（与 E1、A14 同一处理）。
    - **多实例会各自跑**：本轮未加 singleton 锁。对账只读，重复跑不会损坏数据，
      代价只是重复查询与 gauge 互相覆盖（同值，无害）。要跨实例只跑一份，
      需复用 `fencing.go` 的 PG advisory lock——**有意未做**，不在本条 DoD 内。
    - 告警未演练（没有真的触发过任何一条）。

- **B5** P6 范围：控制台。**本轮交付后端 API，不含前端界面**（理由与取舍见 `docs/B5-CONSOLE.md` §1）。
  **状态：后端 API 已完成（2026-09-11）。** 证据见 `docs/EVIDENCE.md` 同名小节。
  新增 `internal/control/console{,_auth,_billing,_held}.go`、`internal/detail/query.go`、
  `migrations/008_billing_resolutions.sql`；控制面首次有了 HTTP 表面（此前只有 metrics server）。
  **DoD**：§6.5 预埋的三处钩子（账号健康 / 用量对账 / 实时请求日志）各有可调用的端点；
  控制面鉴权与租户凭据**分离**，未配置 token 时控制台整体关闭而非无鉴权开放；
  三个写操作（held 裁定 / 充值 / 单价录入）各自与账目在同一事务内落盘，且**各自关闭一处
  此前被显式记录为"没有入口"的缺口**；人工计费行在 B4.5 对账下仍然平衡。
  结论摘要：端点见 `docs/B5-CONSOLE.md` §3；身份决策（**不复用租户 token**，也不加运营标记）
  见 §2，含两个被否决选项；`bill` 在无单价时落到终态 `unpriced` 而非拒绝或按零（§4.1）；
  明细查询全部走 ClickHouse 绑定参数、只返回行不做求和（§5）。
  **本轮未做**：前端界面（§1）；运营者目录 / 真实 RBAC——`X-Operator` 只是审计归属，
  **不参与授权**，见 §2；账号的人工操作（封禁、清冷却、清 `excluded_models`）仍只读（§6）；
  对账的定时运行与指标仍属 B4.5 遗留；列表无游标分页；控制台无速率限制；
  控制台端点的**无真实流量校准**（与仓库现有常量同一口径）。
  `held` 的人工处理入口**到此闭合**（B4.3/B4.5/E1 三处记录的缺口）。
  充值入口**到此闭合**（B4.1 遗留）。单价录入入口**到此闭合**（B4.1 只交付了表与触发器，没有写入方）。

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
  **no-cred 部分 2026-09-11 完成，结论：不需要。**详见 `docs/C6-CODEX-PER-REQUEST-CHALLENGE.md`。
  ~~**爆炸半径是契约级的，不是一个渠道的适配问题**~~ —— **本轮据此降级**：
  蓝本证据显示本平台所用端点 `https://chatgpt.com/backend-api/codex/responses`
  是**纯 Bearer** 调用、不带 sentinel 头、不调 `chat-requirements`
  （`chatgpt2api/services/openai_backend_api.py:596` / `:785`，与 `profile.go:57-58` 同一路径）。
  故 `UpstreamProfile` 静态配方够用，**不需要** per-request minter 旁路，总览 §5 热/慢拆分不破。
  **本轮未写任何实现、未改任何契约**——核实结果就是"不需要改"。
  出处：总览"附：未决架构问题"与 `fluxgate/docs/architecture.md` §7。原文标注"P2 打通 codex 前必须核实"，
  但 P2、P3 都已过去，这条既未核实也未关闭，一直以附录脚注形式存在——2026-09-10 复盘将其提升为编号任务。
  **顺带更正**：TODO 原指路"读 `chatgpt2api/services/proxy_service.py`"有误——
  该文件**没有任何** PoW/turnstile/sentinel 引用（它管代理链与 Cloudflare clearance），
  调用点全在 `services/openai_backend_api.py`。
  **剩余 live-gate 部分**：拿真实 codex 账号对该端点发一次只带 Bearer 的**通用文本推理**请求，
  确认不被要求 sentinel。（蓝本对该端点的唯一调用是**图像生成**，通用推理路径属推断而非证据。）
  若被拒，需回到附录的两条路，**并重新评估上面"无需改动"的结论**。

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
  **状态：已完成（2026-09-11）。** 规则见 `configs/alerts/gateway-platform.rules.yml`，
  验证命令见 `configs/alerts/README.md`。
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

  **实现摘要**
  - `configs/alerts/gateway-platform.rules.yml`：16 条规则、6 个分组。DoD 要求的五个方向全覆盖，
    另把此前无消费方的 B4.2/B4.3/B4.4 计费指标与 A12 明细指标一并接上。
  - 阈值来源逐条写在 `threshold_source` 注解里，分两类：`uncalibrated`（起始值，待回调）与
    "由后果推出"（可容忍量本身为 0，与流量无关，如 DLQ 增长 = 计量缺口）。文件头部有总声明。
  - 校验双轨：`promtool check rules`（Docker，`prom/prometheus:v3.7.3`）验 PromQL 语法；
    `internal/observability/alertrules_test.go` 验语义且**不依赖 Docker**——它扫源码取出真正被
    发射的指标名，规则里引用了仓库不存在的指标就红。写错指标名的规则永远不会触发，
    读起来却像有覆盖，这是本任务真正要防的失效模式。

  **本轮未做**
  - 无 CI，两条校验命令均为手动执行（仓库本来就没有 CI 配置）。
  - 所有阈值未经真实流量校准，且未做告警演练（没有触发过任意一条规则）。
  - ~~B4.5 对账作业**不发射任何指标**，因此"对账发现不一致"目前无法告警。不在 E1 DoD 的五项内，
    此处记录，本轮不改。~~ —— **已由 B4.7 闭合（2026-09-14）**：对账现在按 15 分钟周期跑，
    发射四个指标，并新增三条告警规则（规则总数 16 → 22）。
  - `control_billing_held_rows` 有了告警，但 held 行仍无人工处理入口（既有未决项）。

- **E2** `[no-cred]` **wrapper 其余 executor 的 no-cred 部分**。
  **状态：2026-09-11 完成。**详见 `docs/E2-WRAPPER-EXECUTORS.md`。
  **基线**：A3 的 DoD 是"至少打通一个 provider 的一种模式"，PKCE 已达标，A3 的验收成立。
  但 device / CLI / PoW / turnstile executor 一个都没有实现。
  ~~`internal/control/wrapper/run.go:43` 在无配置时仍回落到 `FixtureExecutor`。~~
  **该基线描述有误，本轮更正**：`executorFromConfig` 早已把 `""` 与 `"oauth"` 映射到 `PKCEExecutor`，
  `ConfigFromEnv` 默认值也是 `ExecutorOAuth`，"无配置回落 stub"在本轮开始前就不成立。
  真实缺口是 `GATEWAY_WRAPPER_EXECUTOR=fixture` **在生产里依然可选且无任何护栏**。
  **结论**：这与当初 A3 是同一性质的缺口——**参数生成、job 分发、错误归类这些不需要真实账号的部分
  现在不在任何清单上**，只有端到端授权属 live-gate。
  **DoD**
  - ~~按 A3 已建立的模式，为 device flow 补 executor 骨架~~ 完成：`internal/control/wrapper/device.go`，
    RFC 8628 参数生成 / 轮询（含 `slow_down` +5s）/ 六类错误归类，本地 fixture HTTP server 验证。
  - ~~`FixtureExecutor` 的生产回落路径收紧~~ 完成：改为**两个独立 env 确认**
    （`GATEWAY_WRAPPER_EXECUTOR=fixture` + `GATEWAY_WRAPPER_ALLOW_FIXTURE=true`），缺一即启动失败；
    并新增 `authModeRouter` 按加密 input 里的 `auth_mode` 分发，无人认领即显式失败。
  - ~~CLI / PoW / turnstile：先只确认各自需要哪些 no-cred 前置~~ 完成：写入
    `docs/E2-WRAPPER-EXECUTORS.md` §3。
  **本轮未做（有意）**
  - `defaultDeviceProfile` 的 copilot `TokenURL` **留空**，生产路径必然 `device_profile_pending` 失败。
    端点在蓝本里有记载，但只轮询它拿到的是 GitHub token 而非 Copilot 凭据；
    缺的第二次交换（`copilot_internal/v2/token`）本轮未实现。**C4 必须两者同时补**，
    否则 job 会"成功"并返回无法服务流量的 token。
  - 真实 device 授权**一次都没跑过**（无凭据，属 live-gate）。
  - CLI / PoW / turnstile 未实现；`WrapperJob` / `TokenBundle` 未扩字段——
    等 `docs/E2-WRAPPER-EXECUTORS.md` §3.1 第 1 条的架构决定（CLI 类会把 worker 绑死在具体主机上）。
  **顺带推进**：总览附录 P2 未决问题"codex CLI 端点是否需要每请求 PoW/turnstile"，
  蓝本证据指向**不需要**（`/backend-api/codex/responses` 只带 Bearer，不走 sentinel）。
  未经真实上游验证，P2 落地前需实测复核。
