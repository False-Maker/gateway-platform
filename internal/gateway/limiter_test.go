package gateway

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/elucid/gateway-platform/pkg/contracts"
	"github.com/redis/go-redis/v9"
)

func TestLimiterFailOpenReturnsSafeRelease(t *testing.T) {
	release, err := (Limiter{}).Acquire(context.Background(), contracts.Lease{Limits: contracts.AccountLimits{DegradePolicy: contracts.DegradeFailOpen}})
	if err != nil || release == nil {
		t.Fatalf("fail-open acquire returned nil release or error: %v", err)
	}
	release()
}

func TestLimiterUsesAtomicLimitsAndTTL(t *testing.T) {
	mini := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	limiter := Limiter{Redis: rdb}
	lease := contracts.Lease{AccountID: "account", Limits: contracts.AccountLimits{MaxConcurrency: 1, DegradePolicy: contracts.DegradeFailClosed}}
	release, err := limiter.Acquire(context.Background(), lease)
	if err != nil {
		t.Fatal(err)
	}
	if ttl := rdb.TTL(context.Background(), "gateway:concurrency:account").Val(); ttl <= 0 {
		t.Fatalf("concurrency key has no TTL: %v", ttl)
	}
	if _, err := limiter.Acquire(context.Background(), lease); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("second concurrent acquire = %v", err)
	}
	mini.FastForward(2 * time.Minute)
	release()
	if rdb.Exists(context.Background(), "gateway:concurrency:account").Val() != 0 {
		t.Fatal("release recreated an expired concurrency key")
	}
}
