package observability_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
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
