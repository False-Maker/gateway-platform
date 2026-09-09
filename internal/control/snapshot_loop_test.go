package control

import (
	"context"
	"errors"
	"testing"

	"github.com/alicebob/miniredis/v2"
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
