package newapi

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	authsnapshot "github.com/elucid/gateway-platform/internal/snapshot"
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
	// A11: the planned tenant/token entities are a second path the plaintext
	// must not reach, and the auth hash must stay out of the records too.
	planned, _ := json.Marshal(struct {
		Tenants []PlannedTenant
		Tokens  []PlannedTenantToken
	}{plan.Tenants, plan.TenantTokens})
	if strings.Contains(string(planned), "sk-secret") {
		t.Fatalf("token secret leaked into planned tenant tokens")
	}
	if len(plan.TenantTokens) != 1 {
		t.Fatalf("got %d planned tenant tokens, want 1", len(plan.TenantTokens))
	}
	if strings.Contains(string(encoded), plan.TenantTokens[0].TokenHash) {
		t.Fatalf("auth token hash leaked into migration records")
	}
}

func TestBuildPlanPromotesUsersAndTokensToTenantsPrincipalsAndTokens(t *testing.T) {
	plan, err := BuildPlan(SourceSnapshot{
		Users: []SourceUser{
			{ID: 3, Username: "alice", Status: 1, Group: "vip"},
			{ID: 4, Username: "bob", Status: 2},
		},
		Tokens: []SourceToken{
			{ID: 11, UserID: 3, Status: 1, ExpiredTime: "-1", Key: "barekey"}, // never expires, group from user
			{ID: 12, UserID: 3, Status: 1, ExpiredTime: "1800000000", Key: "sk-prefixed", Group: "team"},
			{ID: 13, UserID: 4, Status: 4, ExpiredTime: "0", Key: "exhausted"}, // exhausted => revoked
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Summary.TenantCount != 2 || plan.Summary.PrincipalCount != 2 || plan.Summary.TenantTokenCount != 3 || plan.Summary.RejectedCount != 0 {
		t.Fatalf("summary = %+v", plan.Summary)
	}
	if len(plan.Tenants) != 2 || len(plan.TenantTokens) != 3 {
		t.Fatalf("planned %d tenants / %d tokens", len(plan.Tenants), len(plan.TenantTokens))
	}

	alice, bob := plan.Tenants[0], plan.Tenants[1]
	if alice.Status != "active" || bob.Status != "suspended" {
		t.Errorf("user status mapping: alice=%q bob=%q", alice.Status, bob.Status)
	}
	// Ids are hashed, not "tenant-<user id>", so they cannot collide with an
	// operator-chosen id from `gwd tenant create`.
	if alice.TenantID == "tenant-3" || !strings.HasPrefix(alice.TenantID, "tenant-") || len(alice.TenantID) != len("tenant-")+32 {
		t.Errorf("tenant id = %q", alice.TenantID)
	}
	if alice.TenantID != importedTenantID(SourceSystem, "user:3") || alice.PrincipalID != importedPrincipalID(SourceSystem, "user:3") {
		t.Error("tenant/principal ids are not deterministic over the source id")
	}
	if alice.TenantID == bob.TenantID || alice.PrincipalID == bob.PrincipalID {
		t.Error("two users mapped to the same tenant or principal id")
	}

	t11, t12, t13 := plan.TenantTokens[0], plan.TenantTokens[1], plan.TenantTokens[2]
	// The hash must be over the bearer form clients actually send: new-api
	// stores the bare key and hands out `sk-<key>`. An already-prefixed value
	// must not be double-prefixed.
	if t11.TokenHash != authsnapshot.HashToken("sk-barekey") {
		t.Errorf("token 11 hash is not over sk-barekey")
	}
	if t12.TokenHash != authsnapshot.HashToken("sk-prefixed") {
		t.Errorf("token 12 hash double-prefixed or altered")
	}
	if t11.TenantID != alice.TenantID || t11.PrincipalID != alice.PrincipalID || t13.TenantID != bob.TenantID {
		t.Error("tokens not attached to their user's tenant/principal")
	}
	if t11.Group != "vip" || t12.Group != "team" {
		t.Errorf("group fallback: t11=%q (want user group vip) t12=%q (want token group team)", t11.Group, t12.Group)
	}
	if t11.ExpiresAt != nil || t13.ExpiresAt != nil {
		t.Error("expired_time -1 / 0 must map to a NULL expiry")
	}
	if t12.ExpiresAt == nil || t12.ExpiresAt.Unix() != 1800000000 {
		t.Errorf("token 12 expiry = %v", t12.ExpiresAt)
	}
	if t11.Status != "active" || t13.Status != "revoked" || t13.SourceStatus != 4 {
		t.Errorf("token status mapping: t11=%q t13=%q/%d", t11.Status, t13.Status, t13.SourceStatus)
	}
	if t11.TokenID == "token-11" || t11.TokenID != importedTenantTokenID(SourceSystem, "token:11") {
		t.Errorf("token id = %q", t11.TokenID)
	}
	// Records carry the target ids and no longer claim to be staging only.
	for _, record := range plan.Records {
		if record.RecordKind != "tenant" && record.RecordKind != "token" {
			continue
		}
		if record.Status != "imported" || record.TargetID == "" {
			t.Errorf("record %s: status=%q target=%q", record.SourceID, record.Status, record.TargetID)
		}
		if _, staging := record.Conversion["staging_only"]; staging {
			t.Errorf("record %s still marked staging_only", record.SourceID)
		}
		if _, quotaStaging := record.Conversion["quota_staging_only"]; !quotaStaging {
			t.Errorf("record %s must state that quota stays in source units", record.SourceID)
		}
	}
}

func TestBuildPlanRejectsUnmappableUsersAndTokensWithoutGuessing(t *testing.T) {
	plan, err := BuildPlan(SourceSnapshot{
		Users: []SourceUser{
			{ID: 1, Username: "ok", Status: 1},
			{ID: 2, Username: "deleted?", Status: 3}, // not a documented user status
			{ID: 0, Username: "noid", Status: 1},
			{ID: 1, Username: "dup", Status: 1},
		},
		Tokens: []SourceToken{
			{ID: 21, UserID: 1, Status: 1, Key: "fine"},
			{ID: 22, UserID: 2, Status: 1, Key: "orphan-of-rejected-user"},
			{ID: 23, UserID: 999, Status: 1, Key: "orphan-of-unknown-user"},
			{ID: 24, UserID: 1, Status: 9, Key: "weird-status"},
			{ID: 25, UserID: 1, Status: 1, Key: ""},
			{ID: 26, UserID: 1, Status: 1, Key: "bad-expiry", ExpiredTime: "soon"},
			{ID: 0, UserID: 1, Status: 1, Key: "noid"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Summary.TenantCount != 1 || plan.Summary.TenantTokenCount != 1 {
		t.Fatalf("summary = %+v", plan.Summary)
	}
	if len(plan.Tenants) != 1 || plan.Tenants[0].SourceUserID != 1 || len(plan.TenantTokens) != 1 || plan.TenantTokens[0].SourceID != "token:21" {
		t.Fatalf("planned %+v / %+v", plan.Tenants, plan.TenantTokens)
	}
	// 3 users + 6 tokens rejected, each with a reason; none silently attached to
	// a default tenant.
	if plan.Summary.RejectedCount != 9 {
		t.Errorf("rejected %d, want 9: %v", plan.Summary.RejectedCount, plan.Summary.RejectionReasons)
	}
	for _, want := range []string{"unsupported user status 3", "user id is not usable", "duplicate user id",
		"token references a user that was not planned as a tenant", "unsupported token status 9", "token key is empty",
		`unparseable expired_time "soon"`, "token id is not usable"} {
		if plan.Summary.RejectionReasons[want] == 0 {
			t.Errorf("missing rejection reason %q in %v", want, plan.Summary.RejectionReasons)
		}
	}
	if plan.Summary.RejectionReasons["token references a user that was not planned as a tenant"] != 2 {
		t.Errorf("both the orphan of a rejected user and the orphan of an unknown user must be rejected: %v", plan.Summary.RejectionReasons)
	}
	// Rejected token records must still hold only a digest of the key.
	encoded, _ := json.Marshal(plan.Records)
	for _, secret := range []string{"orphan-of-rejected-user", "orphan-of-unknown-user", "weird-status", "bad-expiry"} {
		if strings.Contains(string(encoded), secret) {
			t.Errorf("rejected token key %q leaked into records", secret)
		}
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
