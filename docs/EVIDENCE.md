# 执行证据记录（EVIDENCE）

> 本文只记录**已执行的命令、结果、验收证据和免责边界**。
> **待办、计划、下一步一律写在 `docs/TODO.md`，不写在本文。**
> 本文原名 `docs/P3-STATUS.md`，2026-09-03 拆分为证据（本文）与进度（`docs/TODO.md`）两份。
>
> 阅读规则：本文的每一条都是**对已发生事实的描述**。不要从本文推断"项目是否可以继续开发"——
> 那个问题由 `docs/TODO.md` 和 `AGENTS.md` §0 回答。

## E1 告警规则落点（2026-09-11）

本轮只做一件事：给自 A9 起一路埋下、但从来没有消费方的指标接上告警规则。
只交付规则与阈值；通知通道按 DoD 划为部署边界，不在本轮。

### 改动
- 新增 `configs/alerts/gateway-platform.rules.yml`：16 条告警、6 个分组
  （egress / stream / usage-trust / billing / detail）。
- 新增 `configs/alerts/README.md`：加载方式与两条可执行校验命令。
- 新增 `internal/observability/alertrules_test.go`：规则文件的语义校验。
- `go.mod`：`gopkg.in/yaml.v3` 由 indirect 提升为 direct（**未引入新模块**，它本就在依赖图里）。
- **未改动任何埋点代码**。

### 覆盖
- DoD 五项：`control_platform_transport_rejections_accounts` / `control_stream_dlq_total` /
  `control_stream_pending` / `release_usage_missing_total` 占比 / `control_platform_starvation_release_total`。
- 另接上此前无消费方的：B4.2 `control_billing_unknown_usage_class_total`、
  B4.3 `control_billing_held_rows`、B4.4 `control_billing_uncapped_wallet_tenants` 与
  `control_billing_blocked_tenants`、A12 `detail_flush_failures_total` 与 `detail_dropped_total`、
  以及 §4.1 的 `usage_ledger_duplicate_total`、`request_attempt_open_age_seconds`、
  `request_attempt_recovered_total{source="synthetic"}`。

### 本轮执行（命令与结果）
- `docker run --rm -v "$PWD/configs/alerts:/rules:ro" --entrypoint promtool prom/prometheus:v3.7.3 check rules /rules/gateway-platform.rules.yml`
  → `SUCCESS: 16 rules found`。PromQL 语法由真实 promtool 校验，非自述。
- `go test ./internal/observability/ -run 'AlertRules|EveryRule|RequiredCoverage'` → 3/3 PASS。
- **对该校验做了变异验证**：把 `control_stream_pending` 改成 `control_stream_pending_typo` 后，
  `TestAlertRulesOnlyReferenceEmittedMetrics` 如期失败并指名 `StreamPendingBacklog`；
  还原后重新通过。即这条守卫确实会红，不是恒真断言。
- `go build ./...`、`go vet ./...` → 通过。
- `gofmt -l .` → 仅 `internal/control/health.go`、`cmd/new-api-migrate/main_test.go` 命中，
  两者均为**既有**问题，本轮未改动这两个文件。
- 无凭据基线 `env -u ... go test -count=1 ./...` → 全绿。

### 明确未由本轮证实
- **所有阈值未经真实流量校准**，且**未做任何告警演练**——本轮没有触发过其中任何一条规则，
  只验证了它们语法正确、指标名真实存在。"规则会在该响的时候响"没有被证明。
- 未验证 Prometheus 实际抓取本平台的 `/metrics`（gateway 与 control 是两个独立注册表，
  需分别配 scrape target；本轮没有起 Prometheus 做端到端）。
- 无 CI，两条校验命令均为手动执行。

### 最高剩余风险
阈值全是推的。其中 `StreamPendingBacklog > 1000`、`AttemptOpenAgeHigh > 900s` 这类量级阈值
若与真实规模差一个数量级，表现是**静默失效**：规则一直不触发，看起来一切正常。
`threshold_source` 注解把这一点逐条写明了，但注解不会替人回调数字——上线后按实际分布重算是必须动作，
不是可选优化。

---

## A12 请求明细存储（2026-09-11）

本轮只做一件事：把高频请求明细从控制状态 PG 的单写者路径上挪走，落一条**异步、有界、可丢**的
ClickHouse 写入路径，并先写决策记录（`docs/A12-REQUEST-DETAIL.md`）。

### 改动
- 新增 `internal/detail`：`schema.go`（`Record` / `FromRelease` / `DDL`）、`sink.go`
  （`Writer` 接口 / `Sink` / `ClickHouseWriter`）。走 ClickHouse HTTP 接口 + `JSONEachRow`，
  **未新增任何 Go 依赖**。
- `internal/control/ledger.go`：`Ledger` 加 `Detail *detail.Sink`，在 `HandleRelease` 的
  `tx.Commit()` **之后**、函数最后一行调用 `l.Detail.Observe(release)`。`Observe` 无 error 返回。
- `internal/control/run.go`：`GATEWAY_DETAIL_CLICKHOUSE_URL` 非空时建 writer + sink，启动时
  `EnsureSchema`，失败只 log 并降级为无明细。
- `pkg/contracts`：`Release` 新增可选 `InboundProtocol`（未进 `Validate()`）。
- `internal/gateway/server.go`：把 `inboundProtocol` 透传进两条 release 构造路径（8 处调用点）。
- `configs/test-infra/docker-compose.yml` / `test.env`：新增 `gateway-test-clickhouse`
  （`clickhouse/clickhouse-server:24.8-alpine`，127.0.0.1:18123）与 `GATEWAY_TEST_CLICKHOUSE_URL`。
- **无新增 PG migration**。

### 本轮执行（命令与结果）
- `gofmt -l .` → 仅 `internal/control/health.go`、`cmd/new-api-migrate/main_test.go` 命中，
  两者均为**既有**问题，本轮未改动这两个文件，按 Change Boundary 不动。本轮新增/修改文件全部干净。
- `go build ./...`、`go vet ./...` → 通过。
- `go test -count=1 -race ./internal/detail/` → `ok 1.032s`。
- 无凭据基线：`env -u GATEWAY_TEST_DATABASE_URL -u GATEWAY_TEST_REDIS_ADDR -u GATEWAY_TEST_CLICKHOUSE_URL go test -count=1 ./...`
  → 全绿（明细套件按设计 skip）。
- 真实 ClickHouse（容器 `24.8.14.39`，healthy）下的门控套件全部 PASS：
  `TestClickHouseWriterRoundTripsARecord`（17 列 TSV 逐字段回读比对 + DDL 幂等）、
  `TestRedeliveredReleaseDoesNotDuplicate`（`FINAL` 下 count=1、`max(ingested_at)` 取后写）、
  `TestSinkDeliversToClickHouse`（10 条经异步 sink 落库）、
  `TestRealClickHouseErrorsStayInsideTheSink`（真实 HTTP 错误只记 `detail_flush_failures_total`）。
- 真实 PG 下 `TestDetailFailureDoesNotAffectMeteringOrAck` PASS：明细存储完全不可用时，
  `HandleRelease` 仍返回 nil（消费者照常 ACK）、`usage_ledger` 行照常写入（tokens 7/9），
  并断言 sink 确实发起过写入（否则该测试"接了个空"也能过）。
- 门控全量 `go test -count=1 -p 1 ./...`：除**既有**的
  `TestBillingJobSettlesLedgerRowsAndWalletInOneTransaction` 失败外全绿。该失败在本轮改动前
  已于干净 HEAD 上复现（`cmd/gwd` 集成测试在共享库里留下 `usage_ledger` 行，而 B4.2 断言的是
  全局 run 计数），与 A12 无关，本轮不修。`internal/gateway` 套件通过——这一条尤其重要，
  因为本轮改了 8 处 release 构造调用点的签名。

### 明确未由本轮证实
- 前一轮我在选型问答里写过"ClickHouse 本地无实例、无法在本轮做真实验证，只能交付未经运行验证的骨架"
  ——**这句话是错的**，已由上面的真实容器验证推翻，此处更正。
- `BatchSize=500` / `FlushInterval=2s` / `BufferSize=10000` / `TTL=30d` **未经真实流量校准**，
  是可运行默认值而非容量规划结论。丢弃率在真实 RPM 下是多少，本轮没有数据。
- 未验证 ClickHouse 的集群/副本/备份/保留策略；未做压测；未验证生产接线在真实 ClickHouse 上启动。
- 明细与 `usage_ledger` 的交叉核对未做。

### 最高剩余风险
明细管道**天然 at-least-once**（写在事务提交之后，消费者在提交与 flush 之间重启会重放），
`ReplacingMergeTree` 只做**最终**收敛——后台 merge 之前，不带 `FINAL` 的 `count()` 会偏高。
**明细永远不能当账加总**，账的权威只有 `usage_ledger`。B5 控制台若直接对明细做求和展示，
就会在 merge 之前给出偏高的数字。

---

## B4.5 对账（2026-09-11）

本轮只做一件事：给 B4 加一个**只读的三方对账**，把"账本怎么算"、"钱包被扣了多少"、
"余额还剩多少"这三份各自独立写出来的记录，在一段时间窗上对齐。不修任何数据。

### 改动

- 新增 `internal/control/billing_reconcile.go`：`Reconciliation.Run(ctx, start, end)`。
  三方是 `usage_ledger`（窗口内按 `billing_state` 的行数、`billed_amount` 求和）、
  `wallet_transactions`（窗口行所属 `billing_run_id` 的 `debit` 求和）、
  `tenant_wallets.balance`（与该租户全量流水求和比）。
- 窗口取 `usage_ledger.occurred_at`，不取扣费时刻。理由写在代码注释里：扣费作业在用量发生
  之后若干分钟才跑，按扣费时刻切窗会在每个边界上报出并不存在的差额。窗口内的行改为顺着
  `billing_run_id` 找到那次运行，再拿**该次运行的完整足迹**（不裁剪到窗口）与它写的那一笔
  扣款比——两侧同口径，边界不再造差。
- `unpriced` / `held` / `pending` / `not_billable` 进 `UnsettledRows`，每行带 `event_id`。
  这是 `docs/B4-BILLING-MODEL.md` §2 对 B4.5 的增补要求：不计入收入，但必须单独列出，
  否则"全窗口都被 hold"会看起来完美平账而实际一分未收。
- 差额分六类，能归因到请求的都带 `EventIDs`：`run_debit_mismatch`、`wallet_balance_drift`、
  `billed_row_without_amount`、`billed_row_without_run`、`amount_on_unsettled_row`、
  `missing_wallet`。
- 只读由**数据库**承载而不是靠约定：整份报告跑在一个
  `pgx.TxOptions{AccessMode: pgx.ReadOnly, IsoLevel: pgx.RepeatableRead}` 事务里。
  只读之外还要求同一快照——否则一次在报告中途提交的扣费作业会被读成差额。
- 金额比较用 `big.Rat` 比值（`sameMoney`），不比字符串：`"1.5"` 与 `"1.500000000000"`
  是同一笔钱，比字符串会在 PG 和 Go 拼写不同的每个数上误报。

### 本轮执行（命令与结果）

```
gofmt -l internal/control/billing_reconcile.go        # 无输出
go build ./... && go vet ./internal/control/          # 通过
go test -count=1 -p 1 -run TestReconciliation -v ./internal/control/
  → 3 PASS（均真实执行，非 skip）
env -u GATEWAY_TEST_DATABASE_URL -u GATEWAY_TEST_REDIS_ADDR go test -count=1 ./...
  → 全绿
set -a; source configs/test-infra/test.env; set +a; go test -count=1 -p 1 ./internal/control/
  → 全绿
set -a; source configs/test-infra/test.env; set +a; go test -count=1 -p 1 ./...
  → 仅 TestBillingJobSettlesLedgerRowsAndWalletInOneTransaction 失败，见下
```

新增测试（`internal/control/billing_reconcile_integration_test.go`，前两个 PG-gated）：

- `TestReconciliationAlignsLedgerDebitsAndWalletOverAWindow`：播一个租户、四行用量
  （2 计费 / 1 unpriced / 1 held），跑扣费作业，再对账。断言三方全对齐
  （`billed=0.0165` == 扣款 == 余额变动）、`Balanced()` 为真、两行未结算按 `event_id` 列出、
  重跑一次结论不变。fixture 的 `occurred_at` 固定在 **2021-05-04**：gated 套件共用一个 PG，
  别的计费测试都在 `now` 附近播数据，用一个远古窗口才能让"对整个窗口的断言"仍然是
  "对本测试的断言"。
- `TestReconciliationAttributesDifferencesToEventIDs`：扣费后人为制造两处不一致——
  改大某一行的 `billed_amount`、以及绕过流水直接改余额。断言两类差额都被报出，
  且金额差额**点名到那个 `event_id`**（DoD 的实质要求）。
- `TestReconciliationSourceContainsNoWrites`（非 gated）：对源码断言
  `billing_reconcile.go` 不含 `INSERT/UPDATE/DELETE/ALTER/TRUNCATE/.Exec(`，
  且仍持有 `pgx.ReadOnly`。这是为了覆盖**没有测试走到的代码路径**：只读事务是执行者，
  这条是在生产上撞见 PG 拒写之前先响的绊线。

### 明确未由本轮证实

- **`./...` 的这一处失败不是本轮引入的。** 在 `git stash -u` 后的干净 HEAD 上执行
  `go test -count=1 -p 1 ./cmd/gwd/ ./internal/control/` 同样复现
  （`B4.3 buckets = held 2, not billable 2`）。成因是 `cmd/gwd` 的集成测试在共用的
  gateway_test 库里留下了 ledger 行，而 B4.2 那个测试断言的是扣费作业的**全局**计数器。
  按 CLAUDE.md 的改动边界，本轮只陈述、不修。单独跑 `./internal/control/` 全绿。
- 只提供库内 API：没有 CLI/HTTP 入口，没有定时运行，没有指标；告警规则仍属 E1。
- 只比金额，不重算单价。本轮不做"拿原始 token 数重新定价、与 `billed_amount` 比"的核对，
  因此"扣费算错了但账本和钱包一致"这一类错误对账查不出来。
- 不校验 `wallet_transactions.balance_after` 的逐行链条。同一 `created_at` 的行顺序不唯一，
  链式校验会产生假阳性；本轮只用与顺序无关的"余额 == 全量流水求和"这一恒等式，
  它能抓住"余额被绕过流水改动"，抓不住"中间某行的 `balance_after` 被单独改写"。
- 未在大窗口/大数据量上跑过：`Run` 会把窗口内每一行读进内存，窗口大小的上界没有压测过。
- new-api quota 整数单位 ↔ 货币金额的换算比例**仍未核对**（B4-BILLING-MODEL D4）。

### 最高剩余风险

对账证明的是"三份记录互相一致"，不是"金额本身正确"。单价配错、`unit_scale` 配错、
或 D4 那个未核实的换算比例错了，三方会**一致地错**，而本轮的报告一律显示 `Balanced()`。
要把这一层补上，需要的是重新定价核对（拿 ledger 的原始 token 数按当时价重算并与
`billed_amount` 比），本轮没做。

---



本轮只做一件事：把"钱包见底"这个状态经**已有的** `snap:auth:tokens:v1` 通道下发到 gateway，
由 gateway 在鉴权中间件里返回 402。热路径不新增任何依赖。

**改动**

- `internal/snapshot/auth.go`：`TokenRecord` 增加 `BillingBlocked bool`。下发的是**结论不是余额**——
  给出余额就等于邀请热路径自己做它没有权威做的算术。同一 tenant 的所有 token 取值必然一致：
  control 是唯一写入方，每个 publish 周期只算一次。
- `internal/control/tenants.go`：`ListActiveTokens` 的 SQL 加 `LEFT JOIN tenant_wallets`，
  判据是 `w.tenant_id IS NOT NULL AND w.balance <= 0`。**没有钱包行的 tenant 永不被拦**——
  "没有钱包"不等于"没有钱"，否则会一次性打掉所有非预付计费方式的租户。
  边界取 `<= 0`（含零）：余额恰为 0 的租户已经没有可花的钱了。
  这次读库发生在 15s 的 publish tick 上，离请求路径十万八千里。
- `internal/gateway/auth.go` / `run.go`：`AuthContext` 增加 `BillingBlocked`，
  `NewRouter` 的鉴权中间件在 `AuthenticateRequest` 成功之后、其余一切之前返回
  **402 Payment Required**，错误文案带 tenant，并计 `gateway_billing_rejected_total{tenant}`。
  放在中间件里是结构性保证，不是约定：被拦的请求根本到不了 handler，
  因此**不打上游、不产生 Release 事件、不写 `usage_ledger`**。
  402 与 401 严格分开：凭据是好的，钱不在。
- 溢出敞口信号：`TokenLoop` 增加可选 `Metrics`，每个 publish 周期发
  `control_billing_blocked_tenants`（按 **tenant** 去重，不是按 token）和
  `control_billing_uncapped_wallet_tenants` 两个 gauge，后者由
  `PGTenantRepository.UncappedWalletTenants` 支撑（`rpm = 0` 且有钱包的 active 租户），
  并同时打一条 log 点名这些租户。

**为什么接受"会欠一点"**

B4.0 D5 定的敞口上界约为
`峰值花费速率 × (快照周期 + 扣费周期 + 在途请求时长)`。
第一项唯一的封顶来自 A10 的 per-tenant `rpm`；**`rpm = 0` 的预付租户敞口无上界**，
这是本设计唯一一处会"坏得难看"而不是"坏得优雅"的地方。本轮选择**报出来而不是偷偷改**：
在这里硬塞一个限流值等于 control 自己发明一个没人拍板过的数字。这条是 E1 告警的落点，
本轮不写告警规则本身。

在途请求也不杀：拦截点在"新请求"上，已经打到上游的请求继续跑完，
否则会同时产生一笔已花的上游成本和一个没有 usage 的截断响应。

**本轮执行（命令与结果）**

- `gofmt -l .`、`go build ./...`、`go vet ./...`：通过（`gofmt` 的两处报告见下方免责）。
- 无凭据全量回归（清空 `GATEWAY_TEST_*` 环境变量）：`go test -count=1 ./...` 全绿。
- PG 门禁全量回归：`set -a; source configs/test-infra/test.env; set +a; go test -count=1 -p 1 ./...`
  连续执行 **2 次**全绿。
- 新增测试，`-v` 确认真实执行而非 skip：
  - `internal/gateway/billing_block_test.go`
    `TestRouterRefusesBillingBlockedTenantsWithoutTouchingUpstream`：`BillingBlocked` 的 token 得 402、
    文案含 tenant、**httptest 上游调用数为 0**、**Redis release 流中一条事件都没有**；
    未知 token 仍是 401（不塌缩成 402）；同一 router 上健康租户仍得 200 且上游恰好被调用 1 次。
  - 同文件 `TestGatewayDoesNotDependOnPostgresOrControl`：从
    `internal/gateway` 出发递归解析**非测试**源文件的 import（`go/parser`，`ImportsOnly`），
    断言整个可达闭包里没有 `jackc/pgx`、没有 `database/sql`、没有 `internal/control`。
    这是 DoD 里"gateway 全程未查 PG / 未调 control"的诚实证明方式：它覆盖所有代码路径，
    包括没有测试跑过的那些；将来谁往 gateway 里加一行 PG 查询，即使永不被调用也会在这里挂。
    另有"必须走到 snapshot/events/contracts"的自检，防止 walk 空转而假绿。
  - `internal/control/billing_block_integration_test.go`
    `TestWalletExhaustionReachesTheTokenSnapshotWithinOneTick`（PG 门禁）：
    充值 1.5 → publish → 未拦；扣到**恰好 0** → **只跑一次** `RunOnce` → 快照里已 `BillingBlocked=true`，
    对应 DoD 的"余额转负后，最多一个快照周期内 gateway 开始拒绝"；再充值 → 下一 tick 解除。
    同一断言里验证隔壁**没有钱包**的租户全程不被拦。
  - 同文件 `TestUncappedWalletTenantsNamesPrepaidTenantsWithNoRateLimit`（PG 门禁）：
    `rpm=0` 且有钱包 → 上报；`rpm=60` → 不报；无钱包 → 不报。
  - gauge 断言写成**相对基线的增减**而不是绝对值：这两个 gauge 统计的是整库租户，
    共享测试库里别的 fixture 会移动绝对数。第一次全量门禁跑就是被这一点打挂的，已修正。

**明确未由本轮证实**

- 真实计费准确性：全部数字来自本地 fixture 与本地 PG，不构成任何真实 provider 或生产计费结论。
- new-api `QuotaPerUnit`（quota 整数单位 ↔ 货币金额）**仍未核对**（B4-BILLING-MODEL D4），
  因此"余额 ≤ 0"这个判据的**绝对刻度**仍未验证——本轮只证明了机制，没证明标尺。
- 敞口上界公式本身未做压测标定：没有实测过"峰值花费速率"，公式是设计约束不是测量结果。
- 未验证在真实并发下 15s 快照周期对具体某租户的实际欠款金额。
- `gofmt -l .` 报告 `cmd/new-api-migrate/main_test.go` 与 `internal/control/health.go` 未格式化。
  两者都在本轮改动范围之外，按 CLAUDE.md 的 Change Boundary 只陈述、不顺手改。

**最高剩余风险**

`rpm = 0` 的预付租户敞口无上界。本轮把它变成了一个可观测、会点名的信号
（`control_billing_uncapped_wallet_tenants` + log），但**没有任何自动处置**：
真出现一个高速烧钱且不限流的预付租户，系统只会一边记账一边看着它欠下去，直到有人看告警。
告警规则落点是 E1。

## B4.3 `UsageSource` 可信度口径（2026-09-11）

本轮只把"什么样的用量算不算钱"写成一张穷举表，接管 B4.2 里一律留 `pending` 的行。热路径不动。

**改动**

- `internal/control/billing_policy.go`（新增）：处置表 `usageDispositions`，键是
  `(usage_source, partial, 是否成功)` 三元组——`是否成功` 必须进键，因为同一个 `usage_source`
  在"回了响应"和"报了错"两种场景下含义完全不同。3 × 2 × 2 = 12 种组合逐条写死，
  取值 `bill` / `hold` / `not_billable`。`dispositionFor` 对表外组合返回 `hold` + `known=false`，
  既不计费也不核销。`releaseSucceeded` 只认 `error_class == "ok"` 且 2xx/3xx：
  无法归类的错误类别按失败处理，因为"算不清还照收钱"是更坏的错。
- 用户拍板的三条口径（其余九条按同一原则推）：
  - 流式响应被截断、上游仍回了 usage（`upstream` + `partial=true` + 成功）→ **照常计费**。
    截断改变的是客户端收到什么，不是上游花了什么。
  - 请求失败、上游仍回了 usage（`upstream` + 失败）→ **不计费**，成本我们吃。
  - 请求成功但 usage 完全丢失（`missing` + 成功）→ **挂起待人工 + 单独告警**。
    这是真实漏收，不按零补齐（AGENTS.md §2 禁止伪造额度）。
  - `estimated` 四种组合一律 **挂起**：本平台目前没有任何 `estimated` 的生产者，
    真出现了也必须先过人。
- `migrations/007_usage_billing_states.sql`（新增）：`billing_state` 放宽到
  `pending`/`billed`/`unpriced`/`held`/`not_billable`；`billing_runs` 加 `rows_held`、
  `rows_not_billable`。006 已用同名约束，且每次启动全量重放，所以 007 先 `DROP CONSTRAINT
  IF EXISTS` 再 `ADD`。`usage_ledger_held_idx` 以 `usage_source` 打头做部分索引，
  因为 held 行永远按"为什么被挂起"来查。
- `internal/control/billing_job.go`：删掉 `billableUsageSource` 常量，`pendingTenants` 与
  `selectBillableRows` 不再在 SQL 里过滤可信度——每行都进作业，由处置表给终态。
  `BillingRunResult` 增加 `RowsHeld` / `RowsNotBillable` / `RowsUnknownClass`，与 `RowsBilled`
  分开上报和落库。`held` / `not_billable` 行不写 `billed_amount`（留 NULL）：写 0 会和
  "确实为零的已计费行"混淆。
- `publishSignals`：`control_billing_rows_total{state}` 计数器、
  `control_billing_unknown_usage_class_total` 计数器，以及
  `control_billing_held_rows{usage_source}` gauge——按 `usage_source` 拆开，
  并对三个已知 source 补零，让告警可以用阈值而不是"指标突然出现"。
  `unpriced`（我们的计价配置缺失）与 `missing`（上游没给数）保持两个桶，不合并。

**本轮执行**

- `gofmt -l .`：只剩 `cmd/new-api-migrate/main_test.go`、`internal/control/health.go` 两个
  既有未格式化文件，均不在本轮改动范围内，未动。
- `go build ./... && go vet ./...`：通过。
- 未开门禁 `go test -count=1 ./...`：全绿。
- 开门禁 `set -a; source configs/test-infra/test.env; set +a; go test -count=1 -p 1 ./...`：全绿。
- `go test -v -run 'Billing|UsageClass|Disposition|ReleaseSucceeded'` 确认新用例**真的跑了**
  而不是 skip：`TestEveryUsageClassHasAnExplicitDisposition`（断言 12 种组合全覆盖且表里没有
  够不着的条目）、`TestDispositionsMatchTheAgreedBillingPolicy`、
  `TestAnUnknownUsageClassIsHeldRatherThanGuessed`、`TestReleaseSucceededOnlyAcceptsAnExplicitOK`、
  `TestOnlyNonBillingDispositionsHaveATerminalState`、`TestBillingStatesWidenIdempotently`
  以及两个 PG 门禁集成用例，全部 PASS。
- `TestBillingJobSettlesLedgerRowsAndWalletInOneTransaction` 扩到 8 行 fixture，实跑验证
  终态：`billed`×3（含截断那笔 0.179982）、`unpriced`×1、`held`×2（`estimated` 与
  `missing`+成功）、`not_billable`×1（`upstream`+500）、窗口外 `pending`×1；
  钱包余额 10 → 9.807418，`wallet_transactions` 恰好一条 `-0.192582000000`，
  与 `billed_amount` 之和逐位相等；`billing_runs.rows_held=2`、`rows_not_billable=1`；
  重跑不再改动任何一行、不再产生第二条扣费。
  注：该 fixture 原先把 `error_class` 硬编码为 `''`，B4.3 之后那等于"全部失败"，本轮改为 `'ok'`。

**未由本轮证实**

- `estimated` 没有生产者，那四行口径**只有单测覆盖，没有端到端证据**。
- `held` 只有 gauge 和索引，**没有人工处理入口**；谁看告警、多久内处理，本轮不涉及。
- 告警规则本身没落地（属 E1），本轮只提供了可被告警的指标。
- 余额扣成负数仍不拦截（B4.4）；三方对账仍未做（B4.5）。
- new-api quota 整数单位 ↔ 货币金额的换算比例**仍未核对**（`docs/B4-BILLING-MODEL.md` D4）。
- 全部为本地 PG/Redis 门禁环境的回归，**不构成任何真实 provider 或生产计费的准入结论**。

## B4.2 从 `usage_ledger` 到扣费的慢路径作业（2026-09-11）

本轮只做扣费作业，不动 gateway 热路径。

**改动**

- `migrations/006_usage_billing.sql`：`usage_ledger` 追加 `billing_state`（`pending`/`billed`/
  `unpriced`/`held`）、`billed_amount NUMERIC(38,12)`、`billing_run_id`，加 `pending` 的部分索引；
  新增 `billing_runs` 记录每轮结果。全部 `ADD COLUMN IF NOT EXISTS`，`ADD CONSTRAINT` 用
  `pg_constraint` 查询显式挡住重复应用（`ALTER TABLE ... ADD CONSTRAINT` 没有 `IF NOT EXISTS`）。
- `internal/control/billing_job.go`：`BillingJob.RunOnce(ctx, windowEnd)`。按 tenant 分事务：
  `SELECT ... FOR UPDATE` 取该 tenant 的待计费行 → 按每行 `occurred_at` 取当时生效的单价 →
  `big.Rat` 算出行金额 → 一笔 `debit` 扣钱包 + 逐行标记 `billed` → 一起提交。
- `internal/control/billing.go`：抽出 `pgQuerier` 接口，`priceAt` / `applyMovement` 既可走连接池
  也可走作业的事务，使"扣钱包"和"标记已计费"落在同一个 `pgx.Tx` 里。
- `internal/control/run.go`：作业挂进 control 的 select 循环，`billingRunInterval = 5 * time.Minute`。

**本轮执行**

```
gofmt -l . && go build ./cmd/gwd && go vet ./... && go test -count=1 ./...   # 全部 ok
set -a; source configs/test-infra/test.env; set +a
go test -count=1 -p 1 ./...                                                  # 全部 ok
go test -count=1 -p 1 -run Billing -v ./internal/control/                    # 2 个 PG 用例 PASS，非 skip
```

- `TestBillingJobSettlesLedgerRowsAndWalletInOneTransaction`（真实 PG）：6 行 fixture 中
  2 行 `billed`、1 行 `unpriced`、3 行留 `pending`（`estimated` / `partial=true` / 窗口之外）；
  钱包余额 `10 → 9.987400000000`，`wallet_transactions` 恰好 1 条 `debit = -0.012600000000`，
  且等于 `sum(billed_amount)`；**再跑一次 `RowsBilled=0`、余额不变、debit 仍只有 1 条**。
- `TestBillingJobLeavesRowsPendingWhenATenantHasNoWallet`（真实 PG）：没有钱包的 tenant 让本轮
  `TenantsFailed=1`、run 记为 `failed`，其行仍是 `pending`；补上钱包后下一轮正常结清。
- `TestLineAmountKeepsPrecisionAFloatWouldLose`：三笔 0.1 相加为精确 `0.300000000000`。
- 价格表为空时整轮 `skipped` 并记一条 `billing_runs`，一行都不动。

**未由本轮证实**

- **没有任何真实单价**：`model_prices` 仍为空表，上述金额全部来自测试 fixture，
  不代表任何真实计费结果。本轮**未执行任何生产写入或真实扣费**。
- new-api quota 整数单位 ↔ 货币金额的换算比例**仍未核对**（`docs/B4-BILLING-MODEL.md` D4）。
  它不影响本轮的 token × 单价算术，但阻塞把 new-api 余额迁进钱包。
- 只验证了单进程顺序执行。`FOR UPDATE` 对并发两个 control 实例的保护**未做并发压测**。
- `held` 状态只在 schema 里预留，无代码路径写入，留给 B4.3。
- 扣费不检查余额，可以扣成负数；拦截点是 B4.4，本轮不做。

## B4.1 计价表与钱包 schema（2026-09-11）

本轮只做 schema 与仓储，不做扣费。

**改动**

- `migrations/005_billing.sql`：`model_prices` / `tenant_wallets` / `wallet_transactions`。
  所有金额列为 `NUMERIC(38,12)`；`model_prices` 上加了 `BEFORE UPDATE OR DELETE` trigger，
  任何改写历史价的尝试直接 `RAISE EXCEPTION`。文件与其余 migration 一样可重复应用
  （`CREATE TABLE IF NOT EXISTS` + `CREATE OR REPLACE FUNCTION` + `DROP TRIGGER IF EXISTS`）。
- `internal/control/billing.go`：`PGBillingRepository`。
  `InsertModelPrice` 在应用层拒绝 `effective_from <= now`（`ErrPriceNotEffectiveInFuture`）、
  拒绝超过 12 位小数（否则 PG 会静默四舍五入）、拒绝负单价；
  `PriceAt` 按传入时刻取价（B4.2 将传 ledger 行的 `occurred_at`），无价时返回 `found=false` 而不是 0；
  `ApplyMovement` 在一个事务里 `SELECT ... FOR UPDATE` → 更新余额 → 写流水，
  `balance_after` 由 PG 从库内 numeric 算出，不由调用方传入；
  `Balance` 对"无钱包"和"余额为 0"返回不同结果。仓储**没有**改价、删价、或单独改余额的方法。

**本轮执行**

- `go build ./cmd/gwd`、`go vet ./...`、`go test -count=1 ./...`（ungated）通过。
- 带库回归：`set -a; source configs/test-infra/test.env; set +a; go test -count=1 -p 1 ./...` 全绿。
  其中 `TestModelPricesOnlyGoForwardAndAreImmutable`、`TestWalletBalanceAndJournalMoveInOneTransaction`
  在真实 PostgreSQL 上实跑（非 skip），已核实：
  - 回溯价与"恰好等于 now"的价被拒；重复 `(provider, model, effective_from)` 被拒；
  - 直接 `UPDATE` / `DELETE` 单价行被数据库拒绝，行数不变；
  - 连续 100 次 `-0.01` 扣减后余额精确为 `9.050000000000`（float64 在此必然漂移）；
  - 流水求和 == `tenant_wallets.balance`，最后一条 `balance_after` == 当前余额；
  - 被守卫拒绝的流水（负充值 / 未知 kind / 重复 id）既不改余额也不留流水行；
  - 删除 tenant 级联清掉钱包与流水；`migrations.Apply` 连续跑两次无错。

**未由本轮证实**

- **未写入任何真实单价**，也未在任何环境建过真实钱包余额。表是空的。
- **new-api 的 `QuotaPerUnit`（quota 整数单位 ↔ 货币金额）本轮仍未核对**，
  这是 B4-BILLING-MODEL D4 明确标注的硬输入；A11 迁移过来的 quota 仍停留在 source unit，
  本轮没有做任何换算，也没有据此给任何租户建钱包。
- `wallet_transactions` 目前只靠"仓储不提供改写入口"保证不可变，**没有**像 `model_prices`
  那样加数据库 trigger。
- 并发正确性只由 `FOR UPDATE` 结构保证，**未做并发压测**。

## A11 迁移 tenant 转正（2026-09-11）

本轮只做一件事：让 `internal/migration/newapi` 把 new-api 的 users / tokens 转正为目标库的
tenants / principals / tenant_tokens 行。**非目标**：不做任何 quota 换算或扣费（B4）、不动 gateway / control、不改 A6 快照通道。

**改动**
- `types.go`：新增 `PlannedTenant` / `PlannedTenantToken`，`Plan` 增加 `Tenants` / `TenantTokens`，
  `Summary` 增加 `tenant_count` / `principal_count` / `tenant_token_count`（dry-run JSON 自动带出）。
- `plan.go`：`addSourceRecords` 的 users / tokens 分支不再是 `staging_only`。用户状态 1→active、2→suspended；
  token 状态 1→active、2/3/4→revoked（源状态保留在 record 的 `source_status`，供 B4 回看 exhausted）；
  `expired_time` ≤0 → NULL；group 回退链 token.group → user.group → `default`；id 为 `source_system+"\0"+source_id`
  的 sha256 前 16 字节（与 `importedAccountID` 同法）。凡 id ≤0、状态未知、key 为空、expired_time 不可解析、
  user 未被规划为 tenant 的，一律 `addRejected` 记原因，不补默认租户。quota / remain_quota 仍只保留原始值。
- `apply.go`：同一事务内 `applyTenant`（tenants + principals upsert，`ON CONFLICT (id)`，**不碰** A10 的
  `max_concurrency` / `rpm`，并校验 principal 归属）与 `applyTenantToken`（`ON CONFLICT (id)`，`revoked_at`
  用 `COALESCE` 保留首次时间，源侧重新启用则清空）。

**核实过的上游事实（读 `/home/elucid/projects/new-api` 源码，不是猜）**
- `common/utils.go:254` `GenerateKey()` 生成 48 位随机串，**不带 `sk-`**；`middleware/auth.go:293` 对入站 bearer
  `TrimPrefix(key, "sk-")`；前端 `api-keys-cells.tsx:56` / `api-keys-provider.tsx:86` 发给用户的是 `` `sk-${key}` ``。
  因此 `tenant_tokens.token_hash = HashToken("sk-" + key)`（已带前缀的值不重复加）。**明确收窄**：new-api 同时接受
  裸 key，本迁移不复刻这一回退——一份凭据两个撤销点是安全隐患，此收窄写在 `bearerForm` 注释与 TODO 里。
- `common/constants.go:226-234`：UserStatusEnabled=1 / Disabled=2；TokenStatusEnabled=1 / Disabled=2 / Expired=3 /
  Exhausted=4。`model/token.go:22,236`：`expired_time=-1` 表示永不过期。
- `migration_records.raw_summary.key_sha256` 仍是 JSON 编码裸 key 的 digest，**刻意不等于** auth hash，records 转储
  不能当凭据校验器用；`TestBuildPlanStagesUsersTokensQuotaWithoutSecrets` 新增断言 auth hash 不出现在 records 中。

**本轮执行**
- `go build ./cmd/gwd && go vet ./... && go test -count=1 ./...`：全部 ok。
- `source configs/test-infra/test.env && go test -count=1 -p 1 ./...`（Docker `gateway-test-pg` / `gateway-test-redis`
  均 healthy）：全部 ok，其中 `internal/migration/newapi` 的 7 个 `TestApply*` 均实跑 PG（非 skip）。
- 新增回归：`TestBuildPlanPromotesUsersAndTokensToTenantsPrincipalsAndTokens`（id 哈希、`sk-` 归一、状态/过期/group
  映射）、`TestBuildPlanRejectsUnmappableUsersAndTokensWithoutGuessing`（9 种拒绝各有原因、拒绝行也不含明文）、
  `TestApplyPromotesTenantsPrincipalsAndTokensIdempotently`（行落库、`control.PGTenantRepository.ListActiveTokens`
  只看见 active token 且 hash 命中 `sk-` 形式、重复 apply 不增行、`revoked_at` 不漂移、手工设的 `rpm=42` 不被覆盖）；
  `TestApplyNeverWritesPlaintextSecretsToAnyColumn` 扩到 tenants / principals / tenant_tokens 三表。

**未由本轮证实**
- 未对真实 new-api 库做 dry-run；上述前缀/状态语义来自源码，不是来自某个部署的实际数据。
- 未验证 new-api 前端之外的分发渠道是否也只发 `sk-` 形式（例如第三方脚本直接读库）；若存在裸 key 用户，迁移后会认证失败，
  需要在切流前用真实库 dry-run 的 `tenant_token_count` 与实际活跃 key 数对照。

## B4.0 计费模型决策记录（2026-09-10，仅文档）

本轮只做一件事：产出 `docs/B4-BILLING-MODEL.md`。**非目标**：不写计费代码、不建表、不动 A11。

**本轮实际执行**
- 为写准文中引用的现有口径，读取并核对了：`migrations/001_initial.sql:81-100`（`usage_ledger` 已有
  `tokens_in` / `tokens_out` / `cache_read_tokens` / `cache_write_tokens` / `usage_source` / `partial`
  与 `event_id UNIQUE`）、`internal/snapshot/auth.go:21,24`（`snap:auth:tokens:v1` 与 `TokenRecord`）、
  `internal/control/tenants.go:135` 与 `internal/control/run.go:105`（token 快照 tick 为 15s）、
  `internal/gateway/auth.go:19-34`（`AuthContext` / `TenantLimits`）、
  `migrations/002_tenants.sql`、`migrations/004_tenant_limits.sql`（`max_concurrency` / `rpm`，0 = 不限）、
  `pkg/contracts/contracts.go:139`（`Decimal`）。文中所有代码位置引用均出自本轮实读，非记忆。
- 三项业务决策由项目所有者本轮拍板：token 计价四类分别单价、预付余额、余额不足拒绝新请求走快照下发。
  其余（单价存放位置、改价生效语义、缺价行为）由本文给出工程结论并附被否决选项。
- `docs/TODO.md` 的 B4.0 条目标记完成并挂上记录链接，头部"下一步顺序"同步划掉 B4.0。

**未由本轮证实**
- 本轮**未写任何代码、未建任何表、未跑任何测试**；`go build` / `go test` 未执行，因为无代码变更。
  文中的 schema 片段是待实现形状，不对应仓库中任何已存在的表。
- **new-api 的 quota 整数单位与货币金额换算比例未核对**（未读真实 new-api 部署配置）。这是 B4.1 的硬输入，
  届时必须实读确认，不得按上游默认值假定。
- 文中的透支上界公式是结构性推导，未经真实流量校准；四类 token 的具体单价数值不在本文范围内。

## 计划补全 + `internal/migration/newapi` 测试补全（2026-09-10）

本轮只做两件事：把第二次结构复盘的结论补进 `docs/TODO.md`（新增 A11、A12、C6、E1、E2，B4 拆为
B4.0–B4.5），以及给 `internal/migration/newapi` 补测试。**未写任何 B4 实现代码，未改设计文档，
未改其他包的非测试代码。**

补测试前的基线：该包 983 行实现只有 116 行测试，且全部针对 `BuildPlan`；`apply.go`（唯一写目标库的
路径）与 `source.go`（唯一读生产 new-api 库的路径）**零测试覆盖**。

新增：
- `source_test.go`（无基础设施，始终运行）：`validIdentifier` 拒绝 8 类可逃逸出 SQL 的 schema 名；
  `requiredInt` 对缺失/空白/非数字一律报错而非退化为 0；`optionalInt` 只在缺失或不可解析时用 fallback
  （显式 `0` 必须存活）；`optionalInt64`；`parseProxy` 对不可用 setting 返回空而不猜；
  `Apply` 的 nil db / nil cipher 双重 fail-closed 守卫（用不拨号的 pool 触达 cipher 守卫）。
- `source_integration_test.go`（`GATEWAY_TEST_DATABASE_URL` 门禁）：在测试库里建一次性 schema 跑
  `LoadSnapshot`——完整 schema 下 channels 按 id 排序、`setting.proxy` 与 codex OAuth key JSON 不丢、
  数值列以原文文本到达；最小 schema（只有 id/type/key）不报错而是产出 users/tokens/quota/groups 四条
  schema warning 且缺失可选列返回空值，warning 传递到 plan summary；`status` 列存在但为 NULL 或未知值时
  **不猜测为 active**，`BuildPlan` 全部 rejected 且理由含 status；缺 `channels.key` 列显式报错并点名该列；
  channels 表整体缺失报错；注入型 schema 名被拒且源表未被破坏，另单独验证该库在
  `pgx.ReadOnly` 事务下确实拒绝 INSERT（否则"从不写源库"这条断言等于没测）。
- `apply_integration_test.go`（同门禁）：一次 run 原子写入 accounts / credentials / migration_records，
  凭据能按 accountID 解密回原 bundle 且换 accountID 解密必失败；**明文扫描**——把 accounts、credentials、
  migration_records、migration_runs 每行整行转文本，断言四个 fixture 明文密钥均不出现，并反向确认
  key 摘要确实在 staging 里（否则扫描不证明什么）；重复 apply 幂等（行数不变、`fence_epoch` 2、
  `credentials.version` 1、records 重新指向最新 run）；run id 冲突时整个 run 回滚且 `fence_epoch` 不前移；
  空 run id 自动生成 `new-api-` 前缀并回填到 records。

本轮实际执行并通过（本机，2026-09-10）：
- 无基础设施：`go build ./cmd/gwd`、`go vet ./...`、`go test -count=1 ./...` 23 包全绿，门禁用例严格 skip。
- 带基础设施（Docker `postgres:16-alpine` + `redis:7.0.15-alpine`，`configs/test-infra/`）：
  `internal/migration/newapi` 22 个用例全部真实跑通（含全部 11 个 PG 门禁用例）；
  `source configs/test-infra/test.env && go test -count=1 -p 1 ./...` 23 包全绿；
  `go test -race -count=1 -p 1 ./pkg/contracts ./internal/control/... ./internal/migration/...` 全绿。

**本轮发现并修掉的一个测试隔离问题**：新增 PG 用例最初会把 `source_system='new-api'` 的账号留在共享测试库里，
这些账号的 credential 是用本包测试密钥加密的，而 `cmd/gwd/import_integration_test.go:41` 只删自己那一行、
随后按全量账号构建快照，于是 `-p 1` 全量跑时 `cmd/gwd` 报 `invalid credential ciphertext` 并连带
`TestProcessE2EControlGatewayRelease` 超时失败。已在本包加 `t.Cleanup` 按 `source_system` 定向清理，
并实测清理后库中 accounts/credentials/migration_runs/migration_records 均为 0、全量套件恢复全绿。
**未改 `cmd/gwd` 的测试**——该包"只删自己的行却读全量账号"的脆弱性本轮只记录，不在本次范围内。

**未由本轮证实**：真实 new-api 生产库上的只读探测与 dry-run（本轮只在本地测试库里用合成 schema 验证）；
任何真实 provider 行为。上述明文不落库的断言只覆盖 `Apply` 写的四张表，不覆盖日志输出。

## A10 粘滞会话 + per-tenant 限流（2026-09-10）

实现见 `docs/TODO.md` A10。本轮实际执行并通过：

- Docker Redis 7.0.15 上 `redis-cli` 实测 ACL：`test-gateway` 可 `SET gateway:sticky:*`，对 `snap:*` 的 `SET`
  被 NOPERM 拒绝。
- 无基础设施 `go test -count=1 ./...` 全部通过；`source configs/test-infra/test.env && go test -count=1 -p 1 ./...`
  连续两轮全部通过；`go vet ./...`、`go test -race` 覆盖 gateway/control/snapshot、`git diff --check` 通过。
- 新增回归：`TestStickyIsTenantScopedAndDegradesWithoutRedis`（跨 tenant 不可见、原始 key 不落 Redis、TTL、
  重 pin、零值 Sticky 惰性、key 校验）；`TestChooserPrefersPinnedAccountOnlyWhenEligible`（pin 命中、pin 到
  排除/未知/冷却账号被忽略、pin 不覆盖 failover 排除）；`TestTenantLimiterIsIndependentOfAccountLimits`
  （0 限额不碰 Redis、并发/RPM 上限、租户间与账号限流互不影响、无 Redis fail-closed）；
  `TestRouterAppliesStickySessionsAndTenantLimits`（HTTP 层：pin 到非默认账号后三次连续命中、其他 session
  与其他 tenant 不受影响、首个请求自动 pin、超长 key 400、并发上限时第二请求 429 而其他 tenant 正常、RPM
  上限）。`TestPGTenantTokensPublishAndRevoke` 增补：创建时设限额并发布、nil 字段保留旧值、负数拒绝。
  `TestProcessE2EControlGatewayRelease` 增补：带 `X-Session-Key` 的真实请求后 Redis 出现对应 pin 且指向
  实际服务账号。

未执行真实 provider 请求、生产写入或部署。

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
