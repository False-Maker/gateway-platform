package contracts

import (
	"encoding/json"
	"errors"
	"testing"
	"time"
)

func TestWrapperRefreshRequiresFence(t *testing.T) {
	job := WrapperJob{
		SchemaVersion:  SchemaVersion,
		JobID:          "job-1",
		Provider:       "codex",
		Operation:      WrapperRefresh,
		AccountID:      "account-1",
		FenceEpoch:     2,
		EncryptedInput: []byte("ciphertext"),
	}
	if err := job.Validate(); err != nil {
		t.Fatal(err)
	}
	job.FenceEpoch = 0
	if err := job.Validate(); !errors.Is(err, ErrInvalidContract) {
		t.Fatalf("got %v, want invalid contract", err)
	}
}

func TestWrapperLeaseAndTerminalMessagesValidate(t *testing.T) {
	lease := WrapperLease{SchemaVersion: SchemaVersion, JobID: "job-1", LeaseID: "lease-1", WorkerID: "worker-1", AccountID: "account-1", FenceEpoch: 2, ExpiresAt: time.Now().Add(time.Minute)}
	if err := lease.Validate(); err != nil {
		t.Fatal(err)
	}
	claim := WrapperClaim{SchemaVersion: SchemaVersion, WorkerID: lease.WorkerID, LeaseTTLSeconds: 30}
	if err := claim.Validate(); err != nil {
		t.Fatal(err)
	}
	completion := WrapperCompletion{SchemaVersion: SchemaVersion, JobID: lease.JobID, LeaseID: lease.LeaseID, WorkerID: lease.WorkerID, AccountID: lease.AccountID, FenceEpoch: lease.FenceEpoch, EncryptedOutput: []byte("ciphertext")}
	if err := completion.ValidateLease(lease); err != nil {
		t.Fatal(err)
	}
	failure := WrapperFailure{SchemaVersion: SchemaVersion, JobID: lease.JobID, LeaseID: lease.LeaseID, WorkerID: lease.WorkerID, AccountID: lease.AccountID, FenceEpoch: lease.FenceEpoch, Code: "oauth_failed", Retryable: true}
	if err := failure.ValidateLease(lease); err != nil {
		t.Fatal(err)
	}
	stale := completion
	stale.FenceEpoch--
	if err := stale.ValidateLease(lease); !errors.Is(err, ErrInvalidContract) {
		t.Fatalf("stale completion got %v, want invalid contract", err)
	}
	failure.LeaseID = ""
	if err := failure.Validate(); !errors.Is(err, ErrInvalidContract) {
		t.Fatalf("got %v, want invalid contract", err)
	}
}

func TestWrapperJobJSONUsesOpaqueBase64Payload(t *testing.T) {
	job := WrapperJob{SchemaVersion: SchemaVersion, JobID: "job-1", Provider: "claude", Operation: WrapperAuthorize, EncryptedInput: []byte{0, 1, 2}}
	raw, err := json.Marshal(job)
	if err != nil {
		t.Fatal(err)
	}
	var decoded WrapperJob
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	if string(decoded.EncryptedInput) != string(job.EncryptedInput) {
		t.Fatalf("payload changed across JSON encoding: %v", decoded.EncryptedInput)
	}
	if err := decoded.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestWrapperContractsRejectOversizedEncryptedPayload(t *testing.T) {
	job := WrapperJob{SchemaVersion: SchemaVersion, JobID: "job-1", Provider: "codex", Operation: WrapperAuthorize, EncryptedInput: make([]byte, MaxWrapperEnvelopeBytes+1)}
	if err := job.Validate(); !errors.Is(err, ErrInvalidContract) {
		t.Fatalf("got %v, want invalid contract", err)
	}
}
