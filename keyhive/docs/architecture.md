# keyhive(control 角色)怎么建(v2,按评审重构)

> 号池 / 控制面的构建说明。keyhive 是**单 repo 单二进制的 `--role=control` 角色**,不是独立 repo。
> 本文只讲 **control 角色自身怎么搭**。系统全局(角色拆分、边界、契约全文、快照形态、共享决策、阶段计划、参考项目)见
> `../../docs/fluxgate-keyhive-overview.md`。配套角色:`--role=gateway`(见 `../../fluxgate/docs/architecture.md`)。
>
> 本版相对 v1 的变更(评审结论):① 全局单 leader → **每账号 fencing**(§3.4);② wrapper 全 Go → **混合语言 lease 契约**(§3.3);③ 建表:**计量与请求明细分表**(§4);④ 迁移无损性按 provider 分别确认(§3.1)。

## 1. 职责(慢路径,不碰流量)

control 做三件事:
1. **import** —— 导入并加密保管上游账号凭据(oauth 与 apikey)
2. **refresh loop** —— 后台保鲜 oauth token(号池心脏),按账号 fencing 串行
3. **产快照** —— 把"可调度账号 + 反代配方"写进 Redis 快照,供 gateway 消费

快照形态、契约结构见总览 §3.1 / §4。control 是快照与契约的**生产者/持有者**(契约是进程内 `pkg/contracts` 包,gateway 编译自同一 module,无跨 repo 版本 skew)。

## 2. Provider 隔离:一渠道一包 + 注册表

一个上游渠道 = OAuth 策略 + upstream profile + quota adapter 三件事绑定。隔离到一个包里。

```
internal/control/provider/
  provider.go        // 只放接口 + registry,零依赖
  codex/  claude/  gemini/  kiro/  github_copilot/  antigravity/  grok/  windsurf/
    <name>.go        // 实现 Provider,init() 自注册
    strategy.go      // OAuth: authorize / refresh / revoke
    profile.go       // 产出 UpstreamProfile(反代配方,给 gateway)
    quota.go         // 额度查询
    const.go         // client_id / scope / cli 版本等魔数,全锁在此
```

```go
type Provider interface {
    Kind() string
    Authorize(ctx context.Context, req ImportRequest) (TokenBundle, error)
    Refresh(ctx context.Context, cred Credential) (TokenBundle, error)
    Revoke(ctx context.Context, cred Credential) error
    Profile(acc Account) UpstreamProfile
    Quota(ctx context.Context, cred Credential) (QuotaInfo, error)
}
func init() { provider.Register(&Codex{}) } // 新增渠道 = 加一个目录 + 一行 import
```

隔离硬边界:
1. 上游魔数只出现在各自 `const.go`,上游改版只改一个文件。
2. 单渠道 refresh 失败 / 被封 JA3,隔离在自己包 fail,不拖累其他渠道保鲜循环。
3. **apikey 静态账号是退化 provider**:`Refresh` 原样返回 token、`Profile` 只给 base_url,不搞两套接口。

首批 provider(现有全集):codex、claude、gemini、kiro、github_copilot、antigravity、grok、windsurf。
**注意(评审 F5)**:这批里只有 codex 能从 new-api 无损迁出;kiro/copilot 在 new-api 无对应渠道类型,claude/gemini 在 new-api 是静态 apikey。非 codex 的 oauth 账号来源与 authorize 流程要在各自 provider 包里独立实现,不能假设迁移能提供。

### 2.1 接口的两个已知张力(P0 定 QuotaInfo/Profile 时注意)
- **QuotaInfo 富度(已定:按 codex + kiro 取并集,P0 一次定死)**:各渠道额度语义差异大(kiro enterprise usage API vs codex 5h/7d 双窗口 vs apikey 无)。P0 直接按**最富的两个渠道取并集**定型,不留"P3 再补"的口子——理由是额度会反过来影响调度策略,晚定会连带改快照结构。并集须能表达:
  - **per-model 额度 与 账号级聚合额度**并存(有的渠道只给其一)
  - **多重置窗口**(codex 的 5h + 7d 同时存在,各有独立余量与 reset 时间)
  - **额度缺失是合法状态**(apikey 渠道无额度概念,不能用 0 冒充"已耗尽")
  - 每项额度带 `unit`(token / request / credit)+ `reset_at`,不假设各渠道同单位
- **Profile 静态 vs 每请求动态**:`Profile(acc) UpstreamProfile` 是纯静态的。若某渠道需**每请求**现场生成 challenge/session/PoW token(见总览附录未决问题 F8),静态 Profile 覆盖不了——这类逻辑要么下沉成 gateway 可调用的 per-request minter,要么该渠道不走无状态热路径。P2 打通 codex 前先核实。

## 3. OAuth 三段

### 3.1 import(导入)
模板驱动(继承 relay 的 accountImportTemplate:provider_type / auth_mode / quota_adapter / credential_hint)。三种入口:
- **批量粘贴**:现成 token_bundle(refresh_token + access_token)直接进库
- **CLI 授权**:control 派 job → wrapper 拉起 codex/claude cli → 回填
- **PKCE / device**:wrapper 开临时回调服务器或轮询 device code

导入携带 token_bundle + 设备/客户端指纹元数据(client_version / UA / device_id …),加密落库。

另有特殊入口:**从 new-api channels 表一次性导入**(总览 §6.1)。迁移脚本硬性要求:
- codex(type=57)按 `channels.key` 的 OAuth JSON 无损搬入(refresh/access/id_token/account_id/email/`expired` RFC3339/last_refresh)。
- **必带** `setting.proxy`(codex 刷新流程本身用它,丢了搞坏 refresh)、自供 codex `client_id`(new-api 里是硬编码常量,不在库)、多 key 数组 → N 账号(per-key 禁用态在 `channel_info`)。
- 非 codex 的 oauth 渠道**逐个人工确认** schema,不一把梭。

### 3.2 refresh loop(保鲜,号池心脏)
- 按 `expires_at` 到期前刷新
- **每账号 fencing 串行**(见 §3.4):refresh_token 轮换的上游(codex)并发刷新会互踢,靠账号级 epoch token 保证同一账号不被两个进程同时刷新
- 失败分类沿用 `ErrorClass` 同一套词汇(总览 §6.3.1),不另造第二套:`auth_invalid`(踢池 + 告警,需重新授权)vs 临时错误(退避重试)
- 成功 → 新 access_token 物化进 Redis 快照(短 TTL)

### 3.3 wrapper(OAuth 执行器):混合语言,lease 契约语言无关(评审 F6)
wrapper 通过 **lease job 队列**(claim → 执行 → complete/fail)与 control 主进程解耦。job 契约定成**语言无关**(JSON over Redis/PG),因此执行器可混合语言:

- **Go 侧**(`keyhive wrapper run` 子命令,即 `--role=control` 的 wrapper 子模式):pow、sentinel、pkce、device code、cli 驱动——这些是 browserless 纯算法/流程,Go 移植便宜(pow ~1-2 天、sentinel ~1 天)。
- **保留 Python turnstile 求解器**:Cloudflare Turnstile 是混淆 opcode 虚拟机的手写解释器(异构寄存器 + 一等 callable + JS 强制类型转换),Go 忠实移植 1-2+ 周且**静默失败**(未知 opcode 被吞、不报错只是登录被拒),每次 Cloudflare 轮换要重新逆向。作为独立 Python job worker 挂在同一 lease 契约后面,不阻塞 P2,不拖累主二进制。

wrapper 通用性质:无状态、可多开;control 主进程仍是唯一 DB 写者(配合每账号 fencing);隔离脏活(跑 cli 子进程、开临时 PKCE 回调 http)。

### 3.4 写者模型:每账号 fencing,不是全局 leader(评审 F4)
v2 用**每账号 fencing token** 替代 v1 的全局单 leader:
- refresh_token 轮换只要求**按账号**串行,不要求全局单写者。PG 是账号 fencing 的唯一权威:写者在读凭据/发起刷新之前取得 `fence_epoch`,写回使用 `account_id + fence_epoch` 条件更新,旧 writer 自动失效。Redis 只负责快照 bucket 的 epoch/version 发布,不承担账号写者权威。
- **好处**:消除 v1 §3.6 的"failover 全站暂停刷新"——只有正被某死节点写的那个账号短暂锁到 TTL,其余账号照常刷新。
- **备/主**:1 主 1 备仍存在,但只承担"谁跑周期性全量任务"这类 singleton 语义,用**每周期重竞争的 singleton 锁**,优先 PG 会话级 advisory lock(连接断开自动释放),而非长活 leader。

## 4. 建表(P0 迁移):计量与请求明细分表(评审 F1)

控制状态与高频明细**物理分离**,否则每请求 Release→明细会把 RPM 级写入压到单写者 PG 上:

**控制状态(单写者 PG,随账号数增长)**
- **accounts** —— 账号记录。上游凭据字段对齐 new-api channel(key / base_url / type / 模型映射 / **proxy** / 分组),供一次性导入无损;留 status 变更时间戳(账号健康看板用);带每账号 fencing 的 epoch/generation 列
- **credentials** —— 凭据,加密存储(static secret / oauth token_bundle)
- **import_templates** —— 导入模板

**计量(可靠 + 可聚合,计费必需)**
- **usage_ledger** —— 消费 Release 落计量流水(tokens_in/out + model + account_id + tenant_id + **usage_source**),P4 计费读它;可按窗口聚合
- `event_id`、`attempt_id` 均有唯一约束;control 在 PG 事务提交后才 ACK Redis Stream,重复投递只 ACK 不重复入账。P1 至少保存 `event_id / request_id / attempt_id / tenant_id / account_id / provider / model / status_code / error_class / tokens_in / tokens_out / cache_read_tokens / cache_write_tokens / usage_source / partial / occurred_at`。

**请求尝试状态(用于 gateway 崩溃恢复)**
- **request_attempts** —— `attempt_id` 主键、`request_id`、`started_at`、`deadline_at`、`terminal_event_id`、`state`、`reconciled_at`。消费 `attempt_started` 后落库;超出 `attempt_lease` 且没有终态 Release 时,回收器写入唯一的 `missing/partial` 终态并关闭尝试。

**请求明细(独立、可丢/可抽样存储,不进控制 PG)**
- **request_logs** —— 请求明细,消费 Release 落明细(实时请求日志用);走 ClickHouse 风格独立存储或抽样,**不经过单写者、不与控制状态共库**

**迁移 staging(新系统首次设计,不复用线上旧 migration map)**
- `migration_runs` —— 源库快照时间、批次状态、校验摘要、dry-run/正式导入标记。
- `migration_records` —— `source_system + source_id` 幂等键、原始字段摘要、转换结果、target ID、拒绝原因和 `imported / reconciled / rolled_back` 状态。
- 首批覆盖 `channels + users + tokens + quota/balance + group`; `logs` 历史明细不进入 P1 运行链路。

P0 索引与回收约束固定为:

- `usage_ledger`:主键 `id`,唯一索引 `event_id`,唯一索引 `attempt_id`,查询索引 `(tenant_id, occurred_at DESC)` 和 `(account_id, occurred_at DESC)`。
- `request_attempts`:主键 `attempt_id`,唯一索引 `terminal_event_id`(允许 NULL),查询索引 `(state, deadline_at)` 和 `(request_id)`;`state` 至少包含 `open / terminal / reconciled`。
- `accounts`:唯一索引 `(source_system, source_id)`,查询索引 `(provider, group, status)`;所有凭据轮换写回都带 `WHERE id=$1 AND fence_epoch=$2`。
- 回收器事务以 `SELECT ... FOR UPDATE SKIP LOCKED` 领取 `state='open' AND deadline_at < now()`,再次确认没有终态后写入确定性 `terminal_event_id=sha256(attempt_id+":terminal")` 的 `missing/partial` Release,再把尝试置为 `reconciled`。与 gateway 迟到的真实 Release 竞争时由唯一键收敛,不得重复入账。
- `migration_records` 唯一键为 `(source_system, source_id, record_kind)`,并保留每次转换的 `source_digest`;重跑只产生同一 target ID 的 `reconciled` 记录。

迁移用独立 `.sql` 文件(参考总览 §9 的 relay 迁移风格)。

## 5. control 角色的 P0 交付物

- [x] `pkg/contracts`:Criteria / Lease / UpstreamProfile / AccountLimits(含 **DegradePolicy**,默认 fail_closed) / Release(含 UsageSource/Partial) + 六个类型定稿(结构见总览 §4);`QuotaInfo` 按 codex+kiro 并集一次定死(§2.1)。本地契约与回归已完成，真实 provider/基础设施验收仍以 `docs/EVIDENCE.md` 为准。
- [ ] `ErrorClass` **已定稿,见总览 §6.3.1**(10 个枚举值 + 两层时长)。control 侧要落的是**权威层**:`auth_expired` 由刷新结果驱动而非固定时长、`auth_invalid` 永久踢池告警、`forbidden_capability` 持久排除该号在该模型的候选集、`upstream_5xx` 计平台级熔断而非罚号
- [x] 快照形态定稿:Redis key 分桶 / 版本 + epoch fencing / grace-TTL / 重建节奏(总览 §3.1)
- [x] `internal/control/provider/provider.go`:接口 + registry
- [x] **每账号 fencing** 写者骨架 + 周期任务 singleton 锁(PG advisory 优先);PG 是 `fence_epoch` 唯一权威,Redis 不承担账号写者权威
- [x] 快照写入 + Redis Stream consumer 的空跑通道(无真实 provider),含 pending reclaim(60s)、最多 5 次重试、死信和坏版本处理。本地/miniredis 已覆盖，真实 Redis 验收 pending。
- [ ] P1:PG `usage_ledger` 幂等写入,事务提交后 ACK,并通过重复投递与 control 崩溃恢复测试
- [ ] P1:事件消费指标 `control_stream_pending` / `control_stream_reclaim_total` / `control_stream_dlq_total{reason}` / `usage_ledger_duplicate_total` / `request_attempt_recovered_total{source}`;合成终态和 `UsageSource=missing` 超阈值告警
- [ ] **平台级速率告警**:消费 Release 时按平台聚合 `forbidden_transport` / `blocked` 速率,超阈值告警——这是 R4(TLS 指纹时效)与出口 IP 被标记的**唯一探测机制**,不能只做账号级处理(总览 §6.3.1 规则 2)
- [x] `keyhive wrapper run` 子命令骨架 + **语言无关** claim/complete/fail lease 契约(Go 与 Python worker 都能领)。本地进程与 Python fixture 已覆盖；临时 Redis 7.0.15 ACL/进程验收已有历史通过证据，本轮是否复现及当前边界以 `docs/EVIDENCE.md` 为准。
- [ ] 建表:accounts / credentials / import_templates(控制 PG)+ usage_ledger(计量)+ request_logs(独立存储)+ migration staging——分表落地

## 6. 技术栈

Go 1.23 + Gin + pgx + Redis;独立 `.sql` 迁移文件;`internal/control/modules/*` 分层(service/repository/types/doc);每账号 fencing 由 PG `fence_epoch` 权威控制;周期任务 singleton 锁优先 PG 会话级 advisory lock。turnstile 求解器为独立 Python job worker(挂 lease 契约)。
