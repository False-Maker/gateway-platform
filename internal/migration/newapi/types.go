package newapi

import (
	"encoding/json"
	"time"

	"github.com/elucid/gateway-platform/pkg/contracts"
)

const SourceSystem = "new-api"

// SourceSnapshot contains values read from a new-api database. Optional
// fields are kept as strings because deployed new-api installations use both
// PostgreSQL and MySQL-compatible JSON/text columns.
type SourceSnapshot struct {
	CapturedAt     time.Time
	Channels       []SourceChannel
	Users          []SourceUser
	Tokens         []SourceToken
	Quota          []SourceQuota
	Groups         []SourceGroup
	SchemaWarnings []string
}

type SourceChannel struct {
	ID           int64
	Type         int
	Key          string
	Status       int
	Name         string
	BaseURL      string
	Models       string
	Group        string
	UsedQuota    string
	Balance      string
	Setting      string
	ModelMapping string
	ChannelInfo  string
}

type SourceUser struct {
	ID        int64
	Username  string
	Email     string
	Status    int
	Quota     string
	UsedQuota string
	Group     string
}

type SourceToken struct {
	ID          int64
	UserID      int64
	Status      int
	ExpiredTime string
	RemainQuota string
	Group       string
	Key         string
}

type SourceQuota struct {
	ID        int64
	UserID    int64
	Model     string
	CreatedAt string
	TokenUsed string
	Count     string
	Quota     string
	Group     string
}

type SourceGroup struct {
	ID     string
	Name   string
	Status string
}

type PlannedAccount struct {
	SourceID     string
	Account      contracts.Account
	Bundle       contracts.TokenBundle
	Proxy        string
	ModelMapping json.RawMessage
	RawSummary   map[string]any
	Conversion   map[string]any
}

type MigrationRecord struct {
	SourceID        string
	RecordKind      string
	SourceDigest    string
	RawSummary      map[string]any
	Conversion      map[string]any
	TargetID        string
	RejectionReason string
	Status          string
}

type Plan struct {
	SourceSystem string
	Snapshot     SourceSnapshot
	Accounts     []PlannedAccount
	Records      []MigrationRecord
	Summary      Summary
}

type Summary struct {
	SourceSystem        string         `json:"source_system"`
	SourceSnapshotAt    time.Time      `json:"source_snapshot_at"`
	SourceDigest        string         `json:"source_digest"`
	RecordCount         int            `json:"record_count"`
	AccountCount        int            `json:"account_count"`
	ImportedCount       int            `json:"imported_count"`
	RejectedCount       int            `json:"rejected_count"`
	ProviderCounts      map[string]int `json:"provider_counts"`
	StatusCounts        map[string]int `json:"status_counts"`
	UnknownChannelTypes map[string]int `json:"unknown_channel_types"`
	RejectionReasons    map[string]int `json:"rejection_reasons"`
	DuplicateSourceKeys int            `json:"duplicate_source_keys"`
	GroupCounts         map[string]int `json:"group_counts"`
	QuotaRows           int            `json:"quota_rows"`
	QuotaRawDigest      string         `json:"quota_raw_digest"`
	SchemaWarnings      []string       `json:"schema_warnings,omitempty"`
}
