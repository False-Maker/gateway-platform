package control

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/alicebob/miniredis/v2"
	providerpkg "github.com/elucid/gateway-platform/internal/control/provider"
	"github.com/elucid/gateway-platform/internal/control/provider/builtin"
	"github.com/elucid/gateway-platform/internal/observability"
	"github.com/elucid/gateway-platform/internal/snapshot"
	"github.com/elucid/gateway-platform/pkg/contracts"
	"github.com/redis/go-redis/v9"
)

type memorySnapshotRepository struct {
	buckets  []SnapshotBucket
	accounts map[string][]contracts.Account
}

func (r *memorySnapshotRepository) ListSnapshotBuckets(context.Context) ([]SnapshotBucket, error) {
	return append([]SnapshotBucket(nil), r.buckets...), nil
}

func (r *memorySnapshotRepository) ListSnapshotAccounts(_ context.Context, platform, group string) ([]contracts.Account, error) {
	key := platform + "\x00" + group
	return append([]contracts.Account(nil), r.accounts[key]...), nil
}

func TestSnapshotLoopPublishesAccountChangesAndQuotaBridge(t *testing.T) {
	mini := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	remaining := int64(100)
	repository := &memorySnapshotRepository{
		buckets: []SnapshotBucket{{Platform: "codex", Group: "default"}},
		accounts: map[string][]contracts.Account{
			"codex\x00default": {
				{ID: "account-1", Provider: "codex", Platform: "codex", Group: "default", Status: "active", FenceEpoch: 3, Credential: contracts.Credential{Kind: "oauth", AccessToken: "access-1", Version: 2}, Limits: contracts.AccountLimits{MaxConcurrency: 2}.WithDefaults(), Quota: contracts.QuotaInfo{Items: []contracts.QuotaItem{{Scope: "account", Unit: "token", Remaining: &remaining}}}},
			},
		},
	}
	loop := SnapshotLoop{Repository: repository, Redis: rdb}
	ctx := context.Background()
	if err := loop.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	loaded, err := (snapshot.Loader{Redis: rdb}).Load(ctx, "codex", "default")
	if err != nil || loaded.Version != 1 || loaded.Epoch != 1 || len(loaded.Accounts) != 1 || loaded.Accounts[0].Credential.AccessToken != "access-1" {
		t.Fatalf("first snapshot=%#v err=%v", loaded, err)
	}

	secondRemaining := int64(50)
	repository.accounts["codex\x00default"] = append(repository.accounts["codex\x00default"], contracts.Account{ID: "account-2", Provider: "codex", Platform: "codex", Group: "default", Status: "active", Credential: contracts.Credential{Kind: "oauth", AccessToken: "access-2"}, Quota: contracts.QuotaInfo{Items: []contracts.QuotaItem{{Scope: "account", Unit: "token", Remaining: &secondRemaining}}}})
	if err := loop.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	loaded, err = (snapshot.Loader{Redis: rdb}).Load(ctx, "codex", "default")
	if err != nil || loaded.Version != 2 || loaded.Epoch != 2 || len(loaded.Accounts) != 2 {
		t.Fatalf("account add snapshot=%#v err=%v", loaded, err)
	}

	repository.accounts["codex\x00default"] = repository.accounts["codex\x00default"][:1]
	if err := loop.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	loaded, err = (snapshot.Loader{Redis: rdb}).Load(ctx, "codex", "default")
	if err != nil || loaded.Version != 3 || loaded.Epoch != 3 || len(loaded.Accounts) != 1 || loaded.Accounts[0].ID != "account-1" {
		t.Fatalf("account delete snapshot=%#v err=%v", loaded, err)
	}

	quotaRemaining := int64(7)
	stored := StoredCredential{AccountID: "account-1", Platform: "codex", Group: "default"}
	quota := contracts.QuotaInfo{Items: []contracts.QuotaItem{{Scope: "account", Unit: "token", Remaining: &quotaRemaining}}}
	if err := (RedisQuotaSnapshotPublisher{Redis: rdb}).PublishQuota(ctx, stored, quota); err != nil {
		t.Fatal(err)
	}
	loaded, err = (snapshot.Loader{Redis: rdb}).Load(ctx, "codex", "default")
	if err != nil || loaded.Version != 4 || loaded.Epoch != 4 || len(loaded.Accounts) != 1 || *loaded.Accounts[0].Quota.Items[0].Remaining != quotaRemaining {
		t.Fatalf("quota bridge snapshot=%#v err=%v", loaded, err)
	}

	repository.accounts["codex\x00default"] = nil
	repository.buckets = nil
	if err := loop.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	loaded, err = (snapshot.Loader{Redis: rdb}).Load(ctx, "codex", "default")
	if err != nil || loaded.Version != 5 || loaded.Epoch != 5 || len(loaded.Accounts) != 0 {
		t.Fatalf("empty snapshot after final account deletion=%#v err=%v", loaded, err)
	}
	registered, err := rdb.HGetAll(ctx, snapshot.BucketRegistryKey).Result()
	if err != nil {
		t.Fatal(err)
	}
	if len(registered) != 0 {
		t.Fatalf("deleted bucket remains registered: %#v", registered)
	}
}

func TestSnapshotLoopUsesPublisherEpochCAS(t *testing.T) {
	mini := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	account := contracts.Account{ID: "account-1", Provider: "apikey", Platform: "apikey", Group: "default", Status: "active"}
	if _, err := (snapshot.Publisher{Redis: rdb}).Publish(context.Background(), "apikey", "default", 0, []contracts.Account{account}); err != nil {
		t.Fatal(err)
	}
	if _, err := (snapshot.Publisher{Redis: rdb}).Publish(context.Background(), "apikey", "default", 1, []contracts.Account{account}); err != nil {
		t.Fatal(err)
	}
	if _, err := (snapshot.Publisher{Redis: rdb}).Publish(context.Background(), "apikey", "default", 1, []contracts.Account{account}); !errors.Is(err, snapshot.ErrStaleEpoch) {
		t.Fatalf("stale epoch error=%v", err)
	}
}

// A13 / overview §6.2: "没有配置 usage_integrity 不得进入可调度快照". The gate is
// fail-closed on purpose -- publishing an account we have no stated way to
// meter is how unbillable usage gets produced.
func TestSnapshotLoopDropsAccountsWhoseProviderDeclaresNoUsageIntegrity(t *testing.T) {
	mini := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	builtin.RegisterAll(nil)
	repository := &memorySnapshotRepository{
		buckets: []SnapshotBucket{{Platform: "codex", Group: "default"}},
		accounts: map[string][]contracts.Account{
			"codex\x00default": {
				{ID: "registered", Provider: providerpkg.KindCodex, Platform: "codex", Group: "default", Status: "active", Credential: contracts.Credential{Kind: "oauth", AccessToken: "a", Version: 1}},
				{ID: "unregistered", Provider: "provider-that-was-never-registered", Platform: "codex", Group: "default", Status: "active", Credential: contracts.Credential{Kind: "oauth", AccessToken: "b", Version: 1}},
			},
		},
	}
	loop := SnapshotLoop{Repository: repository, Redis: rdb}
	if err := loop.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	loaded, err := (snapshot.Loader{Redis: rdb}).Load(context.Background(), "codex", "default")
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Accounts) != 1 || loaded.Accounts[0].ID != "registered" {
		t.Fatalf("unregistered provider reached the schedulable snapshot: %+v", loaded.Accounts)
	}
	// Stamped from the registry, not from a column: §6.2 makes the policy a
	// property of the provider, so two accounts of one provider cannot disagree.
	if loaded.Accounts[0].UsageIntegrity != contracts.UsageIntegrityFailover {
		t.Fatalf("account was published without a usage_integrity stamp: %+v", loaded.Accounts[0])
	}
}

// A22: the starvation gate firing is a one-shot event, so the increment that
// creates its series is the whole signal StarvationGateFired watches, and
// increase() cannot see that increment. Unlike the other primed counters the
// `platform` label is free-form text out of `accounts`, so the enumeration
// comes from the bucket list the loop already reads every minute rather than
// from a hand-kept list that would rot when a platform is added.
//
// The counter is published to observability.Default because its real emitter
// (PGSnapshotRepository.ReleaseStarvedCooldowns) is too; a separate registry
// here would prove nothing about the series the alert actually reads. The
// platform names are unique to this test so the pre-assertion stays meaningful
// even though the registry is process-wide.
func TestPrimeStarvationCountersZeroesEveryPlatformInTheBucketList(t *testing.T) {
	const alpha, beta = "a22-prime-alpha", "a22-prime-beta"
	loop := SnapshotLoop{}

	before := scrape(t, observability.Default)
	if strings.Contains(before, alpha) || strings.Contains(before, beta) {
		t.Fatalf("this test's platforms already had series before priming: %s", before)
	}

	// Two groups on one platform: the label is the platform, so the duplicate
	// must collapse rather than being counted twice.
	loop.primeStarvationCounters([]SnapshotBucket{
		{Platform: alpha, Group: "g1"},
		{Platform: alpha, Group: "g2"},
		{Platform: beta, Group: "g1"},
	})

	primed := scrape(t, observability.Default)
	for _, platform := range []string{alpha, beta} {
		want := `control_platform_starvation_release_total{platform="` + platform + `"} 0`
		if !strings.Contains(primed, want) {
			t.Errorf("missing primed series %s; got:\n%s", want, primed)
		}
	}

	// A nil repository must not panic: PrimeMetrics is best-effort and runs
	// before the loop has proven it can talk to the database.
	SnapshotLoop{}.PrimeMetrics(context.Background())
}
