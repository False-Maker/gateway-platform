package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/elucid/gateway-platform/internal/control/wrapper"
	"github.com/elucid/gateway-platform/pkg/contracts"
	"github.com/redis/go-redis/v9"
)

type importAuthorizeExecutor struct {
	cipher *wrapper.EnvelopeCipher
	seen   chan contracts.WrapperJob
}

func (e importAuthorizeExecutor) Execute(_ context.Context, job contracts.WrapperJob) ([]byte, error) {
	if _, err := e.cipher.Decrypt(job.JobID, job.EncryptedInput); err != nil {
		return nil, err
	}
	e.seen <- job
	plaintext, err := json.Marshal(struct {
		TokenBundle contracts.TokenBundle `json:"token_bundle"`
	}{TokenBundle: contracts.TokenBundle{AccessToken: "wrapper-access", RefreshToken: "wrapper-refresh"}})
	if err != nil {
		return nil, err
	}
	return e.cipher.Encrypt(job.JobID, plaintext)
}

func TestRunImportCommandEnqueuesPKCEAuthorizeJob(t *testing.T) {
	mini := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	defer rdb.Close()
	envelopeKey := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x35}, 32))
	envelopeCipher, err := wrapper.NewEnvelopeCipher(envelopeKey)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("GATEWAY_REDIS_ADDR", mini.Addr())
	t.Setenv("GATEWAY_REDIS_USERNAME", "")
	t.Setenv("GATEWAY_REDIS_PASSWORD", "")
	t.Setenv("GATEWAY_CREDENTIAL_KEY", base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x48}, 32)))
	t.Setenv("GATEWAY_WRAPPER_ENVELOPE_KEY", envelopeKey)
	t.Setenv("GATEWAY_WRAPPER_AUTHORIZE_TIMEOUT_SECONDS", "3")

	seen := make(chan contracts.WrapperJob, 1)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- (wrapper.Worker{
			Queue:           wrapper.Queue{Redis: rdb},
			WorkerID:        "import-pkce-worker",
			LeaseTTLSeconds: 30,
			PollInterval:    time.Millisecond,
			Executor:        importAuthorizeExecutor{cipher: envelopeCipher, seen: seen},
		}).Run(ctx)
	}()

	file := filepath.Join(t.TempDir(), "import.json")
	payload := `{"source_system":"a3-cli-fixture","source_id":"codex-pkce-1","provider":"codex","auth_mode":"pkce","metadata":{"group":"a3","proxy":"http://proxy.example.test:8080"},"dry_run":true}`
	if err := os.WriteFile(file, []byte(payload), 0o600); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := runImportCommand([]string{"--file", file}, &output); err != nil {
		cancel()
		<-done
		t.Fatal(err)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("worker cancellation: %v", err)
	}
	job := <-seen
	if job.Provider != "codex" || job.Operation != contracts.WrapperAuthorize || job.AccountID != "" || job.FenceEpoch != 0 {
		t.Fatalf("unexpected authorize job: %#v", job)
	}
	plaintext, err := envelopeCipher.Decrypt(job.JobID, job.EncryptedInput)
	if err != nil {
		t.Fatal(err)
	}
	var authorizeInput struct {
		AuthMode string `json:"auth_mode"`
		Proxy    string `json:"proxy"`
	}
	if err := json.Unmarshal(plaintext, &authorizeInput); err != nil {
		t.Fatal(err)
	}
	if authorizeInput.AuthMode != "pkce" || authorizeInput.Proxy != "http://proxy.example.test:8080" {
		t.Fatalf("unexpected wrapper input: %#v", authorizeInput)
	}
	if _, err := (wrapper.Queue{Redis: rdb}).Terminal(context.Background(), job.JobID); err != nil {
		t.Fatalf("authorize terminal: %v", err)
	}
	var result importCommandResult
	if err := json.Unmarshal(output.Bytes(), &result); err != nil {
		t.Fatalf("output=%q err=%v", output.String(), err)
	}
	if result.Provider != "codex" || result.Group != "a3" || result.CredentialKind != "oauth" || !result.DryRun {
		t.Fatalf("unexpected import result: %#v", result)
	}
}
