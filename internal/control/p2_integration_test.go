package control

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/elucid/gateway-platform/internal/control/credentials"
	providerapi "github.com/elucid/gateway-platform/internal/control/provider"
	"github.com/elucid/gateway-platform/internal/control/provider/codex"
	"github.com/elucid/gateway-platform/pkg/contracts"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestP2PostgresImportAndRefresh(t *testing.T) {
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
	const sourceID = "p2-integration"
	accountID := importedAccountID("fixture", sourceID)
	if _, err := db.Exec(ctx, `DELETE FROM accounts WHERE id=$1`, accountID); err != nil {
		t.Fatal(err)
	}
	defer db.Exec(context.Background(), `DELETE FROM accounts WHERE id=$1`, accountID)
	key := bytes.Repeat([]byte{0x73}, 32)
	cipher, err := credentials.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	p := codex.NewOAuth(nil)
	lookup := func(kind string) (providerapi.Provider, bool) { return p, kind == providerapi.KindCodex }
	request := contracts.ImportRequest{SourceSystem: "fixture", SourceID: sourceID, Provider: providerapi.KindCodex, AuthMode: "oauth", TokenBundle: &contracts.TokenBundle{AccessToken: "old-access", RefreshToken: "old-refresh"}}
	account, err := (ImportService{Repository: PGImportRepository{DB: db}, Cipher: cipher, Provider: lookup}).Import(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if account.ID != accountID || account.Credential.AccessToken != "old-access" {
		t.Fatalf("imported account=%#v", account)
	}
	repository := PGCredentialRepository{DB: db}
	unlock, acquired, err := repository.TryLock(ctx, accountID)
	if err != nil || !acquired {
		t.Fatalf("acquire account refresh lock: acquired=%v err=%v", acquired, err)
	}
	if _, acquired, err := repository.TryLock(ctx, accountID); err != nil || acquired {
		t.Fatalf("contended account refresh lock: acquired=%v err=%v", acquired, err)
	}
	if err := unlock(); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"new-access","refresh_token":"new-refresh","expires_in":60}`))
	}))
	defer server.Close()
	p.HTTP.Client = server.Client()
	p.Config.OAuthTokenURL = server.URL
	p.Config.APIBaseURL = server.URL
	refreshed, err := (RefreshService{Repository: repository, Cipher: cipher, Provider: lookup}).Refresh(ctx, accountID)
	if err != nil {
		t.Fatal(err)
	}
	if refreshed.AccessToken != "new-access" || refreshed.RefreshToken != "new-refresh" {
		t.Fatalf("refreshed bundle=%#v", refreshed)
	}
	var encrypted []byte
	var version, epoch int64
	if err := db.QueryRow(ctx, `SELECT c.encrypted_secret,c.version,a.fence_epoch FROM credentials c JOIN accounts a ON a.id=c.account_id WHERE a.id=$1`, accountID).Scan(&encrypted, &version, &epoch); err != nil {
		t.Fatal(err)
	}
	plaintext, err := cipher.Decrypt(accountID, encrypted)
	if err != nil {
		t.Fatal(err)
	}
	var stored contracts.TokenBundle
	if err := json.Unmarshal(plaintext, &stored); err != nil {
		t.Fatal(err)
	}
	if stored.RefreshToken != "new-refresh" || version != 1 || epoch != 1 {
		t.Fatalf("stored=%#v version=%d epoch=%d", stored, version, epoch)
	}
	if err := (Fencer{DB: db}).UpdateCredential(ctx, accountID, epoch-1, []byte("stale-secret"), nil); !errors.Is(err, ErrFenceLost) {
		t.Fatalf("stale credential writer was accepted: %v", err)
	}
	restarted, err := NewRefreshLoop(db, base64.StdEncoding.EncodeToString(key))
	if err != nil {
		t.Fatal(err)
	}
	reopened, err := restarted.Service.Cipher.Decrypt(accountID, encrypted)
	if err != nil || !bytes.Equal(reopened, plaintext) {
		t.Fatalf("reopen credential after key reload: equal=%v err=%v", bytes.Equal(reopened, plaintext), err)
	}
}
