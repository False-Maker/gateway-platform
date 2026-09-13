package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/elucid/gateway-platform/internal/control/credentials"
	"github.com/elucid/gateway-platform/internal/control/provider"
	"github.com/elucid/gateway-platform/internal/observability"
	"github.com/elucid/gateway-platform/internal/snapshot"
	"github.com/elucid/gateway-platform/pkg/contracts"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
)

type SnapshotBucket struct {
	Platform string
	Group    string
}

// SnapshotRepository returns the account set that control is authoritative for.
// The bucket list includes inactive accounts so a bucket can be published empty
// after its last account is disabled.
type SnapshotRepository interface {
	ListSnapshotBuckets(context.Context) ([]SnapshotBucket, error)
	ListSnapshotAccounts(context.Context, string, string) ([]contracts.Account, error)
}

type PGSnapshotRepository struct {
	DB     *pgxpool.Pool
	Cipher *credentials.Cipher
}

func (r PGSnapshotRepository) ListSnapshotBuckets(ctx context.Context) ([]SnapshotBucket, error) {
	if r.DB == nil {
		return nil, errors.New("snapshot database pool is nil")
	}
	rows, err := r.DB.Query(ctx, `SELECT DISTINCT platform,"group" FROM accounts ORDER BY platform,"group"`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var buckets []SnapshotBucket
	for rows.Next() {
		var bucket SnapshotBucket
		if err := rows.Scan(&bucket.Platform, &bucket.Group); err != nil {
			return nil, err
		}
		buckets = append(buckets, bucket)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return buckets, nil
}

func (r PGSnapshotRepository) ListSnapshotAccounts(ctx context.Context, platform, group string) ([]contracts.Account, error) {
	if r.DB == nil {
		return nil, errors.New("snapshot database pool is nil")
	}
	if r.Cipher == nil {
		return nil, errors.New("snapshot credential cipher is nil")
	}
	rows, err := r.DB.Query(ctx, `
		SELECT a.id,a.provider,a.platform,a."group",a.status,a.fence_epoch,
		       a.base_url,a.profile,a.limits,a.quota,a.capabilities,a.proxy,a.excluded_models,
		       c.kind,c.encrypted_secret,c.expires_at,c.version
		FROM accounts a
		JOIN credentials c ON c.account_id=a.id
		WHERE a.platform=$1 AND a."group"=$2 AND a.status='active'
		  AND (a.cooldown_until IS NULL OR a.cooldown_until <= now())
		ORDER BY a.id`, platform, group)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var accounts []contracts.Account
	for rows.Next() {
		var (
			account                            contracts.Account
			baseURL, credentialKind            string
			profileJSON, limitsJSON, quotaJSON []byte
			capabilitiesJSON, encryptedSecret  []byte
			excludedModelsJSON                 []byte
			proxy                              string
			databaseExpiresAt                  *time.Time
			credentialVersion                  int64
		)
		if err := rows.Scan(
			&account.ID, &account.Provider, &account.Platform, &account.Group,
			&account.Status, &account.FenceEpoch, &baseURL, &profileJSON,
			&limitsJSON, &quotaJSON, &capabilitiesJSON, &proxy, &excludedModelsJSON, &credentialKind,
			&encryptedSecret, &databaseExpiresAt, &credentialVersion,
		); err != nil {
			return nil, err
		}
		plaintext, err := r.Cipher.Decrypt(account.ID, encryptedSecret)
		if err != nil {
			return nil, fmt.Errorf("decrypt credential for account %s: %w", account.ID, err)
		}
		var bundle contracts.TokenBundle
		if err := json.Unmarshal(plaintext, &bundle); err != nil {
			return nil, fmt.Errorf("decode credential for account %s: %w", account.ID, err)
		}
		account.Credential = contracts.Credential{
			Kind:        credentialKind,
			AccessToken: bundle.AccessToken,
			Proxy:       proxy,
			Metadata:    snapshotCredentialMetadata(bundle.Metadata),
			ExpiresAt:   bundle.ExpiresAt,
			Version:     credentialVersion,
		}
		if databaseExpiresAt != nil {
			account.Credential.ExpiresAt = databaseExpiresAt.UTC()
		}
		if err := decodeSnapshotJSON(account.ID, "profile", profileJSON, &account.Profile); err != nil {
			return nil, err
		}
		if account.Profile.BaseURL == "" {
			account.Profile.BaseURL = baseURL
		}
		if proxy != "" {
			account.Profile.Proxy = proxy
		}
		if err := decodeSnapshotJSON(account.ID, "limits", limitsJSON, &account.Limits); err != nil {
			return nil, err
		}
		account.Limits = account.Limits.WithDefaults()
		if err := decodeSnapshotJSON(account.ID, "quota", quotaJSON, &account.Quota); err != nil {
			return nil, err
		}
		if err := decodeSnapshotJSON(account.ID, "capabilities", capabilitiesJSON, &account.Capabilities); err != nil {
			return nil, err
		}
		if err := decodeSnapshotJSON(account.ID, "excluded_models", excludedModelsJSON, &account.ExcludedModels); err != nil {
			return nil, err
		}
		accounts = append(accounts, account)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return accounts, nil
}

func snapshotCredentialMetadata(source map[string]string) map[string]string {
	if len(source) == 0 {
		return nil
	}
	metadata := make(map[string]string, 3)
	for _, key := range []string{"project_id", "profile_arn", "machine_id", "region"} {
		if value := strings.TrimSpace(source[key]); value != "" {
			metadata[key] = value
		}
	}
	if len(metadata) == 0 {
		return nil
	}
	return metadata
}

func decodeSnapshotJSON(accountID, field string, raw []byte, target any) error {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	if err := json.Unmarshal(raw, target); err != nil {
		return fmt.Errorf("decode %s for account %s: %w", field, accountID, err)
	}
	return nil
}

const snapshotRefreshInterval = time.Minute

type SnapshotLoop struct {
	Repository SnapshotRepository
	Redis      redis.UniversalClient
}

func (l SnapshotLoop) RunOnce(ctx context.Context) error {
	if l.Repository == nil {
		return errors.New("snapshot repository is not configured")
	}
	if l.Redis == nil {
		return errors.New("snapshot redis client is nil")
	}
	if guard, ok := l.Repository.(StarvationGuard); ok {
		if released, err := guard.ReleaseStarvedCooldowns(ctx, time.Now().UTC()); err != nil {
			return fmt.Errorf("release starved cooldowns: %w", err)
		} else if len(released) > 0 {
			log.Printf("control released oldest cooldown on starved platforms: %v", released)
		}
	}
	buckets, err := l.Repository.ListSnapshotBuckets(ctx)
	if err != nil {
		return fmt.Errorf("list snapshot buckets: %w", err)
	}
	sort.Slice(buckets, func(i, j int) bool {
		if buckets[i].Platform == buckets[j].Platform {
			return buckets[i].Group < buckets[j].Group
		}
		return buckets[i].Platform < buckets[j].Platform
	})
	registered, err := snapshot.RegisteredBuckets(ctx, l.Redis)
	if err != nil {
		return fmt.Errorf("list registered snapshot buckets: %w", err)
	}
	current := make(map[string]struct{}, len(buckets))
	for _, bucket := range buckets {
		current[bucket.Platform+"\x00"+bucket.Group] = struct{}{}
	}
	var publishErr error
	for _, bucket := range buckets {
		if err := ctx.Err(); err != nil {
			return err
		}
		accounts, err := l.Repository.ListSnapshotAccounts(ctx, bucket.Platform, bucket.Group)
		if err != nil {
			publishErr = errors.Join(publishErr, fmt.Errorf("load snapshot accounts for %s/%s: %w", bucket.Platform, bucket.Group, err))
			continue
		}
		accounts = l.stampUsageIntegrity(accounts, bucket)
		expectedEpoch, err := currentSnapshotEpoch(ctx, l.Redis, bucket.Platform, bucket.Group)
		if err != nil {
			publishErr = errors.Join(publishErr, fmt.Errorf("read snapshot epoch for %s/%s: %w", bucket.Platform, bucket.Group, err))
			continue
		}
		if _, err := (snapshot.Publisher{Redis: l.Redis}).Publish(ctx, bucket.Platform, bucket.Group, expectedEpoch, accounts); err != nil {
			publishErr = errors.Join(publishErr, fmt.Errorf("publish snapshot for %s/%s: %w", bucket.Platform, bucket.Group, err))
		}
	}
	for _, bucket := range registered {
		if _, exists := current[bucket.Platform+"\x00"+bucket.Group]; exists {
			continue
		}
		expectedEpoch, err := currentSnapshotEpoch(ctx, l.Redis, bucket.Platform, bucket.Group)
		if err != nil {
			publishErr = errors.Join(publishErr, fmt.Errorf("read deleted snapshot epoch for %s/%s: %w", bucket.Platform, bucket.Group, err))
			continue
		}
		if _, err := (snapshot.Publisher{Redis: l.Redis}).Publish(ctx, bucket.Platform, bucket.Group, expectedEpoch, nil); err != nil {
			publishErr = errors.Join(publishErr, fmt.Errorf("publish deleted snapshot for %s/%s: %w", bucket.Platform, bucket.Group, err))
			continue
		}
		if err := snapshot.RemoveRegisteredBucket(ctx, l.Redis, bucket.Platform, bucket.Group); err != nil {
			publishErr = errors.Join(publishErr, fmt.Errorf("remove deleted snapshot bucket %s/%s: %w", bucket.Platform, bucket.Group, err))
		}
	}
	return publishErr
}

// stampUsageIntegrity enforces overview §6.2's admission rule: "P0 为每个已注册
// provider 必须显式配置 usage_integrity（没有配置不得进入可调度快照）".
//
// The gate lives here rather than in the SQL of ListSnapshotAccounts because the
// policy is registry knowledge, not a column -- §6.2 makes it a property of the
// provider, so storing it per account would let two accounts of one provider
// disagree. Putting it on the publish path instead of inside one repository
// implementation means every repository, including test fakes, is gated.
//
// Dropping accounts is the fail-closed direction and it is deliberate: serving
// traffic we have no stated way to meter is how unbillable usage is produced in
// the first place. The drop is logged per provider and counted, because an
// unregistered provider silently emptying a bucket would look exactly like
// "no accounts configured".
func (l SnapshotLoop) stampUsageIntegrity(accounts []contracts.Account, bucket SnapshotBucket) []contracts.Account {
	if len(accounts) == 0 {
		return accounts
	}
	kept := accounts[:0]
	dropped := make(map[string]int)
	for _, account := range accounts {
		policy, ok := provider.UsageIntegrityFor(account.Provider)
		if !ok {
			dropped[account.Provider]++
			continue
		}
		account.UsageIntegrity = policy
		kept = append(kept, account)
	}
	for providerKind, count := range dropped {
		log.Printf("control excluded %d account(s) of provider %q from snapshot %s/%s: no usage_integrity declared (overview §6.2)",
			count, providerKind, bucket.Platform, bucket.Group)
		observability.Default.AddCounter("control_snapshot_accounts_dropped_total", float64(count),
			"provider", providerKind, "reason", "usage_integrity_unset")
	}
	return kept
}

func currentSnapshotEpoch(ctx context.Context, client redis.UniversalClient, platform, group string) (int64, error) {
	_, _, _, epochKey, _, _ := snapshot.Keys(snapshot.BucketID(platform, group))
	value, err := client.Get(ctx, epochKey).Result()
	if errors.Is(err, redis.Nil) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	epoch, err := strconv.ParseInt(value, 10, 64)
	if err != nil || epoch < 0 {
		return 0, fmt.Errorf("invalid snapshot epoch %q", value)
	}
	return epoch, nil
}

var _ SnapshotRepository = PGSnapshotRepository{}
