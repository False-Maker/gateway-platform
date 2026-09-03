# fluxgate(gateway 角色)怎么建(v2,按评审重构)

> 纯网关 / 热路径的构建说明。fluxgate 是**单 repo 单二进制的 `--role=gateway` 角色**,不是独立 repo。
> 本文只讲 **gateway 角色自身怎么搭**。系统全局(角色拆分、边界、契约全文、快照形态、共享决策、阶段计划、参考项目)见
> `../../docs/fluxgate-keyhive-overview.md`。配套角色:`--role=control`(契约与快照的持有者,见 `../../keyhive/docs/architecture.md`)。
>
> 本版相对 v1 的变更(评审结论):① 热路径补**请求内 failover + 本地负缓存**(§2.1,评审 F2);② usage 解析升级为**契约级完整性策略**(§4,评审 F3);③ 明确 **Redis 依赖与降级**(§6,评审 F7);④ 记录**每请求反自动化**未决(§7,评审 F8)。

## 1. 职责(热路径,无状态)

接收客户端请求 → 本地内存选号 → 按快照里的反代配方伪装并转发 → 异步回报结果。
**不存任何上游凭据**,凭据每次从快照现取、用完即弃。无 DB。

契约用进程内 `pkg/contracts`(与 control 同一 module 构建,无跨 repo 版本 skew)。

## 2. 热路径

```
inbound request
  → 协议识别(客户端说的是什么协议)
  → 选号 Acquire(criteria) —— 纯本地内存,查快照 + 过本地负缓存,零网络
  → 协议互转(入站协议 → 上游协议)
  → XADD `attempt_started`;失败则不调用上游并返回 503
  → 反代执行:按 UpstreamProfile 建 utls transport + 注入 header + 转发/SSE 流
  → 若上游返回明确 HTTP 401/403/429/5xx → 在安全边界内运行时 failover 换号重试(§2.1)
  → 协议互转(上游响应 → 入站协议;usage 字段不丢,§4)
  → XADD 终态 Release 到 Redis Stream;成功写入后才向客户端确认
```

选号、限流、粘滞会话都在 gateway **运行时**用 Redis 原子操作(INCR / Lua)强制;规则由 control 在快照里定义。快照的加载与形态见总览 §3.1。

### 2.1 运行时失效处理:请求内 failover + 本地负缓存(评审 F2)
这是 v2 最重要的补充。快照有新鲜度延迟,失效账号可能在快照更新前仍被选中;运行时限流挡不住"已失效账号被继续选中"。gateway 必须自己在运行时兜住,分两件事:

- **请求内 failover 重试**:P1 仅在收到明确 HTTP `401/403/429/5xx` 且尚未向客户端发送首字节时换号重试,最多两次并受单请求 deadline 限制。`network_error` 暂不自动重试,避免上游已执行但响应丢失时重复消耗。P1 不支持 tool-call 重放。
- **本地负缓存/冷却**:收到上述错误立即写**本地内存冷却表**,后续选号跳过该号,**不等下一份 control 快照**。control 消费 Release 后产的新快照作为持久兜底,不是唯一剔除路径。

错误分类用 `ErrorClass` 枚举,**权威定义在总览 §6.3.1(唯一一份,此处不复述表格)**。gateway 侧只需落实本地层的这几条实现约束:

- **本地冷却是"桥接"不是"惩罚"**:所有本地时长秒级到 60s。它唯一的职责是补住快照延迟窗口,不承担权威判罚——判罚在 control。
- **事件驱动解冻优先于计时器**:收到该账号的**新凭据版本 / 新快照 epoch** 时**立即清除**本地冷却,不等计时器到期。`auth_expired` 尤其如此:control 的 refresh loop 通常几秒就修好了,傻等 60s 是在浪费号。
- **两类错误不写账号冷却表**:`forbidden_transport`(403 HTML)走**出口路径** 5–10s 短路,不碰账号;`forbidden_capability`(结构化 403)按 **(账号, 模型)** 维度停选,不冷却整号。混进账号冷却表会误杀健康号。
- **`rate_limited_unknown` 用退避不用长冷却**:5s → 15s → 60s,**上限 5min**。无 reset 头的 429 多半是突发限速,猜成小时级等于自杀。
- **所有本地冷却加 ±20% 抖动**,避免同批被限的账号同秒集体恢复再集体被打挂。
- **粘滞会话**:被 pin 的账号若进冷却,须重新 pin 到新号(session→account 映射走 Redis,见 §6)。
- **防饿死闸**:某平台本地冷却占比超 **50%** 时,提前释放最老的冷却并告警——这多半是平台级问题,不该让用户吃 503(见总览 §6.3.1 规则 3)。

## 3. 反代执行(承接 control 的配方)

- **TLS 指纹注册表**:每个 profile 名字(`codex_rustls` / `node24` …)对应一个 utls 实现,搬 relay 的 `codex_tls.go`
- gateway 拿快照里的 `TLSFingerprint` 字符串**查表**,不写死任何渠道逻辑
- 加渠道时:复用已有指纹 → gateway 不用动;全新指纹 → 才在此加一个 profile

gateway 是纯执行器 —— 渠道的业务逻辑(client_id、refresh)全在 control,与 gateway 无关。这是两角色的唯一深耦合点(总览 §5);单 repo 下只是同 module 两包的约定,不需要跨 repo 兼容协议。

## 4. 协议互转(protokit)+ usage 完整性(评审 F3)

独立零依赖 Go module(仿 new-api 的 relaykit),放本仓库。
- **P1 只实现一个方向**:OpenAI Chat Completions 非流式 JSON → 同形态 OpenAI-compatible 上游;保留 `model/messages` 中 P1 明确允许的字段,不接受客户端自带 `tenant/group/base_url`。
- P1 `stream=true`、tool-call、thinking、multimodal 直接返回结构化 `unsupported_capability`,不得半转换后调用上游。
- **当前本地切片**:OpenAI Chat、Anthropic Messages、OpenAI Responses 的非流式纯文本/标准 function tool 互转，以及 OpenAI Chat 入站到 Anthropic/Responses/Gemini 的纯文本 SSE 转换均有 fixture；非 OpenAI 入站的任意跨协议 streaming、多模态和真实 provider 协议仍 pending（详见 `docs/EVIDENCE.md`）。
- **硬约束:usage 字段不得丢**——按 token 计费依赖它(总览 §6.2)。usage 解析是**契约级**要求,不是实现细节,直接照 sub2api 实战:
  - **强制注入**:OpenAI Chat Completions 向上游强注 `stream_options.include_usage=true`(`openai_gateway_chat_completions_raw.go:384-391`),否则流式不吐 usage。取**最后一个** usage chunk(上游可能重发)。
  - **协议原生解析**:Anthropic 从 `message_start`/`message_delta` 累加;OpenAI /responses 从终止事件(`response.completed` 等)取;Gemini 每 chunk `usageMetadata`(注意 `promptTokenCount` 含 cache 而 Claude `input_tokens` 不含,别重复计)。
  - **上游真不给 usage 的兜底**:按**每上游 usage-integrity 策略**处理(failover / 计 0 / 估算,总览 §6.2 已定首批取值),`Release.UsageSource` 记录来源。如 Grok 成功但无 usage → 转 failover 报错,不静默计 0。
  - **断连不漏计**:客户端断开后**继续 drain 上游**捕获完整 usage,`Release.Partial=true`,已计的 input/cache token 不丢(收入泄漏主要发生在这里)。

## 5. gateway 角色的 P0/P1 交付物

- [x] P0:依赖 `pkg/contracts`,快照消费 + 本地选号 Acquire 骨架(含**本地负缓存过滤**)
- [x] P0:Release Stream producer 骨架,`attempt_started`/终态 Release、事件 ID/schema version 贯穿,失败可观测
- [x] P1:**请求内 failover 重试** + 本地冷却表:明确 HTTP 错误最多两次,仅首字节前;`network_error` 不自动重试
- [x] P1:Release `XADD` 成功后才确认非流式请求,包含 tokens/model/account/tenant + **UsageSource + Partial**
- [ ] P1 验收:按总览 §7.1 注入 `XADD`→`XACK` 间 control 崩溃、重复 event/attempt 重放、`attempt_started`→终态 Release 间 gateway 崩溃;分别断言至少一次投递、ledger 单行幂等和 synthetic `missing/partial` 终态回收
- [ ] P0:TLS profile 注册表 + 一个 profile(codex_rustls)从 relay 搬入
- [x] P0:`protokit` module 接口与本地协议转换实现(usage 字段 + UsageSource 贯穿约束)
- [x] P1:运行时限流(Redis 原子)骨架 + **按渠道读 `AccountLimits.DegradePolicy` 的降级分支**(§6,默认 fail_closed)

## 6. Redis 依赖与降级(评审 F7)

gateway 无 DB,但 Redis 在热路径上承担三件事:**消费快照(唯一数据面)、运行时原子限流、粘滞会话映射**。因此 Redis 是被低估的 SPOF,须显式设计:
- **已启动节点**:快照已在本地内存,Redis 短暂抖动仍可选号服务;Release 写入失败时不得向客户端确认成功,限流/粘滞按渠道策略退化。
- **新节点冷启动**:拉不到快照 → 无法加入,须等 Redis 恢复。
- **限流降级策略:按渠道分别配(已定)**。Redis 失联时的 fail-open/fail-closed 不是全局开关,而是 `AccountLimits.DegradePolicy`(`fail_closed` | `fail_open`)带在快照里,由 control 按渠道下发:
  - codex / claude 等**上游风控严**的渠道 → `fail_closed`:限流失效时拒绝请求,宁可服务短暂不可用,也不让不受控流量打上游触发封号(号池是核心资产)。
  - 自建 / 风控宽松的 apikey 渠道 → `fail_open`:限流失效时放行,保可用性。
  - **默认值 `fail_closed`**:新渠道未显式配置时按保守处理,避免"忘了配"变成号池风险。
  - 参 sub2api 的 QPS 限速 DB 兜底 + 可关闭开关思路,避免抖动击穿。
- 部署侧:Redis 上 HA。

## 7. 未决:每请求反自动化可能击穿热/慢拆分(评审 F8)

`UpstreamProfile` 是**静态**配方,gateway 盲注 header。但 chatgpt2api 的 PoW/Turnstile 调用点在 **`chat-requirements`(每条消息)**,不只是登录。若 **codex provider 走这种 web-backend 路径**,则"每请求现场生成 challenge token"属于 provider(control 慢路径)却必须在 gateway **每请求**执行,静态 Profile 覆盖不了。
**P2 打通 codex 前必须核实**:codex CLI 的 API 端点是否需要每请求 PoW/turnstile。若需要,要么把该 provider 的 per-request minter 作为 gateway 可调用的旁路(热路径可承受的本地计算),要么承认该渠道不适用无状态热路径模型。

## 8. 技术栈

Go 1.23 + Gin;utls(github.com/refraction-networking/utls)做 JA3 伪装;Redis 客户端(消费快照 + 原子限流 + 事件产出 + 粘滞映射)。无 DB —— gateway 无持久状态。
