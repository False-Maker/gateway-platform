package control

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/elucid/gateway-platform/internal/control/credentials"
	providerapi "github.com/elucid/gateway-platform/internal/control/provider"
	"github.com/elucid/gateway-platform/internal/control/provider/codex"
	"github.com/elucid/gateway-platform/internal/observability"
	"github.com/elucid/gateway-platform/internal/snapshot"
	"github.com/elucid/gateway-platform/pkg/contracts"
	"github.com/redis/go-redis/v9"
)

func (r *memoryCredentialRepository) CommitQuota(_ context.Context, stored StoredCredential, quota contracts.QuotaInfo) error {
	if !r.locked || stored.FenceEpoch != r.stored.FenceEpoch {
		return ErrFenceLost
	}
	r.quota = quota
	r.quotaSet = true
	return nil
}

func TestQuotaServiceFencesAndPublishesSnapshot(t *testing.T) {
	cipher, err := credentials.NewCipher(bytes.Repeat([]byte{0x52}, 32))
	if err != nil {
		t.Fatal(err)
	}
	plaintext, err := json.Marshal(contracts.TokenBundle{AccessToken: "quota-access"})
	if err != nil {
		t.Fatal(err)
	}
	encrypted, err := cipher.Encrypt("account-quota", plaintext)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/quota" || r.Header.Get("Authorization") != "Bearer quota-access" {
			t.Fatalf("quota request method=%s path=%s headers=%#v", r.Method, r.URL.Path, r.Header)
		}
		_, _ = io.WriteString(w, `{"items":[{"scope":"account","unit":"token","limit":100,"remaining":64}]}`)
	}))
	defer server.Close()
	p := codex.NewOAuth(server.Client())
	p.Config.APIBaseURL = server.URL
	p.Config.QuotaPath = "/quota"
	repository := &memoryCredentialRepository{stored: StoredCredential{AccountID: "account-quota", Provider: providerapi.KindCodex, Platform: "codex", Group: "default", CredentialKind: "oauth", FenceEpoch: 4, EncryptedSecret: encrypted}}
	mini := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	oldRemaining := int64(10)
	old := contracts.Account{ID: "account-quota", Provider: "codex", Platform: "codex", Group: "default", Status: "active", Quota: contracts.QuotaInfo{Items: []contracts.QuotaItem{{Scope: "account", Unit: "token", Remaining: &oldRemaining}}}}
	if _, err := (snapshot.Publisher{Redis: rdb}).Publish(context.Background(), "codex", "default", 0, []contracts.Account{old}); err != nil {
		t.Fatal(err)
	}
	service := QuotaService{
		Repository: repository,
		Cipher:     cipher,
		Provider:   func(string) (providerapi.Provider, bool) { return p, true },
		Snapshot:   RedisQuotaSnapshotPublisher{Redis: rdb},
	}
	got, err := service.Refresh(context.Background(), "account-quota")
	if err != nil {
		t.Fatal(err)
	}
	if !repository.quotaSet || len(got.Items) != 1 || *repository.quota.Items[0].Remaining != 64 || repository.locked {
		t.Fatalf("quota=%#v stored=%#v locked=%v", got, repository.quota, repository.locked)
	}
	loaded, err := (snapshot.Loader{Redis: rdb}).Load(context.Background(), "codex", "default")
	if err != nil || loaded.Version != 2 || len(loaded.Accounts) != 1 || *loaded.Accounts[0].Quota.Items[0].Remaining != 64 {
		t.Fatalf("snapshot=%#v err=%v", loaded, err)
	}
}

func TestQuotaServicePreservesMissingQuotaAndPersistsProviderFailure(t *testing.T) {
	cipher, err := credentials.NewCipher(bytes.Repeat([]byte{0x62}, 32))
	if err != nil {
		t.Fatal(err)
	}
	plaintext, _ := json.Marshal(contracts.TokenBundle{AccessToken: "quota-access"})
	encrypted, err := cipher.Encrypt("account-quota", plaintext)
	if err != nil {
		t.Fatal(err)
	}
	missingRepo := &memoryCredentialRepository{stored: StoredCredential{AccountID: "account-quota", Provider: providerapi.KindCodex, CredentialKind: "oauth", FenceEpoch: 1, EncryptedSecret: encrypted}}
	missingProvider := codex.NewOAuth(nil)
	if _, err := (QuotaService{Repository: missingRepo, Cipher: cipher, Provider: func(string) (providerapi.Provider, bool) { return missingProvider, true }}).Refresh(context.Background(), "account-quota"); err != nil || missingRepo.quotaSet {
		t.Fatalf("missing quota err=%v committed=%v", err, missingRepo.quotaSet)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":"invalid_token"}`)
	}))
	defer server.Close()
	failingProvider := codex.NewOAuth(server.Client())
	failingProvider.Config.APIBaseURL = server.URL
	failingProvider.Config.QuotaPath = "/quota"
	failingRepo := &memoryCredentialRepository{stored: StoredCredential{AccountID: "account-quota", Provider: providerapi.KindCodex, CredentialKind: "oauth", FenceEpoch: 2, EncryptedSecret: encrypted}}
	_, err = (QuotaService{Repository: failingRepo, Cipher: cipher, Provider: func(string) (providerapi.Provider, bool) { return failingProvider, true }}).Refresh(context.Background(), "account-quota")
	if providerapi.ErrorClass(err) != contracts.ErrorAuthInvalid || failingRepo.failure != contracts.ErrorAuthInvalid || failingRepo.quotaSet {
		t.Fatalf("quota failure err=%v class=%s failure=%s committed=%v", err, providerapi.ErrorClass(err), failingRepo.failure, failingRepo.quotaSet)
	}
}

func TestRedisQuotaSnapshotPublisherRejectsMissingAccount(t *testing.T) {
	mini := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	account := contracts.Account{ID: "other-account", Provider: "codex", Platform: "codex", Group: "default", Status: "active"}
	if _, err := (snapshot.Publisher{Redis: rdb}).Publish(context.Background(), "codex", "default", 0, []contracts.Account{account}); err != nil {
		t.Fatal(err)
	}
	stored := StoredCredential{AccountID: "account-quota", Platform: "codex", Group: "default"}
	if err := (RedisQuotaSnapshotPublisher{Redis: rdb}).PublishQuota(context.Background(), stored, contracts.QuotaInfo{Items: []contracts.QuotaItem{{Scope: "account", Unit: "token"}}}); !errors.Is(err, contracts.ErrInvalidContract) {
		t.Fatalf("missing account error = %v", err)
	}
}

func TestQuotaLoopUsesBoundedAccountBatch(t *testing.T) {
	cipher, err := credentials.NewCipher(bytes.Repeat([]byte{0x72}, 32))
	if err != nil {
		t.Fatal(err)
	}
	plaintext, _ := json.Marshal(contracts.TokenBundle{AccessToken: "quota-access"})
	encrypted, err := cipher.Encrypt("account-quota", plaintext)
	if err != nil {
		t.Fatal(err)
	}
	repository := &memoryCredentialRepository{stored: StoredCredential{AccountID: "account-quota", Provider: providerapi.KindCodex, CredentialKind: "oauth", FenceEpoch: 3, EncryptedSecret: encrypted}}
	providerStub := &quotaCountingProvider{}
	listCalled := false
	loop := QuotaLoop{
		Service: QuotaService{Repository: repository, Cipher: cipher, Provider: func(string) (providerapi.Provider, bool) { return providerStub, true }},
		List: func(_ context.Context, limit int) ([]string, error) {
			listCalled = true
			if limit != quotaBatchSize {
				t.Fatalf("quota batch limit = %d", limit)
			}
			return []string{"account-quota"}, nil
		},
	}
	if err := loop.RunOnce(context.Background()); err != nil || !listCalled || providerStub.calls != 1 {
		t.Fatalf("quota loop err=%v list_called=%v provider_calls=%d", err, listCalled, providerStub.calls)
	}
}

func TestQuotaReconcilerRetriesAndFencesOutbox(t *testing.T) {
	repository := &memoryQuotaOutboxRepository{
		memoryCredentialRepository: memoryCredentialRepository{stored: StoredCredential{AccountID: "account-reconcile", FenceEpoch: 7}},
		outbox:                     []QuotaOutboxItem{{AccountID: "account-reconcile", Platform: "codex", Group: "default", FenceEpoch: 7, Quota: contracts.QuotaInfo{Items: []contracts.QuotaItem{{Scope: "account", Unit: "token"}}}}},
	}
	publisher := &flakyQuotaSnapshotPublisher{fail: true}
	now := time.Unix(100, 0)
	metrics := observability.NewRegistry()
	reconciler := QuotaReconciler{Repository: repository, Snapshot: publisher, Metrics: metrics, Now: func() time.Time { return now }}
	if err := reconciler.RunOnce(context.Background()); err == nil || repository.retryCount != 1 || publisher.calls != 1 {
		t.Fatalf("failed reconcile err=%v retries=%d publishes=%d", err, repository.retryCount, publisher.calls)
	}
	if repository.outbox[0].Attempts != 1 || repository.retryCount != 1 {
		t.Fatalf("retry state=%#v", repository.outbox[0])
	}
	recorder := httptest.NewRecorder()
	metrics.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if !strings.Contains(recorder.Body.String(), `quota_snapshot_reconcile_total{result="retry"} 1`) {
		t.Fatalf("missing retry metric: %s", recorder.Body.String())
	}
	publisher.fail = false
	if err := reconciler.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if repository.ackCount != 1 || len(repository.outbox) != 0 || publisher.calls != 2 {
		t.Fatalf("successful reconcile ack=%d outbox=%#v publishes=%d", repository.ackCount, repository.outbox, publisher.calls)
	}

	stale := &memoryQuotaOutboxRepository{
		memoryCredentialRepository: memoryCredentialRepository{stored: StoredCredential{AccountID: "account-stale", FenceEpoch: 8}},
		outbox:                     []QuotaOutboxItem{{AccountID: "account-stale", Platform: "codex", Group: "default", FenceEpoch: 7}},
	}
	stalePublisher := &flakyQuotaSnapshotPublisher{}
	if err := (QuotaReconciler{Repository: stale, Snapshot: stalePublisher, Now: func() time.Time { return now }}).RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if stalePublisher.calls != 0 || stale.ackCount != 1 || len(stale.outbox) != 0 {
		t.Fatalf("stale reconcile publishes=%d ack=%d outbox=%#v", stalePublisher.calls, stale.ackCount, stale.outbox)
	}
}

func TestQuotaServiceQueuesSnapshotAfterImmediatePublishFailure(t *testing.T) {
	cipher, err := credentials.NewCipher(bytes.Repeat([]byte{0x82}, 32))
	if err != nil {
		t.Fatal(err)
	}
	plaintext, err := json.Marshal(contracts.TokenBundle{AccessToken: "quota-access"})
	if err != nil {
		t.Fatal(err)
	}
	encrypted, err := cipher.Encrypt("account-queue", plaintext)
	if err != nil {
		t.Fatal(err)
	}
	remaining := int64(64)
	repository := &memoryQuotaOutboxRepository{memoryCredentialRepository: memoryCredentialRepository{stored: StoredCredential{AccountID: "account-queue", Provider: providerapi.KindCodex, Platform: "codex", Group: "default", CredentialKind: "oauth", FenceEpoch: 2, EncryptedSecret: encrypted}}}
	publisher := &flakyQuotaSnapshotPublisher{fail: true}
	service := QuotaService{
		Repository: repository,
		Cipher:     cipher,
		Provider: func(string) (providerapi.Provider, bool) {
			return &quotaCountingProvider{quota: contracts.QuotaInfo{Items: []contracts.QuotaItem{{Scope: "account", Unit: "token", Remaining: &remaining}}}}, true
		},
		Snapshot: publisher,
	}
	if _, err := service.Refresh(context.Background(), "account-queue"); err == nil {
		t.Fatal("snapshot failure was accepted")
	}
	if !repository.quotaSet || len(repository.outbox) != 1 || repository.locked {
		t.Fatalf("failed publish quota_set=%v outbox=%#v locked=%v", repository.quotaSet, repository.outbox, repository.locked)
	}
	publisher.fail = false
	if err := (QuotaReconciler{Repository: repository, Snapshot: publisher}).RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(repository.outbox) != 0 || repository.ackCount != 1 || publisher.calls != 2 {
		t.Fatalf("reconciled outbox=%#v ack=%d publishes=%d", repository.outbox, repository.ackCount, publisher.calls)
	}
}

func TestQuotaReconcilerHonorsContextCancellation(t *testing.T) {
	repository := &memoryQuotaOutboxRepository{
		memoryCredentialRepository: memoryCredentialRepository{stored: StoredCredential{AccountID: "account-cancel", FenceEpoch: 1}},
		outbox:                     []QuotaOutboxItem{{AccountID: "account-cancel", Platform: "codex", Group: "default", FenceEpoch: 1}},
	}
	publisher := &flakyQuotaSnapshotPublisher{cancel: true}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := (QuotaReconciler{Repository: repository, Snapshot: publisher}).RunOnce(ctx)
	if err == nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled reconcile err=%v", err)
	}
	if repository.ackCount != 0 || repository.retryCount != 0 {
		t.Fatalf("cancelled reconcile ack=%d retries=%d", repository.ackCount, repository.retryCount)
	}
}

func TestRedisQuotaSnapshotPublisherIsIdempotentForSameQuota(t *testing.T) {
	mini := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	remaining := int64(64)
	quota := contracts.QuotaInfo{Items: []contracts.QuotaItem{{Scope: "account", Unit: "token", Remaining: &remaining}}}
	account := contracts.Account{ID: "account-quota", Provider: "codex", Platform: "codex", Group: "default", Status: "active", Quota: quota}
	if _, err := (snapshot.Publisher{Redis: rdb}).Publish(context.Background(), "codex", "default", 0, []contracts.Account{account}); err != nil {
		t.Fatal(err)
	}
	stored := StoredCredential{AccountID: account.ID, Platform: account.Platform, Group: account.Group}
	if err := (RedisQuotaSnapshotPublisher{Redis: rdb}).PublishQuota(context.Background(), stored, quota); err != nil {
		t.Fatal(err)
	}
	loaded, err := (snapshot.Loader{Redis: rdb}).Load(context.Background(), "codex", "default")
	if err != nil || loaded.Version != 1 || loaded.Epoch != 1 {
		t.Fatalf("idempotent snapshot=%#v err=%v", loaded, err)
	}
}

func TestQuotaOutboxBackoffAndErrorState(t *testing.T) {
	for _, tc := range []struct {
		attempts int
		want     time.Duration
	}{
		{attempts: 0, want: quotaOutboxMinBackoff},
		{attempts: 2, want: 2 * quotaOutboxMinBackoff},
		{attempts: 99, want: quotaOutboxMaxBackoff},
	} {
		if got := quotaOutboxBackoff(tc.attempts); got != tc.want {
			t.Fatalf("backoff attempts=%d got=%s want=%s", tc.attempts, got, tc.want)
		}
	}
	if got := quotaOutboxErrorState(errors.New("token=secret")); got != "snapshot_publish_failed" {
		t.Fatalf("unexpected generic error state %q", got)
	}
}

type memoryQuotaOutboxRepository struct {
	memoryCredentialRepository
	outbox      []QuotaOutboxItem
	retryCount  int
	ackCount    int
	currentTime time.Time
}

func (r *memoryQuotaOutboxRepository) CommitQuotaAndQueue(ctx context.Context, stored StoredCredential, quota contracts.QuotaInfo) error {
	if err := r.CommitQuota(ctx, stored, quota); err != nil {
		return err
	}
	r.outbox = []QuotaOutboxItem{{AccountID: stored.AccountID, Platform: stored.Platform, Group: stored.Group, FenceEpoch: stored.FenceEpoch, Quota: quota}}
	return nil
}

func (r *memoryQuotaOutboxRepository) ClaimQuotaOutbox(context.Context, int) ([]QuotaOutboxItem, error) {
	if len(r.outbox) == 0 {
		return nil, nil
	}
	item := r.outbox[0]
	item.Attempts++
	r.outbox[0].Attempts = item.Attempts
	return []QuotaOutboxItem{item}, nil
}

func (r *memoryQuotaOutboxRepository) AckQuotaOutbox(_ context.Context, item QuotaOutboxItem) error {
	if len(r.outbox) == 0 || r.outbox[0].FenceEpoch != item.FenceEpoch {
		return ErrFenceLost
	}
	r.outbox = nil
	r.ackCount++
	return nil
}

func (r *memoryQuotaOutboxRepository) RetryQuotaOutbox(_ context.Context, item QuotaOutboxItem, _ error, _ time.Time) error {
	if len(r.outbox) == 0 || r.outbox[0].FenceEpoch != item.FenceEpoch {
		return ErrFenceLost
	}
	r.retryCount++
	return nil
}

func (r *memoryQuotaOutboxRepository) CurrentFence(context.Context, string) (int64, error) {
	return r.stored.FenceEpoch, nil
}

type flakyQuotaSnapshotPublisher struct {
	fail   bool
	cancel bool
	calls  int
}

func (p *flakyQuotaSnapshotPublisher) PublishQuota(ctx context.Context, _ StoredCredential, _ contracts.QuotaInfo) error {
	p.calls++
	if p.cancel {
		return ctx.Err()
	}
	if p.fail {
		return errors.New("snapshot unavailable")
	}
	return nil
}

type quotaCountingProvider struct {
	calls int
	quota contracts.QuotaInfo
}

func (p *quotaCountingProvider) Kind() string { return providerapi.KindCodex }
func (p *quotaCountingProvider) Authorize(context.Context, contracts.ImportRequest) (contracts.TokenBundle, error) {
	return contracts.TokenBundle{}, nil
}
func (p *quotaCountingProvider) Refresh(context.Context, contracts.Credential) (contracts.TokenBundle, error) {
	return contracts.TokenBundle{}, nil
}
func (p *quotaCountingProvider) Revoke(context.Context, contracts.Credential) error { return nil }
func (p *quotaCountingProvider) Profile(contracts.Account) contracts.UpstreamProfile {
	return contracts.UpstreamProfile{}
}
func (p *quotaCountingProvider) Quota(context.Context, contracts.Credential) (contracts.QuotaInfo, error) {
	p.calls++
	return p.quota, nil
}
