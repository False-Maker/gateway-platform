package gateway

import "os"

type Config struct {
	RedisAddr     string
	RedisUsername string
	RedisPassword string
	ListenAddr    string
	ProducerID    string
	// Provider is only the fallback for leases that carry no provider; the
	// gateway serves every bucket control publishes. Tenant identity comes
	// from verified bearer tokens, never from configuration.
	Provider string
}

func ConfigFromEnv() Config {
	return Config{
		RedisAddr:     getenv("GATEWAY_REDIS_ADDR", "127.0.0.1:6379"),
		RedisUsername: os.Getenv("GATEWAY_REDIS_USERNAME"),
		RedisPassword: os.Getenv("GATEWAY_REDIS_PASSWORD"),
		ListenAddr:    getenv("GATEWAY_LISTEN_ADDR", ":8080"),
		ProducerID:    getenv("GATEWAY_PRODUCER_ID", "gateway-1"),
		Provider:      getenv("GATEWAY_PROVIDER", "apikey"),
	}
}

func getenv(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
