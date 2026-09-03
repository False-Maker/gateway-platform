package contracts

import (
	"fmt"
	"strings"
	"time"
)

const MaxWrapperEnvelopeBytes = 1 << 20

type WrapperOperation string

const (
	WrapperAuthorize WrapperOperation = "authorize"
	WrapperRefresh   WrapperOperation = "refresh"
)

func (o WrapperOperation) Valid() bool {
	return o == WrapperAuthorize || o == WrapperRefresh
}

// WrapperJob is the language-neutral control-to-worker request. Sensitive
// input is an opaque encrypted envelope and must not be copied into logs.
type WrapperJob struct {
	SchemaVersion  int              `json:"schema_version"`
	JobID          string           `json:"job_id"`
	Provider       string           `json:"provider"`
	Operation      WrapperOperation `json:"operation"`
	AccountID      string           `json:"account_id,omitempty"`
	FenceEpoch     int64            `json:"fence_epoch,omitempty"`
	EncryptedInput []byte           `json:"encrypted_input"`
}

func (j WrapperJob) Validate() error {
	if !supportedSchemaVersion(j.SchemaVersion) {
		return fmt.Errorf("%w: unsupported schema_version %d", ErrInvalidContract, j.SchemaVersion)
	}
	if !validWrapperID(j.JobID) || strings.TrimSpace(j.Provider) == "" || len(j.Provider) > 64 || !j.Operation.Valid() || len(j.EncryptedInput) == 0 || len(j.EncryptedInput) > MaxWrapperEnvelopeBytes {
		return fmt.Errorf("%w: incomplete wrapper job", ErrInvalidContract)
	}
	if !validFenceIdentity(j.AccountID, j.FenceEpoch) {
		return fmt.Errorf("%w: invalid wrapper job fence", ErrInvalidContract)
	}
	if j.Operation == WrapperRefresh && (strings.TrimSpace(j.AccountID) == "" || j.FenceEpoch <= 0) {
		return fmt.Errorf("%w: refresh requires account_id and fence_epoch", ErrInvalidContract)
	}
	return nil
}

type WrapperClaim struct {
	SchemaVersion   int    `json:"schema_version"`
	WorkerID        string `json:"worker_id"`
	LeaseTTLSeconds int    `json:"lease_ttl_seconds"`
}

func (c WrapperClaim) Validate() error {
	if !supportedSchemaVersion(c.SchemaVersion) || !validWrapperID(c.WorkerID) || c.LeaseTTLSeconds <= 0 {
		return fmt.Errorf("%w: invalid wrapper claim", ErrInvalidContract)
	}
	return nil
}

type WrapperLease struct {
	SchemaVersion int       `json:"schema_version"`
	JobID         string    `json:"job_id"`
	LeaseID       string    `json:"lease_id"`
	WorkerID      string    `json:"worker_id"`
	AccountID     string    `json:"account_id,omitempty"`
	FenceEpoch    int64     `json:"fence_epoch,omitempty"`
	ExpiresAt     time.Time `json:"expires_at"`
}

func (l WrapperLease) Validate() error {
	if !supportedSchemaVersion(l.SchemaVersion) || !validWrapperID(l.JobID) || !validWrapperID(l.LeaseID) || !validWrapperID(l.WorkerID) || l.ExpiresAt.IsZero() || !validFenceIdentity(l.AccountID, l.FenceEpoch) {
		return fmt.Errorf("%w: invalid wrapper lease", ErrInvalidContract)
	}
	return nil
}

type WrapperCompletion struct {
	SchemaVersion   int    `json:"schema_version"`
	JobID           string `json:"job_id"`
	LeaseID         string `json:"lease_id"`
	WorkerID        string `json:"worker_id"`
	AccountID       string `json:"account_id,omitempty"`
	FenceEpoch      int64  `json:"fence_epoch,omitempty"`
	EncryptedOutput []byte `json:"encrypted_output"`
}

func (c WrapperCompletion) Validate() error {
	if !validWrapperResult(c.SchemaVersion, c.JobID, c.LeaseID, c.WorkerID, c.AccountID, c.FenceEpoch) || len(c.EncryptedOutput) == 0 || len(c.EncryptedOutput) > MaxWrapperEnvelopeBytes {
		return fmt.Errorf("%w: invalid wrapper completion", ErrInvalidContract)
	}
	return nil
}

func (c WrapperCompletion) ValidateLease(lease WrapperLease) error {
	if err := c.Validate(); err != nil {
		return err
	}
	return validateWrapperLeaseResult(lease, c.JobID, c.LeaseID, c.WorkerID, c.AccountID, c.FenceEpoch)
}

type WrapperFailure struct {
	SchemaVersion int    `json:"schema_version"`
	JobID         string `json:"job_id"`
	LeaseID       string `json:"lease_id"`
	WorkerID      string `json:"worker_id"`
	AccountID     string `json:"account_id,omitempty"`
	FenceEpoch    int64  `json:"fence_epoch,omitempty"`
	Code          string `json:"code"`
	Retryable     bool   `json:"retryable"`
}

func (f WrapperFailure) Validate() error {
	if !validWrapperResult(f.SchemaVersion, f.JobID, f.LeaseID, f.WorkerID, f.AccountID, f.FenceEpoch) || strings.TrimSpace(f.Code) == "" || len(f.Code) > 128 {
		return fmt.Errorf("%w: invalid wrapper failure", ErrInvalidContract)
	}
	return nil
}

func (f WrapperFailure) ValidateLease(lease WrapperLease) error {
	if err := f.Validate(); err != nil {
		return err
	}
	return validateWrapperLeaseResult(lease, f.JobID, f.LeaseID, f.WorkerID, f.AccountID, f.FenceEpoch)
}

func validWrapperResult(schemaVersion int, jobID, leaseID, workerID, accountID string, fenceEpoch int64) bool {
	return supportedSchemaVersion(schemaVersion) && validWrapperID(jobID) && validWrapperID(leaseID) && validWrapperID(workerID) && validFenceIdentity(accountID, fenceEpoch)
}

func validateWrapperLeaseResult(lease WrapperLease, jobID, leaseID, workerID, accountID string, fenceEpoch int64) error {
	if err := lease.Validate(); err != nil {
		return err
	}
	if jobID != lease.JobID || leaseID != lease.LeaseID || workerID != lease.WorkerID || accountID != lease.AccountID || fenceEpoch != lease.FenceEpoch {
		return fmt.Errorf("%w: wrapper result does not match lease", ErrInvalidContract)
	}
	return nil
}

func validFenceIdentity(accountID string, fenceEpoch int64) bool {
	if strings.TrimSpace(accountID) == "" {
		return fenceEpoch == 0
	}
	return len(accountID) <= 256 && fenceEpoch > 0
}

func validWrapperID(value string) bool {
	return strings.TrimSpace(value) != "" && len(value) <= 256
}
