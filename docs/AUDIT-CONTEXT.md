# 审核上下文(AUDIT-CONTEXT,v2 已按评审重构)

> 给外部评审者的补充材料。三份主文档(`fluxgate-keyhive-overview.md`(同目录) / `../keyhive/docs/architecture.md` / `../fluxgate/docs/architecture.md`)是**结论式**的;本文补上评审所需的**取舍理由、待定边界、参考现状、已知风险**。
>
> **项目状态:已有 P2/P3 本地实现切片，但尚未完成全量 admission。** 真实 provider 凭据测试固定在项目结束验收阶段执行，不作为当前 P3 本地开发的阻塞项。本文仍是设计与审核上下文，不把本地 fixture/回归测试或未执行的真实基础设施、provider 验收写成生产完成。
> **v2 说明:** 三份主文档已按上一轮评审(见 §6 评审结论汇总)重写。本文同步更新了决策、已解决/剩余风险。

## 0. 一分钟背景

要新建**一个 repo、一个二进制、两个运行角色**替换现网系统:
- **`--role=gateway`(fluxgate)** = API 网关(热路径,无状态,3 节点)
- **`--role=control`(keyhive)** = 号池/控制面(慢路径,有状态,1 主 1 备,每账号 fencing)

热/慢是**逻辑角色**分离,不是两个 repo(v1 曾是双 repo,评审后合并为单 repo 双角色以消除跨 repo 契约 skew)。

现网线上跑 **new-api**(开源 LLM 网关),几十个上游账号(channel)、真实用户和余额。新系统接管它。核心业务:把一批**上游 AI 账号**(apikey + oauth 登录态如 codex/claude/gemini/kiro)组织成池,统一对外提供 API,并**冒充官方客户端**(TLS 指纹 + header 伪装)打上游。

## 1. 关键决策 + 备选 + 为什么(v2)

### 1.1 热/慢拆成两个**角色**(v2:单 repo 双角色,非双 repo)
- **选的**:gateway 处理所有推理流量,control 只管账号且不碰流量;两者仅通过 Redis 快照(control→gateway)+ **Redis Streams 至少一次投递**(gateway→control)异步相连,请求路径上无同步 RPC。**同一 module/二进制,`--role` 切换,契约是进程内 Go 包。**
- **备选**:① 单体(网关内嵌账号管理);② 拆进程但同步查号池;③(v1 原案)拆成两个独立 repo。
- **为什么单 repo 双角色而非双 repo**:两角色唯一深耦合是 `pkg/contracts`,放进程内则版本 skew 不可能发生,无需设计跨 repo 兼容协议(v1 风险 §3.5 由此消解)。独立扩展靠"同二进制不同 role 进程"照样成立(sub2api/new-api 都是单 repo 多角色)。
- **代价/可质疑点**:快照有**新鲜度延迟**。v2 的补偿已明确(见 §1.7):gateway 请求内 failover + 本地负缓存,control 快照做持久兜底;Release 事件采用至少一次投递并以 PG ledger 幂等收敛。

### 1.2 control 单主 + 每账号 fencing(v2:替代全局单 leader)
- **选的**:control 1 主 1 备,写者模型用**每账号 `fence_epoch` fencing token**串行刷新,PG 是唯一权威;Redis 只发布快照 bucket 的版本/epoch,而非全局单 leader。周期任务用每周期重竞争的 singleton 锁(PG advisory 优先)。
- **为什么**:refresh_token 轮换(codex 互踢)只要求**按账号**串行,不要求全局单写者。每账号 fencing 消除了 v1 "leader 切换窗口全站暂停刷新"的风险(v1 §3.6)——只有正被死节点写的那个账号短暂锁到 TTL。
- **可质疑点**:fencing 实现的正确性(epoch 必须在读凭据前由 PG 夺取,写回带 `account_id + fence_epoch` 条件);Redis 发布脚本只负责快照版本指针的原子切换。参 sub2api `scheduler_cache.go` 范式,但不把 Redis 当账号写者权威。

### 1.3 迁移:一次性脚本从 new-api 导入(策略 C),无损性**只对 codex 成立**(v2 修正)
- **选的**:写一次性脚本,读 new-api `channels` 表灌进 control。
- **v2 核实结论**:codex(type=57)OAuth 可无损迁出(凭据是 `channels.key` 里自洽 JSON,且是权威库);但**此 new-api 无 kiro/copilot 渠道类型**,claude(14)/gemini(24)是**静态 apikey**。"首批 provider 全集"不能假设都从 new-api 迁出——非 codex 的 oauth 账号来源要各 provider 独立实现。
- **三个易丢字段**:`setting.proxy`(codex 刷新流程本身用,丢了搞坏 refresh)、codex `client_id`(new-api 硬编码常量,不在库,新系统自供)、多 key 数组→N 账号。
- **可质疑点**:字段映射须按 provider 分别定,不一把梭。

**新系统首次迁移模型(已定)**:从 new-api 的 `channels + users + tokens + quota/balance + group` 建立新系统记录,`logs` 历史明细不进入 P1 运行链路。一个 new-api user 映射为一个 tenant 和默认 principal;所有迁移记录保留 `source_system + source_id` 幂等键。使用新系统的 `migration_runs` / `migration_records` staging 追踪原始摘要、转换结果、target ID、拒绝原因和回滚状态;线上遗留 `elucid_*_migration_map` 仅作事实参考,不作为新系统迁移模型。

### 1.4 计费:按 token,计量/计费分离 + usage 完整性策略(v2 强化)
- **选的**:P0 只采集用量(Release 带 tokens + model + account + tenant + **UsageSource + Partial**),计费逻辑推迟 P4。
- **v2 新增**:usage 是**契约级**决策——强注 `include_usage`、每上游 usage-integrity 策略(failover/计0/估算)、断连继续 drain。`UsageSource` 溯源供 P4 判可信度。
- **可质疑点**:部分上游流式不吐 usage(如 Grok),已在契约层给出策略,而非留作实现细节。

**P1 可靠性边界(已定)**:一次尝试先写 `attempt_started`,再调用上游,终态写入唯一 `Release`。control 消费者在 PG 事务提交后 ACK;`usage_ledger.event_id` 和 `attempt_id` 均唯一。gateway 在上游完成但终态事件写入前崩溃时,回收器在 `attempt_lease` 超时后只生成唯一的 `missing/partial` 合成终态,不猜测 token;这保证可恢复与幂等,但不承诺恢复崩溃窗口内未观测到的真实 usage。

### 1.5 wrapper(OAuth 执行器):混合语言(v2:非全 Go)
- **选的**:lease job 契约定成语言无关;pow/sentinel/pkce/device/cli 用 Go,**turnstile 保留 Python 求解器**挂同一契约后面。
- **为什么**:核实后 turnstile 是 Cloudflare 混淆 opcode VM 的手写解释器,Go 移植 1-2+ 周且静默失败、每次轮换重逆向;保留 Python 化解风险、不阻塞 P2。pow/sentinel Go 移植便宜(各 1-2 天)。
- **可质疑点**:双语言 worker 的运维成本 vs 全 Go 的移植/维护成本权衡。

### 1.6 Provider 隔离:一渠道一包 + init 自注册(不变)
- 每个上游渠道一个 Go 包,统一 Provider 接口,魔数锁在各自 const.go。apikey 是退化 provider。
- **v2 补充的两个接口张力**:QuotaInfo 富度(各渠道额度语义差异大)、Profile 静态 vs 每请求动态(见 §3 剩余风险 R1)。

### 1.7 失效感知归属(v2 新增决策,评审 F2)
- **选的**:失效账号处理分两层——gateway **运行时**(请求内 failover 换号重试 + 本地负缓存,立即生效)+ control **持久**(消费 Release 更新状态,产新快照兜底)。
- **为什么**:运行时限流挡不住"已失效账号被继续选中";把剔除只放慢路径会让快照延迟变成用户可见报错。ErrorClass 分类(401 OAuth 冷却 / 403 HTML 不罚 / 429 无头秒级兜底)抄 sub2api 实战。

## 2. 已定 vs 待 P0 具体化

**已定**:单 repo 双角色、角色边界、Redis 快照 + Streams 至少一次投递、PG `fence_epoch` 唯一权威、provider 隔离、OAuth 三段、混合语言 wrapper、迁移策略 C(codex 无损)、首次迁移 staging 模型、P1 Release/ledger 幂等与崩溃恢复边界、计量/计费分离 + usage 溯源、失效两层归属、控制台封存 P6、阶段计划、技术栈(Go1.23+Gin+pgx+Redis)。生产环境只允许只读 schema/data/dry-run 核验,不写入、不部署、不切流。

**已定(本轮补,原为待定)**:
- **限流降级按渠道分别配**:`AccountLimits.DegradePolicy`(`fail_closed` | `fail_open`)随快照下发,**默认 `fail_closed`**。风控严的渠道(codex/claude)保号池优先,自建/宽松 apikey 渠道保可用性优先。不做全局开关。
- **`QuotaInfo` 按 codex + kiro 取并集,P0 一次定死**:须表达 per-model 与账号级聚合并存、多重置窗口(codex 5h+7d)、额度缺失为合法状态、每项带 unit + reset_at。不留"P3 再补"口子——额度会反过来影响调度策略,晚定连带改快照结构。
- **`ErrorClass` 枚举表已定稿(10 个值,权威定义在总览 §6.3.1)**。核心是**分层定时长**:gateway 本地冷却只桥接快照延迟(秒级~60s,凭据版本变更即清除),control 才是权威判罚(按真实恢复语义,不猜惩罚时长)。修正了 v1 抄来的"401 冷却 10min"——那是把两层混为一谈。三条硬规则:429 无 reset 头绝不猜长(5s→15s→60s 上限 5min)、403 HTML 与 blocked 不罚号但**其速率是 R4 的探测信号**、小池必配 50% 防饿死闸。
- **P1 可靠性验收**:Release 至少一次投递、`usage_ledger` 按 event/attempt 幂等、gateway 崩溃后由 attempt lease 合成终态恢复;验收故障注入与断言见总览 §7.1。
- **P0 具体契约**:快照键/版本 CAS/60s grace-TTL、`Credential`/`TokenBundle`/`AccountLimits`/`Account`/`ImportRequest`/`QuotaInfo` 字段、Streams pending/DLQ/指标、usage-integrity 首批策略、首批 protokit 方向和首次迁移语义映射均已写入总览与角色文档。

**实现阶段仍需验证(不再是架构待定)**:
- 生产只读 dry-run 中 `information_schema` 的真实列名别名和 provider 自定义 `key` 格式;不匹配时进入 rejected,不得猜测映射。
- Redis HA 部署拓扑与演练参数(部署侧);P1 依赖单 Redis 连接契约不变。
- P2 Codex CLI 端点是否每请求需要 PoW/Turnstile 仍需在对应协议依据明确后确认；各 OAuth provider 的真实 usage fixture 统一放到项目结束验收阶段，不作为当前 P3 本地开发阻塞项。

## 3. 剩余风险 / 未决(v2 精简后最该被审)

1. **R1 每请求反自动化可能击穿热/慢拆分(最尖锐,评审 F8)**:`UpstreamProfile` 静态,但 chatgpt2api 的 PoW/Turnstile 在 `chat-requirements`(每消息)。若 codex provider 走 web-backend 路径,per-request challenge 逻辑属 control 却要在 gateway 每请求执行。**P2 前必须核实 codex CLI API 端点是否每请求需 PoW。**
2. **R2 快照新鲜度**:延迟窗口仍在,但 v1.7 的两层补偿(运行时 failover + 本地负缓存 + 持久兜底)已覆盖"已失效账号被选中"。可质疑补偿的重试上限与冷却时长取值。
3. **R3 usage 完整性**:每上游策略已进契约,但需逐上游核实哪些流式不吐 usage、断连 drain 的实现完整性。
4. **R4 TLS 指纹时效**:utls JA3 profile 是对官方客户端某版本的快照,上游升级客户端后可能失配被封。**本轮补上了检测**——`forbidden_transport`/`blocked` 的**平台级速率告警**(总览 §6.3.1 规则 2)能在多个账号同时异常时把它和账号级故障区分开。**剩余缺口是"更新"而非"发现"**:指纹跟随上游客户端版本刷新仍是人工流程,无自动化。
5. **R5 turnstile 静默失败**:保留 Python 求解器降低了移植风险,但 Cloudflare 轮换时仍会静默失败(登录被拒不报错),需登录成功率监控告警。
6. **R6 Redis SPOF**:快照传输 + 运行时限流 + 粘滞映射都压 Redis。降级策略已定(按渠道 `DegradePolicy`,默认 fail_closed),**剩余未决只有 Redis HA 方案本身**(部署侧)。另:新节点冷启动拉不到快照就无法加入,这条无降级路径,依赖 Redis 可用性。
7. **R7 Release 崩溃窗口**:gateway 可能在上游已完成、终态 `Release` 尚未 `XADD` 时崩溃。`attempt_started` + `attempt_lease` 回收器可保证尝试最终收敛到唯一 `missing/partial` ledger,但无法凭空恢复未观测的真实 token。P1 成功标准必须接受“至少一次投递、ledger 幂等、gateway 崩溃后可恢复”，同时把合成终态数量和 token 缺失率纳入告警;若业务要求精确恢复该窗口,需额外的上游幂等键/查询能力,不在当前 P1 范围。

## 4. 参考项目现状(评审者读不到这些仓库,以下是摘要)

已有的、可搬用代码的来源,不是新系统的一部分:

- **elucid-relay**(Node+Go):成熟 oauth-wrapper(租约式 job 队列)、账号导入模板、codex utls 反代(`codex_tls.go`)。keyhive OAuth 与 gateway 反代主蓝本。线上不用,只借代码。
- **new-api**(Go):**现网线上系统**,迁移数据源。codex oauth 在 `relay/channel/codex/oauth_key.go`(凭据打包进 `channels.key`);无 kiro/copilot 渠道类型;claude/gemini 静态 apikey。40 个渠道适配器目录可参考 provider 分包。
- **sub2api**(Go):**每账号 fencing / 版本化快照 + grace-TTL / 事务性 outbox + watermark / 401-403-429 分类冷却 / SSE usage(强注 include_usage、断连 drain、Grok 无 usage 转 failover)** 的实现,是 control 调度与 gateway usage 的主参考。
- **Kiro-account-manager**(Python+Electron):kiro enterprise header/refresh/usage API 实测,做 kiro provider 参考。
- **chatgpt2api**(Python):codex 逆向,PKCE + PoW/turnstile/sentinel 反自动化求解器(turnstile 拟直接保留其 Python 实现)。做 codex provider authorize 关键参考。
- **未采用**:ElucidRouter(与 sub2api 同源重叠)、TokenRouter(仅 deploy 无代码)。

## 5. 请评审者重点回答

1. §1 决策(尤其 PG 唯一 fencing、首次迁移模型、1.7 失效两层归属)哪个取舍仍站不住?
2. §3 剩余风险,R1(每请求反自动化)和 R7(Release 崩溃窗口的合成终态)之外还有被低估的吗?
3. §2 实现阶段验证项里,哪些生产 schema 差异、Redis HA 演练或 provider fixture 会影响 P0 验收?
4. P1 成功标准中的至少一次投递、ledger 幂等、gateway 崩溃可恢复是否已足够可验收?是否有必要为崩溃窗口引入上游幂等键/查询(这会扩大 P1 范围)?
5. 单 repo 双角色 + 每账号 fencing 后,这个"号池 + 冒充官方客户端反代"架构是否已到合理的最简形态?还有可去掉的复杂度吗?

## 6. 上一轮评审结论汇总(v1→v2 变更依据)

本次重构落实了以下评审发现:
- **F1** 计量与请求明细分表(修正"control 负载不随 RPM"的自相矛盾)→ overview §6.4、keyhive §4
- **F2** gateway 请求内 failover + 本地负缓存(失效两层归属)→ overview §6.3、fluxgate §2.1
- **F3** Release 补 UsageSource/Partial,usage 完整性升为契约级 → overview §6.2、fluxgate §4
- **F4** 全局单 leader → 每账号 fencing(消除 failover 暂停)→ keyhive §3.4
- **F5** 迁移无损性只对 codex 成立,provider 全集不能假设全从 new-api 迁出 → overview §6.1、keyhive §3.1
- **F6** wrapper 混合语言,保留 Python turnstile → keyhive §3.3
- **F7** Redis SPOF 降级策略 → fluxgate §6
- **F8** 每请求反自动化未决(P2 前核实 codex)→ overview 附、fluxgate §7
- **F9** 双 repo → 单 repo 双角色(消除契约 skew)→ overview §1、§8
