package wrapper

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/elucid/gateway-platform/pkg/contracts"
	"github.com/redis/go-redis/v9"
)

// Executor is intentionally passed the complete job, including only the
// opaque encrypted input. Implementations must not log or persist it.
type Executor interface {
	Execute(context.Context, contracts.WrapperJob) ([]byte, error)
}

// FixtureExecutor is the P2 local stub. It treats the envelope as opaque and
// returns a copy, so no provider or account credentials are accessed.
type FixtureExecutor struct {
	Delay time.Duration
}

func (e FixtureExecutor) Execute(ctx context.Context, job contracts.WrapperJob) ([]byte, error) {
	if e.Delay > 0 {
		timer := time.NewTimer(e.Delay)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
	output := make([]byte, len(job.EncryptedInput))
	copy(output, job.EncryptedInput)
	return output, nil
}

type Worker struct {
	Queue           Queue
	WorkerID        string
	LeaseTTLSeconds int
	PollInterval    time.Duration
	Executor        Executor
}

func (w Worker) Run(ctx context.Context) error {
	if w.Queue.Redis == nil {
		return errors.New("wrapper worker redis is nil")
	}
	if w.WorkerID == "" {
		return fmt.Errorf("%w: wrapper worker id is empty", contracts.ErrInvalidContract)
	}
	if w.LeaseTTLSeconds <= 0 {
		return fmt.Errorf("%w: wrapper lease ttl must be positive", contracts.ErrInvalidContract)
	}
	if w.PollInterval <= 0 {
		w.PollInterval = 100 * time.Millisecond
	}
	if w.Executor == nil {
		w.Executor = FixtureExecutor{}
	}
	claim := contracts.WrapperClaim{
		SchemaVersion:   contracts.SchemaVersion,
		WorkerID:        w.WorkerID,
		LeaseTTLSeconds: w.LeaseTTLSeconds,
	}
	for {
		if err := ctx.Err(); err != nil {
			return nil
		}
		job, lease, err := w.Queue.Claim(ctx, claim)
		if errors.Is(err, redis.Nil) {
			if err := waitFor(ctx, w.PollInterval); err != nil {
				return nil
			}
			continue
		}
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("wrapper claim: %w", err)
		}

		output, executeErr := w.Executor.Execute(ctx, job)
		if executeErr != nil {
			if ctx.Err() != nil {
				return nil
			}
			failure := contracts.WrapperFailure{
				SchemaVersion: contracts.SchemaVersion,
				JobID:         job.JobID,
				LeaseID:       lease.LeaseID,
				WorkerID:      lease.WorkerID,
				AccountID:     lease.AccountID,
				FenceEpoch:    lease.FenceEpoch,
				Code:          "executor_failed",
				Retryable:     true,
			}
			if err := w.Queue.Fail(ctx, lease, failure); err != nil {
				if ctx.Err() != nil || errors.Is(err, ErrLeaseLost) {
					return nil
				}
				return fmt.Errorf("wrapper fail: %w", err)
			}
			continue
		}
		if ctx.Err() != nil {
			return nil
		}
		completion := contracts.WrapperCompletion{
			SchemaVersion:   contracts.SchemaVersion,
			JobID:           job.JobID,
			LeaseID:         lease.LeaseID,
			WorkerID:        lease.WorkerID,
			AccountID:       lease.AccountID,
			FenceEpoch:      lease.FenceEpoch,
			EncryptedOutput: output,
		}
		if err := w.Queue.Complete(ctx, lease, completion); err != nil {
			if ctx.Err() != nil || errors.Is(err, ErrLeaseLost) {
				return nil
			}
			return fmt.Errorf("wrapper complete: %w", err)
		}
	}
}

func waitFor(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
