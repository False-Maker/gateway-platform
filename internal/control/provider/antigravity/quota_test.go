package antigravity

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/elucid/gateway-platform/pkg/contracts"
)

func TestQuotaFetchesModelsWithProjectMetadata(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1internal:fetchAvailableModels" || r.Header.Get("Authorization") != "Bearer token" {
			t.Fatalf("request=%s %s headers=%#v", r.Method, r.URL, r.Header)
		}
		body, _ := io.ReadAll(r.Body)
		if string(body) != `{"project":"project-1"}` {
			t.Fatalf("body=%s", body)
		}
		_, _ = io.WriteString(w, `{"models":{"gemini":{"quotaInfo":{"remainingFraction":0.25}}}}`)
	}))
	defer server.Close()
	p := NewOAuth(server.Client())
	p.Config.APIBaseURL = server.URL
	quota, err := p.Quota(context.Background(), contracts.Credential{AccessToken: "token", Metadata: map[string]string{"project_id": "project-1"}})
	if err != nil || len(quota.Items) != 1 || string(*quota.Items[0].RemainingFraction) != "0.25" {
		t.Fatalf("quota=%#v err=%v", quota, err)
	}
}

func TestParseAvailableModelsQuotaPreservesFractionAndMissing(t *testing.T) {
	quota, err := ParseAvailableModelsQuota([]byte(`{"models":{"gemini-3-flash":{"quotaInfo":{"remainingFraction":0.99833333,"resetTime":"2026-09-01T12:00:00Z"}},"internal":{"quotaInfo":{}},"unknown":{}}}`))
	if err != nil || len(quota.Items) != 1 {
		t.Fatalf("quota=%#v err=%v", quota, err)
	}
	item := quota.Items[0]
	if string(*item.RemainingFraction) != "0.99833333" || item.Unit != "fraction" || item.ResetAt == nil || !item.ResetAt.Equal(time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)) {
		t.Fatalf("item=%#v", item)
	}
}

func TestParseAvailableModelsQuotaUnknownEnvelopeIsMissing(t *testing.T) {
	quota, err := ParseAvailableModelsQuota([]byte(`{"models":{"gemini":{"quotaInfo":{"resetTime":"2026-09-01T12:00:00Z"}}}}`))
	if err != nil || quota.Items != nil {
		t.Fatalf("quota=%#v err=%v", quota, err)
	}
}
