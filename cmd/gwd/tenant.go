package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/elucid/gateway-platform/internal/control"
	"github.com/jackc/pgx/v5/pgxpool"
)

type tenantCreateResult struct {
	TenantID    string `json:"tenant_id"`
	PrincipalID string `json:"principal_id"`
	TokenID     string `json:"token_id"`
	Group       string `json:"group"`
	// Token is printed exactly once; only its SHA-256 is stored.
	Token     string `json:"token"`
	ExpiresAt string `json:"expires_at,omitempty"`
}

// runTenantCreateCommand mints a tenant API token. Control publishes the hash
// to Redis on its next token tick (15s), after which gateway accepts it.
func runTenantCreateCommand(args []string, output io.Writer) error {
	if output == nil {
		return errors.New("tenant output writer is nil")
	}
	flags := flag.NewFlagSet("tenant create", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	tenantID := flags.String("tenant", "", "tenant ID (created if missing)")
	name := flags.String("name", "", "tenant display name (defaults to the ID)")
	principalID := flags.String("principal", "", "principal ID (defaults to <tenant>-default)")
	group := flags.String("group", "default", "account group this token schedules from")
	ttl := flags.Duration("ttl", 0, "token lifetime, e.g. 720h (0 = no expiry)")
	maxConcurrency := flags.Int("max-concurrency", -1, "tenant-wide concurrent request ceiling (0 = unlimited; omit to keep existing)")
	rpm := flags.Int("rpm", -1, "tenant-wide requests per minute (0 = unlimited; omit to keep existing)")
	if err := flags.Parse(args); err != nil {
		return fmt.Errorf("parse tenant flags: %w", err)
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected tenant argument %q", flags.Arg(0))
	}
	if strings.TrimSpace(*tenantID) == "" {
		return errors.New("tenant create requires --tenant=<id>")
	}
	if strings.TrimSpace(*principalID) == "" {
		*principalID = *tenantID + "-default"
	}
	config := control.ConfigFromEnv()
	if strings.TrimSpace(config.DatabaseURL) == "" {
		return errors.New("GATEWAY_DATABASE_URL is required for tenant create")
	}
	ctx := context.Background()
	db, err := pgxpool.New(ctx, config.DatabaseURL)
	if err != nil {
		return fmt.Errorf("connect PostgreSQL: %w", err)
	}
	defer db.Close()
	var expiresAt *time.Time
	result := tenantCreateResult{TenantID: *tenantID, PrincipalID: *principalID, Group: *group}
	if *ttl > 0 {
		at := time.Now().Add(*ttl).UTC()
		expiresAt = &at
		result.ExpiresAt = at.Format(time.RFC3339)
	}
	var limits control.TenantLimitSpec
	if *maxConcurrency >= 0 {
		limits.MaxConcurrency = maxConcurrency
	}
	if *rpm >= 0 {
		limits.RPM = rpm
	}
	tokenID, raw, err := control.CreateTenantTokenWithLimits(ctx, db, *tenantID, *name, *principalID, *group, expiresAt, limits)
	if err != nil {
		return err
	}
	result.TokenID, result.Token = tokenID, raw
	return json.NewEncoder(output).Encode(result)
}
