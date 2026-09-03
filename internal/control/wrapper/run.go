package wrapper

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/redis/go-redis/v9"
)

// Run starts the standalone wrapper worker. It has no database handle and
// cannot write control-plane state directly.
func Run(cfg Config) error {
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
		Executor:        FixtureExecutor{Delay: cfg.FixtureDelay},
	}).Run(ctx)
}
