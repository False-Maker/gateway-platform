package gateway

import (
	"errors"
	"testing"
	"time"

	"github.com/elucid/gateway-platform/pkg/contracts"
)

func TestChooserSkipsCoolingAccount(t *testing.T) {
	now := time.Unix(100, 0)
	chooser := NewChooser(func() time.Time { return now })
	accounts := []contracts.Account{
		{ID: "a", Provider: "apikey", Platform: "apikey", Group: "default", Status: "active", Credential: contracts.Credential{AccessToken: "a", Version: 1}},
		{ID: "b", Provider: "apikey", Platform: "apikey", Group: "default", Status: "active", Credential: contracts.Credential{AccessToken: "b", Version: 1}},
	}
	chooser.Replace(accounts)
	chooser.MarkFailure("a", "m", contracts.ErrorRateLimitedUnknown, 10*time.Second)
	lease, err := chooser.Acquire(contracts.Criteria{Platform: "apikey", Group: "default", Model: "m"})
	if err != nil || lease.AccountID != "b" {
		t.Fatalf("expected b after cooling a, got %#v, %v", lease, err)
	}
}

func TestChooserClearsCooldownOnNewSnapshotEpoch(t *testing.T) {
	now := time.Unix(100, 0)
	chooser := NewChooser(func() time.Time { return now })
	accounts := []contracts.Account{
		{ID: "a", Provider: "apikey", Platform: "apikey", Group: "default", Status: "active", Credential: contracts.Credential{AccessToken: "a", Version: 1}},
		{ID: "b", Provider: "apikey", Platform: "apikey", Group: "default", Status: "active", Credential: contracts.Credential{AccessToken: "b", Version: 1}},
	}
	chooser.ReplaceAtEpoch(accounts, 1)
	chooser.MarkFailure("a", "m", contracts.ErrorRateLimitedUnknown, time.Minute)
	if lease, err := chooser.Acquire(contracts.Criteria{Platform: "apikey", Group: "default", Model: "m"}); err != nil || lease.AccountID != "b" {
		t.Fatalf("cooldown was not applied: lease=%#v err=%v", lease, err)
	}
	chooser.ReplaceAtEpoch(accounts, 2)
	if lease, err := chooser.Acquire(contracts.Criteria{Platform: "apikey", Group: "default", Model: "m"}); err != nil || lease.AccountID != "a" {
		t.Fatalf("new epoch did not clear cooldown: lease=%#v err=%v", lease, err)
	}
}

func TestChooserQuotaAdmission(t *testing.T) {
	zero, remaining := int64(0), int64(3)
	chooser := NewChooser(nil)
	chooser.Replace([]contracts.Account{
		{ID: "account-exhausted", Provider: "apikey", Platform: "apikey", Group: "default", Status: "active", Quota: contracts.QuotaInfo{Items: []contracts.QuotaItem{{Scope: "account", Unit: "token", Remaining: &zero}}}},
		{ID: "model-exhausted", Provider: "apikey", Platform: "apikey", Group: "default", Status: "active", Quota: contracts.QuotaInfo{Items: []contracts.QuotaItem{{Scope: "model", Model: "target", Unit: "request", Remaining: &zero}}}},
		{ID: "other-model", Provider: "apikey", Platform: "apikey", Group: "default", Status: "active", Quota: contracts.QuotaInfo{Items: []contracts.QuotaItem{{Scope: "model", Model: "other", Unit: "request", Remaining: &zero}}}},
		{ID: "known-remaining", Provider: "apikey", Platform: "apikey", Group: "default", Status: "active", Quota: contracts.QuotaInfo{Items: []contracts.QuotaItem{{Scope: "account", Unit: "token", Remaining: &remaining}}}},
		{ID: "unknown-remaining", Provider: "apikey", Platform: "apikey", Group: "default", Status: "active", Quota: contracts.QuotaInfo{Items: []contracts.QuotaItem{{Scope: "account", Unit: "token"}}}},
	})

	lease, err := chooser.Acquire(contracts.Criteria{Platform: "apikey", Group: "default", Model: "target"})
	if err != nil || lease.AccountID != "known-remaining" {
		t.Fatalf("quota admission selected account=%q err=%v", lease.AccountID, err)
	}

	chooser.Replace([]contracts.Account{{ID: "exhausted", Provider: "apikey", Platform: "apikey", Group: "default", Status: "active", Quota: contracts.QuotaInfo{Items: []contracts.QuotaItem{{Scope: "account", Unit: "token", Remaining: &zero}}}}})
	if _, err := chooser.Acquire(contracts.Criteria{Platform: "apikey", Group: "default", Model: "target"}); !errors.Is(err, ErrQuotaExhausted) {
		t.Fatalf("exhausted account error=%v, want ErrQuotaExhausted", err)
	}
}

func TestChooserCooldownIsNotReportedAsQuotaExhaustion(t *testing.T) {
	now := time.Unix(100, 0)
	zero := int64(0)
	chooser := NewChooser(func() time.Time { return now })
	chooser.Replace([]contracts.Account{{
		ID:       "cooling",
		Provider: "apikey",
		Platform: "apikey",
		Group:    "default",
		Status:   "active",
		Quota:    contracts.QuotaInfo{Items: []contracts.QuotaItem{{Scope: "account", Unit: "token", Remaining: &zero}}},
	}})
	chooser.MarkFailure("cooling", "target", contracts.ErrorRateLimitedKnown, time.Minute)
	if _, err := chooser.Acquire(contracts.Criteria{Platform: "apikey", Group: "default", Model: "target"}); !errors.Is(err, ErrNoAccount) {
		t.Fatalf("cooling account error=%v, want ErrNoAccount", err)
	}
}
