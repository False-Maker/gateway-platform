package wrapper

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/elucid/gateway-platform/pkg/contracts"
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
	mode := strings.ToLower(strings.TrimSpace(cfg.ExecutorMode))
	switch mode {
	case ExecutorFixture:
		// E2: the fixture executor completes every job by echoing its input
		// back, without contacting any provider. A worker that reaches this
		// branch by accident looks perfectly healthy -- jobs get claimed,
		// completed and ACKed -- while no account is ever authorized. Selecting
		// it therefore takes two independent env vars, not one, so no single
		// misconfiguration can land a production worker on the stub.
		if !cfg.AllowFixtureExecutor {
			return nil, errors.New("GATEWAY_WRAPPER_EXECUTOR=fixture runs a stub that authorizes nothing; set GATEWAY_WRAPPER_ALLOW_FIXTURE=true to confirm this worker is not production")
		}
		log.Printf("wrapper is running the FIXTURE executor: jobs will be completed without any real authorization")
		return FixtureExecutor{Delay: cfg.FixtureDelay}, nil
	case "", ExecutorOAuth, ExecutorDevice:
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
		device := DeviceExecutor{Cipher: cipher, Timeout: callbackTimeout}
		if mode == ExecutorDevice {
			return device, nil
		}
		return authModeRouter{
			Cipher: cipher,
			Executors: map[string]Executor{
				"pkce":         PKCEExecutor{Cipher: cipher, CallbackTimeout: callbackTimeout},
				deviceAuthMode: device,
			},
		}, nil
	default:
		return nil, fmt.Errorf("unsupported GATEWAY_WRAPPER_EXECUTOR %q", cfg.ExecutorMode)
	}
}

// authModeRouter picks an executor by the auth_mode inside the encrypted job
// input. Routing on the envelope rather than on the cleartext provider field
// keeps the decision with the side that owns the flow: control decides which
// flow to run when it enqueues, and a job whose auth_mode nobody handles fails
// explicitly instead of falling through to whichever executor happens to be
// configured.
type authModeRouter struct {
	Cipher    *EnvelopeCipher
	Executors map[string]Executor
}

func (r authModeRouter) Execute(ctx context.Context, job contracts.WrapperJob) ([]byte, error) {
	if err := job.Validate(); err != nil {
		return nil, executionError(ErrorUnsupportedJob, false, err)
	}
	if r.Cipher == nil {
		return nil, executionError(ErrorInvalidEnvelope, false, errors.New("wrapper envelope cipher is not configured"))
	}
	plaintext, err := r.Cipher.Decrypt(job.JobID, job.EncryptedInput)
	if err != nil {
		return nil, executionError(ErrorInvalidEnvelope, false, err)
	}
	var input authorizeInput
	if err := json.Unmarshal(plaintext, &input); err != nil {
		return nil, executionError(ErrorInvalidEnvelope, false, err)
	}
	executor, ok := r.Executors[input.AuthMode]
	if !ok {
		return nil, executionError(ErrorUnsupportedJob, false,
			fmt.Errorf("%w: no wrapper executor handles auth mode %q", contracts.ErrUnsupportedCapability, input.AuthMode))
	}
	return executor.Execute(ctx, job)
}
