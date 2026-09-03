package wrapper

import (
	"os"
	"strconv"
	"time"
)

// Config contains only the Redis identity and runtime settings needed by a
// wrapper worker. It deliberately does not read the gateway or control Redis
// credentials.
type Config struct {
	RedisAddr       string
	RedisUsername   string
	RedisPassword   string
	WorkerID        string
	LeaseTTLSeconds int
	PollInterval    time.Duration
	FixtureDelay    time.Duration
}

func ConfigFromEnv() Config {
	return Config{
		RedisAddr:       getenv("GATEWAY_WRAPPER_REDIS_ADDR", "127.0.0.1:6379"),
		RedisUsername:   os.Getenv("GATEWAY_WRAPPER_REDIS_USERNAME"),
		RedisPassword:   os.Getenv("GATEWAY_WRAPPER_REDIS_PASSWORD"),
		WorkerID:        getenv("GATEWAY_WRAPPER_WORKER_ID", "wrapper-1"),
		LeaseTTLSeconds: getenvInt("GATEWAY_WRAPPER_LEASE_TTL_SECONDS", 30),
		PollInterval:    time.Duration(getenvInt("GATEWAY_WRAPPER_POLL_INTERVAL_MS", 100)) * time.Millisecond,
		FixtureDelay:    time.Duration(getenvInt("GATEWAY_WRAPPER_FIXTURE_DELAY_MS", 0)) * time.Millisecond,
	}
}

func getenv(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func getenvInt(key string, fallback int) int {
	value, err := strconv.Atoi(os.Getenv(key))
	if err != nil {
		return fallback
	}
	return value
}
