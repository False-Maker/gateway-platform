package gateway

import "testing"

func TestConfigFromEnvIncludesRedisACL(t *testing.T) {
	t.Setenv("GATEWAY_REDIS_ADDR", "redis.example:6380")
	t.Setenv("GATEWAY_REDIS_USERNAME", "gateway")
	t.Setenv("GATEWAY_REDIS_PASSWORD", "secret")
	t.Setenv("GATEWAY_PROVIDER", "claude")

	cfg := ConfigFromEnv()
	if cfg.RedisAddr != "redis.example:6380" || cfg.RedisUsername != "gateway" || cfg.RedisPassword != "secret" {
		t.Fatalf("unexpected Redis config: addr=%q username=%q password_set=%t", cfg.RedisAddr, cfg.RedisUsername, cfg.RedisPassword != "")
	}
	if cfg.Provider != "claude" {
		t.Fatalf("unexpected provider: %q", cfg.Provider)
	}
}
