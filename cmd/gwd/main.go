package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"time"

	"github.com/elucid/gateway-platform/internal/control"
	"github.com/elucid/gateway-platform/internal/control/credentials"
	"github.com/elucid/gateway-platform/internal/control/provider/builtin"
	"github.com/elucid/gateway-platform/internal/control/wrapper"
	"github.com/elucid/gateway-platform/internal/gateway"
	"github.com/elucid/gateway-platform/pkg/contracts"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "import" {
		if err := runImportCommand(os.Args[2:], os.Stdout); err != nil {
			log.Fatal(err)
		}
		return
	}
	role := flag.String("role", "", "runtime role: gateway, control, or wrapper")
	flag.Parse()

	var err error
	switch *role {
	case "gateway":
		err = gateway.Run(gateway.ConfigFromEnv())
	case "control":
		err = control.Run(control.ConfigFromEnv())
	case "wrapper":
		err = wrapper.Run(wrapper.ConfigFromEnv())
	default:
		args := flag.Args()
		if len(args) == 2 && args[0] == "wrapper" && args[1] == "run" {
			err = wrapper.Run(wrapper.ConfigFromEnv())
			break
		}
		flag.Usage()
		fmt.Fprintln(os.Stderr, "--role must be gateway, control, or wrapper (or use: wrapper run)")
		os.Exit(2)
	}
	if err != nil {
		log.Fatal(err)
	}
}

type importCommandResult struct {
	AccountID         string `json:"account_id"`
	Provider          string `json:"provider"`
	Platform          string `json:"platform"`
	Group             string `json:"group"`
	CredentialKind    string `json:"credential_kind"`
	CredentialVersion int64  `json:"credential_version"`
	FenceEpoch        int64  `json:"fence_epoch"`
	DryRun            bool   `json:"dry_run"`
}

func runImportCommand(args []string, output io.Writer) error {
	if output == nil {
		return errors.New("import output writer is nil")
	}
	flags := flag.NewFlagSet("import", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	filePath := flags.String("file", "", "path to an ImportRequest JSON file")
	dryRun := flags.Bool("dry-run", false, "authorize and validate without writing PostgreSQL")
	if err := flags.Parse(args); err != nil {
		return fmt.Errorf("parse import flags: %w", err)
	}
	if strings.TrimSpace(*filePath) == "" {
		return errors.New("import requires --file=path")
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected import argument %q", flags.Arg(0))
	}
	payload, err := os.ReadFile(*filePath)
	if err != nil {
		return fmt.Errorf("read import file: %w", err)
	}
	var request contracts.ImportRequest
	if err := json.Unmarshal(payload, &request); err != nil {
		return fmt.Errorf("decode import file: %w", err)
	}
	request.DryRun = request.DryRun || *dryRun

	config := control.ConfigFromEnv()
	if strings.TrimSpace(config.CredentialKey) == "" {
		return errors.New("GATEWAY_CREDENTIAL_KEY is required for import")
	}
	key, err := base64.StdEncoding.Strict().DecodeString(config.CredentialKey)
	if err != nil {
		return errors.New("GATEWAY_CREDENTIAL_KEY must be valid base64")
	}
	cipher, err := credentials.NewCipher(key)
	if err != nil {
		return fmt.Errorf("GATEWAY_CREDENTIAL_KEY: %w", err)
	}

	ctx := context.Background()
	if request.AuthMode == "pkce" && request.TokenBundle == nil {
		if err := authorizeImportWithWrapper(ctx, &request, config); err != nil {
			return err
		}
	}
	var (
		db         *pgxpool.Pool
		repository control.ImportRepository
	)
	if !request.DryRun {
		if strings.TrimSpace(config.DatabaseURL) == "" {
			return errors.New("GATEWAY_DATABASE_URL is required for import")
		}
		db, err = pgxpool.New(ctx, config.DatabaseURL)
		if err != nil {
			return fmt.Errorf("connect PostgreSQL: %w", err)
		}
		defer db.Close()
		if err := db.Ping(ctx); err != nil {
			return fmt.Errorf("ping PostgreSQL: %w", err)
		}
		repository = control.PGImportRepository{DB: db}
	}
	builtin.RegisterAll(nil)
	account, err := (control.ImportService{Repository: repository, Cipher: cipher}).Import(ctx, request)
	if err != nil {
		return err
	}
	return json.NewEncoder(output).Encode(importCommandResult{
		AccountID:         account.ID,
		Provider:          account.Provider,
		Platform:          account.Platform,
		Group:             account.Group,
		CredentialKind:    account.Credential.Kind,
		CredentialVersion: account.Credential.Version,
		FenceEpoch:        account.FenceEpoch,
		DryRun:            request.DryRun,
	})
}

func authorizeImportWithWrapper(ctx context.Context, request *contracts.ImportRequest, config control.Config) error {
	if request == nil {
		return errors.New("import request is nil")
	}
	cipher, err := wrapper.NewEnvelopeCipher(config.WrapperEnvelopeKey)
	if err != nil {
		return fmt.Errorf("GATEWAY_WRAPPER_ENVELOPE_KEY: %w", err)
	}
	rdb := redis.NewClient(&redis.Options{
		Addr:     config.RedisAddr,
		Username: config.RedisUsername,
		Password: config.RedisPassword,
	})
	defer rdb.Close()
	timeout := config.WrapperAuthorizeTimeout
	if timeout <= 0 {
		timeout = 6 * time.Minute
	}
	authorizeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	bundle, err := (wrapper.AuthorizeClient{Queue: wrapper.Queue{Redis: rdb}, Cipher: cipher}).AuthorizeWithProxy(authorizeCtx, request.Provider, request.Metadata["proxy"])
	if err != nil {
		return err
	}
	request.TokenBundle = &bundle
	return nil
}
