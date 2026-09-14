package control

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/elucid/gateway-platform/internal/control/provider/builtin"
	"github.com/elucid/gateway-platform/internal/detail"
	"github.com/elucid/gateway-platform/internal/observability"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
)

type Config struct {
	RedisAddr               string
	RedisUsername           string
	RedisPassword           string
	DatabaseURL             string
	ConsumerID              string
	MetricsAddr             string
	CredentialKey           string
	WrapperEnvelopeKey      string
	WrapperAuthorizeTimeout time.Duration

	// DetailClickHouseURL enables A12's request-detail store. Empty means
	// detail is off, which is a supported deployment: detail is droppable by
	// design, so a platform without ClickHouse loses observability, not
	// metering.
	DetailClickHouseURL string
	DetailDatabase      string
	DetailTable         string

	// ConsoleToken enables B5's operator API. Empty means no console at all:
	// the role simply does not serve those routes, so a missing token cannot
	// become an unauthenticated console.
	ConsoleToken string
	// ConsoleReadOnlyToken is B5.1's optional read-only level. Empty means
	// every console caller uses ConsoleToken and may write.
	ConsoleReadOnlyToken string
	// ConsoleAddr defaults to loopback. It is a separate listener from the
	// metrics server, which has no authentication and must not acquire a
	// privileged neighbour on the same port.
	ConsoleAddr string
}

func ConfigFromEnv() Config {
	return Config{
		RedisAddr:               getenv("GATEWAY_REDIS_ADDR", "127.0.0.1:6379"),
		RedisUsername:           os.Getenv("GATEWAY_REDIS_USERNAME"),
		RedisPassword:           os.Getenv("GATEWAY_REDIS_PASSWORD"),
		DatabaseURL:             os.Getenv("GATEWAY_DATABASE_URL"),
		ConsumerID:              getenv("GATEWAY_CONSUMER_ID", "control-1"),
		MetricsAddr:             getenv("GATEWAY_METRICS_ADDR", ":9091"),
		CredentialKey:           os.Getenv("GATEWAY_CREDENTIAL_KEY"),
		WrapperEnvelopeKey:      os.Getenv("GATEWAY_WRAPPER_ENVELOPE_KEY"),
		WrapperAuthorizeTimeout: getenvSeconds("GATEWAY_WRAPPER_AUTHORIZE_TIMEOUT_SECONDS", 6*time.Minute),
		DetailClickHouseURL:     os.Getenv("GATEWAY_DETAIL_CLICKHOUSE_URL"),
		DetailDatabase:          getenv("GATEWAY_DETAIL_DATABASE", detail.DefaultDatabase),
		DetailTable:             getenv("GATEWAY_DETAIL_TABLE", detail.DefaultTable),
		ConsoleToken:            os.Getenv(ConsoleTokenEnv),
		ConsoleReadOnlyToken:    os.Getenv(ConsoleReadOnlyTokenEnv),
		ConsoleAddr:             getenv(ConsoleAddrEnv, defaultConsoleAddr),
	}
}

func getenv(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func getenvSeconds(key string, fallback time.Duration) time.Duration {
	value, err := strconv.Atoi(os.Getenv(key))
	if err != nil || value <= 0 {
		return fallback
	}
	return time.Duration(value) * time.Second
}

func Run(cfg Config) error {
	if cfg.DatabaseURL == "" {
		return errors.New("GATEWAY_DATABASE_URL is required for control role")
	}
	if cfg.CredentialKey == "" {
		return errors.New("GATEWAY_CREDENTIAL_KEY is required for control role")
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
	ledger := Ledger{DB: db, Signals: NewPlatformSignals(observability.Default)}
	// A12: request detail. Wiring failures here are logged and then ignored on
	// purpose -- a control plane that refuses to boot because the detail store
	// is down would have made metering depend on detail, which is precisely
	// what this package is built to prevent.
	detailWriter := detail.ClickHouseWriter{
		URL:      cfg.DetailClickHouseURL,
		Database: cfg.DetailDatabase,
		Table:    cfg.DetailTable,
	}
	if cfg.DetailClickHouseURL != "" {
		schemaCtx, cancelSchema := context.WithTimeout(ctx, 30*time.Second)
		err := detailWriter.EnsureSchema(schemaCtx)
		cancelSchema()
		if err != nil {
			log.Printf("control detail schema unavailable, running without request detail: %v", err)
		} else {
			sink := &detail.Sink{Writer: detailWriter, Metrics: observability.Default}
			sink.Start()
			defer sink.Stop()
			ledger.Detail = sink
		}
	}
	consumer := eventsConsumer(rdb, cfg.ConsumerID, ledger)
	refreshLoop, err := NewRefreshLoop(db, cfg.CredentialKey)
	if err != nil {
		return err
	}
	refreshTicker := time.NewTicker(time.Minute)
	defer refreshTicker.Stop()
	refreshC := refreshTicker.C
	refreshLoop.QuotaService.Snapshot = RedisQuotaSnapshotPublisher{Redis: rdb}
	quotaLoop := &QuotaLoop{Repository: refreshLoop.Repository, Service: refreshLoop.QuotaService}
	quotaReconciler := &QuotaReconciler{Repository: refreshLoop.Repository, Snapshot: refreshLoop.QuotaService.Snapshot}
	quotaTicker := time.NewTicker(5 * time.Minute)
	defer quotaTicker.Stop()
	quotaC := quotaTicker.C
	quotaReconcileTicker := time.NewTicker(quotaReconcileInterval)
	defer quotaReconcileTicker.Stop()
	quotaReconcileC := quotaReconcileTicker.C
	snapshotLoop := &SnapshotLoop{Repository: PGSnapshotRepository{DB: db, Cipher: refreshLoop.Service.Cipher}, Redis: rdb}
	snapshotTicker := time.NewTicker(snapshotRefreshInterval)
	defer snapshotTicker.Stop()
	snapshotC := snapshotTicker.C
	if err := snapshotLoop.RunOnce(ctx); err != nil {
		log.Printf("control initial snapshot publish failed: %v", err)
	}
	tokenLoop := TokenLoop{Repository: PGTenantRepository{DB: db}, Redis: rdb, Metrics: observability.Default}
	tokenTicker := time.NewTicker(tokenRefreshInterval)
	defer tokenTicker.Stop()
	if err := tokenLoop.RunOnce(ctx); err != nil {
		log.Printf("control initial token publish failed: %v", err)
	}
	billingJob := BillingJob{DB: db, Metrics: observability.Default}
	billingTicker := time.NewTicker(billingRunInterval)
	defer billingTicker.Stop()
	// B4.5 shipped the algorithm and B5 an on-demand endpoint, but nothing ran
	// it on a schedule, so "the books do not balance" stayed a question someone
	// had to think to ask. This is the periodic consumer; it is read-only.
	reconcileLoop := ReconciliationLoop{Reconciliation: Reconciliation{DB: db}, Metrics: observability.Default}
	reconcileTicker := time.NewTicker(reconcileRunInterval)
	defer reconcileTicker.Stop()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	trimTicker := time.NewTicker(time.Minute)
	defer trimTicker.Stop()
	metricsErr := make(chan error, 1)
	go func() {
		metricsErr <- http.ListenAndServe(cfg.MetricsAddr, observability.Default)
	}()
	// B5: the operator API is a separate listener with its own credential. It
	// is off unless a token was configured, and its failure takes the role down
	// the same way the metrics server's does -- silently serving without the
	// console would leave an operator believing a held backlog was empty.
	if cfg.ConsoleToken != "" {
		auth := ConsoleAuth{Token: cfg.ConsoleToken, ReadOnlyToken: cfg.ConsoleReadOnlyToken}
		// Refuse to boot on an ambiguous token pair rather than silently
		// picking a reading of it. See ConsoleAuth.Validate.
		if err := auth.Validate(); err != nil {
			return fmt.Errorf("console auth: %w", err)
		}
		console := Console{Auth: auth, DB: db, Detail: detailWriter, Metrics: observability.Default}
		if cfg.ConsoleReadOnlyToken != "" {
			log.Printf("control console: read-only token configured; writes require %s", ConsoleTokenEnv)
		}
		log.Printf("control console listening on %s", cfg.ConsoleAddr)
		go func() {
			metricsErr <- http.ListenAndServe(cfg.ConsoleAddr, console.Handler())
		}()
	} else {
		log.Printf("control console disabled: %s is not set", ConsoleTokenEnv)
	}
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
		case <-snapshotC:
			if err := snapshotLoop.RunOnce(ctx); err != nil {
				log.Printf("control snapshot publish failed: %v", err)
			}
		case <-tokenTicker.C:
			if err := tokenLoop.RunOnce(ctx); err != nil {
				log.Printf("control token publish failed: %v", err)
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
		case <-billingTicker.C:
			billed, err := billingJob.RunOnce(ctx, time.Now())
			if err != nil {
				log.Printf("control billing run %s failed: %v", billed.RunID, err)
			} else if billed.Skipped {
				log.Printf("control billing run %s skipped: %s", billed.RunID, billed.SkipReason)
			} else if billed.RowsBilled > 0 || billed.RowsUnpriced > 0 || billed.RowsHeld > 0 || billed.RowsNotBillable > 0 {
				log.Printf("control billing run %s: %d rows billed (%s debited), %d unpriced, %d held, %d not billable, %d tenants failed",
					billed.RunID, billed.RowsBilled, billed.TotalDebited, billed.RowsUnpriced,
					billed.RowsHeld, billed.RowsNotBillable, billed.TenantsFailed)
			}
			if billed.RowsUnknownClass > 0 {
				log.Printf("control billing run %s saw %d usage rows with no policy entry; they were held, not billed", billed.RunID, billed.RowsUnknownClass)
			}
		case <-reconcileTicker.C:
			reconciled, err := reconcileLoop.RunOnce(ctx, time.Now())
			if err != nil {
				log.Printf("control billing reconciliation failed: %v", err)
			} else if !reconciled.Balanced {
				log.Printf("control billing reconciliation [%s, %s) is unbalanced: %v (%d unsettled rows across %d tenants)",
					reconciled.Start.Format(time.RFC3339), reconciled.End.Format(time.RFC3339),
					reconciled.Discrepancies, reconciled.UnsettledRows, reconciled.TenantCount)
			}
		case <-ticker.C:
		}
	}
}
