package control

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/elucid/gateway-platform/internal/control/credentials"
	providerapi "github.com/elucid/gateway-platform/internal/control/provider"
	"github.com/elucid/gateway-platform/internal/control/provider/claude"
	"github.com/elucid/gateway-platform/internal/control/provider/codex"
	"github.com/elucid/gateway-platform/pkg/contracts"
)

type memoryCredentialRepository struct {
	stored    StoredCredential
	committed []byte
	locked    bool
	busy      bool
	failure   contracts.ErrorClass
	quota     contracts.QuotaInfo
	quotaSet  bool
}

func (r *memoryCredentialRepository) TryLock(context.Context, string) (func() error, bool, error) {
	if r.busy || r.locked {
		return nil, false, nil
	}
	r.locked = true
	return func() error { r.locked = false; return nil }, true, nil
}

func (r *memoryCredentialRepository) Acquire(context.Context, string) (StoredCredential, error) {
	if !r.locked {
		return StoredCredential{}, errors.New("account lock is not held")
	}
	return r.stored, nil
}

func (r *memoryCredentialRepository) Commit(_ context.Context, stored StoredCredential, encrypted []byte, bundle *contracts.TokenBundle) error {
	if !r.locked || stored.FenceEpoch != r.stored.FenceEpoch || bundle == nil {
		return ErrFenceLost
	}
	r.committed = append([]byte(nil), encrypted...)
	return nil
}

func (r *memoryCredentialRepository) Fail(_ context.Context, stored StoredCredential, class contracts.ErrorClass) error {
	if !r.locked || stored.FenceEpoch != r.stored.FenceEpoch {
		return ErrFenceLost
	}
	r.failure = class
	return nil
}

func TestRefreshServiceEncryptsAndFencesCodexAndClaude(t *testing.T) {
	cipher, err := credentials.NewCipher(bytes.Repeat([]byte{0x42}, 32))
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"new-access","refresh_token":"new-refresh","expires_in":60}`))
	}))
	defer server.Close()
	for _, tc := range []struct {
		kind string
		make func() providerapi.Provider
	}{
		{kind: providerapi.KindCodex, make: func() providerapi.Provider {
			p := codex.NewOAuth(server.Client())
			p.Config.OAuthTokenURL = server.URL
			p.Config.APIBaseURL = server.URL
			return p
		}},
		{kind: providerapi.KindClaude, make: func() providerapi.Provider {
			p := claude.NewConsoleOAuth(server.Client())
			p.Config.OAuthTokenURL = server.URL
			p.Config.APIBaseURL = server.URL
			return p
		}},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			current, err := json.Marshal(contracts.TokenBundle{AccessToken: "old-access", RefreshToken: "old-refresh"})
			if err != nil {
				t.Fatal(err)
			}
			encrypted, err := cipher.Encrypt("account-1", current)
			if err != nil {
				t.Fatal(err)
			}
			repository := &memoryCredentialRepository{stored: StoredCredential{AccountID: "account-1", Provider: tc.kind, FenceEpoch: 7, EncryptedSecret: encrypted}}
			service := RefreshService{Repository: repository, Cipher: cipher, Provider: func(kind string) (providerapi.Provider, bool) { return tc.make(), kind == tc.kind }}
			got, err := service.Refresh(context.Background(), "account-1")
			if err != nil {
				t.Fatal(err)
			}
			if got.AccessToken != "new-access" || got.RefreshToken != "new-refresh" || repository.locked {
				t.Fatalf("bundle=%#v locked=%v", got, repository.locked)
			}
			plaintext, err := cipher.Decrypt("account-1", repository.committed)
			if err != nil {
				t.Fatal(err)
			}
			if bytes.Contains(repository.committed, []byte("new-refresh")) || !bytes.Contains(plaintext, []byte("new-refresh")) {
				t.Fatal("refresh token was not stored only inside the encrypted envelope")
			}
		})
	}
}

func TestRefreshServiceRejectsConcurrentAccountRefresh(t *testing.T) {
	cipher, err := credentials.NewCipher(bytes.Repeat([]byte{0x24}, 32))
	if err != nil {
		t.Fatal(err)
	}
	repository := &memoryCredentialRepository{busy: true}
	_, err = (RefreshService{Repository: repository, Cipher: cipher}).Refresh(context.Background(), "account-1")
	if !errors.Is(err, ErrRefreshBusy) {
		t.Fatalf("got %v, want refresh busy", err)
	}
}

func TestRefreshServicePersistsAuthoritativeFailureClass(t *testing.T) {
	cipher, err := credentials.NewCipher(bytes.Repeat([]byte{0x31}, 32))
	if err != nil {
		t.Fatal(err)
	}
	current, err := json.Marshal(contracts.TokenBundle{AccessToken: "old-access", RefreshToken: "old-refresh"})
	if err != nil {
		t.Fatal(err)
	}
	encrypted, err := cipher.Encrypt("account-1", current)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
	}))
	defer server.Close()
	p := codex.NewOAuth(server.Client())
	p.Config.OAuthTokenURL = server.URL
	p.Config.APIBaseURL = server.URL
	repository := &memoryCredentialRepository{stored: StoredCredential{AccountID: "account-1", Provider: providerapi.KindCodex, FenceEpoch: 7, EncryptedSecret: encrypted}}
	service := RefreshService{Repository: repository, Cipher: cipher, Provider: func(string) (providerapi.Provider, bool) { return p, true }}
	if _, err := service.Refresh(context.Background(), "account-1"); providerapi.ErrorClass(err) != contracts.ErrorAuthInvalid {
		t.Fatalf("error=%v class=%s", err, providerapi.ErrorClass(err))
	}
	if repository.failure != contracts.ErrorAuthInvalid || repository.locked {
		t.Fatalf("failure=%s locked=%v", repository.failure, repository.locked)
	}
}
