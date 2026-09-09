package wrapper

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/redis/go-redis/v9"
)

// Run starts the standalone wrapper worker. It has no database handle and
// cannot write control-plane state directly.
func Run(cfg Config) error {
	executor, err := executorFromConfig(cfg)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	rdb := redis.NewClient(&redis.Options{
		Addr:     cfg.RedisAddr,
		Username: cfg.RedisUsername,
		Password: cfg.RedisPassword,
	})
	defer rdb.Close()
	return (Worker{
		Queue:           Queue{Redis: rdb},
		WorkerID:        cfg.WorkerID,
		LeaseTTLSeconds: cfg.LeaseTTLSeconds,
		PollInterval:    cfg.PollInterval,
		Executor:        executor,
	}).Run(ctx)
}

func executorFromConfig(cfg Config) (Executor, error) {
	switch strings.ToLower(strings.TrimSpace(cfg.ExecutorMode)) {
	case ExecutorFixture:
		return FixtureExecutor{Delay: cfg.FixtureDelay}, nil
	case "", ExecutorOAuth:
		callbackTimeout := cfg.CallbackTimeout
		if callbackTimeout <= 0 {
			callbackTimeout = defaultCallbackTimeout
		}
		leaseTTL := time.Duration(cfg.LeaseTTLSeconds) * time.Second
		if leaseTTL > 0 && callbackTimeout+30*time.Second >= leaseTTL {
			return nil, errors.New("wrapper callback and exchange timeout must be shorter than the lease TTL")
		}
		cipher, err := NewEnvelopeCipher(cfg.EnvelopeKey)
		if err != nil {
			return nil, fmt.Errorf("GATEWAY_WRAPPER_ENVELOPE_KEY: %w", err)
		}
		return PKCEExecutor{Cipher: cipher, CallbackTimeout: callbackTimeout}, nil
	default:
		return nil, fmt.Errorf("unsupported GATEWAY_WRAPPER_EXECUTOR %q", cfg.ExecutorMode)
	}
}
