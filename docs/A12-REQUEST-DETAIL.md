# A12 请求明细存储 · 决策记录

日期：2026-09-11
状态：已决策并落地
决策人：用户（形态与字段两项均由用户拍板）

## 1. 为什么需要这个决策

总览 §6.4 把「计量流水与高频请求明细的物理分离」列为 P0：控制面的负载应该随**账号数**增长，而不是随 **RPM** 增长。
`usage_ledger` 在控制面 PostgreSQL 上是单写者路径，计费、结算、对账都压在它身上；如果每一次请求尝试的明细
也往这条路径上写，控制面的写入量就直接和网关流量绑定了。

同时 §6.5 的控制台需要「按租户/账号/模型/时间看请求明细与重试链」，这份数据必须存在于某处。

## 2. 候选形态

| 形态 | 优点 | 代价 |
| --- | --- | --- |
| **A. 引入 ClickHouse** | 列存、高写入吞吐、TTL 原生、聚合查询快，控制台可以直接查 | 引入一个新中间件；部署/运维/备份都多一份 |
| B. 抽样落结构化文件 | 零新依赖，符合「不默认引入新中间件」 | 只有抽样，重试链会断；查询要靠外部工具 |
| C. 直接丢弃，只留指标 | 最省 | 控制台明细能力落空，事后无法复盘单请求 |

**用户选择 A（引入 ClickHouse）。**

需要显式记录的一点：DoD 里写的是「不默认引入新中间件」，本决策**与该默认相悖**，是用户明确覆盖后的选择。
代价被两件事约束住：

- 明细存储是**可选**的。`GATEWAY_DETAIL_CLICKHOUSE_URL` 为空即关闭，控制面照常启动、照常计费。
- 不引入 ClickHouse Go 驱动依赖。走 HTTP 接口（8123）+ `JSONEachRow`，只用标准库 `net/http`。

## 3. 解耦方式：靠形状，不靠自觉

DoD 要求「写入路径与 `usage_ledger` 事务解耦，明细写失败不得影响计量与 ACK」。

`internal/events/stream.go` 只在 `HandleRelease` 返回 nil 时 ACK。所以这里不是「小心不要把错误传上去」的问题，
而是**必须让明细写入根本没有错误可传**：

- `Sink.Observe(release)` **没有 error 返回值**，也不会阻塞（对有界 channel 做非阻塞 send）。
- 它在 `Ledger.HandleRelease` 里被调用的位置是 `tx.Commit()` **之后**，事务之外，函数最后一行。
- `*detail.Sink` 为 nil 是合法的「明细关闭」配置，不 panic。

缓冲满了就丢，而不是排队：在明细存储允许失败的前提下，一个无界队列只是把「明细故障」翻译成「控制面 OOM」。
同理，flush 失败的那批直接丢弃，不在内存里重试。

丢弃必须可见，否则静默丢明细和静默丢流量长得一模一样。指标：

- `detail_records_total` / `detail_written_total`
- `detail_buffer_depth`（gauge）
- `detail_dropped_total{reason="buffer_full"|"write_failed"}`
- `detail_flush_failures_total`

## 4. 字段

用户选择「Release 全字段 + 请求路由信息」。落地为 `internal/detail.Record`：

- 身份：`event_id` / `request_id` / `attempt_id` / `attempt_no` / `occurred_at` / `ingested_at`
- 路由：`producer_id` / `tenant_id` / `account_id` / `provider` / `model` / `inbound_protocol`
- 结果：`status_code` / `latency_ms` / `error_class` / `usage_source` / `partial` /
  `tokens_in` / `tokens_out` / `cache_read_tokens` / `cache_write_tokens`

`occurred_at` 与 `ingested_at` 分开保存：两者之差就是明细管道的滞后，合并会把它藏起来。

`inbound_protocol` 是本轮为 A12 新加到 `contracts.Release` 上的字段，**可选**，`Validate()` 未做要求 ——
它是路由上下文，不是计量输入，缺失绝不能让一条 release 变成不可计费。

### 凭据不入明细

DoD：「凭据、token 明文、refresh token 不得进入明细。」

这里的保证是**结构性**的，不是过滤性的：`Record` 里没有 map、没有 interface、没有 header 袋子、
没有 request/response body 字段，一个密钥没有地方可以落进来。
`TestRecordCarriesNoFreeFormFields` 用反射守住这条：加一个非标量字段、或者加一个名字里带
token/secret/credential/auth/header/body/prompt/response/cookie 的字段，测试就红。

另外，ClickHouse 的账号密码从 URL 里摘出来改走 Basic-Auth header，避免出错时的请求行把凭据带进日志。

## 5. 表结构与去重

```
ENGINE = ReplacingMergeTree(ingested_at)
PARTITION BY toYYYYMMDD(occurred_at)
ORDER BY (tenant_id, occurred_at, event_id)
TTL toDateTime(occurred_at) + INTERVAL 30 DAY
```

因为写入发生在事务提交之后，消费者在「提交完成」与「flush 完成」之间重启会重放这条 release ——
这条管道**天然是 at-least-once**。`ReplacingMergeTree` 让重放收敛成一行而不是翻倍。

**由此产生一条硬约束：明细永远不能当账来加总。** 后台 merge 之前，不带 `FINAL` 的 `count()` 可以是 2。
账的权威是 `usage_ledger`，明细只是运维视图。

TTL 30 天：明细是运维辅助而不是账本记录，让它自己过期，不要无限增长。

## 6. 本决策未覆盖的

- `BatchSize=500` / `FlushInterval=2s` / `BufferSize=10000` / `TTL=30d` 这几个常量**未经真实流量校准**，
  是可运行的默认值，不是容量规划结论。
- 没有 ClickHouse 的集群、副本、备份与保留策略；本轮只有单实例。
- 控制台查询界面属于 B5，本轮不做。
- 明细与 `usage_ledger` 的交叉核对（「明细里有、账里没有」）没有做，B4.5 对账只覆盖账侧三方。
