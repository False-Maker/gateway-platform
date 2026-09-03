package gateway

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/elucid/gateway-platform/pkg/contracts"
	"github.com/redis/go-redis/v9"
)

var ErrRateLimited = errors.New("rate limit exceeded")

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

func (l Limiter) Acquire(ctx context.Context, lease contracts.Lease) (func(), error) {
	if l.Redis == nil {
		return l.fallback(lease.Limits)
	}
	key := fmt.Sprintf("gateway:concurrency:%s", lease.AccountID)
	rpmKey := fmt.Sprintf("gateway:rpm:%s:%d", lease.AccountID, time.Now().Unix()/60)
	limits := lease.Limits.WithDefaults()
	result, err := acquireScript.Run(ctx, l.Redis, []string{key, rpmKey}, limits.MaxConcurrency, limits.RPM, (2 * time.Minute).Milliseconds()).Int64()
	if err != nil {
		return l.fallback(limits)
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

func (l Limiter) fallback(limits contracts.AccountLimits) (func(), error) {
	if limits.WithDefaults().DegradePolicy == contracts.DegradeFailOpen {
		return noopRelease, nil
	}
	return nil, errors.New("redis unavailable and rate limiting is fail_closed")
}
