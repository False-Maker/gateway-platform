package control

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/elucid/gateway-platform/internal/control/credentials"
	providerapi "github.com/elucid/gateway-platform/internal/control/provider"
	"github.com/elucid/gateway-platform/internal/control/provider/antigravity"
	"github.com/elucid/gateway-platform/internal/control/provider/builtin"
	"github.com/elucid/gateway-platform/internal/control/provider/claude"
	"github.com/elucid/gateway-platform/internal/control/provider/codex"
	"github.com/elucid/gateway-platform/internal/control/provider/copilot"
	"github.com/elucid/gateway-platform/internal/control/provider/kiro"
	"github.com/elucid/gateway-platform/pkg/contracts"
)

type memoryImportRepository struct {
	record ImportRecord
	saved  bool
	locked bool
	epoch  int64
}

func (r *memoryImportRepository) TryLock(context.Context, string) (func() error, bool, error) {
	if r.locked {
		return nil, false, nil
	}
	r.locked = true
	return func() error { r.locked = false; return nil }, true, nil
}

func (r *memoryImportRepository) Save(_ context.Context, record ImportRecord) (string, int64, int64, error) {
	if !r.locked {
		return "", 0, 0, errors.New("account lock is not held")
	}
	r.record, r.saved = record, true
	r.epoch++
	return record.Account.ID, r.epoch, r.epoch, nil
}

func TestImportServiceEncryptsOAuthBundleAndReturnsAccessOnlyAccount(t *testing.T) {
	cipher, err := credentials.NewCipher(bytes.Repeat([]byte{0x52}, 32))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		kind string
		make func() providerapi.Provider
	}{
		{kind: providerapi.KindCodex, make: func() providerapi.Provider { return codex.NewOAuth(nil) }},
		{kind: providerapi.KindClaude, make: func() providerapi.Provider { return claude.NewConsoleOAuth(nil) }},
		{kind: providerapi.KindCopilot, make: func() providerapi.Provider { return copilot.NewOAuth(nil) }},
		{kind: providerapi.KindAntigravity, make: func() providerapi.Provider { return antigravity.NewOAuth(nil) }},
		{kind: providerapi.KindKiro, make: func() providerapi.Provider { return kiro.NewOAuth(nil) }},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			repository := &memoryImportRepository{}
			service := ImportService{Repository: repository, Cipher: cipher, Provider: func(kind string) (providerapi.Provider, bool) { return tc.make(), kind == tc.kind }}
			req := contracts.ImportRequest{SourceSystem: "fixture", SourceID: tc.kind, Provider: tc.kind, AuthMode: "oauth", TokenBundle: &contracts.TokenBundle{AccessToken: "access-1", RefreshToken: "refresh-1"}}
			if tc.kind == providerapi.KindAntigravity {
				req.Metadata = map[string]string{"project_id": "fixture-project", "client_secret": "fixture-secret"}
			}
			if tc.kind == providerapi.KindKiro {
				req.Metadata = map[string]string{"profile_arn": "arn:aws:codewhisperer:us-east-1:123456789012:profile/test", "machine_id": "fixture-machine"}
			}
			account, err := service.Import(context.Background(), req)
			if err != nil {
				t.Fatal(err)
			}
			if !repository.saved || repository.locked || account.Credential.AccessToken != "access-1" || account.Credential.Kind != "oauth" || account.FenceEpoch != 1 {
				t.Fatalf("account=%#v saved=%v locked=%v", account, repository.saved, repository.locked)
			}
			if bytes.Contains(repository.record.EncryptedSecret, []byte("refresh-1")) {
				t.Fatal("refresh token is visible in encrypted_secret")
			}
			plaintext, err := cipher.Decrypt(account.ID, repository.record.EncryptedSecret)
			if err != nil {
				t.Fatal(err)
			}
			var bundle contracts.TokenBundle
			if err := json.Unmarshal(plaintext, &bundle); err != nil || bundle.RefreshToken != "refresh-1" {
				t.Fatalf("bundle=%#v err=%v", bundle, err)
			}
			if tc.kind == providerapi.KindKiro && (bundle.Metadata["profile_arn"] == "" || bundle.Metadata["machine_id"] == "") {
				t.Fatalf("Kiro credential metadata was not retained: %#v", bundle.Metadata)
			}
			if tc.kind == providerapi.KindAntigravity && bundle.Metadata["client_secret"] != "fixture-secret" {
				t.Fatalf("Antigravity OAuth client secret was not retained in encrypted bundle: %#v", bundle.Metadata)
			}
		})
	}
}

func TestImportServiceDryRunDoesNotWrite(t *testing.T) {
	cipher, err := credentials.NewCipher(bytes.Repeat([]byte{0x61}, 32))
	if err != nil {
		t.Fatal(err)
	}
	repository := &memoryImportRepository{}
	p := codex.NewOAuth(nil)
	req := contracts.ImportRequest{SourceSystem: "fixture", SourceID: "dry-run", Provider: providerapi.KindCodex, AuthMode: "oauth", TokenBundle: &contracts.TokenBundle{RefreshToken: "refresh-1"}, DryRun: true}
	account, err := (ImportService{Repository: repository, Cipher: cipher, Provider: func(string) (providerapi.Provider, bool) { return p, true }}).Import(context.Background(), req)
	if err != nil || repository.saved || account.ID == "" {
		t.Fatalf("account=%#v saved=%v err=%v", account, repository.saved, err)
	}
}

func TestImportServiceAcceptsRegisteredStaticAPIKeyProfiles(t *testing.T) {
	cipher, err := credentials.NewCipher(bytes.Repeat([]byte{0x71}, 32))
	if err != nil {
		t.Fatal(err)
	}
	builtin.RegisterAll(nil)
	for _, tc := range []struct {
		provider string
		protocol string
	}{
		{provider: providerapi.KindCodex, protocol: "openai_responses"},
		{provider: providerapi.KindClaude, protocol: "anthropic_messages"},
		{provider: providerapi.KindGemini, protocol: "gemini_generate"},
		{provider: providerapi.KindGrok, protocol: "openai_chat"},
		{provider: providerapi.KindWindsurf, protocol: "openai_chat"},
	} {
		t.Run(tc.provider, func(t *testing.T) {
			repository := &memoryImportRepository{}
			req := contracts.ImportRequest{
				SourceSystem: "fixture",
				SourceID:     tc.provider + "-static",
				Provider:     tc.provider,
				AuthMode:     providerapi.AuthModeAPIKey,
				StaticKey:    tc.provider + "-key",
			}
			if tc.provider == providerapi.KindWindsurf {
				req.Metadata = map[string]string{"protocol": "openai_chat", "inference_path": "/configured/chat"}
			}
			account, err := (ImportService{Repository: repository, Cipher: cipher}).Import(context.Background(), req)
			if err != nil {
				t.Fatal(err)
			}
			if !repository.saved || account.Credential.Kind != "static" || account.Credential.AccessToken != tc.provider+"-key" {
				t.Fatalf("account=%#v saved=%v", account, repository.saved)
			}
			if account.Profile.Protocol != tc.protocol {
				t.Fatalf("profile=%#v", account.Profile)
			}
			plaintext, err := cipher.Decrypt(account.ID, repository.record.EncryptedSecret)
			if err != nil {
				t.Fatal(err)
			}
			var bundle contracts.TokenBundle
			if err := json.Unmarshal(plaintext, &bundle); err != nil || bundle.AccessToken != tc.provider+"-key" {
				t.Fatalf("bundle=%#v err=%v", bundle, err)
			}
		})
	}
}

func TestImportedAccountIDIsStableAndSourceScoped(t *testing.T) {
	first := importedAccountID("new-api", "57")
	if first != importedAccountID("new-api", "57") || first == importedAccountID("other", "57") {
		t.Fatal("imported account ID is not deterministic and source-scoped")
	}
}
