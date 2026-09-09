package snapshot

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func TestPublishAndLoadTokensReplacesAtomically(t *testing.T) {
	mini := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	ctx := context.Background()

	loaded, err := LoadTokens(ctx, rdb)
	if err != nil || len(loaded) != 0 {
		t.Fatalf("unpublished token set: %#v err=%v", loaded, err)
	}

	first := HashToken("gw_first")
	second := HashToken("gw_second")
	if err := PublishTokens(ctx, rdb, map[string]TokenRecord{
		first:  {TokenID: "t1", TenantID: "tenant-a", PrincipalID: "p1", Group: "default"},
		second: {TokenID: "t2", TenantID: "tenant-b", PrincipalID: "p2", Group: "vip", ExpiresAt: time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)},
	}); err != nil {
		t.Fatal(err)
	}
	loaded, err = LoadTokens(ctx, rdb)
	if err != nil || len(loaded) != 2 || loaded[first].TenantID != "tenant-a" || loaded[second].Group != "vip" || loaded[second].ExpiresAt.Year() != 2030 {
		t.Fatalf("published token set: %#v err=%v", loaded, err)
	}
	if _, raw := loaded[first], mini.HGet(TokenKey, first); raw == "" || contains(raw, "gw_first") {
		t.Fatalf("token hash value leaked raw token or is empty: %q", raw)
	}

	// Revocation: republishing without `second` must remove it, and an empty
	// publish must leave an empty (not stale) set.
	if err := PublishTokens(ctx, rdb, map[string]TokenRecord{first: {TokenID: "t1", TenantID: "tenant-a", PrincipalID: "p1", Group: "default"}}); err != nil {
		t.Fatal(err)
	}
	loaded, _ = LoadTokens(ctx, rdb)
	if len(loaded) != 1 || loaded[second].TenantID != "" {
		t.Fatalf("revoked token survived republish: %#v", loaded)
	}
	if err := PublishTokens(ctx, rdb, nil); err != nil {
		t.Fatal(err)
	}
	loaded, _ = LoadTokens(ctx, rdb)
	if len(loaded) != 0 {
		t.Fatalf("empty publish kept tokens: %#v", loaded)
	}
	for _, key := range mini.Keys() {
		if key != TokenKey {
			t.Fatalf("staging key leaked: %q", key)
		}
	}
}

func TestPublishTokensRejectsInvalidRecords(t *testing.T) {
	mini := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	ctx := context.Background()
	if err := PublishTokens(ctx, rdb, map[string]TokenRecord{HashToken("x"): {TokenID: "t", TenantID: "", PrincipalID: "p", Group: "g"}}); err == nil {
		t.Fatal("missing tenant_id was accepted")
	}
	if err := PublishTokens(ctx, rdb, map[string]TokenRecord{"not-a-hash": {TokenID: "t", TenantID: "a", PrincipalID: "p", Group: "g"}}); err == nil {
		t.Fatal("non-SHA-256 field was accepted")
	}
	if mini.Exists(TokenKey) {
		t.Fatal("rejected publish wrote the live key")
	}
}

func contains(haystack, needle string) bool { return strings.Contains(haystack, needle) }
