# B4.0 计费模型决策记录（2026-09-10）

> **本文是决策记录，不是实现说明。** B4.1–B4.5 的编码以本文为准；本文落地时仓库内**没有任何计费代码**。
> 文中所有 schema 片段是**待实现的形状描述**，不是已存在的表。
> 上游任务：`docs/TODO.md` §B4.0。硬前置：A11（迁移用户转正为 tenants 行）未完成前不进入 B4.1 编码。

---

## 0. 决策摘要

| 编号 | 决策项 | 结论 |
| --- | --- | --- |
| D1 | 计费单位 | **token 计价**，input / output / cache read / cache write **四类分别单价** |
| D2 | 单价存放位置 | **目标库 PG 表**，按 `(provider, model, effective_from)` 组织 |
| D3 | 改价语义 | **只向未来生效，绝不追溯**；历史单价行不可变 |
| D4 | 钱包形态 | **预付余额**（top-up 充值，用量从余额扣减） |
| D5 | 余额不足行为 | **拒绝新请求**，余额状态经 **A6 已有的 `snap:auth:tokens:v1` 快照通道**下发；gateway 热路径**零 PG、零 control 调用** |
| D6 | 找不到单价 | **不按 0 计费**，该 ledger 行进入 `unpriced`，单独可查、单独告警 |

D1、D4、D5 是业务决策，由项目所有者在 2026-09-10 拍板。D2、D3、D6 是本文给出的工程结论。

---

## D1 计费单位：token 计价，四类分别单价

**结论**：计费单位是 token。每个 `(provider, model)` 维护四个独立单价：

- `price_input` —— 对应 `usage_ledger.tokens_in`
- `price_output` —— 对应 `usage_ledger.tokens_out`
- `price_cache_read` —— 对应 `usage_ledger.cache_read_tokens`
- `price_cache_write` —— 对应 `usage_ledger.cache_write_tokens`

这四列在 `migrations/001_initial.sql:92-95` 已经存在于 `usage_ledger`，**计费不需要新的埋点**，只需要一张单价表和一个乘法。

单价以 **每 1,000,000 token** 为标价刻度（与上游厂商公开定价的习惯一致，避免单价写成一长串前导零）。刻度作为列存在表里（`unit_scale`），不硬编码在代码里。

**被否决的选项**

- **按请求计价（每次请求固定价）** —— 否决。同一模型下一次请求的 token 量跨度可达三个数量级，固定价要么劝退长上下文用户，要么把短请求用户的钱补贴给长请求用户。而且我们已经在 ledger 里存了 token 数，按请求计价等于主动丢弃已有的精度。
- **四类合并成一个单价（只按总 token 计）** —— 否决。cache read 的上游成本通常是 input 的一个数量级以下，合并单价会在缓存命中率高的租户上系统性高估、在缓存命中率低的租户上系统性低估，而缓存命中率**不受租户控制**（取决于我们的路由和账号复用），把它折进价格等于向租户收一笔他无法优化的随机费用。
- **抽象 credit 单位（1 credit = 若干 token，按 credit 标价）** —— 否决为首版方案。它多一层换算，对账时多一层可能对不平的地方；new-api 的 `quota` 正是这种抽象，而它带来的"我这 500000 到底是多少钱"的困惑是本次替换要消除的东西之一。**保留可再引入**：若将来需要预售套餐，可在钱包之上加一层充值面额，不影响本文的计费内核。

---

## D2 单价存放位置：PG 表

**结论**：单价放目标库的一张表，形状（B4.1 实现）：

```
model_prices(
    id              TEXT PRIMARY KEY,
    provider        TEXT NOT NULL,
    model           TEXT NOT NULL,
    currency        TEXT NOT NULL,          -- 首版固定 'USD'
    unit_scale      BIGINT NOT NULL,        -- 单价对应的 token 数，首版 1000000
    price_input        NUMERIC(38,12) NOT NULL,
    price_output       NUMERIC(38,12) NOT NULL,
    price_cache_read   NUMERIC(38,12) NOT NULL,
    price_cache_write  NUMERIC(38,12) NOT NULL,
    effective_from  TIMESTAMPTZ NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (provider, model, effective_from)
)
```

单价只在 **control 的慢路径**被读取（B4.2 的扣费作业）。**gateway 永远不读这张表**——它甚至不需要知道价格存在。

金额一律走精确十进制：库内 `NUMERIC`，Go 侧沿用 B3 已建立的 `contracts.Decimal`（`pkg/contracts/contracts.go:139`），**不经 `float64`**。这不是风格偏好：`float64` 无法精确表示 0.01，逐行累加后对账必然出现无法解释到 `event_id` 的尾差，而 B4.5 的 DoD 要求差额可解释到具体 `event_id`。

**被否决的选项**

- **放配置文件 / 环境变量** —— 否决。改价需要重启或重新发布；没有历史版本，出账后无法回放"当时用的是哪个价"；多实例之间可能短暂持有不同价格。
- **随 provider adapter 走（价格写在各 provider 包里）** —— 否决。那里存的是**我们的成本口径**，而单价是**售价**。把两者放同一处，第一次做差异化定价（不同租户不同价、促销价）时就要拆开。售价属于业务数据，不属于协议适配代码。
- **放 Redis** —— 否决。Redis 在本系统里是快照数据面，是**可重建的派生状态**；单价是权威账务数据，丢失后无法从别处重建，必须待在 PG。

---

## D3 改价语义：只向未来生效，绝不追溯

**结论**：

1. 改价的唯一方式是**插入一条新的 `effective_from` 行**。已有行不允许 `UPDATE`、不允许 `DELETE`。
2. 新行的 `effective_from` **必须晚于当前时间**（B4.1 在应用层校验并给出明确错误；DB 层可加 CHECK 兜底）。不允许插入一条生效时间在过去的价格来"补价"。
3. 一条 `usage_ledger` 行的适用单价，由它的 **`occurred_at`** 决定：取该 `(provider, model)` 下 `effective_from <= occurred_at` 的最新一行。**不是按扣费作业的运行时间取价**——否则同一批请求会因为作业跑得早或晚而算出不同的钱。
4. 已经扣过费的 ledger 行不因后续改价而重算。修正只能通过 `wallet_transactions` 里一条显式的 `adjustment` 记录完成，留下痕迹。

**被否决的选项**

- **改价追溯（新价格适用于历史用量）** —— 否决。已经告诉租户的账单会在他背后变化；而且一旦允许追溯，B4.5 的对账就不可能成立——同一时间窗重跑对账会得出不同的应收总额，"差额"这个概念本身失去意义。
- **单价表可原地 `UPDATE`** —— 否决。等价于追溯，且连"当时是什么价"的证据都不留。
- **按扣费作业运行时刻取价** —— 否决。它把"钱算多少"和"作业什么时候跑"耦合在一起：一次作业延迟、一次重跑、一次事故恢复都会改变金额。计费必须是 ledger 行的**纯函数**。

---

## D4 钱包形态：预付余额

**结论**：租户先充值，用量从余额扣减。形状（B4.1 实现）：

```
tenant_wallets(
    tenant_id   TEXT PRIMARY KEY REFERENCES tenants(id) ON DELETE CASCADE,
    currency    TEXT NOT NULL,
    balance     NUMERIC(38,12) NOT NULL DEFAULT 0,
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
)

wallet_transactions(
    id            TEXT PRIMARY KEY,
    tenant_id     TEXT NOT NULL,
    kind          TEXT NOT NULL,          -- topup | debit | adjustment
    amount        NUMERIC(38,12) NOT NULL, -- 有符号
    balance_after NUMERIC(38,12) NOT NULL,
    billing_run_id TEXT,                   -- debit 指向产生它的扣费作业
    note          TEXT NOT NULL DEFAULT '',
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
)
```

`tenant_wallets.balance` 是当前余额；`wallet_transactions` 是不可变流水。两者必须在同一事务内变动，B4.5 的对账正是拿"流水求和"和"余额"对齐。

选预付还有一个迁移上的理由：new-api 的 `users.quota` / `tokens.remain_quota` 本身就是预付余量语义，A11 迁移过来时是同构映射，不需要在迁移里发明一套语义转换。

> **未核实**：new-api 的 quota 整数单位与货币金额之间的换算比例（其默认配置中的 `QuotaPerUnit`）**本轮未做任何核对**。A11 的 DoD 已明确"quota / balance 仍只保留 source unit 与原始值，本任务不做任何换算"。换算比例是 B4.1 的输入，届时必须**读实际 new-api 部署的配置**确认，不得按默认值假定。

**被否决的选项**

- **后付账单（先用后结）** —— 否决。它需要一整套本项目现在没有的东西：信用额度评估、账期、催收、坏账处理。而且没有余额这个天然闸门，一个被盗用的 token 可以在一个账期内产生无上限的真实上游成本。
- **预付 + 后付混合** —— 否决为首版。它是两套结算逻辑并存，在只有一种租户形态的当下没有收益。预付模型不阻碍将来给特定租户开后付通道。

---

## D5 余额不足：拒绝新请求，经快照通道下发

这是 B4.0 里唯一会反过来改架构的问题，`docs/TODO.md` §B4.0 因此要求**显式回答**。

**结论**：余额耗尽时**拒绝该租户的新请求**，且判断所需的状态**经 A6 已有的 tenant token 快照通道下发**。

### 数据流

1. control 的 token 快照 tick（`internal/control/run.go:105`，`tokenRefreshInterval = 15s`，`internal/control/tenants.go:135`）在构建 `TokenRecord` 时，顺带按 tenant 读一次钱包余额。
2. `snapshot.TokenRecord`（`internal/snapshot/auth.go:24`）**增加一个字段**：

   ```go
   // BillingBlocked is true when control last saw this tenant's wallet at or
   // below zero. Gateway refuses new requests for the tenant without reading
   // any balance itself.
   BillingBlocked bool `json:"billing_blocked,omitempty"`
   ```

   同一 tenant 的多个 token 携带相同的值。值由 control 这个单写者一次性算出，不存在 token 之间不一致的可能。
3. gateway 在鉴权时**已经**拿到了 `TokenRecord`（它就是 `AuthContext` 的来源，`internal/gateway/auth.go:19`）。`BillingBlocked=true` 时直接拒绝，**不产生任何额外的读**。
4. 被拒请求返回 **HTTP 402 Payment Required**，错误文案标明 tenant（与 A10 的 429 限流文案同风格）。它**不打上游**，因此**不产生 Release 事件、不写 `usage_ledger`**，只计一个指标 `gateway_billing_rejected_total{tenant}`。
5. 已经通过准入、正在进行中的请求**不受影响**，跑完为止。半途掐断一个流式响应既不会省下已经付出的上游成本，又会把一次已经发生的用量变成 `Partial`（污染 B4.3 的可信度口径）。

### 透支上界（必须写明，不得只说"一个快照周期"）

这个设计接受有界透支。上界不是单一的 15s，而是：

```
最大透支 ≈ tenant 峰值消耗速率 × (快照周期 + 扣费作业周期 + 在途请求耗时)
```

三项都在我们手里：快照周期当前 15s；扣费作业周期由 B4.2 决定；在途请求耗时由请求本身决定。

关键在于**峰值消耗速率必须有上界，否则透支无界**。系统里唯一能给它封顶的是 **A10 已经实现的 per-tenant 限流**（`tenants.rpm` / `tenants.max_concurrency`，`migrations/004_tenant_limits.sql`，0 = 不限）。因此：

> **预付租户的 `rpm` 不得为 0。** 这条约束写进 B4.4 的 DoD，并在 control 侧对"余额型租户但 rpm=0"给出告警。

这是一个**取舍**，不是一个被忽略的漏洞：用一个可计算、可封顶的透支窗口，换掉热路径上的一次同步余额查询。

### 被否决的选项

- **热路径同步查余额（每请求查 PG 或调 control）** —— 否决，且这是本文最重要的一条否决。总览 §1 的立论是"gateway 热路径不调用 control"，总览 §5 把 `UpstreamProfile` 定为两角色**唯一**的深耦合点。每请求同步查余额会：(a) 在热路径上引入一个 PG 往返，把 gateway 的可用性绑到 control 状态库的可用性上；(b) 让 control 的负载随 **RPM** 增长，而不是随账号数增长——这正是 A12 里同一条立论要保护的东西；(c) 新增一个契约级耦合点。用一个 15s 的透支窗口换掉这三样，是划算的。
- **允许透支到一个显式负数阈值** —— 未采纳为独立机制，因为它已经被上面的设计**吸收**了：快照延迟本身就是一个有界透支窗口。再叠一层显式阈值只是把同一个宽容度写两遍，两个数还可能互相矛盾。
- **只告警不拒绝** —— 否决。上游成本是真实发生的，一个不会停的余额没有任何约束力，等于把预付模型退化成无追索的后付。
- **gateway 在 Redis 里本地实时累减余额** —— 否决为首版方案。它能把透支窗口压到接近零，但代价是 gateway 成为余额的**第二个写者**，与"control 是控制状态唯一单写者"的原则冲突：Redis 上的实时余额和 PG 里的权威余额之间需要一套收敛协议，任何一次 Redis 丢数据都变成账务事故。**保留为后续可选增强**，若将来出现单租户高速烧钱的实际案例再单独立项，届时应做成"预扣额度令牌"而不是"余额镜像"。
- **新开一个 `snap:billing:tenants:v1` 快照键** —— 否决。gateway 要多读一个键，且两个快照之间存在时序偏斜（token 说这个租户存在、billing 快照还没有它的行，反之亦然），需要额外的一致性处理。复用 `TokenRecord` 没有这个问题：一次读，一个原子 RENAME，一个时间点。

---

## D6 找不到单价：不按 0 计费

**结论**：扣费作业为某条 ledger 行解析单价时，若 `(provider, model)` 没有任何 `effective_from <= occurred_at` 的行，**不得按 0 计费、不得回落到某个默认价、不得跳过后当作已计费**。该行标记为 `unpriced` 并保持未计费，同时：

- `unpriced` 的行数与 token 量**单独可查询**；
- 出现 `unpriced` 行即告警（属 E1 的规则文件范围）;
- 补上单价后（用一条 `effective_from` 覆盖该时间点的新价格是不允许的，见 D3 —— 因此这里的补救是**人工确认后的 `adjustment`**，或在开新模型时**先配价再放量**）。

这条与 AGENTS.md §2 的反幻觉基线同源：**未核实的额度一律返回 missing，不按零补齐**。计费侧的对应物就是"没有价就不出价"。上新模型时忘了配价，正确的结果是被告警发现，而不是静默地免费服务一个月。

**被否决的选项**

- **默认单价兜底** —— 否决。它把配置遗漏变成一个静默的收入损失或超收，且事后无法区分"这个模型本来就是这个价"和"这个模型没配价"。

---

## 1. 不在本文范围内的东西

以下都是真实的问题，但**不属于 B4.0**，不在这里下结论：

- **`UsageSource` 不可信时怎么计费** —— `upstream` / `estimated` / `missing` × `Partial` 四种组合各自的计费行为，是 **B4.3** 的产物。本文只固定一条前置口径：**这四种组合必须各自有显式结论，不得由代码隐式默认**（例如"零值就当没用量"这种隐式行为是被禁止的）。
- **充值入口**（谁能充、怎么充、支付渠道）—— 属控制台/运营面，B5 范围。B4 只需要 `wallet_transactions` 能记下一条 `topup`。
- **发票、税、多币种** —— 首版固定单一货币（`USD`），`currency` 列先存在但不做换算。
- **不同租户不同价 / 折扣** —— schema 预留了扩展空间（在 `model_prices` 上加 tenant 维度即可），首版不实现。

---

## 2. 据此细化的 B4.1–B4.5 DoD 增补

本文不改写 `docs/TODO.md` 里 B4.1–B4.5 的原有 DoD，只**追加**由本文推导出的条目：

- **B4.1** 追加：`model_prices` 的 `effective_from` 必须晚于插入时刻（应用层校验 + 明确错误）；已有单价行不可 UPDATE / DELETE；`tenant_wallets` 与 `wallet_transactions` 同事务变动；金额列一律 `NUMERIC`，Go 侧一律 `contracts.Decimal`。
- **B4.2** 追加：取价按 ledger 行的 **`occurred_at`**，不是作业运行时刻；找不到单价的行进入 `unpriced` 且**不**标记为已计费；`usage_ledger` 增加 `billing_state` / `billed_amount` / `billing_run_id`，幂等性由已有的 `usage_ledger.event_id UNIQUE`（`migrations/001_initial.sql:83`）承载。
- **B4.3** 追加：`unpriced` 与 `missing` 是**两个不同的桶**，不得合并统计——前者是我们的配置问题，后者是上游的数据问题。
- **B4.4** 追加：`snapshot.TokenRecord` 增加 `BillingBlocked`；gateway 拒绝返回 **402**、不打上游、不产生 Release 事件；余额型租户 `rpm=0` 需在 control 侧告警；回归测试必须覆盖"余额转负后，最多一个快照周期内 gateway 开始拒绝"和"gateway 全程未查 PG / 未调 control"。
- **B4.5** 追加：对账三方为 `usage_ledger`（已计费行的 `billed_amount` 求和）、`wallet_transactions`（同窗口 `debit` 求和）、`tenant_wallets.balance`（期初期末差）；`unpriced` / `held` 的行不计入应收但必须单独列出，否则对账会"看起来平了"而实际漏收。

---

## 3. 本轮未验证的事项

按 AGENTS.md §2，明确区分本轮结论与未验证事项：

- 本文**未写任何代码、未建任何表、未跑任何测试**。文中所有 schema 是待实现形状。
- 引用到的现有代码位置（`usage_ledger` 四列、`snap:auth:tokens:v1`、`TokenRecord`、15s tick、`contracts.Decimal`、A10 的 `tenants.rpm`）均在本轮**实际读取源码核对**，不是凭记忆。
- **new-api quota 单位与货币金额的换算比例未核对**，见 D4 中的说明。这是 B4.1 的硬输入。
- 四类 token 的**具体单价数值本文不给**——那是业务定价，不是工程决策；B4.1 只负责让它可配、可追溯、不可篡改历史。
- 透支上界公式是**结构性推导**，未经真实流量校准。E1 的告警阈值同理，按其 DoD 必须标注为"未经真实流量校准"。
