package control

import (
	"testing"
	"time"
)

func TestConfigFromEnvIncludesRedisACL(t *testing.T) {
	t.Setenv("GATEWAY_REDIS_ADDR", "redis.example:6380")
	t.Setenv("GATEWAY_REDIS_USERNAME", "control")
	t.Setenv("GATEWAY_REDIS_PASSWORD", "secret")
	t.Setenv("GATEWAY_CREDENTIAL_KEY", "encoded-key")
	t.Setenv("GATEWAY_WRAPPER_ENVELOPE_KEY", "encoded-envelope-key")
	t.Setenv("GATEWAY_WRAPPER_AUTHORIZE_TIMEOUT_SECONDS", "45")

	cfg := ConfigFromEnv()
	if cfg.RedisAddr != "redis.example:6380" || cfg.RedisUsername != "control" || cfg.RedisPassword != "secret" {
		t.Fatalf("unexpected Redis config: addr=%q username=%q password_set=%t", cfg.RedisAddr, cfg.RedisUsername, cfg.RedisPassword != "")
	}
	if cfg.CredentialKey != "encoded-key" {
		t.Fatalf("credential key was not loaded")
	}
	if cfg.WrapperEnvelopeKey != "encoded-envelope-key" || cfg.WrapperAuthorizeTimeout != 45*time.Second {
		t.Fatalf("unexpected wrapper authorize config: %#v", cfg)
	}
}
