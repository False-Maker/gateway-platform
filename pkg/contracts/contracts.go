package contracts

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	SchemaVersion = 1

	EventTypeAttemptStarted = "attempt_started"
	EventTypeRelease        = "release"

	UsageSourceUpstream  = "upstream"
	UsageSourceEstimated = "estimated"
	UsageSourceMissing   = "missing"

	DegradeFailClosed = "fail_closed"
	DegradeFailOpen   = "fail_open"
)

var (
	ErrInvalidContract       = errors.New("invalid contract")
	ErrUnsupportedCapability = errors.New("unsupported_capability")
)

// ErrorClass is shared by gateway runtime handling and control persistence.
type ErrorClass string

const (
	ErrorOK                  ErrorClass = "ok"
	ErrorAuthExpired         ErrorClass = "auth_expired"
	ErrorAuthInvalid         ErrorClass = "auth_invalid"
	ErrorForbiddenTransport  ErrorClass = "forbidden_transport"
	ErrorForbiddenCapability ErrorClass = "forbidden_capability"
	ErrorRateLimitedKnown    ErrorClass = "rate_limited_known"
	ErrorRateLimitedUnknown  ErrorClass = "rate_limited_unknown"
	ErrorUpstream5xx         ErrorClass = "upstream_5xx"
	ErrorBlocked             ErrorClass = "blocked"
	ErrorNetwork             ErrorClass = "network_error"
)

func (e ErrorClass) Valid() bool {
	switch e {
	case ErrorOK, ErrorAuthExpired, ErrorAuthInvalid,
		ErrorForbiddenTransport, ErrorForbiddenCapability, ErrorRateLimitedKnown,
		ErrorRateLimitedUnknown, ErrorUpstream5xx, ErrorBlocked, ErrorNetwork:
		return true
	default:
		return false
	}
}

type Criteria struct {
	Platform   string `json:"platform"`
	Model      string `json:"model"`
	Group      string `json:"group"`
	SessionKey string `json:"session_key,omitempty"`
}

type Credential struct {
	Kind        string            `json:"kind"`
	AccessToken string            `json:"access_token"`
	Proxy       string            `json:"proxy,omitempty"`
	Metadata    map[string]string `json:"metadata,omitempty"`
	ExpiresAt   time.Time         `json:"expires_at"`
	Version     int64             `json:"version"`
}

type TokenBundle struct {
	AccessToken  string            `json:"access_token"`
	RefreshToken string            `json:"refresh_token,omitempty"`
	IDToken      string            `json:"id_token,omitempty"`
	ExpiresAt    time.Time         `json:"expires_at"`
	Scopes       []string          `json:"scopes,omitempty"`
	AccountID    string            `json:"account_id,omitempty"`
	Email        string            `json:"email,omitempty"`
	Metadata     map[string]string `json:"metadata,omitempty"`
}

type UpstreamProfile struct {
	BaseURL        string            `json:"base_url"`
	Proxy          string            `json:"proxy,omitempty"`
	Protocol       string            `json:"protocol,omitempty"`
	InferencePath  string            `json:"inference_path,omitempty"`
	TLSFingerprint string            `json:"tls_fingerprint,omitempty"`
	UserAgent      string            `json:"user_agent,omitempty"`
	ExtraHeaders   map[string]string `json:"extra_headers,omitempty"`
	Metadata       map[string]string `json:"metadata,omitempty"`
	WS             bool              `json:"ws,omitempty"`
}

type AccountLimits struct {
	MaxConcurrency int    `json:"max_concurrency"`
	RPM            int    `json:"rpm"`
	StickyTTL      int    `json:"sticky_ttl"`
	DegradePolicy  string `json:"degrade_policy"`
}

func (l AccountLimits) WithDefaults() AccountLimits {
	if l.DegradePolicy == "" {
		l.DegradePolicy = DegradeFailClosed
	}
	return l
}

func (l AccountLimits) Valid() bool {
	l = l.WithDefaults()
	return l.MaxConcurrency >= 0 && l.RPM >= 0 && l.StickyTTL >= 0 &&
		(l.DegradePolicy == DegradeFailClosed || l.DegradePolicy == DegradeFailOpen)
}

type Lease struct {
	AccountID string `json:"account_id"`
	// Provider is the account's provider kind. Gateway derives per-provider
	// request shaping from it so one process can serve mixed buckets.
	Provider   string          `json:"provider,omitempty"`
	Credential Credential      `json:"credential"`
	Profile    UpstreamProfile `json:"profile"`
	Limits     AccountLimits   `json:"limits"`
	TTL        int             `json:"ttl"`
}

type QuotaInfo struct {
	Items []QuotaItem `json:"items,omitempty"`
}

// Decimal preserves a provider's decimal spelling without routing it through
// float64 or an integer. It is serialized as a JSON number, so snapshots and
// PostgreSQL retain the original precision while remaining interoperable.
type Decimal string

var decimalPattern = regexp.MustCompile(`^-?(0|[1-9][0-9]*)(\.[0-9]+)?([eE][+-]?[0-9]+)?$`)

func (d Decimal) Valid() bool {
	raw := strings.TrimSpace(string(d))
	if raw == "" {
		return false
	}
	return decimalPattern.MatchString(raw)
}

func (d Decimal) Sign() (int, bool) {
	if !d.Valid() {
		return 0, false
	}
	v, ok := new(big.Rat).SetString(strings.TrimSpace(string(d)))
	if !ok {
		return 0, false
	}
	return v.Sign(), true
}

func (d Decimal) Sub(other Decimal) (Decimal, bool) {
	if !d.Valid() || !other.Valid() {
		return "", false
	}
	left, ok := new(big.Rat).SetString(strings.TrimSpace(string(d)))
	if !ok {
		return "", false
	}
	right, ok := new(big.Rat).SetString(strings.TrimSpace(string(other)))
	if !ok {
		return "", false
	}
	result := new(big.Rat).Sub(left, right)
	precision := decimalPlaces(d)
	if places := decimalPlaces(other); places > precision {
		precision = places
	}
	return Decimal(result.FloatString(precision)), true
}

func decimalPlaces(d Decimal) int {
	raw := strings.TrimSpace(string(d))
	exponent := 0
	if index := strings.IndexAny(raw, "eE"); index >= 0 {
		exponent, _ = strconv.Atoi(raw[index+1:])
		raw = raw[:index]
	}
	if dot := strings.IndexByte(raw, '.'); dot >= 0 {
		places := len(raw) - dot - 1 - exponent
		if places > 0 {
			return places
		}
		return 0
	}
	if exponent < 0 {
		return -exponent
	}
	return 0
}

func (d Decimal) MarshalJSON() ([]byte, error) {
	if !d.Valid() {
		return nil, fmt.Errorf("%w: invalid decimal %q", ErrInvalidContract, d)
	}
	return []byte(strings.TrimSpace(string(d))), nil
}

func (d *Decimal) UnmarshalJSON(data []byte) error {
	raw := strings.TrimSpace(string(data))
	if len(raw) >= 2 && raw[0] == '"' && raw[len(raw)-1] == '"' {
		var quoted string
		if err := json.Unmarshal(data, &quoted); err != nil {
			return err
		}
		raw = quoted
	}
	value := Decimal(raw)
	if !value.Valid() {
		return fmt.Errorf("%w: invalid decimal %q", ErrInvalidContract, raw)
	}
	*d = value
	return nil
}

type QuotaPrecision struct {
	Used        *Decimal `json:"used,omitempty"`
	Limit       *Decimal `json:"limit,omitempty"`
	Remaining   *Decimal `json:"remaining,omitempty"`
	OverageRate *Decimal `json:"overage_rate,omitempty"`
	OverageCap  *Decimal `json:"overage_cap,omitempty"`
	Overages    *Decimal `json:"overages,omitempty"`
}

type QuotaItem struct {
	Scope             string          `json:"scope"`
	Model             string          `json:"model,omitempty"`
	Unit              string          `json:"unit"`
	Limit             *int64          `json:"limit,omitempty"`
	Remaining         *int64          `json:"remaining,omitempty"`
	LimitExact        *Decimal        `json:"limit_exact,omitempty"`
	RemainingExact    *Decimal        `json:"remaining_exact,omitempty"`
	RemainingFraction *Decimal        `json:"remaining_fraction,omitempty"`
	Precision         *QuotaPrecision `json:"precision,omitempty"`
	ResetAt           *time.Time      `json:"reset_at,omitempty"`
}

type Account struct {
	ID           string          `json:"id"`
	Provider     string          `json:"provider"`
	Platform     string          `json:"platform"`
	Group        string          `json:"group"`
	Credential   Credential      `json:"credential"`
	Profile      UpstreamProfile `json:"profile"`
	Limits       AccountLimits   `json:"limits"`
	Status       string          `json:"status"`
	FenceEpoch   int64           `json:"fence_epoch"`
	Quota        QuotaInfo       `json:"quota,omitempty"`
	Capabilities []string        `json:"capabilities,omitempty"`
	// ExcludedModels lists models control has persistently marked as
	// forbidden_capability for this account. Gateway never schedules them.
	ExcludedModels []string `json:"excluded_models,omitempty"`
}

type ImportRequest struct {
	SourceSystem string            `json:"source_system"`
	SourceID     string            `json:"source_id"`
	Provider     string            `json:"provider"`
	AuthMode     string            `json:"auth_mode"`
	StaticKey    string            `json:"static_key,omitempty"`
	TokenBundle  *TokenBundle      `json:"token_bundle,omitempty"`
	Metadata     map[string]string `json:"metadata,omitempty"`
	DryRun       bool              `json:"dry_run"`
}

type Release struct {
	SchemaVersion    int        `json:"schema_version"`
	EventID          string     `json:"event_id"`
	RequestID        string     `json:"request_id"`
	AttemptID        string     `json:"attempt_id"`
	AttemptNo        int        `json:"attempt_no"`
	ProducerID       string     `json:"producer_id"`
	OccurredAt       time.Time  `json:"occurred_at"`
	AccountID        string     `json:"account_id"`
	Provider         string     `json:"provider"`
	StatusCode       int        `json:"status_code"`
	LatencyMS        int        `json:"latency_ms"`
	TokensIn         int        `json:"tokens_in"`
	TokensOut        int        `json:"tokens_out"`
	CacheReadTokens  int        `json:"cache_read_tokens"`
	CacheWriteTokens int        `json:"cache_write_tokens"`
	Model            string     `json:"model"`
	TenantID         string     `json:"tenant_id"`
	ErrorClass       ErrorClass `json:"error_class"`
	UsageSource      string     `json:"usage_source"`
	Partial          bool       `json:"partial"`
}

type AttemptStarted struct {
	SchemaVersion int       `json:"schema_version"`
	EventID       string    `json:"event_id"`
	RequestID     string    `json:"request_id"`
	AttemptID     string    `json:"attempt_id"`
	AttemptNo     int       `json:"attempt_no"`
	ProducerID    string    `json:"producer_id"`
	OccurredAt    time.Time `json:"occurred_at"`
	AccountID     string    `json:"account_id"`
	Provider      string    `json:"provider"`
	Model         string    `json:"model"`
	TenantID      string    `json:"tenant_id"`
	DeadlineAt    time.Time `json:"deadline_at"`
}

func (a AttemptStarted) Validate() error {
	if !supportedSchemaVersion(a.SchemaVersion) {
		return fmt.Errorf("%w: unsupported schema_version %d", ErrInvalidContract, a.SchemaVersion)
	}
	for name, value := range map[string]string{
		"event_id": a.EventID, "request_id": a.RequestID, "attempt_id": a.AttemptID,
		"producer_id": a.ProducerID, "account_id": a.AccountID, "provider": a.Provider,
		"model": a.Model, "tenant_id": a.TenantID,
	} {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("%w: missing %s", ErrInvalidContract, name)
		}
	}
	if a.AttemptNo <= 0 || a.OccurredAt.IsZero() || !a.DeadlineAt.After(a.OccurredAt) {
		return fmt.Errorf("%w: invalid attempt timing or sequence", ErrInvalidContract)
	}
	return nil
}

type StreamEvent struct {
	EventType     string          `json:"event_type"`
	SchemaVersion int             `json:"schema_version"`
	EventID       string          `json:"event_id"`
	RequestID     string          `json:"request_id"`
	AttemptID     string          `json:"attempt_id"`
	ProducerID    string          `json:"producer_id"`
	OccurredAt    time.Time       `json:"occurred_at"`
	Payload       json.RawMessage `json:"payload"`
}

func NewStreamEvent(eventType, eventID, requestID, attemptID, producerID string, occurredAt time.Time, payload any) (StreamEvent, error) {
	b, err := json.Marshal(payload)
	if err != nil {
		return StreamEvent{}, err
	}
	return StreamEvent{EventType: eventType, SchemaVersion: SchemaVersion, EventID: eventID, RequestID: requestID, AttemptID: attemptID, ProducerID: producerID, OccurredAt: occurredAt, Payload: b}, nil
}

func (e StreamEvent) Validate() error {
	if !supportedSchemaVersion(e.SchemaVersion) {
		return fmt.Errorf("%w: unsupported schema_version %d", ErrInvalidContract, e.SchemaVersion)
	}
	if e.EventType != EventTypeAttemptStarted && e.EventType != EventTypeRelease {
		return fmt.Errorf("%w: event_type %q", ErrInvalidContract, e.EventType)
	}
	for name, value := range map[string]string{"event_id": e.EventID, "request_id": e.RequestID, "attempt_id": e.AttemptID, "producer_id": e.ProducerID} {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("%w: missing %s", ErrInvalidContract, name)
		}
	}
	if len(e.Payload) == 0 || !json.Valid(e.Payload) {
		return fmt.Errorf("%w: invalid payload", ErrInvalidContract)
	}
	if e.OccurredAt.IsZero() {
		return fmt.Errorf("%w: missing occurred_at", ErrInvalidContract)
	}
	return nil
}

func TerminalEventID(attemptID string) string {
	sum := sha256.Sum256([]byte(attemptID + ":terminal"))
	return hex.EncodeToString(sum[:])
}

func (r Release) Validate() error {
	if !supportedSchemaVersion(r.SchemaVersion) {
		return fmt.Errorf("%w: unsupported schema_version %d", ErrInvalidContract, r.SchemaVersion)
	}
	for name, value := range map[string]string{
		"event_id": r.EventID, "request_id": r.RequestID, "attempt_id": r.AttemptID,
		"producer_id": r.ProducerID, "account_id": r.AccountID, "provider": r.Provider,
		"model": r.Model, "tenant_id": r.TenantID,
	} {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("%w: missing %s", ErrInvalidContract, name)
		}
	}
	if r.EventID != TerminalEventID(r.AttemptID) {
		return fmt.Errorf("%w: terminal event_id must be derived from attempt_id", ErrInvalidContract)
	}
	if r.AttemptNo < 0 || r.OccurredAt.IsZero() || r.StatusCode < 0 || r.LatencyMS < 0 || r.TokensIn < 0 || r.TokensOut < 0 || r.CacheReadTokens < 0 || r.CacheWriteTokens < 0 {
		return fmt.Errorf("%w: invalid release values", ErrInvalidContract)
	}
	if !r.ErrorClass.Valid() || (r.UsageSource != UsageSourceUpstream && r.UsageSource != UsageSourceEstimated && r.UsageSource != UsageSourceMissing) {
		return fmt.Errorf("%w: invalid release classification", ErrInvalidContract)
	}
	return nil
}

func supportedSchemaVersion(version int) bool {
	return version > 0 && (version == SchemaVersion || version/100 == SchemaVersion/100)
}
