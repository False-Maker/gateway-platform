package newapi

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestBuildPlanMapsProvidersGroupsKeysAndPreservesProxy(t *testing.T) {
	snapshot := SourceSnapshot{
		CapturedAt: time.Date(2026, 9, 4, 1, 2, 3, 0, time.UTC),
		Channels: []SourceChannel{
			{ID: 7, Type: 57, Key: `[{"access_token":"access-1","refresh_token":"refresh-1","id_token":"id-1","account_id":"acct-1","email":"one@example.test","expired":"2027-01-01T00:00:00Z"},{"access_token":"access-2","refresh_token":"refresh-2"}]`, Status: 1, Group: "primary, backup", Setting: `{"proxy":"http://proxy.example.test:8080"}`, Models: `gpt-5,gpt-5-mini`, ChannelInfo: `{"multi_key_status_list":{"1":2}}`},
			{ID: 8, Type: 14, Key: "claude-key", Status: 1, Group: "default"},
			{ID: 9, Type: 24, Key: `[` + `"gemini-key"` + `]`, Status: 1},
			{ID: 10, Type: 48, Key: "grok-key", Status: 2, Group: "x"},
		},
	}
	plan, err := BuildPlan(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Accounts) != 7 {
		t.Fatalf("accounts = %d, want 7", len(plan.Accounts))
	}
	if plan.Summary.ProviderCounts["codex"] != 4 || plan.Summary.ProviderCounts["claude"] != 1 || plan.Summary.ProviderCounts["gemini"] != 1 {
		t.Fatalf("provider counts = %#v", plan.Summary.ProviderCounts)
	}
	if plan.Summary.StatusCounts["disabled"] != 3 {
		t.Fatalf("status counts = %#v", plan.Summary.StatusCounts)
	}
	var sawProxy, sawDisabledKey bool
	for _, account := range plan.Accounts {
		if account.Proxy == "http://proxy.example.test:8080" {
			sawProxy = true
		}
		if account.Account.Provider == "codex" && strings.HasSuffix(account.SourceID, "#key:1#group:primary") {
			sawDisabledKey = account.Account.Status == "disabled"
			if account.Bundle.RefreshToken != "refresh-2" {
				t.Fatalf("second codex bundle lost refresh token")
			}
		}
	}
	if !sawProxy || !sawDisabledKey {
		t.Fatalf("proxy=%v disabled_key=%v", sawProxy, sawDisabledKey)
	}
}

func TestBuildPlanRejectsUnknownAndMalformedWithoutGuessing(t *testing.T) {
	plan, err := BuildPlan(SourceSnapshot{Channels: []SourceChannel{
		{ID: 1, Type: 1, Key: "openai-key", Status: 1},
		{ID: 2, Type: 57, Key: "not-json", Status: 1},
		{ID: 3, Type: 14, Key: "claude-key", Status: 99},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Accounts) != 0 || plan.Summary.RejectedCount != 3 {
		t.Fatalf("accounts=%d rejected=%d", len(plan.Accounts), plan.Summary.RejectedCount)
	}
	if plan.Summary.UnknownChannelTypes["1"] != 1 {
		t.Fatalf("unknown type summary = %#v", plan.Summary.UnknownChannelTypes)
	}
	for _, record := range plan.Records {
		encoded, _ := json.Marshal(record)
		if strings.Contains(string(encoded), "openai-key") || strings.Contains(string(encoded), "not-json") {
			t.Fatalf("record leaked key: %s", encoded)
		}
	}
}

func TestBuildPlanStagesUsersTokensQuotaWithoutSecrets(t *testing.T) {
	plan, err := BuildPlan(SourceSnapshot{
		Users:  []SourceUser{{ID: 3, Username: "alice", Email: "alice@example.test", Status: 1, Quota: "1000", UsedQuota: "12", Group: "default"}},
		Tokens: []SourceToken{{ID: 4, UserID: 3, Status: 2, ExpiredTime: "-1", RemainQuota: "77", Key: "sk-secret"}},
		Quota:  []SourceQuota{{ID: 5, UserID: 3, Model: "gpt-5", TokenUsed: "9", Count: "2", Quota: "123", Group: "default"}},
		Groups: []SourceGroup{{ID: "default", Name: "default", Status: "active"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Summary.QuotaRows != 1 || plan.Summary.QuotaRawDigest == "" {
		t.Fatalf("quota summary = %#v", plan.Summary)
	}
	encoded, _ := json.Marshal(plan.Records)
	if strings.Contains(string(encoded), "sk-secret") {
		t.Fatalf("token secret leaked into staging record")
	}
	if !strings.Contains(string(encoded), "key_sha256") {
		t.Fatalf("token digest missing")
	}
}

func TestBuildPlanMarksInvalidModelDataForReview(t *testing.T) {
	plan, err := BuildPlan(SourceSnapshot{Channels: []SourceChannel{{ID: 1, Type: 14, Key: "key", Status: 1, Models: "{"}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Accounts) != 1 || plan.Accounts[0].Conversion["manual_review"] != true {
		t.Fatalf("plan = %#v", plan.Accounts)
	}
}

func TestBuildPlanReportsDuplicateSourceKeys(t *testing.T) {
	plan, err := BuildPlan(SourceSnapshot{Channels: []SourceChannel{
		{ID: 1, Type: 14, Key: "same-key", Status: 1},
		{ID: 2, Type: 14, Key: "same-key", Status: 1},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Summary.DuplicateSourceKeys != 1 {
		t.Fatalf("duplicate source keys = %d", plan.Summary.DuplicateSourceKeys)
	}
}
