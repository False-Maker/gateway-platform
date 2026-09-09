package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/elucid/gateway-platform/internal/control"
	"github.com/elucid/gateway-platform/internal/control/credentials"
	"github.com/elucid/gateway-platform/internal/gateway"
	"github.com/elucid/gateway-platform/internal/snapshot"
	"github.com/elucid/gateway-platform/migrations"
	"github.com/elucid/gateway-platform/pkg/contracts"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
)

func TestImportCommandPublishesSnapshotAndGatewayCanChooseAccount(t *testing.T) {
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
	if err := migrations.Apply(ctx, db); err != nil {
		t.Fatal(err)
	}

	const sourceSystem = "a2-cli-fixture"
	const sourceID = "grok-1"
	if _, err := db.Exec(ctx, `DELETE FROM accounts WHERE source_system=$1 AND source_id=$2`, sourceSystem, sourceID); err != nil {
		t.Fatal(err)
	}
	defer db.Exec(context.Background(), `DELETE FROM accounts WHERE source_system=$1 AND source_id=$2`, sourceSystem, sourceID)

	key := bytes.Repeat([]byte{0x54}, 32)
	t.Setenv("GATEWAY_DATABASE_URL", databaseURL)
	t.Setenv("GATEWAY_CREDENTIAL_KEY", base64.StdEncoding.EncodeToString(key))
	file := filepath.Join(t.TempDir(), "import.json")
	if err := os.WriteFile(file, []byte(`{"source_system":"a2-cli-fixture","source_id":"grok-1","provider":"grok","auth_mode":"api_key","static_key":"grok-a2-secret","metadata":{"group":"a2"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := runImportCommand([]string{"--file", file}, &output); err != nil {
		t.Fatal(err)
	}
	var result importCommandResult
	if err := json.Unmarshal(output.Bytes(), &result); err != nil {
		t.Fatalf("output=%q err=%v", output.String(), err)
	}
	if result.AccountID == "" || result.CredentialKind != "static" || result.DryRun {
		t.Fatalf("unexpected import result=%#v", result)
	}

	var encrypted []byte
	if err := db.QueryRow(ctx, `SELECT encrypted_secret FROM credentials WHERE account_id=$1`, result.AccountID).Scan(&encrypted); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encrypted, []byte("grok-a2-secret")) {
		t.Fatal("credential was stored as plaintext")
	}
	cipher, err := credentials.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	plaintext, err := cipher.Decrypt(result.AccountID, encrypted)
	if err != nil {
		t.Fatal(err)
	}
	var bundle contracts.TokenBundle
	if err := json.Unmarshal(plaintext, &bundle); err != nil || bundle.AccessToken != "grok-a2-secret" {
		t.Fatalf("decrypted bundle=%#v err=%v", bundle, err)
	}

	mini := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	loop := control.SnapshotLoop{Repository: control.PGSnapshotRepository{DB: db, Cipher: cipher}, Redis: rdb}
	if err := loop.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	loaded, err := (snapshot.Loader{Redis: rdb}).Load(ctx, "grok", "a2")
	if err != nil || loaded.Epoch != 1 || len(loaded.Accounts) != 1 || loaded.Accounts[0].ID != result.AccountID {
		t.Fatalf("published snapshot=%#v err=%v", loaded, err)
	}

	chooser := gateway.NewChooser(func() time.Time { return time.Now() })
	chooser.ReplaceAtEpoch(loaded.Accounts, loaded.Epoch)
	lease, err := chooser.Acquire(contracts.Criteria{Platform: "grok", Group: "a2", Model: "grok-model"})
	if err != nil || lease.AccountID != result.AccountID || lease.Credential.AccessToken != "grok-a2-secret" {
		t.Fatalf("gateway lease=%#v err=%v", lease, err)
	}
}
