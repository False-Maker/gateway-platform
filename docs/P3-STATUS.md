# P3 Status

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

仍未完成：

- Codex/Claude/Gemini/Grok/Copilot/Antigravity/Kiro/Windsurf 的真实 provider OAuth/refresh、inference、usage 和 quota schema 对账是项目结束阶段的最终 admission gate；本轮只验证本地 fixture/provider adapter。当前整数 `QuotaInfo` 不承载 Antigravity 的小数 remaining fraction 或 Kiro 的精度字段，因此相关 quota 明确保持 missing。
- Antigravity/Kiro/Windsurf 的真实协议 harness、Copilot 的 device authorization 端到端流程，以及 Grok/Gemini/Copilot/Windsurf 账号级 quota endpoint 仍未在无凭据环境中宣称完成。
- 非 OpenAI 入站到其他协议的跨协议 streaming、Gemini/Kiro 等 provider 的真实协议与 quota adapter、多模态内容块。
- quota 的真实用量扣减/调度、计费、协议控制台和部署切流。

## 最终判断

**P3 本地可验证部分已完成。** 本轮全量本地回归、race、vet 和 `-buildvcs=false` build 通过；临时 Redis 7.0.15 wrapper/ACL 仅保留 2026-09-02 历史通过证据，本轮未复现。真实 provider 测试统一安排在项目结束验收阶段；在该阶段完成前，P3 全量仍不能宣称完成：各 provider 的真实 OAuth、refresh、inference、quota、usage schema 仍 pending；这不构成真实 provider 或生产准入结论。

最高剩余风险是：真实 provider 服务端行为仍未验证，尤其无法确认真实额度/usage schema 与本地 adapter 的一致性。当前本地代码只对已审计 fixture 字段做严格转换；没有账号时不能证明 refresh token 生命周期、服务端拒绝条件、quota reset 语义和 usage 字段在真实服务端与 fixture 一致。多模态内容和非 OpenAI 入站的任意跨协议 streaming 仍不在当前实现边界内。
