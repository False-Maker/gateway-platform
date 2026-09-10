# fluxgate + keyhive 系统总览(v2,按评审重构)

> **一个 repo,一个二进制,两个运行角色**:`--role=gateway`(热路径,fluxgate) + `--role=control`(慢路径,keyhive)。
> 热/慢是**逻辑**分离,不是两个 repo。契约用进程内 Go 类型,跨进程/跨节点的数据面仍走 Redis 快照。
> 本文只讲**全局**:角色关系、边界、契约、共享决策、阶段计划、参考项目。
> 各角色"怎么建"见:`../keyhive/docs/architecture.md`(control)、`../fluxgate/docs/architecture.md`(gateway)。
>
> 本版相对 v1 的结构性变更(评审结论):① 双 repo → 单 repo 双角色(消除跨 repo 契约 skew);② 全局单 leader → 每账号 fencing;③ wrapper 全 Go → 混合(Go + 保留 Python turnstile);④ 计量与请求明细分表;⑤ 热路径补请求内 failover + 本地负缓存;⑥ Release 契约补 usage 溯源与完整性策略。

## 1. 为什么按热/慢分角色(而不是拆两个 repo)

按**热路径 / 慢路径**拆**角色**:

- **gateway 角色(fluxgate)= 热路径**:每个推理请求都过它。无状态,水平扩展(3 节点合理)。不存任何上游凭据。
- **control 角色(keyhive)= 慢路径**:不处理一个字节的推理流量。管账号、保鲜 token、产快照。负载随**账号数**增长(见 §6.4 对"不随 RPM"的修正),单主控制面(每账号 fencing,见 keyhive doc §3.4)。

**为什么同一个 repo/二进制、两个角色,而不是两个 repo**(评审 Q4/F9):
- 两个角色的唯一深耦合是 `pkg/contracts`(快照/事件的类型定义)。放在**同一 module 的进程内 Go 包**里,`gateway` 与 `control` 编译自同一次构建,**版本 skew 不可能发生**——不需要设计"旧 gateway 读新 control 快照"的跨 repo 兼容协议。
- 独立扩展照样成立:同一二进制以不同 `--role` 起**不同进程**,gateway 起 3 个、control 起 1 主 1 备。blast-radius 隔离由"不同进程 + 不同资源配额"提供,不需要不同 repo。
- 参考项目自证:sub2api 是同一份代码每实例都跑 snapshot service;new-api 是同一二进制 master/slave。单 repo 多角色是这类系统的常态。

快照是唯一跨进程数据面:control 挂了,gateway 靠最后一份 Redis 快照继续服务;control 的控制面不被 QPS 冲垮。

## 2. 边界(两个角色)

| | control 角色(keyhive) | gateway 角色(fluxgate) |
|---|---|---|
| 路径 | 慢路径 / 有状态控制面 | 热路径 / 无状态执行器 |
| 进程 | 1 主 + 1 备 | 水平扩展,3 节点 |
| 写者模型 | **每账号 fencing token**(非全局 leader) | 无写者,只读快照 + Redis 原子 |
| 触碰流量 | 否 | 是 |
| 凭据 | 加密落库 + 保鲜 | 从快照拿短 TTL token,用完即弃 |
| 失效感知 | 权威/持久层(消费 Release 更新状态) | **运行时**层(本地负缓存 + 请求内 failover) |
| 反代执行 | 只定义配方 | 按配方 utls 伪装 + 转发 |
| 持久状态 | PG(账号/凭据) + 计量/明细分离存储 | 无 DB |

## 3. 连接:快照 + Redis Streams,无同步 RPC

两条异步通道,gateway 请求路径上从不调用 control:
- control → gateway:Redis **快照**(可调度账号 + credential + upstream_profile + 限额规则)
- gateway → control:Redis **Streams**(Release 事件),**至少一次投递**;control 仅在 PG 事务提交后 ACK

### 3.1 快照形态(P0 定稿,借 sub2api 的 fencing 范式)
快照是唯一数据面,以下键格式和发布规则固定:

- **分桶**: `bucket_id = hex(sha256(platform + "\\x00" + group))[:16]`;原始 `platform/group` 仍写入快照 envelope,便于排查。键族为 `snap:buckets`(Set)、`snap:active:<bucket>`(当前版本)、`snap:ver:<bucket>`(单调版本)、`snap:epoch:<bucket>`(发布 generation)、`snap:data:<bucket>:<version>`(Hash, field=`account_id`, value=账号 JSON)、`snap:retired:<bucket>`(ZSet)、`snap:lock:<bucket>`(重建锁)。
- **版本 + fencing**:账号写者的 fencing 权威在 **PG**;写者在读凭据/刷新前取得账号 fencing token,写回使用 `account_id + fence_epoch` 条件更新,旧 writer 自动失效。Redis 只负责快照发布:每次 publish 用 Lua 原子执行“校验 `snap:epoch:<bucket>` → `INCR snap:ver:<bucket>` → 写 `snap:data:<bucket>:<version>` → 翻转 `snap:active:<bucket>` → 记录旧版本”;旧版本保留 **grace-TTL 60s**,数据键 TTL 为 120s。
- **发布 CAS**:Lua 收到 `expected_epoch` 不等于当前 epoch 时返回 `STALE_EPOCH`,不得覆盖 active 指针;成功返回新 version。`snap:epoch` 只是 bucket 发布 generation,不是账号 fencing token,不得据此判断凭据写者合法性。
- **重建节奏**:启动全量 + 定期全量兜底 + 事件驱动增量(消费 Release 后只重建受影响的 `platform:group` 桶)。全量重建**按 generation 计数合并**,mid-rebuild 到达的请求不被正在跑的旧重建满足(它可能早于事件的事务提交)。
- **gateway 加载方式**:启动全量拉进本地内存,之后订阅变更(pub/sub 或版本号轮询)增量更新;读取顺序固定为 `GET active` → `HGETALL data:<version>` → 校验 envelope/version,选号是纯本地内存操作,不回读 Redis(限流/粘滞除外)。版本不存在或校验失败时保留旧快照并告警,不得加载不完整桶。

## 4. 契约(pkg/contracts,进程内 Go 包,control 定义 gateway 消费)

```go
// Acquire:gateway 本地内存选号的输入(进程内,不跨网络)
type Criteria struct {
    Platform   string   // codex / claude / gemini ...
    Model      string
    Group      string   // 租户 / 池分组
    SessionKey string   // 粘滞会话
}

// Lease:快照里每个可调度账号的完整信息,gateway 选中后直接发请求
type Lease struct {
    AccountID  string
    Credential Credential      // 短 TTL access_token 或 apikey
    Profile    UpstreamProfile // 反代配方
    Limits     AccountLimits   // 并发 / RPM + Redis 失联降级策略,gateway 运行时用 Redis 原子强制
    TTL        int             // 秒
}

type UpstreamProfile struct {
    BaseURL        string
    TLSFingerprint string            // "codex_rustls" —— gateway 查表实现
    UserAgent      string
    ExtraHeaders   map[string]string // anthropic-beta 等
    WS             bool
    // 见附录未决问题:若某渠道需每请求现场生成 challenge，此静态 Profile 不足以覆盖
    // （2026-09-11：codex 已核实不需要；该风险目前只对未接入的 ChatGPT web 渠道成立）
}

// Release:一次上游尝试的终态事件,gateway 写入 Redis Stream 后由 control 消费
type Release struct {
    SchemaVersion int
    EventID       string // 重投时不变; usage_ledger 唯一键
    RequestID     string // gateway 生成,不信任客户端
    AttemptID     string // 每次上游尝试唯一
    AttemptNo     int
    ProducerID    string
    OccurredAt    time.Time
    AccountID   string
    Provider     string
    StatusCode  int
    LatencyMS   int
    TokensIn    int
    TokensOut   int
    CacheReadTokens  int
    CacheWriteTokens int
    Model       string
    TenantID    string
    ErrorClass  string // ok / rate_limited / auth_expired / upstream_5xx / blocked ...(见 §6.3)
    UsageSource string // upstream / estimated / missing —— 计费可信度溯源(评审 F3)
    Partial     bool   // 断连/超时的部分用量,已计的 input/cache token 不丢(评审 F3)
}
```

P0 共享类型固定为以下语义(字段名可直接作为 Go/JSON 契约,新增字段只能向后兼容追加):

```go
type Credential struct {
    Kind       string    // static | oauth
    AccessToken string   // static apikey 或 oauth access token; Lease 中只放短 TTL 值
    ExpiresAt  time.Time
    Version    int64     // 凭据轮换版本,用于清除 gateway 本地冷却
}

type TokenBundle struct {
    AccessToken  string
    RefreshToken string   // 仅 control/wrapper 使用,不得进入 Lease、Release 或日志
    IDToken      string
    ExpiresAt    time.Time
    Scopes       []string
    AccountID    string
    Email        string
}

type AccountLimits struct {
    MaxConcurrency int
    RPM            int
    StickyTTL      int
    DegradePolicy  string  // fail_closed | fail_open, default fail_closed
}

type Account struct {
    ID          string
    Provider    string
    Platform    string
    Group       string
    Credential  Credential
    Profile     UpstreamProfile
    Limits      AccountLimits
    Status      string
    FenceEpoch  int64
    Quota       QuotaInfo
}

type ImportRequest struct {
    SourceSystem string
    SourceID     string
    Provider     string
    AuthMode     string    // static | oauth | cli | pkce | device
    StaticKey    string
    TokenBundle  *TokenBundle
    Metadata     map[string]string
    DryRun       bool
}

type QuotaInfo struct {
    Items []QuotaItem
}

type QuotaItem struct {
    Scope             string          // account | model
    Model             string
    Unit              string          // token | request | credit | fraction
    Limit             *int64
    Remaining         *int64
    LimitExact        *Decimal
    RemainingExact    *Decimal
    RemainingFraction *Decimal        // 0..1, nil means unknown
    Precision         *QuotaPrecision // precise usage/limit/overage fields
    ResetAt           *time.Time
}

// Decimal is a strictly validated decimal string serialized as a JSON number.
```

`credentials` 表保存 refresh token/static secret 的密文;上面的 `Credential` 是解密后发布到受 ACL 保护的快照中的最小短期值。gateway 不接收 `RefreshToken`,也不能通过任何契约字段索取它。

### 4.1 Release 投递与 P1 响应顺序

- P1 统一使用 Redis Stream `gateway:events:v1`,consumer group 为 `control`;每条记录至少包含 `event_type`(`attempt_started` 或 `release`)、`schema_version`、`event_id`、`request_id`、`attempt_id`、`producer_id`、`occurred_at` 和 JSON `payload`。Stream 由定时 trim 任务按 `occurred_at` 保留 7 天,不得按固定长度静默丢弃未消费 pending。
- gateway 在发起上游请求前先写 `attempt_started`;写入失败则不调用上游并返回 503。每次上游尝试只产生一条终态 `Release`;failover 的不同尝试使用不同 `AttemptID`。
- terminal `event_id` 由 `attempt_id` 确定性生成,因此 gateway 与超时回收器并发补写时仍只有一个 ledger 记录。若 gateway 在上游完成后、终态 `XADD` 前崩溃,control 在 `attempt_lease` 超时后生成 `UsageSource=missing`、`Partial=true` 的合成终态事件;不猜测实际 token 数。
- control 使用 consumer group,PG 事务提交后才 `XACK`。pending 事件空闲 60s 后可 reclaim,最多重试 5 次;坏版本或格式错误进入 `gateway:events:dlq:v1`,不得静默丢弃。
- `usage_ledger.event_id` 和 `usage_ledger.attempt_id` 均有唯一约束。重复投递只 ACK,不得重复计量或重复更新账号状态。
- P1 的非流式静态 apikey 请求只有在 `XADD` 成功后才向客户端返回成功;写入失败返回 5xx。gateway 在写入 Stream 后崩溃不影响 control 重放。
- P1 暂不处理同一 `AttemptID` 的增量 usage;流式 drain 和多次 usage 更新留到后续阶段。

消费与死信规则固定为:消费事务先锁定 `request_attempts`/幂等键并写 PG,提交成功后 `XACK`;pending 空闲超过 60s 用 `XAUTOCLAIM`/等价 reclaim 重新投递。每条消息最多处理 5 次,重试次数写入消息元数据;超过上限先写 `gateway:events:dlq:v1`(保留原始字段、最后错误、次数、时间),再 ACK 原消息。未知 major `schema_version`、必填字段缺失、JSON 无法解析均走 DLQ,不得当作成功 ACK。

P0 必须暴露以下可验收指标: `control_stream_pending`、`control_stream_reclaim_total`、`control_stream_dlq_total{reason}`、`usage_ledger_duplicate_total`、`request_attempt_open_age_seconds`、`request_attempt_recovered_total{source=gateway|synthetic}`、`release_xadd_total{result}`。合成终态和 `UsageSource=missing` 的比例单独告警,不得被普通成功率掩盖。

P1 的网关请求 deadline 为 120s,`attempt_lease` 至少为 300s,回收器不得在有效请求 deadline 内制造合成终态。`schema_version` 的未知 major 版本直接进入 DLQ;同一 major 内只允许向后兼容的新增字段。

**以下类型 P0 定稿**(此处只列引用):
- `Credential` —— 区分 static/oauth,含加密 secret + 短 TTL access_token
- `AccountLimits` —— 并发/RPM/粘滞策略 + **`DegradePolicy`**(`fail_closed` | `fail_open`,默认 `fail_closed`):Redis 失联时该渠道的限流降级行为,**按渠道分别配**而非全局开关(见 fluxgate doc §6)
- `TokenBundle` —— refresh loop 与 wrapper 交换的 access+refresh+expires_at+scopes
- `QuotaInfo` —— **按 codex + kiro 取并集**定型(见 keyhive doc §2.1)
- `Account`、`ImportRequest`
`ErrorClass` 分类表是 P0 交付物,决定冷却与踢号逻辑(粒度见 §6.3)。

## 5. 唯一深耦合点:反代配方

`UpstreamProfile.TLSFingerprint` 是一个名字(如 `codex_rustls`)。control 写下名字,gateway 必须真的实现该 utls profile。
- 加**普通/apikey 渠道** = 只动 control 侧代码
- 加**需要全新 TLS 指纹的渠道** = control 加 provider 包 + gateway 加 utls profile(罕见)

单 repo 下这不再是"跨 repo 契约"问题,只是同一 module 内两个包的约定:profile 名字定死不改,加字段只增不删。

## 6. 共享决策(跨两个角色)

### 6.1 遗留迁移:一次性从 new-api 导入(策略 C),但"无损"只对 Codex 成立(评审 F5 修正)
线上是 **new-api**(非 relay)。核实其真实 schema 后的结论:

- **Codex(`channel.type=57`)OAuth 可无损迁出**:refresh/access/id_token/account_id/email/expiry 全打包成一个自洽 JSON 存在 `channels.key`,且刷新写回同列,它就是权威库。
- **但这个 new-api 没有 Kiro、没有 Copilot 渠道类型**;**Claude(14)、Gemini(24)是纯静态 apikey**,无 OAuth 路径。设计的"首批 provider 全集"(codex/claude/gemini/kiro/copilot/antigravity/grok/windsurf)**不能假设都从 new-api 迁出**——codex 之外的 oauth 账号若线上存在,是塞在某个 Custom/Sub2API 渠道 `key` 里的自定义格式,**迁移脚本不能假设 schema,须逐 provider 人工确认**。
- **三个必须一起搬否则丢信息/搞坏刷新的字段**:① `setting` 里的 **proxy**(codex 刷新流程本身用它,丢了直接搞坏 refresh);② codex `client_id` 是 new-api 里的**硬编码常量**、不在库里,新系统得自己供;③ 多 key 渠道 `key` 是 JSON **数组** → 映射 N 个账号,per-key 禁用态只在 `channel_info`。
- 量级几十个 channel,一次性脚本可行;但**字段映射按 provider 分别定**,不一把梭。硬约束:control 的 accounts 表上游凭据字段须对齐 new-api channel(含上述三项),否则导入丢信息。

**新系统首次迁移模型(不复用线上旧 migration map)**:
- 首批迁移覆盖 `channels + users + tokens + quota/balance + group`; `logs` 历史明细不进入 P1 运行链路。
- 一个 new-api user 映射为一个新系统 tenant 和一个默认 principal;该用户的 tokens 归属该 tenant。保留 `source_system + source_id` 作为幂等来源标识。
- 迁移通过 staging 记录源快照时间、原始字段摘要、转换结果、target ID、拒绝原因和 `imported / reconciled / rolled_back` 状态。线上旧的 `elucid_*_migration_map` 仅作事实参考,不作为新系统契约。
- P1 只使用合成数据验证技术闭环;允许生产只读 schema/数据核对和 dry-run,不允许生产写入、部署或流量切换。

首次迁移的语义映射固定如下,具体列名以 dry-run 的 `information_schema` 结果为准,不在脚本中硬编码未核实的列:

| new-api 语义 | 新系统目标 | 规则 |
|---|---|---|
| `channels.id` | `accounts.source_id` | `source_system='new-api'`;同一来源键重复执行只更新 staging 状态,不新建账号 |
| `channels.type` / provider 标识 | `accounts.provider` | 仅 codex/claude/gemini 等已注册 provider 可导入;未知类型进入 rejected |
| `channels.key` | `credentials` 密文 | codex OAuth JSON 原样解析后拆入 `TokenBundle`;JSON 数组按 key 展开多个账号 |
| `channels.setting.proxy` | `accounts.proxy`/profile 元数据 | 必须保留,不得从默认代理推断覆盖 |
| `channels.models` / model mapping | `accounts` 能力集合 | 未识别模型不丢弃账号,写入待人工确认状态 |
| `channels.group` / `channel_info` 禁用态 | `accounts.group` / status | per-key 禁用态优先于 channel 级状态 |
| `users.id` / 用户标识 | `tenants.source_id` + 默认 principal | 一个 source user 只生成一个 tenant 和一个默认 principal |
| `tokens` | tenant API token | 只迁移 hash/过期/撤销状态;明文仅在受控 dry-run 输入窗口存在,不写日志 |
| quota/balance 语义字段 | `tenant_quota_snapshot` / wallet opening balance | 保留 source unit、原始整数值和快照时间,不在迁移脚本中换算或扣费 |
| `group` | tenant/account group | 先按 source ID 建组,缺失组归入显式 `default` |

每批 dry-run 必须输出:源/目标按 provider 和状态计数、未知类型数、拒绝原因计数、quota/balance 原值摘要、重复 source key 数和字段 digest。只有摘要与目标 ID 写入 `migration_records`,不得把 credential 明文写入 staging。

### 6.2 计费:按 token,计量/计费分离 + usage 完整性策略(评审 F3)
- **计量与计费分离**:P0 Release 采集 `TokensIn/Out + Model + AccountID + TenantID`;计费逻辑 P4 读计量流水 × 单价 → 扣钱包,在 control 慢路径算,不碰热路径。
- **usage 完整性是契约级决策,不是实现细节**(sub2api 实战证明):
  - **强制注入**:OpenAI Chat Completions 必须向上游强注 `stream_options.include_usage=true`,否则流式不吐 usage。
  - **上游会真不给 usage**:如 Grok 返回成功但无 usage。P0 定**每上游 usage-integrity 策略**三选一——failover(换号重试)/ 计 0 / tokenizer 估算;`Release.UsageSource` 记录实际来源(`upstream|estimated|missing`),供 P4 计费区分可信度。sub2api 只实现了 failover 与计 0 两种,估算需自行决定是否引入。
- **断连是收入泄漏点**:客户端断开后 gateway **继续 drain 上游**以捕获完整 usage,`Release.Partial=true` 标记部分用量,已计的 input/cache token 不丢。

P0 为每个已注册 provider 必须显式配置 `usage_integrity`(没有配置不得进入可调度快照),首批取值如下:

| provider/场景 | 策略 | 具体行为 |
|---|---|---|
| P1 非流式静态 apikey(OpenAI-compatible) | `failover` | 完整响应已缓冲但无 usage 时换号重试;达到预算则返回 502,Release 标记 `UsageSource=missing` |
| Anthropic Messages、Gemini | `failover` | 首字节前缺 usage 可换号;首字节后不得重放,保留响应并标记 `missing/partial` |
| Grok | `failover` | 成功响应无 usage 视为完整性失败;按上行边界执行换号或 `missing` 终态,不得静默计 0 |
| Codex、Kiro OAuth(P2) | `failover` | 接入前先用真实响应确认 usage 字段;未确认前禁止标记为 `upstream` |

`estimated` 需要单独的 tokenizer 版本和校准报告,P1/P2 不启用;`zero` 仅允许未来 provider 在评审后显式选择。策略结果必须写入 `Release.UsageSource`(`upstream|estimated|missing`)和 `Partial`,不能由计费层猜测。

### 6.3 失效感知的归属:运行时踢除在 gateway,持久状态在 control(评审 F1/F2)
这是 v2 最重要的修正。失效账号的处理**分两层**:

- **gateway 运行时层(快)**:选中的账号出错时,gateway ①请求内 **failover 换号重试**(N 次,对用户屏蔽单号失效);②写**本地负缓存/冷却**,立即本地停选该号,不等下一份快照。
- **control 权威/持久层(慢)**:消费 Release 更新账号 status、冷却到期、配额,产新快照作为持久兜底。

`ErrorClass` 枚举表是这两层的**共同词汇**,定稿见 §6.3.1。

#### 6.3.1 `ErrorClass` 定稿(P0 交付物)

**分层原则(先读这条,否则下表的数字会看错)**:
gateway 本地冷却的唯一职责是**桥接快照延迟窗口**,不是惩罚账号。因此本地冷却一律**秒级到 60s**,且**收到该账号的新凭据版本/新快照 epoch 时立即清除**(事件驱动解冻优于等计时器)。真正的权威冷却在 control,按各错误的**真实恢复语义**定,不是猜一个惩罚时长。
v1 抄来的"401 冷却 10min"是把两层混为一谈:control 的 refresh loop 几秒即可修好,本地却把号闲置十分钟——在只有几十个号的池子里这是实打实的浪费。

| ErrorClass | 触发 | gateway 本地冷却(桥接) | control 持久处理(权威) | failover | 计账号处罚 |
|---|---|---|---|---|---|
| `ok` | 成功 | — | 重置该号失败计数 | — | 否 |
| `auth_expired` | 401,OAuth 号可刷新 | **60s**,收到新凭据版本立即清除 | 不设固定时长,由刷新结果驱动:成功即恢复,失败退避重试 | 是 | 否 |
| `auth_invalid` | 401,缺/废 refresh_token | **不用计时器**,直接本地停选 | **永久踢池 + 告警**,需重新授权 | 是 | 是(终态) |
| `forbidden_transport` | 403 且响应体是 HTML(CDN/WAF 拦截) | **不罚账号**;改为出口路径 5–10s 短路 | 不罚账号;**统计速率,超阈值告警**(见下"探测信号") | 是 | **否** |
| `forbidden_capability` | 403 结构化(账号确无该模型/功能权限) | 按 **(账号, 模型)** 维度停选,不冷却整号 | 标记能力缺失,**持久**排除该号在该模型的候选集 | 是(换号) | 否(非健康问题) |
| `rate_limited_known` | 429 且带 reset 头 | 严格按头部时间,**加 ±20% 抖动** | 同步窗口余量(codex 5h/7d 各自独立计) | 是 | 否 |
| `rate_limited_unknown` | 429 无 reset 头 | **5s → 15s → 60s 退避,上限 5min** | 连续多轮触发才升级为长冷却 | 是 | 仅连续触发时 |
| `upstream_5xx` | 上游 5xx | **2–5s** | 不罚账号,计入**平台级熔断** | 是 | 否 |
| `blocked` | 风控/JA3 被拒 | **60s** | 账号级短冷却 + **平台级告警**(见下) | 是 | 轻 |
| `network_error` | 超时/连接重置 | **2–5s** | 不计账号处罚 | **P1 否** | 否 |

**三条比数字更重要的规则**:

1. **429 无 reset 头绝不能猜长**(最有把握的一条,sub2api 实战教训)。无头 429 多半是每分钟突发限速而非配额耗尽;猜成 5 小时等于亲手杀掉一个健康号,还烧光 failover 预算。宁可短冷却多试几次。
2. **`forbidden_transport` 与 `blocked` 的速率是 R4(TLS 指纹时效)的探测信号**。单次不罚号,但若**同平台多个账号同时**开始返回 HTML 403 或被风控拒,那不是账号问题,是出口 IP 或 TLS 指纹被标记了。接到告警上,R4 就从"没有检测机制"变成"有了"——不要一个号一个号地烧着去发现这件事。
3. **池子防饿死闸**(小池必需):若某平台被冷却账号占比超过 **50%**,大概率是平台级问题而非账号问题,此时**提前释放最老的冷却**并告警,而非让用户吃 503。
4. 所有冷却**统一加 ±20% 抖动**,否则一批同时被 429 的账号会在同一秒集体恢复、集体再被打挂。

**待线上回调的数字**:`auth_expired` 的 60s 桥接值取决于快照重建的实际延迟——若事件驱动增量能在 5s 内到达,可降到 15–20s。**P0 先按上表落,P1 跑通闭环后用真实快照延迟分布回调一次**。429 退避阶梯、403 HTML 不罚号、5xx 不罚号三条有参考实现背书,可直接定死。

### 6.4 对"control 负载不随 RPM 增长"的修正:计量与请求明细分表(评审 F1)
v1 立论"control 负载随账号数而非 RPM 增长"对**控制状态**成立,但**每请求 Release → 请求明细日志**这条路径是 RPM 级的。因此:
- **计量流水**(计费必需):可靠、可聚合,低频落 control 的持久存储。
- **请求明细日志**(调试用,高频):走**独立、可丢/可抽样**的存储(ClickHouse 风格或抽样),**不经过单写者、不进控制状态 PG**。
两者物理分离,control 的单写者路径才真正只随账号数增长。

### 6.4.1 P1 身份与凭据边界

- gateway 由已认证的内部 `AuthContext` 决定 `TenantID`、用户和分组;客户端请求体中的同名字段不可信。`SessionKey` 只能在当前 tenant 内生效。
- `BaseURL`、代理、TLS profile 和上游凭据只来自 control 快照;客户端不能指定任意上游地址或 profile。
- gateway Redis ACL 只允许读取快照、写入 Release Stream、执行限流/粘滞脚本;不能发布快照、读取 refresh token 或写 PG。凭据不得进入日志、Release 或错误响应。

### 6.5 控制台:封存 P6,后端预留三处钩子
控制台本身 P6 做。P0 先埋:
- 账号健康看板 → control accounts 表 status 变更时间戳
- 用量报表/对账 → 计量流水(6.2 已覆盖)
- 实时请求日志 → 请求明细存储(6.4 的独立存储,不进控制 PG)

## 7. 阶段计划(两角色对齐)

P0 契约与骨架:Release Streams/consumer group/幂等键/schema version、PG fencing、快照原子发布、AuthContext 与 Redis ACL、迁移 staging 结构、Codex challenge 前置验证 → P1 最小可靠闭环:合成 tenant + **非流式静态 apikey**,明确 HTTP failover 边界(最多两次,只在客户端首字节前; `network_error` 不自动重试),Release 至少一次投递、PG ledger 幂等、pending 恢复与 gateway/control 崩溃测试 → P2 仅在前置验证通过后接入 OAuth(codex/claude 两 provider,**wrapper 混合语言 lease 契约**) → P3 全 provider + quota + protokit 协议互转(**usage 溯源贯穿**) → P4 多租户计费(读计量流水,用 UsageSource 判可信度) → P5 调度深化 → P6 控制台

### 7.1 P1 验收标准(已定)

- **Release 至少一次投递**:在 `XADD` 后、`XACK` 前注入 control 进程崩溃;事件必须进入 pending 并在 reclaim 后再次消费,最终 ACK。不得以丢事件或客户端重试作为通过条件。
- **ledger 幂等**:将同一 `event_id` / `attempt_id` 重放多次,`usage_ledger` 只能保留一条计量记录,账号状态只能产生一次有效变更;重复消息最终 ACK。
- **gateway 崩溃后可恢复**:在 `attempt_started` 后、终态 `Release` 前注入 gateway 崩溃;`attempt_lease` 到期后 control 必须生成唯一 `missing/partial` 终态并关闭 `request_attempts`,不得留下永久 pending 或重复入账。该断言不要求恢复崩溃窗口内未观测到的真实 token,缺失量通过 `UsageSource=missing` 和监控指标显式呈现。

生产迁移是独立上线闸门,不属于 P1 技术闭环。允许对生产做只读 schema/数据核对和迁移 dry-run,**不允许生产写入、部署或流量切换**。

## 8. 技术栈 + 单 repo 布局

Go 1.23 + Gin + pgx + Redis。**单一 Go module,一个二进制,`--role=gateway|control` 切换角色**:

```
gateway-platform/            # 单 repo,单 module
  cmd/gwd/                   # 入口:--role=gateway|control|wrapper,子命令 import / tenant create
  cmd/new-api-migrate/       # 一次性 new-api 迁移 CLI(默认 dry-run)
  pkg/contracts/             # Criteria/Lease/UpstreamProfile/AccountLimits/Release/Quota + wrapper job 契约(进程内共享)
  pkg/protokit/              # OpenAI Chat / Anthropic / Responses / Gemini 协议互转
  internal/control/          # keyhive 角色:import / refresh / quota / 产快照 / 租户 token 发布 / 每账号 fencing(扁平包)
  internal/control/provider/ # 一渠道一包 + registry + 共享 EndpointProfile
  internal/control/wrapper/  # OAuth 执行器:Redis Stream 租约队列 / worker / PKCE
  internal/control/credentials/ # AES-GCM 凭据信封
  internal/gateway/          # fluxgate 角色:鉴权 / 多 bucket 选号 / 反代 / utls 指纹 / 运行时限流 + 负缓存
  internal/snapshot/         # 快照与 token 哈希读写(Redis,fencing/版本/grace-TTL),两角色共用
  internal/events/           # Redis Stream producer / consumer(attempt_started / release / DLQ)
  internal/migration/newapi/ # new-api schema 读取与映射
  migrations/                # 独立 .sql 迁移文件 + embed Apply(control 用;gateway 无 DB)
```

- control:目前是扁平包按文件划分,未采用 `internal/modules/*` 分层;独立 `.sql` 迁移。
- gateway:无 DB;utls 做 JA3 伪装;Redis 消费快照 + 原子限流 + 事件产出。
- 每账号 fencing 由 PG 的 `fence_epoch` 权威控制;Redis 只保存快照 bucket epoch/version。若保留任何全局 singleton 锁(定时任务),优先 PG 会话级 advisory lock(断连自动释放)。
- **文档仍分三份,`fluxgate`/`keyhive` 作为角色名保留**;目录 `keyhive/`、`fluxgate/` 现阶段只放各自 doc,代码统一在 `internal/{control,gateway}`。
- 第三个角色 `--role=wrapper`(OAuth 执行器)在 §1/§2 的两角色表述之外,以独立进程和独立 Redis ACL 身份运行,见 keyhive doc §3.3。

## 9. 参考项目总表

下列参考项目路径均相对 `/home/elucid/projects/`(本 `gateway-platform` 的上级目录)。

| 用途 | 角色 | 项目 | 具体路径 |
|---|---|---|---|
| OAuth 策略引擎(Go 重写蓝本) | control | elucid-relay | `elucid-relay/apps/oauth-wrapper/src/strategies.mjs` |
| wrapper 领 job 循环(claim/complete/fail) | control | elucid-relay | `elucid-relay/apps/oauth-wrapper/src/runner.mjs` |
| wrapper CLI 入口 | control | elucid-relay | `elucid-relay/apps/oauth-wrapper/src/index.mjs` |
| import 模板/payload 结构蓝本 | control | elucid-relay | `elucid-relay/services/gateway-api/internal/httpserver/account_import.go` |
| kiro provider:OAuth/refresh/enterprise 头 实测 | control | Kiro-account-manager | `Kiro-account-manager/test/`、`Kiro-account-manager/Kiro-account-manager/docs/API-Proxy-Guide.md` |
| codex provider:authorize + PoW/turnstile/sentinel(**turnstile 保留 Python**) | control | chatgpt2api | `chatgpt2api/services/openai_oauth.py` / `oauth_login_service.py`、`chatgpt2api/utils/pkce.py` / `pow.py` / `turnstile.py` / `sentinel.py`;号池/导入 `account_service.py` |
| provider 分渠道多适配器目录结构 | control | new-api | `new-api/relay/channel/`(40 个适配器目录) |
| 迁移源:上游账号表(codex 无损,余者逐个确认) | control | new-api | `new-api` 的 `channels` 表(`model/channel.go`;codex oauth 在 `relay/channel/codex/oauth_key.go`) |
| 迁移源:用户/额度/用量(P4) | control | new-api | `new-api` 的 `users` / `tokens` / `logs` 表 |
| **每账号 fencing / 版本化快照 / outbox / 限流分类** 参考 | 两角色 | sub2api | `sub2api/backend/internal/service/`(scheduler_snapshot_service.go / scheduler_outbox.go / leader_lock.go / ratelimit_service.go / concurrency_service.go)、`internal/repository/scheduler_cache.go` / `scheduler_outbox_repo.go` |
| SSE usage 解析(强注 include_usage / 断连 drain / 每上游 integrity) | gateway | sub2api | `sub2api/backend/internal/service/`(gateway_upstream_response.go / openai_gateway_chat_completions_raw.go / openai_gateway_usage_integrity.go / openai_gateway_response_handling.go) |
| 模块分层风格 | control | elucid-gateway | `elucid-gateway/internal/modules/*` |
| `.sql` 独立迁移文件风格 | control | elucid-relay | `elucid-relay/internal/migrations/sql/`(27 个 .sql) |
| utls JA3 指纹反代(codex_rustls 搬入蓝本) | gateway | elucid-relay | `elucid-relay/services/gateway-api/internal/httpserver/codex_tls.go` |
| codex 反代/代理链 + 反自动化 另一份参考 | gateway | chatgpt2api | `chatgpt2api/services/proxy_service.py`(代理链/Cloudflare clearance,**不含** PoW)、`chatgpt2api/services/openai_backend_api.py`(PoW/turnstile 真实调用点;codex 端点已核实**不需要**,见附录)、`chatgpt2api/utils/pow.py` / `turnstile.py` / `sentinel.py` |
| 协议互转独立 module 参考 | gateway | new-api | `new-api/relay/`(relaykit 独立 go module + channel 适配器) |
| 入站客户端协议标记(http/ws) | gateway | sub2api | `sub2api/backend/internal/service/openai_client_transport.go` |

**未采用**:ElucidRouter(与 sub2api 同源重叠)、TokenRouter(仅 deploy 无代码)。

## 附:未决架构问题(P2 前必须核实,评审 F8)

> **2026-09-11 更新:no-cred 层面已核实,结论"不需要"。** 见 `docs/C6-CODEX-PER-REQUEST-CHALLENGE.md`。
> 下段的条件句"**若 codex provider 走的是这种 web-backend 路径**"不成立:codex 与 web 通道**同主机不同 path**。
> 蓝本里 `/backend-api/codex/responses` 是**纯 Bearer** 调用(`openai_backend_api.py:596`/`:785`),
> 不带 sentinel 头、不调 `chat-requirements`;而本平台正是这个端点(`internal/control/provider/profile.go:57-58`)。
> 故 `UpstreamProfile` 静态配方够用,**不开** per-request minter 旁路,§5 热/慢拆分**不破**。
> 剩余部分:拿真实 codex 账号对该端点发一次只带 Bearer 的**通用文本推理**请求确认(live-gate,TODO §C6)。
> 另注:**需要每请求 PoW/turnstile 的是 ChatGPT web 渠道**,本平台当前未把它建模为 provider;若将来接入,本问题原样回来。

`UpstreamProfile` 是**静态**配方。chatgpt2api 的 PoW/Turnstile 调用点在 **`chat-requirements`(每条消息)**,不只是登录。若 **codex provider 走的是这种 web-backend 路径**,则"每请求现场生成 challenge token"属于 provider(control 慢路径)却必须在**热路径每请求执行**,干净的热/慢拆分会破。**P2 打通 codex 前必须先确认**:codex CLI 的 API 端点是否需要每请求 PoW/turnstile。若需要,要么把该 provider 的 per-request minter 作为 gateway 可调用的旁路,要么承认该渠道不适用无状态热路径模型。chatgpt2api 的 PoW/Turnstile 调用点在 **`chat-requirements`(每条消息)**,不只是登录。若 **codex provider 走的是这种 web-backend 路径**,则"每请求现场生成 challenge token"属于 provider(control 慢路径)却必须在**热路径每请求执行**,干净的热/慢拆分会破。**P2 打通 codex 前必须先确认**:codex CLI 的 API 端点是否需要每请求 PoW/turnstile。若需要,要么把该 provider 的 per-request minter 作为 gateway 可调用的旁路,要么承认该渠道不适用无状态热路径模型。
