package wrapper

import (
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
	if _, err := executorFromConfig(Config{ExecutorMode: ExecutorFixture}); err != nil {
		t.Fatalf("fixture executor should be explicitly selectable: %v", err)
	}
}
