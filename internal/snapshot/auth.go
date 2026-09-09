package snapshot

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// TokenKey is the Redis hash of active tenant API tokens. field = SHA-256 hex
// of the raw token, value = JSON TokenRecord. Control replaces the hash
// atomically (build under a temporary key, RENAME); gateway only reads it.
// It lives under snap:* so the gateway ACL's read-only snapshot selector covers it.
const TokenKey = "snap:auth:tokens:v1"

// TokenRecord is the gateway-visible identity behind a token hash. It never
// carries the raw token.
type TokenRecord struct {
	TokenID     string    `json:"token_id"`
	TenantID    string    `json:"tenant_id"`
	PrincipalID string    `json:"principal_id"`
	Group       string    `json:"group"`
	ExpiresAt   time.Time `json:"expires_at,omitempty"`
}

func (r TokenRecord) Validate() error {
	for name, value := range map[string]string{"token_id": r.TokenID, "tenant_id": r.TenantID, "principal_id": r.PrincipalID, "group": r.Group} {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("token record missing %s", name)
		}
	}
	return nil
}

// HashToken derives the lookup key for a raw bearer token.
func HashToken(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

// PublishTokens replaces the token hash atomically. An empty set publishes an
// empty hash so revocations propagate; it never leaves the previous set live.
func PublishTokens(ctx context.Context, client redis.UniversalClient, records map[string]TokenRecord) error {
	if client == nil {
		return errors.New("redis client is nil")
	}
	var nonce [8]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return err
	}
	staging := TokenKey + ":staging:" + hex.EncodeToString(nonce[:])
	values := make(map[string]any, len(records)+1)
	values["__published_at"] = time.Now().UTC().Format(time.RFC3339Nano)
	for hash, record := range records {
		if err := record.Validate(); err != nil {
			return err
		}
		if len(hash) != 64 {
			return fmt.Errorf("token hash %q is not a SHA-256 hex digest", hash)
		}
		raw, err := json.Marshal(record)
		if err != nil {
			return err
		}
		values[hash] = string(raw)
	}
	pipe := client.TxPipeline()
	pipe.HSet(ctx, staging, values)
	pipe.Rename(ctx, staging, TokenKey)
	if _, err := pipe.Exec(ctx); err != nil {
		_ = client.Del(ctx, staging).Err()
		return err
	}
	return nil
}

// LoadTokens reads the full token hash. A missing key is an empty set, not an
// error, so a control that has never published simply authenticates nothing.
func LoadTokens(ctx context.Context, client redis.UniversalClient) (map[string]TokenRecord, error) {
	if client == nil {
		return nil, errors.New("redis client is nil")
	}
	values, err := client.HGetAll(ctx, TokenKey).Result()
	if err != nil {
		return nil, err
	}
	records := make(map[string]TokenRecord, len(values))
	for hash, raw := range values {
		if strings.HasPrefix(hash, "__") {
			continue
		}
		var record TokenRecord
		if err := json.Unmarshal([]byte(raw), &record); err != nil {
			return nil, fmt.Errorf("decode token record %s: %w", hash[:8], err)
		}
		if err := record.Validate(); err != nil {
			return nil, err
		}
		records[hash] = record
	}
	return records, nil
}
