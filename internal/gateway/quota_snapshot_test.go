package gateway

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/elucid/gateway-platform/internal/events"
	"github.com/elucid/gateway-platform/internal/snapshot"
	"github.com/elucid/gateway-platform/pkg/contracts"
	"github.com/redis/go-redis/v9"
)

func TestGatewayRejectsExhaustedRedisSnapshotBeforeAttempt(t *testing.T) {
	mini := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	zero := int64(0)
	account := contracts.Account{
		ID:       "quota-exhausted",
		Provider: "apikey",
		Platform: "apikey",
		Group:    "default",
		Status:   "active",
		Quota:    contracts.QuotaInfo{Items: []contracts.QuotaItem{{Scope: "account", Unit: "token", Remaining: &zero}}},
	}
	if _, err := (snapshot.Publisher{Redis: rdb}).Publish(context.Background(), "apikey", "default", 0, []contracts.Account{account}); err != nil {
		t.Fatal(err)
	}
	store := snapshot.NewStore(snapshot.Loader{Redis: rdb})
	if err := store.Refresh(context.Background(), "apikey", "default"); err != nil {
		t.Fatal(err)
	}
	current, ok := store.CurrentFresh("apikey", "default")
	if !ok {
		t.Fatal("published snapshot was not fresh")
	}
	server := NewServer(events.Producer{Redis: rdb, ProducerID: "gateway-test"}, "apikey")
	server.Chooser.ReplaceAtEpoch(current.Accounts, current.Epoch)
	server.snapshotFresh = func() bool {
		_, ok := store.CurrentFresh("apikey", "default")
		return ok
	}

	_, status, err := server.HandleNonStreaming(context.Background(), "tenant", "default", ChatCompletionRequest{Model: "m", Messages: []map[string]any{{"role": "user", "content": "hi"}}})
	if status != http.StatusTooManyRequests || !errors.Is(err, ErrQuotaExhausted) {
		t.Fatalf("quota result: status=%d err=%v", status, err)
	}
	if got := rdb.XLen(context.Background(), events.StreamKey).Val(); got != 0 {
		t.Fatalf("quota rejection must not create attempt events: %d", got)
	}
}

func TestGatewayRejectsStaleSnapshotBeforeSelection(t *testing.T) {
	server := NewServer(events.Producer{}, "apikey")
	server.snapshotFresh = func() bool { return false }
	_, status, err := server.HandleNonStreaming(context.Background(), "tenant", "default", ChatCompletionRequest{Model: "m", Messages: []map[string]any{{"role": "user", "content": "hi"}}})
	if status != http.StatusServiceUnavailable || !errors.Is(err, ErrSnapshotStale) {
		t.Fatalf("stale snapshot result: status=%d err=%v", status, err)
	}
}
