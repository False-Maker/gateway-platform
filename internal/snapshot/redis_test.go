package snapshot

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/elucid/gateway-platform/pkg/contracts"
	"github.com/redis/go-redis/v9"
)

func TestPublishLoadAndStaleEpoch(t *testing.T) {
	mini := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	limit, remaining := int64(1000), int64(750)
	account := contracts.Account{ID: "a1", Provider: "apikey", Platform: "apikey", Group: "default", Status: "active", Credential: contracts.Credential{Kind: "static", AccessToken: "secret", Version: 1}, Quota: contracts.QuotaInfo{Items: []contracts.QuotaItem{{Scope: "account", Unit: "token", Limit: &limit, Remaining: &remaining}}}}
	publisher := Publisher{Redis: rdb}
	version, err := publisher.Publish(context.Background(), "apikey", "default", 0, []contracts.Account{account})
	if err != nil || version != 1 {
		t.Fatalf("publish: version=%d err=%v", version, err)
	}
	loaded, err := (Loader{Redis: rdb}).Load(context.Background(), "apikey", "default")
	if err != nil || loaded.Epoch != 1 || len(loaded.Accounts) != 1 || loaded.Accounts[0].ID != "a1" || len(loaded.Accounts[0].Quota.Items) != 1 || *loaded.Accounts[0].Quota.Items[0].Remaining != remaining {
		t.Fatalf("load: %#v %v", loaded, err)
	}
	store := NewStore(Loader{Redis: rdb})
	if err := store.Refresh(context.Background(), "apikey", "default"); err != nil {
		t.Fatal(err)
	}
	if _, err := rdb.Set(context.Background(), activeKeyFor("apikey", "default"), "not-a-version", 0).Result(); err != nil {
		t.Fatal(err)
	}
	if err := store.Refresh(context.Background(), "apikey", "default"); err == nil {
		t.Fatal("corrupt active version must be rejected")
	}
	if current, ok := store.Current("apikey", "default"); !ok || current.Version != loaded.Version {
		t.Fatalf("last known-good snapshot was replaced: %#v", current)
	}
	if err := rdb.Set(context.Background(), activeKeyFor("apikey", "default"), version, 0).Err(); err != nil {
		t.Fatal(err)
	}
	if _, err := publisher.Publish(context.Background(), "apikey", "default", loaded.Epoch, []contracts.Account{account}); err != nil {
		t.Fatalf("second generation should accept current epoch: %v", err)
	}
	second, err := (Loader{Redis: rdb}).Load(context.Background(), "apikey", "default")
	if err != nil || second.Version != 2 || second.Epoch != 2 {
		t.Fatalf("second generation did not advance version and epoch: %#v %v", second, err)
	}
	oldDataKey := "snap:data:" + BucketID("apikey", "default") + ":1"
	if ttl := rdb.TTL(context.Background(), oldDataKey).Val(); ttl <= 0 || ttl > GraceTTL {
		t.Fatalf("old snapshot grace TTL = %v", ttl)
	}
	if _, err := publisher.Publish(context.Background(), "apikey", "default", loaded.Epoch, []contracts.Account{account}); err != ErrStaleEpoch {
		t.Fatalf("expected stale epoch, got %v", err)
	}
}

func TestPublishLoadPreservesDecimalQuota(t *testing.T) {
	mini := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	fraction := contracts.Decimal("0.99833333")
	precise := contracts.Decimal("2028.68")
	account := contracts.Account{ID: "decimal", Provider: "kiro", Platform: "kiro", Group: "default", Status: "active", Quota: contracts.QuotaInfo{Items: []contracts.QuotaItem{{Scope: "model", Model: "gemini", Unit: "fraction", RemainingFraction: &fraction, Precision: &contracts.QuotaPrecision{Remaining: &precise}}}}}
	if _, err := (Publisher{Redis: rdb}).Publish(context.Background(), "kiro", "default", 0, []contracts.Account{account}); err != nil {
		t.Fatal(err)
	}
	loaded, err := (Loader{Redis: rdb}).Load(context.Background(), "kiro", "default")
	if err != nil || len(loaded.Accounts) != 1 {
		t.Fatalf("loaded=%#v err=%v", loaded, err)
	}
	item := loaded.Accounts[0].Quota.Items[0]
	if string(*item.RemainingFraction) != "0.99833333" || string(*item.Precision.Remaining) != "2028.68" {
		t.Fatalf("decimal quota changed after snapshot round trip: %#v", item)
	}
}

func TestPublishCASAllowsOneWriterPerEpoch(t *testing.T) {
	mini := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	publisher := Publisher{Redis: rdb}
	account := contracts.Account{ID: "a1", Provider: "apikey", Platform: "apikey", Group: "default", Status: "active"}
	if _, err := publisher.Publish(context.Background(), "apikey", "default", 0, []contracts.Account{account}); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errorsByWriter := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := publisher.Publish(context.Background(), "apikey", "default", 1, []contracts.Account{account})
			errorsByWriter <- err
		}()
	}
	wg.Wait()
	close(errorsByWriter)
	var success, stale int
	for err := range errorsByWriter {
		switch {
		case err == nil:
			success++
		case errors.Is(err, ErrStaleEpoch):
			stale++
		default:
			t.Fatalf("unexpected publish error: %v", err)
		}
	}
	if success != 1 || stale != 1 {
		t.Fatalf("CAS results: success=%d stale=%d", success, stale)
	}
}

func TestStoreCurrentFreshAtRejectsExpiredSnapshot(t *testing.T) {
	now := time.Unix(100, 0)
	store := NewStore(Loader{})
	store.items["apikey\x00default"] = Snapshot{Platform: "apikey", Group: "default", ValidUntil: now.Add(time.Second)}
	if _, ok := store.CurrentFreshAt("apikey", "default", now); !ok {
		t.Fatal("unexpired snapshot was rejected")
	}
	if _, ok := store.CurrentFreshAt("apikey", "default", now.Add(time.Second)); ok {
		t.Fatal("expired snapshot was accepted")
	}
}

func activeKeyFor(platform, group string) string {
	_, active, _, _, _, _ := Keys(BucketID(platform, group))
	return active
}
