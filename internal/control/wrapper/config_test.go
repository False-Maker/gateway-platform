package wrapper

import (
	"strings"
	"testing"
	"time"
)

func TestConfigFromEnvUsesDedicatedWrapperRedisCredentials(t *testing.T) {
	t.Setenv("GATEWAY_WRAPPER_REDIS_ADDR", "redis.example:6380")
	t.Setenv("GATEWAY_WRAPPER_REDIS_USERNAME", "wrapper-user")
	t.Setenv("GATEWAY_WRAPPER_REDIS_PASSWORD", "wrapper-secret")
	t.Setenv("GATEWAY_WRAPPER_WORKER_ID", "worker-test")
	t.Setenv("GATEWAY_WRAPPER_LEASE_TTL_SECONDS", "17")
	t.Setenv("GATEWAY_WRAPPER_POLL_INTERVAL_MS", "25")
	t.Setenv("GATEWAY_WRAPPER_EXECUTOR", "fixture")
	t.Setenv("GATEWAY_WRAPPER_ENVELOPE_KEY", "encoded-envelope-key")
	t.Setenv("GATEWAY_WRAPPER_CALLBACK_TIMEOUT_SECONDS", "41")
	t.Setenv("GATEWAY_WRAPPER_FIXTURE_DELAY_MS", "40")
	t.Setenv("GATEWAY_REDIS_USERNAME", "control-user")
	t.Setenv("GATEWAY_REDIS_PASSWORD", "control-secret")

	cfg := ConfigFromEnv()
	if cfg.RedisAddr != "redis.example:6380" || cfg.RedisUsername != "wrapper-user" || cfg.RedisPassword != "wrapper-secret" {
		t.Fatalf("unexpected wrapper Redis config: %#v", cfg)
	}
	if cfg.WorkerID != "worker-test" || cfg.LeaseTTLSeconds != 17 || cfg.PollInterval != 25*time.Millisecond || cfg.ExecutorMode != ExecutorFixture || cfg.EnvelopeKey != "encoded-envelope-key" || cfg.CallbackTimeout != 41*time.Second || cfg.FixtureDelay != 40*time.Millisecond {
		t.Fatalf("unexpected wrapper runtime config: %#v", cfg)
	}
}

func TestConfigFromEnvDoesNotFallBackToGatewayRedisCredentials(t *testing.T) {
	t.Setenv("GATEWAY_WRAPPER_REDIS_USERNAME", "")
	t.Setenv("GATEWAY_WRAPPER_REDIS_PASSWORD", "")
	t.Setenv("GATEWAY_REDIS_USERNAME", "gateway-user")
	t.Setenv("GATEWAY_REDIS_PASSWORD", "gateway-secret")
	cfg := ConfigFromEnv()
	if cfg.RedisUsername != "" || cfg.RedisPassword != "" {
		t.Fatalf("wrapper config reused gateway credentials: %#v", cfg)
	}
}

func TestExecutorFromConfigRequiresEnvelopeKeyOutsideFixtureMode(t *testing.T) {
	if _, err := executorFromConfig(Config{ExecutorMode: ExecutorOAuth, LeaseTTLSeconds: 600}); err == nil {
		t.Fatal("OAuth executor accepted an empty envelope key")
	}
	if _, err := executorFromConfig(Config{ExecutorMode: ExecutorFixture, AllowFixtureExecutor: true}); err != nil {
		t.Fatalf("fixture executor should be selectable once explicitly confirmed: %v", err)
	}
}

// E2: a single misconfigured env var must not be enough to put a worker on the
// stub. The stub completes every job, so the failure is silent -- the whole
// point of this check is that it happens at startup instead.
func TestFixtureExecutorRequiresASecondExplicitConfirmation(t *testing.T) {
	executor, err := executorFromConfig(Config{ExecutorMode: ExecutorFixture})
	if err == nil {
		t.Fatalf("fixture executor was selected without confirmation: %#v", executor)
	}
	if !strings.Contains(err.Error(), "GATEWAY_WRAPPER_ALLOW_FIXTURE") {
		t.Fatalf("refusal does not name the confirmation it wants: %v", err)
	}
}

func TestConfigFromEnvDoesNotAllowFixtureByDefault(t *testing.T) {
	t.Setenv("GATEWAY_WRAPPER_ALLOW_FIXTURE", "")
	if ConfigFromEnv().AllowFixtureExecutor {
		t.Fatal("fixture executor is allowed by default")
	}
	t.Setenv("GATEWAY_WRAPPER_ALLOW_FIXTURE", "1")
	if ConfigFromEnv().AllowFixtureExecutor {
		t.Fatal("a truthy-looking value other than \"true\" enabled the fixture executor")
	}
	t.Setenv("GATEWAY_WRAPPER_ALLOW_FIXTURE", "true")
	if !ConfigFromEnv().AllowFixtureExecutor {
		t.Fatal("explicit confirmation was not honoured")
	}
}
