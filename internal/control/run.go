package control

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/elucid/gateway-platform/internal/control/provider/builtin"
	"github.com/elucid/gateway-platform/internal/observability"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
)

type Config struct {
	RedisAddr     string
	RedisUsername string
	RedisPassword string
	DatabaseURL   string
	ConsumerID    string
	MetricsAddr   string
	CredentialKey string
}

func ConfigFromEnv() Config {
	return Config{
		RedisAddr:     getenv("GATEWAY_REDIS_ADDR", "127.0.0.1:6379"),
		RedisUsername: os.Getenv("GATEWAY_REDIS_USERNAME"),
		RedisPassword: os.Getenv("GATEWAY_REDIS_PASSWORD"),
		DatabaseURL:   os.Getenv("GATEWAY_DATABASE_URL"),
		ConsumerID:    getenv("GATEWAY_CONSUMER_ID", "control-1"),
		MetricsAddr:   getenv("GATEWAY_METRICS_ADDR", ":9091"),
		CredentialKey: os.Getenv("GATEWAY_CREDENTIAL_KEY"),
	}
}

func getenv(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func Run(cfg Config) error {
	if cfg.DatabaseURL == "" {
		return errors.New("GATEWAY_DATABASE_URL is required for control role")
	}
	if cfg.MetricsAddr == "" {
		cfg.MetricsAddr = ":9091"
	}
	builtin.RegisterAll(nil)
	ctx := context.Background()
	db, err := pgxpool.New(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer db.Close()
	rdb := redis.NewClient(&redis.Options{Addr: cfg.RedisAddr, Username: cfg.RedisUsername, Password: cfg.RedisPassword})
	defer rdb.Close()
	ledger := Ledger{DB: db}
	consumer := eventsConsumer(rdb, cfg.ConsumerID, ledger)
	var refreshLoop *RefreshLoop
	var refreshTicker *time.Ticker
	var refreshC <-chan time.Time
	var quotaLoop *QuotaLoop
	var quotaReconciler *QuotaReconciler
	var quotaTicker *time.Ticker
	var quotaC <-chan time.Time
	var quotaReconcileTicker *time.Ticker
	var quotaReconcileC <-chan time.Time
	if cfg.CredentialKey != "" {
		refreshLoop, err = NewRefreshLoop(db, cfg.CredentialKey)
		if err != nil {
			return err
		}
		refreshTicker = time.NewTicker(time.Minute)
		defer refreshTicker.Stop()
		refreshC = refreshTicker.C
		refreshLoop.QuotaService.Snapshot = RedisQuotaSnapshotPublisher{Redis: rdb}
		quotaLoop = &QuotaLoop{Repository: refreshLoop.Repository, Service: refreshLoop.QuotaService}
		quotaReconciler = &QuotaReconciler{Repository: refreshLoop.Repository, Snapshot: refreshLoop.QuotaService.Snapshot}
		quotaTicker = time.NewTicker(5 * time.Minute)
		defer quotaTicker.Stop()
		quotaC = quotaTicker.C
		quotaReconcileTicker = time.NewTicker(quotaReconcileInterval)
		defer quotaReconcileTicker.Stop()
		quotaReconcileC = quotaReconcileTicker.C
	}
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	trimTicker := time.NewTicker(time.Minute)
	defer trimTicker.Stop()
	metricsErr := make(chan error, 1)
	go func() {
		metricsErr <- http.ListenAndServe(cfg.MetricsAddr, observability.Default)
	}()
	for {
		if err := consumer.RunOnce(ctx); err != nil {
			log.Printf("control consumer iteration failed: %v", err)
		}
		select {
		case err := <-metricsErr:
			return fmt.Errorf("metrics server: %w", err)
		case <-trimTicker.C:
			if err := consumer.Trim(ctx); err != nil {
				log.Printf("control stream trim failed: %v", err)
			}
		case <-refreshC:
			if err := refreshLoop.RunOnce(ctx); err != nil {
				log.Printf("control credential refresh failed: %v", err)
			}
		case <-quotaC:
			if err := quotaLoop.RunOnce(ctx); err != nil {
				log.Printf("control quota refresh failed: %v", err)
			}
			if err := quotaReconciler.RunOnce(ctx); err != nil {
				log.Printf("control quota snapshot reconcile failed: %v", err)
			}
		case <-quotaReconcileC:
			if err := quotaReconciler.RunOnce(ctx); err != nil {
				log.Printf("control quota snapshot reconcile failed: %v", err)
			}
		case <-ticker.C:
		}
	}
}
