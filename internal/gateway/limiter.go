package gateway

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/elucid/gateway-platform/pkg/contracts"
	"github.com/redis/go-redis/v9"
)

var (
	ErrRateLimited       = errors.New("rate limit exceeded")
	ErrTenantRateLimited = errors.New("tenant rate limit exceeded")
)

type Limiter struct{ Redis redis.UniversalClient }

var acquireScript = redis.NewScript(`
local concurrency = redis.call('INCR', KEYS[1])
if concurrency == 1 then redis.call('PEXPIRE', KEYS[1], ARGV[3]) end
if tonumber(ARGV[1]) > 0 and concurrency > tonumber(ARGV[1]) then
  redis.call('DECR', KEYS[1])
  return -1
end
if tonumber(ARGV[2]) > 0 then
  local rpm = redis.call('INCR', KEYS[2])
  if rpm == 1 then redis.call('PEXPIRE', KEYS[2], ARGV[3]) end
  if rpm > tonumber(ARGV[2]) then
    redis.call('DECR', KEYS[1])
    return -2
  end
end
return concurrency
`)

var releaseScript = redis.NewScript(`
local current = redis.call('GET', KEYS[1])
if not current then return 0 end
if tonumber(current) <= 1 then
  redis.call('DEL', KEYS[1])
  return 0
end
return redis.call('DECR', KEYS[1])
`)

var noopRelease = func() {}

// Acquire enforces the per-account limits carried on the lease.
func (l Limiter) Acquire(ctx context.Context, lease contracts.Lease) (func(), error) {
	limits := lease.Limits.WithDefaults()
	key := fmt.Sprintf("gateway:concurrency:%s", lease.AccountID)
	rpmKey := fmt.Sprintf("gateway:rpm:%s:%d", lease.AccountID, time.Now().Unix()/60)
	release, err := l.acquireKeys(ctx, key, rpmKey, limits.MaxConcurrency, limits.RPM, limits.DegradePolicy == contracts.DegradeFailOpen)
	if errors.Is(err, ErrRateLimited) {
		return nil, ErrRateLimited
	}
	return release, err
}

// AcquireTenant enforces the tenant's own concurrency/RPM ceiling, independent
// of which account serves the request. Zero limits are unlimited and cost no
// Redis round-trip. Tenant limits always fail closed: a tenant ceiling exists
// to protect the pool from one caller, so losing Redis must not lift it.
func (l Limiter) AcquireTenant(ctx context.Context, tenantID string, limits TenantLimits) (func(), error) {
	if tenantID == "" || (limits.MaxConcurrency <= 0 && limits.RPM <= 0) {
		return noopRelease, nil
	}
	key := fmt.Sprintf("gateway:tenant:concurrency:%s", tenantID)
	rpmKey := fmt.Sprintf("gateway:tenant:rpm:%s:%d", tenantID, time.Now().Unix()/60)
	release, err := l.acquireKeys(ctx, key, rpmKey, limits.MaxConcurrency, limits.RPM, false)
	if errors.Is(err, ErrRateLimited) {
		return nil, ErrTenantRateLimited
	}
	return release, err
}

func (l Limiter) acquireKeys(ctx context.Context, key, rpmKey string, maxConcurrency, rpm int, failOpen bool) (func(), error) {
	if l.Redis == nil {
		return l.fallback(failOpen)
	}
	result, err := acquireScript.Run(ctx, l.Redis, []string{key, rpmKey}, maxConcurrency, rpm, (2 * time.Minute).Milliseconds()).Int64()
	if err != nil {
		return l.fallback(failOpen)
	}
	if result < 0 {
		return nil, ErrRateLimited
	}
	return func() {
		releaseCtx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = releaseScript.Run(releaseCtx, l.Redis, []string{key}).Err()
	}, nil
}

func (l Limiter) fallback(failOpen bool) (func(), error) {
	if failOpen {
		return noopRelease, nil
	}
	return nil, errors.New("redis unavailable and rate limiting is fail_closed")
}
