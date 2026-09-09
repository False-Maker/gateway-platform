package gateway

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/elucid/gateway-platform/internal/snapshot"
	"github.com/redis/go-redis/v9"
)

var ErrUnauthorized = errors.New("tenant authentication required")

// AuthContext is the gateway-internal identity derived from a verified tenant
// token. Request bodies cannot override any of these fields.
type AuthContext struct {
	TenantID    string
	PrincipalID string
	TokenID     string
	Group       string
}

// Authenticator resolves bearer tokens against the control-published token
// snapshot. It never sees raw token storage; only hashes travel through Redis.
type Authenticator struct {
	Redis redis.UniversalClient
	Now   func() time.Time

	mu      sync.RWMutex
	records map[string]snapshot.TokenRecord
	loaded  bool
}

func NewAuthenticator(client redis.UniversalClient) *Authenticator {
	return &Authenticator{Redis: client, records: make(map[string]snapshot.TokenRecord)}
}

// Refresh reloads the token snapshot. A failed refresh keeps the last-known
// set so a Redis blip does not log every tenant out; revocations still
// propagate on the next successful refresh.
func (a *Authenticator) Refresh(ctx context.Context) error {
	records, err := snapshot.LoadTokens(ctx, a.Redis)
	if err != nil {
		return err
	}
	a.mu.Lock()
	a.records = records
	a.loaded = true
	a.mu.Unlock()
	return nil
}

// Replace installs an explicit token set (tests and static configurations).
func (a *Authenticator) Replace(records map[string]snapshot.TokenRecord) {
	copied := make(map[string]snapshot.TokenRecord, len(records))
	for hash, record := range records {
		copied[hash] = record
	}
	a.mu.Lock()
	a.records = copied
	a.loaded = true
	a.mu.Unlock()
}

// Authenticate verifies a raw bearer token. Empty, unknown, or expired tokens
// fail closed with ErrUnauthorized.
func (a *Authenticator) Authenticate(raw string) (AuthContext, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return AuthContext{}, ErrUnauthorized
	}
	hash := snapshot.HashToken(raw)
	a.mu.RLock()
	record, ok := a.records[hash]
	a.mu.RUnlock()
	if !ok {
		return AuthContext{}, ErrUnauthorized
	}
	now := time.Now
	if a.Now != nil {
		now = a.Now
	}
	if !record.ExpiresAt.IsZero() && !record.ExpiresAt.After(now()) {
		return AuthContext{}, ErrUnauthorized
	}
	group := record.Group
	if group == "" {
		group = "default"
	}
	return AuthContext{TenantID: record.TenantID, PrincipalID: record.PrincipalID, TokenID: record.TokenID, Group: group}, nil
}

// AuthenticateRequest extracts the bearer token from the Authorization header
// or, for Anthropic-style clients, the x-api-key header.
func (a *Authenticator) AuthenticateRequest(r *http.Request) (AuthContext, error) {
	if r == nil {
		return AuthContext{}, ErrUnauthorized
	}
	raw := r.Header.Get("Authorization")
	if strings.HasPrefix(strings.ToLower(raw), "bearer ") {
		raw = raw[len("bearer "):]
	} else if raw == "" {
		raw = r.Header.Get("x-api-key")
	} else {
		return AuthContext{}, ErrUnauthorized
	}
	return a.Authenticate(raw)
}
