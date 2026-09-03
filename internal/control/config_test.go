package control

import "testing"

func TestConfigFromEnvIncludesRedisACL(t *testing.T) {
	t.Setenv("GATEWAY_REDIS_ADDR", "redis.example:6380")
	t.Setenv("GATEWAY_REDIS_USERNAME", "control")
	t.Setenv("GATEWAY_REDIS_PASSWORD", "secret")
	t.Setenv("GATEWAY_CREDENTIAL_KEY", "encoded-key")

	cfg := ConfigFromEnv()
	if cfg.RedisAddr != "redis.example:6380" || cfg.RedisUsername != "control" || cfg.RedisPassword != "secret" {
		t.Fatalf("unexpected Redis config: addr=%q username=%q password_set=%t", cfg.RedisAddr, cfg.RedisUsername, cfg.RedisPassword != "")
	}
	if cfg.CredentialKey != "encoded-key" {
		t.Fatalf("credential key was not loaded")
	}
}
