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
	t.Setenv("GATEWAY_WRAPPER_FIXTURE_DELAY_MS", "40")
	t.Setenv("GATEWAY_REDIS_USERNAME", "control-user")
	t.Setenv("GATEWAY_REDIS_PASSWORD", "control-secret")

	cfg := ConfigFromEnv()
	if cfg.RedisAddr != "redis.example:6380" || cfg.RedisUsername != "wrapper-user" || cfg.RedisPassword != "wrapper-secret" {
		t.Fatalf("unexpected wrapper Redis config: %#v", cfg)
	}
	if cfg.WorkerID != "worker-test" || cfg.LeaseTTLSeconds != 17 || cfg.PollInterval != 25*time.Millisecond || cfg.FixtureDelay != 40*time.Millisecond {
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
