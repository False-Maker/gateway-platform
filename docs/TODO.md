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

harness 契约（变量、约束）见 `docs/EVIDENCE.md` 中"项目结束真实 provider 验收"一节。
harness 默认严格 skipped，只接受已审计官方 base URL，不接受 fixture 或 loopback。

---

## §D 环境与部署门槛

- **D1** `[no-cred]` PostgreSQL + 真实 Redis 集成测试的复现。这些测试已存在但默认 skipped
  （`GATEWAY_TEST_DATABASE_URL` / `GATEWAY_TEST_REDIS_*`）。2026-09-02 有历史通过证据，
  之后未复现。**这需要的是本地容器，不是 provider 凭据**，因此是 `[no-cred]`。
  **状态：2026-09-07 已复核，待测试基础设施可用后复现。** 本轮本地集成入口与 fixture 全部通过，
  PostgreSQL/真实 Redis 用例因环境变量未配置而按设计严格 skipped；Docker CLI 可见但 WSL 中无法连接
  Docker daemon，尚未获得真实 PG/Redis 验收证据。
- **D2** 生产 AES 密钥的 KMS / secret 托管与轮换。属部署边界，非仓库内工程任务。
- **D3** Redis HA 拓扑与演练参数（部署侧）。冷启动拉不到快照即无法加入，无降级路径。
- **D4** utls JA3 指纹跟随上游客户端版本更新 —— 目前是人工流程，无自动化
  （检测已有，见总览 §6.3.1 规则 2；缺的是"更新"不是"发现"）。
