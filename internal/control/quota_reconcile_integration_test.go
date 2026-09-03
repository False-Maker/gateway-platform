package control

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/elucid/gateway-platform/internal/snapshot"
	"github.com/elucid/gateway-platform/pkg/contracts"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
)

func TestP3PostgresQuotaOutboxReconcileAndFence(t *testing.T) {
	databaseURL := os.Getenv("GATEWAY_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("GATEWAY_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	db, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	migration, err := os.ReadFile(filepath.Join("..", "..", "migrations", "001_initial.sql"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, string(migration)); err != nil {
		t.Fatal(err)
	}
	const accountID = "p3-quota-outbox"
	if _, err := db.Exec(ctx, `DELETE FROM accounts WHERE id=$1`, accountID); err != nil {
		t.Fatal(err)
	}
	defer db.Exec(context.Background(), `DELETE FROM accounts WHERE id=$1`, accountID)
	if _, err := db.Exec(ctx, `INSERT INTO accounts (id,provider,platform,"group",source_system,source_id) VALUES ($1,'codex','codex','default','fixture',$1)`, accountID); err != nil {
		t.Fatal(err)
	}

	repository := PGCredentialRepository{DB: db}
	epoch, err := (Fencer{DB: db}).Acquire(ctx, accountID)
	if err != nil {
		t.Fatal(err)
	}
	remaining := int64(64)
	quota := contracts.QuotaInfo{Items: []contracts.QuotaItem{{Scope: "account", Unit: "token", Remaining: &remaining}}}
	stored := StoredCredential{AccountID: accountID, Platform: "codex", Group: "default", FenceEpoch: epoch}
	if err := repository.CommitQuotaAndQueue(ctx, stored, quota); err != nil {
		t.Fatal(err)
	}
	var payload string
	var outboxCount int
	if err := db.QueryRow(ctx, `SELECT quota FROM accounts WHERE id=$1`, accountID).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(ctx, `SELECT count(*) FROM quota_snapshot_outbox WHERE account_id=$1 AND fence_epoch=$2`, accountID, epoch).Scan(&outboxCount); err != nil {
		t.Fatal(err)
	}
	if outboxCount != 1 || !jsonContainsQuota(t, []byte(payload), remaining) {
		t.Fatalf("quota transaction payload=%s outbox=%d", payload, outboxCount)
	}

	mini := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	oldRemaining := int64(10)
	account := contracts.Account{ID: accountID, Provider: "codex", Platform: "codex", Group: "default", Status: "active", Quota: contracts.QuotaInfo{Items: []contracts.QuotaItem{{Scope: "account", Unit: "token", Remaining: &oldRemaining}}}}
	if _, err := (snapshot.Publisher{Redis: rdb}).Publish(ctx, "codex", "default", 0, []contracts.Account{account}); err != nil {
		t.Fatal(err)
	}
	reconciler := QuotaReconciler{Repository: repository, Snapshot: RedisQuotaSnapshotPublisher{Redis: rdb}}
	if err := reconciler.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(ctx, `SELECT count(*) FROM quota_snapshot_outbox WHERE account_id=$1`, accountID).Scan(&outboxCount); err != nil {
		t.Fatal(err)
	}
	loaded, err := (snapshot.Loader{Redis: rdb}).Load(ctx, "codex", "default")
	if err != nil || outboxCount != 0 || loaded.Version != 2 || *loaded.Accounts[0].Quota.Items[0].Remaining != remaining {
		t.Fatalf("reconciled outbox=%d snapshot=%#v err=%v", outboxCount, loaded, err)
	}

	currentEpoch, err := (Fencer{DB: db}).Acquire(ctx, accountID)
	if err != nil {
		t.Fatal(err)
	}
	if currentEpoch <= epoch {
		t.Fatalf("fence did not advance: old=%d current=%d", epoch, currentEpoch)
	}
	stalePayload, err := json.Marshal(contracts.QuotaInfo{Items: []contracts.QuotaItem{{Scope: "account", Unit: "token"}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `INSERT INTO quota_snapshot_outbox (account_id,platform,"group",fence_epoch,quota) VALUES ($1,'codex','default',$2,$3::jsonb)`, accountID, epoch, string(stalePayload)); err != nil {
		t.Fatal(err)
	}
	if err := reconciler.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(ctx, `SELECT count(*) FROM quota_snapshot_outbox WHERE account_id=$1`, accountID).Scan(&outboxCount); err != nil {
		t.Fatal(err)
	}
	afterStale, err := (snapshot.Loader{Redis: rdb}).Load(ctx, "codex", "default")
	if err != nil || outboxCount != 0 || afterStale.Version != loaded.Version || *afterStale.Accounts[0].Quota.Items[0].Remaining != remaining {
		t.Fatalf("stale outbox=%d snapshot=%#v err=%v", outboxCount, afterStale, err)
	}
	if err := repository.CommitQuotaAndQueue(ctx, stored, quota); !errors.Is(err, ErrFenceLost) {
		t.Fatalf("stale quota writer was accepted: %v", err)
	}
}

func jsonContainsQuota(t *testing.T, payload []byte, remaining int64) bool {
	t.Helper()
	var quota contracts.QuotaInfo
	if err := json.Unmarshal(payload, &quota); err != nil {
		t.Fatal(err)
	}
	return len(quota.Items) == 1 && quota.Items[0].Remaining != nil && *quota.Items[0].Remaining == remaining
}
