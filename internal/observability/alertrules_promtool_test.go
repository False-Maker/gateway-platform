package observability_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

// A25 closes the one gap A21 through A24 all had to declare and none could
// close: every guarantee those rounds shipped stopped at "the series has the
// right shape", because nothing in this repository could evaluate PromQL.
// "The rule actually fires" was taken from the Prometheus documentation, never
// observed. The three fixes and the bug they fixed were, until this file, an
// argument rather than a measurement.
//
// The other tests in this package are deliberately dependency-free and run
// everywhere. This one is not: it needs a real promtool. It is env-gated the
// same way the PostgreSQL integration tests are, and it t.Skip()s loudly rather
// than pretending to have checked something.
//
//	GATEWAY_TEST_PROMTOOL_IMAGE=prom/prometheus:latest go test ./internal/observability/
//
// or put a promtool binary on PATH.
//
// What this file does NOT cover, stated so the coverage is not overread:
// positive cases are asserted with promql_expr_test, which evaluates the
// expression and ignores the rule's `for:` duration and annotation templates.
// Negative cases use alert_rule_test, which does exercise the whole rule. The
// split exists because promtool compares annotations by exact equality, and
// copying this repository's multi-paragraph descriptions into a fixture would
// turn every wording edit into a red test.
const promtoolImageEnv = "GATEWAY_TEST_PROMTOOL_IMAGE"

// scenario is one claim, written as the claim rather than as a fixture.
type inputSeries struct {
	selector string
	values   string
}

type scenario struct {
	name  string
	claim string
	// series are the promtool input_series entries.
	series []inputSeries
	// alert is the alert whose expression (positive) or whole rule (negative)
	// is under test. The expression is read out of the rules file, never
	// copied here -- a test carrying its own copy of the expression would stay
	// green after someone edited the rule.
	alert    string
	evalTime string
	// wantValue is the expected sample; empty means "expect no alert at all".
	wantLabels string
	wantValue  string
}

func promtoolScenarios() []scenario {
	return []scenario{
		{
			name:  "A21_unpriced_fires_when_the_series_starts_at_zero",
			claim: "序列 0→3 后平稳，BillingUnpricedRows 必须响，$value = 3。注意：本形状下 increase() 本就返回精确的 3，round() 是空操作——A21 记的\"单次 +1 常报成 1.0166…\"对密集平坦序列不成立，round() 真正消去的是持续增长计数器上的浮点噪声，见 quota 场景",
			series: []inputSeries{
				{selector: `control_billing_rows_total{state="unpriced"}`, values: `0+0x9 3+0x20`},
			},
			alert:      "BillingUnpricedRows",
			evalTime:   "25m",
			wantLabels: `{state="unpriced"}`,
			wantValue:  "3",
		},
		{
			name:  "A21_unpriced_stays_silent_when_the_series_is_born_at_three",
			claim: "这是 A21 修复前的真实状态：序列诞生即为 3、之后平稳。必须不告警——这条测试证明那个 bug 是真的，也证明上一条不是空跑",
			series: []inputSeries{
				{selector: `control_billing_rows_total{state="unpriced"}`, values: `3+0x30`},
			},
			alert:    "BillingUnpricedRows",
			evalTime: "25m",
		},
		{
			name:  "A22_unknown_usage_class_fires_from_a_zero_start",
			claim: "control_billing_unknown_usage_class_total 0→1，BillingUnknownUsageClass（critical）必须响",
			series: []inputSeries{
				{selector: `control_billing_unknown_usage_class_total`, values: `0+0x9 1+0x20`},
			},
			alert:      "BillingUnknownUsageClass",
			evalTime:   "25m",
			wantLabels: `{}`,
			wantValue:  "1",
		},
		{
			name:  "A22_unknown_usage_class_stays_silent_when_born_at_one",
			claim: "修复前状态：该计数器自己的注释写着 never expected to be non-zero，于是唯一那次增量就是序列的诞生，规则看不见",
			series: []inputSeries{
				{selector: `control_billing_unknown_usage_class_total`, values: `1+0x30`},
			},
			alert:    "BillingUnknownUsageClass",
			evalTime: "25m",
		},
		{
			name:  "A22_dlq_fires_while_a_reason_keeps_growing",
			claim: "StreamDLQGrowing（critical）在某个 reason 持续进 DLQ 时会响，且按 reason 分别归因",
			series: []inputSeries{
				{selector: `control_stream_dlq_total{reason="invalid_json"}`, values: `0+1x20`},
			},
			alert:      "StreamDLQGrowing",
			evalTime:   "15m",
			wantLabels: `{reason="invalid_json"}`,
			wantValue:  "10",
		},
		{
			name:  "A22_dlq_stays_silent_when_the_reason_series_is_born_at_one",
			claim: "A22 为 DLQ 预置 4 个 reason 之前的真实状态：某类目的第一条消息让序列诞生即为 1、之后平稳，规则看不见它。这是那次预置存在的全部理由",
			series: []inputSeries{
				{selector: `control_stream_dlq_total{reason="invalid_json"}`, values: `1+0x30`},
			},
			alert:    "StreamDLQGrowing",
			evalTime: "15m",
		},
		{
			name:  "A23_pending_stuck_stays_silent_while_the_reclaim_series_is_absent",
			claim: "A23 的缺陷本体：pending 持续非零但 reclaim 序列不存在时，and 取交集为空，整条 critical 规则不产生结果",
			series: []inputSeries{
				{selector: `control_stream_pending`, values: `5+0x40`},
			},
			alert:    "StreamPendingStuck",
			evalTime: "35m",
		},
		{
			name:  "A23_pending_stuck_fires_once_the_reclaim_series_exists_at_zero",
			claim: "预置 control_stream_reclaim_total 为 0 之后，== 0 成为一次真实比较，规则在完全停摆时终于会响",
			series: []inputSeries{
				{selector: `control_stream_pending`, values: `5+0x40`},
				{selector: `control_stream_reclaim_total`, values: `0+0x40`},
			},
			alert:      "StreamPendingStuck",
			evalTime:   "35m",
			wantLabels: `{}`,
			wantValue:  "5",
		},
		{
			name:  "A24_snapshot_accounts_dropped_fires",
			claim: "第七次复盘 F2 新增的规则确实会响；快照循环每分钟重复发射，所以不需要 A22 的预置",
			series: []inputSeries{
				{selector: `control_snapshot_accounts_dropped_total{provider="unregistered",reason="usage_integrity_unset"}`, values: `2+2x20`},
			},
			alert:      "SnapshotAccountsDropped",
			evalTime:   "18m",
			wantLabels: `{provider="unregistered",reason="usage_integrity_unset"}`,
			wantValue:  "30",
		},
		{
			name:  "A24_quota_not_draining_fires_on_sustained_retry",
			claim: "第七次复盘 F3 新增的规则确实会响（retry 分支），且 $value 是整数 30。这一条同时是 round() 的活证据：去掉它，promtool 实测返回 30.000000000000004",
			series: []inputSeries{
				{selector: `quota_snapshot_reconcile_total{result="retry"}`, values: `0+1x40`},
			},
			alert:      "QuotaSnapshotNotDraining",
			evalTime:   "35m",
			wantLabels: `{result="retry"}`,
			wantValue:  "30",
		},
		{
			name:  "egress_platform_transport_rejection_fires",
			claim: "平台级 transport 拒绝判定置位即触发（critical）",
			series: []inputSeries{
				{selector: `control_platform_alert{provider="anthropic",reason="transport_rejections"}`, values: `1+0x20`},
			},
			alert:      "PlatformTransportRejection",
			evalTime:   "10m",
			wantLabels: `{__name__="control_platform_alert",provider="anthropic",reason="transport_rejections"}`,
			wantValue:  "1",
		},
		{
			name:  "egress_transport_rejection_spread_fires",
			claim: "拒绝波及的账号数超过 3 个即触发（看广度而非单账号）",
			series: []inputSeries{
				{selector: `control_platform_transport_rejections_accounts{provider="anthropic"}`, values: `5+0x20`},
			},
			alert:      "PlatformTransportRejectionSpread",
			evalTime:   "15m",
			wantLabels: `{__name__="control_platform_transport_rejections_accounts",provider="anthropic"}`,
			wantValue:  "5",
		},
		{
			name:  "egress_starvation_gate_fires_from_a_zero_start",
			claim: "A22 为该指标按 platform 预置 0 之后，防饿死闸触发可见",
			series: []inputSeries{
				{selector: `control_platform_starvation_release_total{platform="anthropic"}`, values: `0+1x25`},
			},
			alert:      "StarvationGateFired",
			evalTime:   "20m",
			wantLabels: `{platform="anthropic"}`,
			wantValue:  "15",
		},
		{
			name:  "egress_starvation_gate_silent_when_born_at_one",
			claim: "A22 预置之前的真实状态：闸只触发过一次，序列诞生即为 1，规则看不见。这是 SnapshotLoop.PrimeMetrics 存在的理由",
			series: []inputSeries{
				{selector: `control_platform_starvation_release_total{platform="anthropic"}`, values: `1+0x30`},
			},
			alert:    "StarvationGateFired",
			evalTime: "20m",
		},
		{
			name:  "stream_pending_backlog_fires",
			claim: "pending 超过 1000 即触发（量级告警，与 StreamPendingStuck 的“动没动”互补）",
			series: []inputSeries{
				{selector: `control_stream_pending`, values: `1500+0x20`},
			},
			alert:      "StreamPendingBacklog",
			evalTime:   "15m",
			wantLabels: `{__name__="control_stream_pending"}`,
			wantValue:  "1500",
		},
		{
			name:  "stream_duplicate_spike_fires_on_sustained_duplicates",
			claim: "重复入账持续发生时触发（阈值 >10）",
			series: []inputSeries{
				{selector: `usage_ledger_duplicate_total`, values: `0+1x25`},
			},
			alert:      "UsageLedgerDuplicateSpike",
			evalTime:   "20m",
			wantLabels: `{}`,
			wantValue:  "15",
		},
		{
			name:  "stream_duplicate_spike_silent_when_eleven_arrive_as_one_burst",
			claim: "A22 那条更正的实测证据：11 次重复挤在一个突发里，序列诞生即为 11、之后平稳，increase 恒为 0，**阈值 >10 并不能免疫**。这是 A22 判定“可容忍”的那一类的真实代价",
			series: []inputSeries{
				{selector: `usage_ledger_duplicate_total`, values: `11+0x30`},
			},
			alert:    "UsageLedgerDuplicateSpike",
			evalTime: "20m",
		},
		{
			name:  "usage_trust_missing_ratio_fires",
			claim: "missing 占成功 xadd 的比例超过 1% 即触发（critical）",
			series: []inputSeries{
				{selector: `release_usage_missing_total`, values: `0+1x30`},
				{selector: `release_xadd_total{result="success"}`, values: `0+50x30`},
			},
			alert:      "UsageSourceMissingRatioHigh",
			evalTime:   "20m",
			wantLabels: `{}`,
			wantValue:  "0.02",
		},
		{
			name:  "usage_trust_synthetic_spike_fires_from_a_zero_start",
			claim: "A22 预置 source=\"synthetic\" 之后，合成终态可见",
			series: []inputSeries{
				{selector: `request_attempt_recovered_total{source="synthetic"}`, values: `0+1x25`},
			},
			alert:      "SyntheticTerminalStateSpike",
			evalTime:   "20m",
			wantLabels: `{source="synthetic"}`,
			wantValue:  "15",
		},
		{
			name:  "usage_trust_synthetic_spike_silent_when_born_at_one",
			claim: "A22 预置之前：一次合成终态让序列诞生即为 1，规则看不见",
			series: []inputSeries{
				{selector: `request_attempt_recovered_total{source="synthetic"}`, values: `1+0x30`},
			},
			alert:    "SyntheticTerminalStateSpike",
			evalTime: "20m",
		},
		{
			name:  "usage_trust_attempt_open_age_fires",
			claim: "尝试开放时长超过 15 分钟即触发",
			series: []inputSeries{
				{selector: `request_attempt_open_age_seconds`, values: `1200+0x20`},
			},
			alert:      "AttemptOpenAgeHigh",
			evalTime:   "10m",
			wantLabels: `{__name__="request_attempt_open_age_seconds"}`,
			wantValue:  "1200",
		},
		{
			name:  "billing_held_rows_accumulating_fires",
			claim: "held 托盘非空即触发，并按 usage_source 归因",
			series: []inputSeries{
				{selector: `control_billing_held_rows{usage_source="missing"}`, values: `4+0x40`},
			},
			alert:      "BillingHeldRowsAccumulating",
			evalTime:   "35m",
			wantLabels: `{__name__="control_billing_held_rows",usage_source="missing"}`,
			wantValue:  "4",
		},
		{
			name:  "billing_uncapped_wallet_tenants_fires",
			claim: "预付租户 rpm 为 0（透支窗口无封顶）即触发",
			series: []inputSeries{
				{selector: `control_billing_uncapped_wallet_tenants`, values: `2+0x25`},
			},
			alert:      "BillingUncappedWalletTenants",
			evalTime:   "20m",
			wantLabels: `{__name__="control_billing_uncapped_wallet_tenants"}`,
			wantValue:  "2",
		},
		{
			name:  "billing_blocked_tenants_fires",
			claim: "有租户处于停用态即触发（info）。这条正是 gateway_billing_rejected_total 被列入豁免名单的依据",
			series: []inputSeries{
				{selector: `control_billing_blocked_tenants`, values: `3+0x40`},
			},
			alert:      "BillingBlockedTenantsSpike",
			evalTime:   "35m",
			wantLabels: `{__name__="control_billing_blocked_tenants"}`,
			wantValue:  "3",
		},
		{
			name:  "billing_reconciliation_unbalanced_fires",
			claim: "对账判定不平即触发（critical）。注意它是 == 0 而不是 > 0，序列缺失时同样无结果——与 A23 同形状",
			series: []inputSeries{
				{selector: `control_billing_reconcile_balanced`, values: `0+0x40`},
			},
			alert:      "BillingReconciliationUnbalanced",
			evalTime:   "35m",
			wantLabels: `{__name__="control_billing_reconcile_balanced"}`,
			wantValue:  "0",
		},
		{
			name:  "billing_reconciliation_discrepancy_kind_fires",
			claim: "对账差异按 kind 归因触发",
			series: []inputSeries{
				{selector: `control_billing_reconcile_discrepancies{kind="orphan_debit"}`, values: `2+0x40`},
			},
			alert:      "BillingReconciliationDiscrepancyKind",
			evalTime:   "35m",
			wantLabels: `{__name__="control_billing_reconcile_discrepancies",kind="orphan_debit"}`,
			wantValue:  "2",
		},
		{
			name:  "billing_reconciliation_not_running_fires",
			claim: "距上次成功对账超过 1 小时即触发。这条不依赖 increase()，用的是 time() 减去一个时间戳 gauge",
			series: []inputSeries{
				{selector: `control_billing_reconcile_last_success_seconds`, values: `0+0x130`},
			},
			alert:      "BillingReconciliationNotRunning",
			evalTime:   "2h",
			wantLabels: `{}`,
			wantValue:  "7200",
		},
		{
			name:  "A27_reconciliation_not_running_is_silent_while_its_gauge_has_never_been_published",
			claim: "A27 的缺陷本体：对账的所有 gauge 都只在一次**成功**运行之后才写，在那之前序列不存在，time() 减一个缺失的序列得到空向量。于是这条看门狗在它唯一该管的状态——\"对账从来没跑起来过\"——上完全沉默。跑到 3 小时仍然无结果。ReconciliationLoop.PrimeMetrics 在启动时把它种上进程启动时刻，就是为了这个",
			series: []inputSeries{
				{selector: `control_billing_held_rows{usage_source="missing"}`, values: `0+0x200`},
			},
			alert:    "BillingReconciliationNotRunning",
			evalTime: "3h",
		},
		{
			name:  "A27_reconciliation_unbalanced_is_silent_while_its_gauge_has_never_been_published",
			claim: "同一形状，且这条是 critical。balanced 有意**不**预置：种 1 等于断言一本从没核过的账是平的，种 0 等于每次启动都报 critical。它的缺失由 BillingReconciliationNotRunning 的时间戳兜底——这个依赖已写进该规则的 threshold_source",
			series: []inputSeries{
				{selector: `control_billing_held_rows{usage_source="missing"}`, values: `0+0x200`},
			},
			alert:    "BillingReconciliationUnbalanced",
			evalTime: "3h",
		},
		{
			name:  "detail_write_failing_fires",
			claim: "明细持续写入失败时触发（for=10m，目标就是“持续”）",
			series: []inputSeries{
				{selector: `detail_flush_failures_total`, values: `0+1x25`},
			},
			alert:      "DetailWriteFailing",
			evalTime:   "20m",
			wantLabels: `{}`,
			wantValue:  "15",
		},
		{
			name:  "detail_buffer_saturated_fires",
			claim: "缓冲区满导致持续丢弃时触发，并与 write_failed 分开归因",
			series: []inputSeries{
				{selector: `detail_dropped_total{reason="buffer_full"}`, values: `0+1x25`},
			},
			alert:      "DetailBufferSaturated",
			evalTime:   "20m",
			wantLabels: `{reason="buffer_full"}`,
			wantValue:  "15",
		},
		{
			name:  "console_auth_rejections_fires",
			claim: "控制台持续收到未通过鉴权的请求时触发（阈值 >5），按 path 归因",
			series: []inputSeries{
				{selector: `control_console_rejected_total{path="/v1/billing/held"}`, values: `0+1x25`},
			},
			alert:      "ConsoleAuthRejections",
			evalTime:   "20m",
			wantLabels: `{path="/v1/billing/held"}`,
			wantValue:  "15",
		},
		{
			name:  "console_auth_rejections_silent_when_six_arrive_as_one_burst",
			claim: "同 UsageLedgerDuplicateSpike：6 次拒绝挤在一个突发里，阈值 >5 也救不回诞生增量。A22 判定可容忍的依据是“扫描/爆破是重复事件”，这条测量的是那个判定不成立时的代价",
			series: []inputSeries{
				{selector: `control_console_rejected_total{path="/v1/billing/held"}`, values: `6+0x30`},
			},
			alert:    "ConsoleAuthRejections",
			evalTime: "20m",
		},
		{
			name:  "console_wallet_topup_fires",
			claim: "运营充值可见（info）",
			series: []inputSeries{
				{selector: `control_console_topup_total`, values: `0+1x15`},
			},
			alert:      "ConsoleWalletTopUp",
			evalTime:   "10m",
			wantLabels: `{}`,
			wantValue:  "5",
		},
		{
			name:  "console_wallet_adjustment_fires",
			claim: "运营对钱包做显式调整可见——A15 之前这个入口根本不存在",
			series: []inputSeries{
				{selector: `control_console_adjustment_total`, values: `0+1x15`},
			},
			alert:      "ConsoleWalletAdjustment",
			evalTime:   "10m",
			wantLabels: `{}`,
			wantValue:  "5",
		},
		{
			name:  "console_wallet_id_reuse_fires",
			claim: "id 复用被拒可见（or 的 adjustment 分支）",
			series: []inputSeries{
				{selector: `control_console_adjustment_conflict_total`, values: `0+1x70`},
			},
			alert:      "ConsoleWalletIdReuse",
			evalTime:   "65m",
			wantLabels: `{}`,
			wantValue:  "60",
		},
		{
			name:  "console_wallet_replay_fires",
			claim: "钱包写入被作为重放返回可见。A15 的 F1 里，这个计数器是那条丢钱路径的唯一可观测信号",
			series: []inputSeries{
				{selector: `control_console_adjustment_replayed_total`, values: `0+1x70`},
			},
			alert:      "ConsoleWalletReplay",
			evalTime:   "65m",
			wantLabels: `{}`,
			wantValue:  "60",
		},
		{
			name:  "console_held_unpriced_resolution_fires",
			claim: "人工裁定落到 unpriced 可见——A22 为它预置了零序列",
			series: []inputSeries{
				{selector: `control_console_held_unpriced_total`, values: `0+1x70`},
			},
			alert:      "ConsoleHeldUnpricedResolution",
			evalTime:   "65m",
			wantLabels: `{}`,
			wantValue:  "60",
		},
		{
			name:  "egress_platform_transport_rejection_is_pending_not_firing_before_for_elapses",
			claim: "表达式从第一个样本起就为真，但 `for:` 还没走完，因此必须处于 pending 而不是 firing。这是本装置唯一覆盖 `for:` 的方向",
			series: []inputSeries{
				{selector: `control_platform_alert{provider="anthropic",reason="transport_rejections"}`, values: `1+0x20`},
			},
			alert:    "PlatformTransportRejection",
			evalTime: "3m",
		},
		{
			name:  "egress_transport_rejection_spread_is_pending_not_firing_before_for_elapses",
			claim: "表达式从第一个样本起就为真，但 `for:` 还没走完，因此必须处于 pending 而不是 firing。这是本装置唯一覆盖 `for:` 的方向",
			series: []inputSeries{
				{selector: `control_platform_transport_rejections_accounts{provider="anthropic"}`, values: `5+0x20`},
			},
			alert:    "PlatformTransportRejectionSpread",
			evalTime: "7m",
		},
		{
			name:  "stream_pending_backlog_is_pending_not_firing_before_for_elapses",
			claim: "表达式从第一个样本起就为真，但 `for:` 还没走完，因此必须处于 pending 而不是 firing。这是本装置唯一覆盖 `for:` 的方向",
			series: []inputSeries{
				{selector: `control_stream_pending`, values: `1500+0x20`},
			},
			alert:    "StreamPendingBacklog",
			evalTime: "7m",
		},
		{
			name:  "usage_trust_attempt_open_age_is_pending_not_firing_before_for_elapses",
			claim: "表达式从第一个样本起就为真，但 `for:` 还没走完，因此必须处于 pending 而不是 firing。这是本装置唯一覆盖 `for:` 的方向",
			series: []inputSeries{
				{selector: `request_attempt_open_age_seconds`, values: `1200+0x20`},
			},
			alert:    "AttemptOpenAgeHigh",
			evalTime: "3m",
		},
		{
			name:  "billing_held_rows_accumulating_is_pending_not_firing_before_for_elapses",
			claim: "表达式从第一个样本起就为真，但 `for:` 还没走完，因此必须处于 pending 而不是 firing。这是本装置唯一覆盖 `for:` 的方向",
			series: []inputSeries{
				{selector: `control_billing_held_rows{usage_source="missing"}`, values: `4+0x40`},
			},
			alert:    "BillingHeldRowsAccumulating",
			evalTime: "25m",
		},
		{
			name:  "billing_uncapped_wallet_tenants_is_pending_not_firing_before_for_elapses",
			claim: "表达式从第一个样本起就为真，但 `for:` 还没走完，因此必须处于 pending 而不是 firing。这是本装置唯一覆盖 `for:` 的方向",
			series: []inputSeries{
				{selector: `control_billing_uncapped_wallet_tenants`, values: `2+0x25`},
			},
			alert:    "BillingUncappedWalletTenants",
			evalTime: "12m",
		},
		{
			name:  "billing_blocked_tenants_is_pending_not_firing_before_for_elapses",
			claim: "表达式从第一个样本起就为真，但 `for:` 还没走完，因此必须处于 pending 而不是 firing。这是本装置唯一覆盖 `for:` 的方向",
			series: []inputSeries{
				{selector: `control_billing_blocked_tenants`, values: `3+0x40`},
			},
			alert:    "BillingBlockedTenantsSpike",
			evalTime: "25m",
		},
		{
			name:  "billing_reconciliation_unbalanced_is_pending_not_firing_before_for_elapses",
			claim: "表达式从第一个样本起就为真，但 `for:` 还没走完，因此必须处于 pending 而不是 firing。这是本装置唯一覆盖 `for:` 的方向",
			series: []inputSeries{
				{selector: `control_billing_reconcile_balanced`, values: `0+0x40`},
			},
			alert:    "BillingReconciliationUnbalanced",
			evalTime: "25m",
		},
		{
			name:  "billing_reconciliation_discrepancy_kind_is_pending_not_firing_before_for_elapses",
			claim: "表达式从第一个样本起就为真，但 `for:` 还没走完，因此必须处于 pending 而不是 firing。这是本装置唯一覆盖 `for:` 的方向",
			series: []inputSeries{
				{selector: `control_billing_reconcile_discrepancies{kind="orphan_debit"}`, values: `2+0x40`},
			},
			alert:    "BillingReconciliationDiscrepancyKind",
			evalTime: "25m",
		},
	}
}

func TestAlertRulesActuallyFireUnderPromtool(t *testing.T) {
	run := promtoolRunner(t)

	exprByAlert := make(map[string]string)
	for _, group := range loadRules(t).Groups {
		for _, rule := range group.Rules {
			exprByAlert[rule.Alert] = strings.TrimSpace(rule.Expr)
		}
	}

	for _, sc := range promtoolScenarios() {
		expr, ok := exprByAlert[sc.alert]
		if !ok {
			t.Errorf("%s: no alert named %q in the rules file", sc.name, sc.alert)
			continue
		}
		t.Run(sc.name, func(t *testing.T) {
			t.Logf("claim: %s", sc.claim)
			if output, err := run(buildPromtoolTest(sc, expr)); err != nil {
				t.Errorf("promtool rejected the claim:\n%s", output)
			}
		})
	}
}

// buildPromtoolTest renders one scenario. Positive cases assert the expression
// read from the rules file; negative cases assert the whole rule produces no
// alert, which needs no annotations and so stays readable.
func buildPromtoolTest(sc scenario, expr string) string {
	var b strings.Builder
	b.WriteString("rule_files:\n  - gateway-platform.rules.yml\nevaluation_interval: 1m\ntests:\n")
	b.WriteString("  - interval: 1m\n    input_series:\n")
	for _, series := range sc.series {
		b.WriteString("      - series: '" + series.selector + "'\n")
		b.WriteString("        values: '" + series.values + "'\n")
	}
	if sc.wantValue == "" {
		b.WriteString("    alert_rule_test:\n")
		b.WriteString("      - eval_time: " + sc.evalTime + "\n")
		b.WriteString("        alertname: " + sc.alert + "\n")
		b.WriteString("        exp_alerts: []\n")
		return b.String()
	}
	b.WriteString("    promql_expr_test:\n")
	b.WriteString("      - expr: " + strconv_Quote(expr) + "\n")
	b.WriteString("        eval_time: " + sc.evalTime + "\n")
	b.WriteString("        exp_samples:\n")
	b.WriteString("          - labels: '" + sc.wantLabels + "'\n")
	b.WriteString("            value: " + sc.wantValue + "\n")
	return b.String()
}

// strconv_Quote is strconv.Quote; named locally so the YAML-quoting intent is
// obvious at the call site -- the expressions contain double quotes and pipes.
func strconv_Quote(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", " ").Replace(s) + `"`
}

// promtoolRunner returns a function that writes the generated fixture next to
// the rules file and runs promtool over it.
//
// The fixture goes into configs/alerts/ rather than t.TempDir() because
// promtool resolves rule_files relative to the test file, and because a
// container has to be able to mount whatever directory it lives in. It is
// removed again on cleanup.
func promtoolRunner(t *testing.T) func(fixture string) (string, error) {
	t.Helper()
	rulesDir, err := filepath.Abs(filepath.Dir(rulesPath))
	if err != nil {
		t.Fatal(err)
	}

	var command func(containerPath, hostPath string) *exec.Cmd
	switch image := os.Getenv(promtoolImageEnv); {
	case image != "":
		command = func(containerPath, _ string) *exec.Cmd {
			return exec.Command("docker", "run", "--rm", "--entrypoint", "promtool",
				"-v", rulesDir+":/rules:ro", image, "test", "rules", "/rules/"+containerPath)
		}
	default:
		if _, err := exec.LookPath("promtool"); err != nil {
			t.Skipf("promtool is not on PATH and %s is not set, so 'the rule actually fires' is NOT verified in this run. "+
				"Run it with: %s=prom/prometheus:latest go test ./internal/observability/",
				promtoolImageEnv, promtoolImageEnv)
		}
		command = func(_, hostPath string) *exec.Cmd {
			return exec.Command("promtool", "test", "rules", hostPath)
		}
	}

	return func(fixture string) (string, error) {
		name := "promtool_generated_test.yml"
		hostPath := filepath.Join(rulesDir, name)
		if err := os.WriteFile(hostPath, []byte(fixture), 0o644); err != nil {
			t.Fatal(err)
		}
		defer os.Remove(hostPath)
		output, err := command(name, hostPath).CombinedOutput()
		return string(output) + "\n--- fixture ---\n" + fixture, err
	}
}

// TestEveryAlertHasAPromtoolScenario is the A24 lesson applied to A25: a check
// that covers "the rules we happened to touch" is a check whose scope someone
// has to remember to extend. A24 found that the metric-consumer check had been
// scoped to two families and the whole gateway role had fallen outside it. The
// same shape of mistake here would be a new alert shipping with no evidence it
// can ever fire.
//
// This test needs no promtool and therefore runs everywhere, which is the
// point: the gate on a new rule must not itself be skippable.
func TestEveryAlertHasAPromtoolScenario(t *testing.T) {
	covered := make(map[string]int)
	for _, sc := range promtoolScenarios() {
		covered[sc.alert]++
	}
	for _, group := range loadRules(t).Groups {
		for _, rule := range group.Rules {
			if covered[rule.Alert] == 0 {
				t.Errorf("alert %s has no promtool scenario: nothing shows it can fire. Add one to promtoolScenarios().", rule.Alert)
			}
		}
	}
	// And the reverse: a scenario naming an alert that no longer exists would
	// pass forever without testing anything.
	declared := make(map[string]struct{})
	for _, group := range loadRules(t).Groups {
		for _, rule := range group.Rules {
			declared[rule.Alert] = struct{}{}
		}
	}
	for alert := range covered {
		if _, ok := declared[alert]; !ok {
			t.Errorf("a promtool scenario names alert %q, which the rules file does not define", alert)
		}
	}
}

// TestEveryNegativeScenarioHasAPositiveTwin guards the one way a negative
// scenario can pass while proving nothing: if its input series never matched
// the rule's selector -- a typo in the metric name, say -- the rule would be
// silent for the wrong reason and exp_alerts: [] would still be satisfied.
// Requiring a positive scenario on the same alert means the metric name in the
// fixture is known to reach the rule.
func TestEveryNegativeScenarioHasAPositiveTwin(t *testing.T) {
	positives := make(map[string]bool)
	for _, sc := range promtoolScenarios() {
		if sc.wantValue != "" {
			positives[sc.alert] = true
		}
	}
	for _, sc := range promtoolScenarios() {
		if sc.wantValue == "" && !positives[sc.alert] {
			t.Errorf("%s asserts %s stays silent but no positive scenario shows that alert firing on the same series, so the silence may be a typo rather than the rule",
				sc.name, sc.alert)
		}
	}
}

// seriesEnd returns the minute of the last sample a promtool `values:`
// expression produces, given the 1m interval every scenario here uses.
// "0+1x20" is a first sample plus twenty more, so the last one is at minute 20.
func seriesEnd(values string) int {
	last := 0
	for _, part := range strings.Fields(values) {
		last++ // the part's own first sample
		if index := strings.LastIndex(part, "x"); index >= 0 {
			repeat, err := strconv.Atoi(part[index+1:])
			if err == nil {
				last += repeat
			}
		}
	}
	return last - 1
}

// parseRuleDuration accepts the subset of Prometheus duration syntax this rules
// file uses (m and h). Deliberately time.ParseDuration rather than
// prometheus/common/model: the units beyond hours are not used here, and a
// whole Prometheus dependency for one parser is not worth it. A rule written
// with "1d" would fail loudly here rather than be misread.
func parseRuleDuration(t *testing.T, value string) time.Duration {
	t.Helper()
	duration, err := time.ParseDuration(value)
	if err != nil {
		t.Fatalf("duration %q is not one this test can parse (only m and h are used in this rules file): %v", value, err)
	}
	return duration
}

func evalMinutes(t *testing.T, evalTime string) int {
	t.Helper()
	return int(parseRuleDuration(t, evalTime).Minutes())
}

// TestScenarioEvalTimesStayInsideTheirSeries guards a way a negative scenario
// passes while proving nothing, found the hard way: promtool marks a series
// stale a few minutes after its last sample, so evaluating past the end of
// `values:` makes the rule silent because the data ran out, not because the
// rule is silent. `exp_alerts: []` is satisfied either way.
//
// The first attempt at proving the `for:` scenarios were live did exactly this
// -- pushed eval_time to 30m against a series ending at 20m, saw green, and
// nearly recorded a dead assertion as a live one.
func TestScenarioEvalTimesStayInsideTheirSeries(t *testing.T) {
	for _, sc := range promtoolScenarios() {
		eval := evalMinutes(t, sc.evalTime)
		for _, series := range sc.series {
			if end := seriesEnd(series.values); eval > end {
				t.Errorf("%s evaluates at %dm but %s ends at %dm: the rule would be silent because the data ran out",
					sc.name, eval, series.selector, end)
			}
		}
	}
}

// TestPendingScenariosEvaluateBeforeTheirForDuration ties the "pending, not
// firing" scenarios to the rule file. Their whole claim is that eval_time falls
// inside the rule's `for:` window; if someone shortened the `for:` the scenario
// would silently become a plain firing test that happens to pass.
func TestPendingScenariosEvaluateBeforeTheirForDuration(t *testing.T) {
	forByAlert := make(map[string]string)
	for _, group := range loadRules(t).Groups {
		for _, rule := range group.Rules {
			forByAlert[rule.Alert] = rule.For
		}
	}
	found := 0
	for _, sc := range promtoolScenarios() {
		if !strings.Contains(sc.name, "_is_pending_") {
			continue
		}
		found++
		held := parseRuleDuration(t, forByAlert[sc.alert])
		if eval := evalMinutes(t, sc.evalTime); eval >= int(held.Minutes()) {
			t.Errorf("%s evaluates at %dm but %s only holds for %s: this is no longer a pending test",
				sc.name, eval, sc.alert, held)
		}
	}
	if found == 0 {
		t.Error("no pending scenarios found; the for: direction is not covered at all")
	}
}

// knownForDurationsWithoutAPendingScenario records the rules whose `for:` has
// no "pending, not firing" scenario, with the reason. The same shape as
// knownUnconsumedMetrics: the gap is allowed, skipping the decision is not.
var knownForDurationsWithoutAPendingScenario = map[string]string{
	"UsageSourceMissingRatioHigh":     "表达式基于 rate()，为真的起点取决于两条序列的累积速度而非规则本身；钉死一个 eval_time 等于在测 fixture",
	"BillingReconciliationNotRunning": "表达式是 time() 减时间戳，为真的时刻由阈值 3600s 决定而非序列；pending 窗口落在 1h 之后，场景成本远高于收益",
	"DetailWriteFailing":              "increase() 需要窗口累积才为真，起点取决于序列形状",
	"DetailBufferSaturated":           "同 DetailWriteFailing",
	"ConsoleAuthRejections":           "increase() 且阈值为 >5，为真的起点取决于序列形状",
	"QuotaSnapshotNotDraining":        "increase() 需要窗口累积才为真，起点取决于序列形状",
	"StreamDLQGrowing":                "for: 0m，没有可测的 pending 窗口",
}

// TestEveryForDurationIsCoveredOrExcused is the A24/A26 pattern applied to the
// `for:` direction: a rule that waits before firing must either have a scenario
// showing it waits, or a written reason why not.
func TestEveryForDurationIsCoveredOrExcused(t *testing.T) {
	pendingFor := make(map[string]bool)
	for _, sc := range promtoolScenarios() {
		if strings.Contains(sc.name, "_is_pending_") {
			pendingFor[sc.alert] = true
		}
	}
	declared := make(map[string]struct{})
	for _, group := range loadRules(t).Groups {
		for _, rule := range group.Rules {
			declared[rule.Alert] = struct{}{}
			if parseRuleDuration(t, rule.For) == 0 {
				continue
			}
			if pendingFor[rule.Alert] {
				if reason, listed := knownForDurationsWithoutAPendingScenario[rule.Alert]; listed {
					t.Errorf("%s now has a pending scenario, so remove it from knownForDurationsWithoutAPendingScenario (listed reason: %s)", rule.Alert, reason)
				}
				continue
			}
			if reason, ok := knownForDurationsWithoutAPendingScenario[rule.Alert]; ok {
				if strings.TrimSpace(reason) == "" {
					t.Errorf("%s is excused with an empty reason, which is the same as not deciding", rule.Alert)
				}
				continue
			}
			t.Errorf("%s waits %s before firing but nothing shows it waits. Add a pending scenario, or excuse it in knownForDurationsWithoutAPendingScenario.", rule.Alert, rule.For)
		}
	}
	for alert := range knownForDurationsWithoutAPendingScenario {
		if _, ok := declared[alert]; !ok {
			t.Errorf("knownForDurationsWithoutAPendingScenario lists %q, which the rules file does not define", alert)
		}
	}
}

// TestEveryAnnotationLabelReferenceIsProducedByTheAlert closes the annotation
// half. promtool compares annotations by exact equality, so asserting rendered
// text would mean copying multi-paragraph descriptions into fixtures and
// breaking on every wording edit. The defect worth catching is narrower and
// checkable without promtool: an annotation that interpolates a label the alert
// does not carry renders as an empty string, so the summary silently loses the
// one piece of information an operator needs to know which provider, tenant or
// reason is affected.
//
// The label set comes from the measured output of the positive scenarios, so
// this also catches the inverse mistake -- a fixture that invents a label the
// emitter never sets. That is not hypothetical: the first version of the two
// platform scenarios used `platform`, while health.go emits `provider`, and
// promql_expr_test happily compared the fixture against itself.
func TestEveryAnnotationLabelReferenceIsProducedByTheAlert(t *testing.T) {
	produced := make(map[string]map[string]bool)
	for _, sc := range promtoolScenarios() {
		if sc.wantLabels == "" {
			continue
		}
		set := produced[sc.alert]
		if set == nil {
			set = make(map[string]bool)
			produced[sc.alert] = set
		}
		for _, pair := range labelNamePattern.FindAllStringSubmatch(sc.wantLabels, -1) {
			set[pair[1]] = true
		}
	}
	for _, group := range loadRules(t).Groups {
		for _, rule := range group.Rules {
			for key, annotation := range rule.Annotations {
				for _, ref := range labelRefPattern.FindAllStringSubmatch(annotation, -1) {
					name := ref[1]
					if rule.Labels[name] != "" || name == "alertname" {
						continue
					}
					if produced[rule.Alert][name] {
						continue
					}
					t.Errorf("%s annotation %q interpolates $labels.%s, but no scenario shows that label on the alert (it would render empty). Known scenario labels: %v",
						rule.Alert, key, name, produced[rule.Alert])
				}
			}
		}
	}
}

var (
	labelRefPattern  = regexp.MustCompile(`\$labels\.([A-Za-z_][A-Za-z0-9_]*)`)
	labelNamePattern = regexp.MustCompile(`([A-Za-z_][A-Za-z0-9_]*)=`)
)
