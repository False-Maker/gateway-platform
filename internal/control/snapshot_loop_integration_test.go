package control

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/elucid/gateway-platform/internal/control/credentials"
	"github.com/elucid/gateway-platform/pkg/contracts"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestPGSnapshotRepositoryReadsSchedulableAccounts(t *testing.T) {
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

	const accountID = "a1-snapshot-repository"
	if _, err := db.Exec(ctx, `DELETE FROM accounts WHERE id=$1`, accountID); err != nil {
		t.Fatal(err)
	}
	defer db.Exec(context.Background(), `DELETE FROM accounts WHERE id=$1`, accountID)
	key := bytes.Repeat([]byte{0x41}, 32)
	cipher, err := credentials.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	bundle := contracts.TokenBundle{AccessToken: "snapshot-access", ExpiresAt: time.Now().Add(time.Hour).UTC()}
	plaintext, err := json.Marshal(bundle)
	if err != nil {
		t.Fatal(err)
	}
	encrypted, err := cipher.Encrypt(accountID, plaintext)
	if err != nil {
		t.Fatal(err)
	}
	profileJSON, _ := json.Marshal(contracts.UpstreamProfile{Protocol: "openai_chat"})
	limitsJSON, _ := json.Marshal(contracts.AccountLimits{MaxConcurrency: 3, RPM: 20, StickyTTL: 9, DegradePolicy: contracts.DegradeFailOpen})
	quotaJSON, _ := json.Marshal(contracts.QuotaInfo{})
	capabilitiesJSON, _ := json.Marshal([]string{"chat", "streaming"})
	if _, err := db.Exec(ctx, `INSERT INTO accounts (id,provider,platform,"group",source_system,source_id,status,fence_epoch,base_url,profile,limits,quota,capabilities) VALUES ($1,'codex','codex','snapshot-fixture','fixture',$1,'active',4,$2,$3::jsonb,$4::jsonb,$5::jsonb,$6::jsonb)`, accountID, "https://snapshot.example", string(profileJSON), string(limitsJSON), string(quotaJSON), string(capabilitiesJSON)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `INSERT INTO credentials (id,account_id,kind,encrypted_secret,expires_at,version) VALUES ($1,$2,'oauth',$3,$4,8)`, accountID+":credential", accountID, encrypted, bundle.ExpiresAt); err != nil {
		t.Fatal(err)
	}

	repository := PGSnapshotRepository{DB: db, Cipher: cipher}
	buckets, err := repository.ListSnapshotBuckets(ctx)
	if err != nil {
		t.Fatal(err)
	}
	foundBucket := false
	for _, bucket := range buckets {
		if bucket.Platform == "codex" && bucket.Group == "snapshot-fixture" {
			foundBucket = true
			break
		}
	}
	if !foundBucket {
		t.Fatalf("snapshot bucket was not listed: %#v", buckets)
	}
	accounts, err := repository.ListSnapshotAccounts(ctx, "codex", "snapshot-fixture")
	if err != nil {
		t.Fatal(err)
	}
	if len(accounts) != 1 {
		t.Fatalf("accounts=%#v", accounts)
	}
	account := accounts[0]
	if account.ID != accountID || account.Credential.AccessToken != bundle.AccessToken || account.Credential.Version != 8 || account.Credential.Kind != "oauth" || account.FenceEpoch != 4 || account.Profile.BaseURL != "https://snapshot.example" || account.Limits.MaxConcurrency != 3 || account.Limits.DegradePolicy != contracts.DegradeFailOpen || len(account.Capabilities) != 2 {
		t.Fatalf("decoded account=%#v", account)
	}
}
