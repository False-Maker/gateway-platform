package kiro

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/elucid/gateway-platform/pkg/contracts"
)

func TestQuotaFetchesUsageWithProfileMetadata(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/getUsageLimits" || r.URL.Query().Get("profileArn") != "arn:test" || r.Header.Get("Authorization") != "Bearer token" {
			t.Fatalf("request=%s %s headers=%#v", r.Method, r.URL, r.Header)
		}
		_, _ = io.WriteString(w, `{"usageBreakdownList":[{"type":"CREDIT","currentUsagePrecise":0.25,"usageLimitPrecise":1}]}`)
	}))
	defer server.Close()
	p := NewOAuth(server.Client())
	p.Config.APIBaseURL = server.URL
	quota, err := p.Quota(context.Background(), contracts.Credential{AccessToken: "token", Metadata: map[string]string{"profile_arn": "arn:test"}})
	if err != nil || len(quota.Items) != 1 || string(*quota.Items[0].Precision.Remaining) != "0.75" {
		t.Fatalf("quota=%#v err=%v", quota, err)
	}
}

func TestParseUsageLimitsQuotaPreservesKiroPrecision(t *testing.T) {
	quota, err := ParseUsageLimitsQuota([]byte(`{"usageBreakdownList":[{"type":"CREDIT","currentUsagePrecise":2028.68,"usageLimitPrecise":10000,"overageRate":0.01,"displayName":"Credit","resetDate":"2026-09-01T12:00:00Z"}]}`))
	if err != nil || len(quota.Items) != 1 {
		t.Fatalf("quota=%#v err=%v", quota, err)
	}
	item := quota.Items[0]
	if item.Precision == nil || string(*item.Precision.Used) != "2028.68" || string(*item.Precision.Limit) != "10000" || string(*item.Precision.Remaining) != "7971.32" || string(*item.Precision.OverageRate) != "0.01" {
		t.Fatalf("item=%#v", item)
	}
}

func TestParseUsageLimitsQuotaUnknownShapeIsMissing(t *testing.T) {
	quota, err := ParseUsageLimitsQuota([]byte(`{"usageBreakdownList":[{"type":"CREDIT","currentUsage":2,"usageLimit":10}]}`))
	if err != nil || quota.Items != nil {
		t.Fatalf("quota=%#v err=%v", quota, err)
	}
}
