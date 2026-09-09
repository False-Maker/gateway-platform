package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// Sticky maps (tenant, session) -> account ID in Redis so a conversation keeps
// hitting the same upstream account (prompt caches, provider-side state).
// Keys are namespaced by tenant, so a session key can never reach another
// tenant's pin (overview §6.4.1). The session key itself is hashed: it is
// client-chosen and must not end up as a raw Redis key.
type Sticky struct {
	Redis redis.UniversalClient
}

const (
	stickyPrefix     = "gateway:sticky:"
	defaultStickyTTL = 5 * time.Minute
	maxSessionKeyLen = 256
)

func stickyKey(tenantID, sessionKey string) string {
	sum := sha256.Sum256([]byte(sessionKey))
	return stickyPrefix + tenantID + ":" + hex.EncodeToString(sum[:16])
}

// Lookup returns the pinned account for a session, or "" when none is pinned
// or Redis is unavailable (a lost pin degrades to normal selection).
func (s Sticky) Lookup(ctx context.Context, tenantID, sessionKey string) string {
	if s.Redis == nil || tenantID == "" || sessionKey == "" {
		return ""
	}
	value, err := s.Redis.Get(ctx, stickyKey(tenantID, sessionKey)).Result()
	if err != nil {
		return ""
	}
	return value
}

// Pin records the account for the session. ttl <= 0 uses the default.
// Errors are swallowed: stickiness is an optimisation, never a correctness gate.
func (s Sticky) Pin(ctx context.Context, tenantID, sessionKey, accountID string, ttl time.Duration) {
	if s.Redis == nil || tenantID == "" || sessionKey == "" || accountID == "" {
		return
	}
	if ttl <= 0 {
		ttl = defaultStickyTTL
	}
	_ = s.Redis.Set(ctx, stickyKey(tenantID, sessionKey), accountID, ttl).Err()
}

// normalizeSessionKey rejects oversized or control-character session keys so
// a client cannot use the header as a side channel.
func normalizeSessionKey(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", nil
	}
	if len(raw) > maxSessionKeyLen {
		return "", errors.New("session key is too long")
	}
	for _, r := range raw {
		if r < 0x20 || r == 0x7f {
			return "", errors.New("session key contains control characters")
		}
	}
	return raw, nil
}
