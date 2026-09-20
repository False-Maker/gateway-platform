package observability_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// E1 delivers alert rules for metrics this repository emits. The failure mode
// that matters is not a YAML typo -- promtool catches those -- it is a rule
// that alerts on a metric name nobody actually publishes, which looks healthy
// forever because it can never fire. These tests are the guard against that,
// and unlike promtool they need no Docker.

const rulesPath = "../../configs/alerts/gateway-platform.rules.yml"

type ruleFile struct {
	Groups []struct {
		Name  string `yaml:"name"`
		Rules []struct {
			Alert       string            `yaml:"alert"`
			Expr        string            `yaml:"expr"`
			For         string            `yaml:"for"`
			Labels      map[string]string `yaml:"labels"`
			Annotations map[string]string `yaml:"annotations"`
		} `yaml:"rules"`
	} `yaml:"groups"`
}

func loadRules(t *testing.T) ruleFile {
	t.Helper()
	raw, err := os.ReadFile(rulesPath)
	if err != nil {
		t.Fatal(err)
	}
	var parsed ruleFile
	if err := yaml.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("alert rules are not valid YAML: %v", err)
	}
	if len(parsed.Groups) == 0 {
		t.Fatal("alert rules file has no groups")
	}
	return parsed
}

// emittedMetrics scans the repository for metric names the code actually
// publishes. It reads the source rather than a hand-kept list so the two cannot
// drift apart silently.
func emittedMetrics(t *testing.T) map[string]struct{} {
	t.Helper()
	pattern := regexp.MustCompile(`(?:AddCounter|SetGauge)\("([a-z][a-z0-9_]*)"`)
	found := make(map[string]struct{})
	for _, root := range []string{"../../internal", "../../cmd", "../../pkg"} {
		err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return err
			}
			source, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			for _, match := range pattern.FindAllSubmatch(source, -1) {
				found[string(match[1])] = struct{}{}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(found) == 0 {
		t.Fatal("scanned the repository and found no metric emitters; the scanner is broken")
	}
	return found
}

// metricNamespaces are the prefixes this platform's metrics use. Identifiers in
// a PromQL expression that start with one of these are metric references;
// anything else is a function, keyword or label and is not our concern.
var metricNamespaces = []string{"control_", "gateway_", "release_", "usage_", "request_", "detail_", "quota_"}

var identifier = regexp.MustCompile(`[a-z][a-z0-9_]*`)

func referencedMetrics(expr string) []string {
	var names []string
	for _, candidate := range identifier.FindAllString(expr, -1) {
		for _, namespace := range metricNamespaces {
			if strings.HasPrefix(candidate, namespace) {
				names = append(names, candidate)
				break
			}
		}
	}
	return names
}

// The load-bearing test: a rule that watches a metric nothing publishes is
// worse than no rule, because it reads as coverage and can never fire.
func TestAlertRulesOnlyReferenceEmittedMetrics(t *testing.T) {
	rules := loadRules(t)
	emitted := emittedMetrics(t)
	for _, group := range rules.Groups {
		for _, rule := range group.Rules {
			for _, metric := range referencedMetrics(rule.Expr) {
				if _, ok := emitted[metric]; !ok {
					t.Errorf("alert %s watches %q, which no code in this repository emits: the rule can never fire",
						rule.Alert, metric)
				}
			}
		}
	}
}

// E1's DoD: every rule must state where its threshold came from, and thresholds
// that were never validated against real traffic must say so rather than
// reading as verified values.
func TestEveryRuleDocumentsItsThreshold(t *testing.T) {
	rules := loadRules(t)
	seen := make(map[string]string)
	for _, group := range rules.Groups {
		for _, rule := range group.Rules {
			if rule.Alert == "" || rule.Expr == "" {
				t.Errorf("group %s has a rule missing alert or expr", group.Name)
				continue
			}
			if previous, ok := seen[rule.Alert]; ok {
				t.Errorf("alert %s is defined twice (%s and %s)", rule.Alert, previous, group.Name)
			}
			seen[rule.Alert] = group.Name

			if rule.Labels["severity"] == "" {
				t.Errorf("alert %s has no severity", rule.Alert)
			}
			if rule.Annotations["summary"] == "" {
				t.Errorf("alert %s has no summary", rule.Alert)
			}
			source := rule.Annotations["threshold_source"]
			if source == "" {
				t.Errorf("alert %s does not say where its threshold came from", rule.Alert)
				continue
			}
			// A threshold is either derived from a stated consequence, or it is
			// a guess -- and a guess must be labelled, because 总览 §6.3.1 has
			// already declared the cooldowns pending real traffic.
			if !strings.Contains(source, "uncalibrated") && !strings.Contains(source, "推出") {
				t.Errorf("alert %s's threshold_source neither marks it uncalibrated nor derives it: %q",
					rule.Alert, source)
			}
		}
	}
}

// moneyMetricPrefixes are the metric families where "emitted but nobody reads
// it" is a real operational hole rather than a dashboard nicety: they describe
// money moving and operators moving it.
var moneyMetricPrefixes = []string{"control_billing_", "control_console_"}

// knownUnconsumedMoneyMetrics are the money-family metrics that deliberately
// have no alert rule, each with the reason. Adding a metric here is a decision;
// the point of the test below is that it cannot happen by accident.
var knownUnconsumedMoneyMetrics = map[string]string{
	"control_billing_reconcile_runs_total":     "对账是否在跑由 control_billing_reconcile_last_success_seconds 告警（BillingReconciliationNotRunning），这个计数器是看板用量",
	"control_billing_reconcile_unsettled_rows": "未结算行按状态分别告警（held / unpriced 两条规则），这是聚合值，看板用",
	"control_console_held_resolved_total":      "裁定本身是正常运营动作；有收入后果的那一半由 ConsoleHeldUnpricedResolution 覆盖",
	"control_console_price_total":              "录入单价是配置动作，只向未来生效且不可改价，无失败后果需要告警",
	"control_console_throttled_total":          "B5.1 的控制台限流；被限流是设计内行为，是否值得告警**尚未决定**——记在此处而不是假装它有消费方",
}

// TestMoneyMetricsHaveAConsumer is the automatic half of the lesson A16 was
// supposed to teach.
//
// TestAlertRulesOnlyReferenceEmittedMetrics checks rules against metrics. It
// cannot see the opposite hole -- a metric nobody reads -- and A16 closed one
// instance of that hole by adding a hand-written entry to the table below.
// A hand-written table has no automation, and the very next commit proved it:
// A15 shipped control_console_adjustment_total and
// control_console_adjustment_replayed_total with no consumer at all, and the
// replayed counter was the only observable signal of a wallet correction being
// silently dropped. This test makes the check structural for the metric
// families where it matters, so the next one cannot slip through unnoticed.
func TestMoneyMetricsHaveAConsumer(t *testing.T) {
	rules := loadRules(t)
	referenced := make(map[string]struct{})
	for _, group := range rules.Groups {
		for _, rule := range group.Rules {
			for _, metric := range referencedMetrics(rule.Expr) {
				referenced[metric] = struct{}{}
			}
		}
	}
	for metric := range emittedMetrics(t) {
		inFamily := false
		for _, prefix := range moneyMetricPrefixes {
			if strings.HasPrefix(metric, prefix) {
				inFamily = true
				break
			}
		}
		if !inFamily {
			continue
		}
		if _, ok := referenced[metric]; ok {
			if reason, listed := knownUnconsumedMoneyMetrics[metric]; listed {
				t.Errorf("%s now has an alert rule, so remove it from knownUnconsumedMoneyMetrics (listed reason: %s)",
					metric, reason)
			}
			continue
		}
		if _, ok := knownUnconsumedMoneyMetrics[metric]; ok {
			continue
		}
		t.Errorf("%s is emitted but no alert rule reads it. Either add a rule, or add it to knownUnconsumedMoneyMetrics with the reason it does not need one.",
			metric)
	}
}

// E1's DoD names five areas the rules must cover. This pins them so a later
// edit cannot quietly drop one.
func TestRequiredCoverageIsPresent(t *testing.T) {
	rules := loadRules(t)
	var expressions []string
	for _, group := range rules.Groups {
		for _, rule := range group.Rules {
			expressions = append(expressions, rule.Expr)
		}
	}
	joined := strings.Join(expressions, "\n")
	required := map[string]string{
		"平台级 transport 拒绝":       "control_platform_transport_rejections_accounts",
		"DLQ 增长":                 "control_stream_dlq_total",
		"stream pending 积压":      "control_stream_pending",
		"UsageSource=missing 占比": "release_usage_missing_total",
		"防饿死闸触发":                 "control_platform_starvation_release_total",
		// A16: B4-BILLING-MODEL D6 requires "出现 unpriced 行即告警". The metric
		// was emitted from the day B4.2 shipped and no rule ever read it, which
		// is the exact hole TestAlertRulesOnlyReferenceEmittedMetrics cannot
		// see: that test checks rules against metrics, never metrics against
		// rules. Pinned here so it cannot go unconsumed again.
		"扣费作业判出的 unpriced 行": "control_billing_rows_total",
	}
	for area, metric := range required {
		if !strings.Contains(joined, metric) {
			t.Errorf("E1 DoD requires coverage of %s, but no rule references %s", area, metric)
		}
	}
}
