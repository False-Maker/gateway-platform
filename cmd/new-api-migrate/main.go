// Command new-api-migrate imports a new-api PostgreSQL snapshot into the
// gateway control-plane schema. It is deliberately dry-run by default.
package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/elucid/gateway-platform/internal/control/credentials"
	"github.com/elucid/gateway-platform/internal/migration/newapi"
	"github.com/jackc/pgx/v5/pgxpool"
)

func main() {
	if err := run(context.Background(), os.Args[1:], os.Getenv); err != nil {
		fmt.Fprintln(os.Stderr, "new-api-migrate:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, getenv func(string) string) error {
	flags := flag.NewFlagSet("new-api-migrate", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	sourceURL := flags.String("source-database-url", getenv("GATEWAY_NEW_API_DATABASE_URL"), "read-only new-api PostgreSQL URL")
	targetURL := flags.String("target-database-url", getenv("GATEWAY_DATABASE_URL"), "gateway PostgreSQL URL (required with --apply)")
	keyValue := flags.String("credential-key", getenv("GATEWAY_CREDENTIAL_KEY"), "base64 AES-256 credential key (required with --apply)")
	schema := flags.String("schema", "public", "source PostgreSQL schema")
	apply := flags.Bool("apply", false, "write the converted plan to the target database")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if strings.TrimSpace(*sourceURL) == "" {
		return errors.New("source database URL is required (GATEWAY_NEW_API_DATABASE_URL)")
	}
	if *apply && strings.TrimSpace(*targetURL) == "" {
		return errors.New("target database URL is required with --apply")
	}
	if *apply && strings.TrimSpace(*keyValue) == "" {
		return errors.New("credential key is required with --apply")
	}

	config, err := pgxpool.ParseConfig(*sourceURL)
	if err != nil {
		return fmt.Errorf("parse source database URL: %w", err)
	}
	sourceDB, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		return fmt.Errorf("connect source database: %w", err)
	}
	defer sourceDB.Close()
	readCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	snapshot, err := newapi.LoadSnapshot(readCtx, sourceDB, *schema)
	if err != nil {
		return err
	}
	plan, err := newapi.BuildPlan(snapshot)
	if err != nil {
		return fmt.Errorf("build migration plan: %w", err)
	}
	if *apply {
		key, err := base64.StdEncoding.Strict().DecodeString(strings.TrimSpace(*keyValue))
		if err != nil {
			return errors.New("credential key must be valid base64")
		}
		cipher, err := credentials.NewCipher(key)
		if err != nil {
			return fmt.Errorf("credential key: %w", err)
		}
		targetConfig, err := pgxpool.ParseConfig(*targetURL)
		if err != nil {
			return fmt.Errorf("parse target database URL: %w", err)
		}
		targetDB, err := pgxpool.NewWithConfig(ctx, targetConfig)
		if err != nil {
			return fmt.Errorf("connect target database: %w", err)
		}
		defer targetDB.Close()
		if err := newapi.Apply(readCtx, targetDB, cipher, plan, ""); err != nil {
			return err
		}
	}
	output := plan.Summary
	outputBytes, err := json.MarshalIndent(output, "", "  ")
	if err != nil {
		return err
	}
	_, err = os.Stdout.Write(append(outputBytes, '\n'))
	return err
}
