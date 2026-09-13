package newapi

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/elucid/gateway-platform/pkg/contracts"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// LoadSnapshot performs a read-only transaction and discovers columns before
// selecting them. This keeps the importer honest across new-api schema
// revisions: optional fields are reported, while required channel fields fail
// explicitly instead of being guessed.
func LoadSnapshot(ctx context.Context, db *pgxpool.Pool, schema string) (SourceSnapshot, error) {
	if db == nil {
		return SourceSnapshot{}, fmt.Errorf("source database is not configured")
	}
	schema = strings.TrimSpace(schema)
	if schema == "" {
		schema = "public"
	}
	if !validIdentifier(schema) {
		return SourceSnapshot{}, fmt.Errorf("invalid source schema identifier %q", schema)
	}
	tx, err := db.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return SourceSnapshot{}, fmt.Errorf("begin read-only source transaction: %w", err)
	}
	defer tx.Rollback(ctx)
	snapshot := SourceSnapshot{CapturedAt: nowUTC()}
	channelColumns, err := tableColumns(ctx, tx, schema, "channels")
	if err != nil {
		return SourceSnapshot{}, err
	}
	for _, required := range []string{"id", "type", "key"} {
		if !channelColumns[required] {
			return SourceSnapshot{}, fmt.Errorf("source channels table is missing required column %q", required)
		}
	}
	channelRows, err := queryRows(ctx, tx, schema, "channels", channelColumns, []string{"id", "type", "key", "status", "name", "base_url", "models", "group", "used_quota", "balance", "setting", "model_mapping", "channel_info"})
	if err != nil {
		return SourceSnapshot{}, fmt.Errorf("read channels: %w", err)
	}
	for _, row := range channelRows {
		id, err := requiredInt(row, "id")
		if err != nil {
			return SourceSnapshot{}, fmt.Errorf("read channels.id: %w", err)
		}
		kind, err := requiredInt(row, "type")
		if err != nil {
			return SourceSnapshot{}, fmt.Errorf("read channels.type for %d: %w", id, err)
		}
		status := 1
		if channelColumns["status"] {
			status = optionalInt(row["status"], 0)
		}
		snapshot.Channels = append(snapshot.Channels, SourceChannel{ID: id, Type: int(kind), Key: row["key"], Status: status, Name: row["name"], BaseURL: row["base_url"], Models: row["models"], Group: row["group"], UsedQuota: row["used_quota"], Balance: row["balance"], Setting: row["setting"], ModelMapping: row["model_mapping"], ChannelInfo: row["channel_info"]})
	}
	snapshot.Users, snapshot.SchemaWarnings, err = loadUsers(ctx, tx, schema, snapshot.SchemaWarnings)
	if err != nil {
		return SourceSnapshot{}, err
	}
	snapshot.Tokens, snapshot.SchemaWarnings, err = loadTokens(ctx, tx, schema, snapshot.SchemaWarnings)
	if err != nil {
		return SourceSnapshot{}, err
	}
	snapshot.Quota, snapshot.SchemaWarnings, err = loadQuota(ctx, tx, schema, snapshot.SchemaWarnings)
	if err != nil {
		return SourceSnapshot{}, err
	}
	snapshot.Groups, snapshot.SchemaWarnings, err = loadGroups(ctx, tx, schema, snapshot.SchemaWarnings)
	if err != nil {
		return SourceSnapshot{}, err
	}
	snapshot.QuotaPerUnit, snapshot.SchemaWarnings, err = loadQuotaPerUnit(ctx, tx, schema, snapshot.SchemaWarnings)
	if err != nil {
		return SourceSnapshot{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return SourceSnapshot{}, fmt.Errorf("commit read-only source transaction: %w", err)
	}
	return snapshot, nil
}

func tableColumns(ctx context.Context, tx pgx.Tx, schema, table string) (map[string]bool, error) {
	rows, err := tx.Query(ctx, `SELECT column_name FROM information_schema.columns WHERE table_schema=$1 AND table_name=$2`, schema, table)
	if err != nil {
		return nil, fmt.Errorf("inspect source table %s: %w", table, err)
	}
	defer rows.Close()
	columns := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		columns[strings.ToLower(name)] = true
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return columns, nil
}

func queryRows(ctx context.Context, tx pgx.Tx, schema, table string, columns map[string]bool, fields []string) ([]map[string]string, error) {
	if len(columns) == 0 {
		return nil, fmt.Errorf("source table %s does not exist", table)
	}
	selects := make([]string, 0, len(fields))
	for _, field := range fields {
		if columns[field] {
			selects = append(selects, `COALESCE(CAST("`+field+`" AS TEXT),'') AS "`+field+`"`)
		} else {
			selects = append(selects, `''::text AS "`+field+`"`)
		}
	}
	query := `SELECT ` + strings.Join(selects, ",") + ` FROM "` + schema + `"."` + table + `"`
	if columns["id"] {
		query += ` ORDER BY "id"`
	}
	rows, err := tx.Query(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]map[string]string, 0)
	for rows.Next() {
		values := make([]*string, len(fields))
		for i := range values {
			values[i] = new(string)
		}
		dest := make([]any, len(values))
		for i := range values {
			dest[i] = values[i]
		}
		if err := rows.Scan(dest...); err != nil {
			return nil, err
		}
		row := make(map[string]string, len(fields))
		for i, field := range fields {
			row[field] = *values[i]
		}
		result = append(result, row)
	}
	return result, rows.Err()
}

func loadUsers(ctx context.Context, tx pgx.Tx, schema string, warnings []string) ([]SourceUser, []string, error) {
	columns, err := tableColumns(ctx, tx, schema, "users")
	if err != nil {
		return nil, warnings, err
	}
	if len(columns) == 0 {
		return nil, append(warnings, "source table users is absent; tenant staging is empty"), nil
	}
	if !columns["id"] {
		return nil, append(warnings, "source table users is missing id; tenant staging is empty"), nil
	}
	rows, err := queryRows(ctx, tx, schema, "users", columns, []string{"id", "username", "email", "status", "quota", "used_quota", "group"})
	if err != nil {
		return nil, warnings, fmt.Errorf("read users: %w", err)
	}
	result := make([]SourceUser, 0, len(rows))
	for _, row := range rows {
		id, err := requiredInt(row, "id")
		if err != nil {
			return nil, warnings, fmt.Errorf("read users.id: %w", err)
		}
		result = append(result, SourceUser{ID: id, Username: row["username"], Email: row["email"], Status: optionalInt(row["status"], 1), Quota: row["quota"], UsedQuota: row["used_quota"], Group: row["group"]})
	}
	return result, warnings, nil
}

func loadTokens(ctx context.Context, tx pgx.Tx, schema string, warnings []string) ([]SourceToken, []string, error) {
	columns, err := tableColumns(ctx, tx, schema, "tokens")
	if err != nil {
		return nil, warnings, err
	}
	if len(columns) == 0 {
		return nil, append(warnings, "source table tokens is absent; token staging is empty"), nil
	}
	if !columns["id"] {
		return nil, append(warnings, "source table tokens is missing id; token staging is empty"), nil
	}
	rows, err := queryRows(ctx, tx, schema, "tokens", columns, []string{"id", "user_id", "status", "expired_time", "remain_quota", "group", "key"})
	if err != nil {
		return nil, warnings, fmt.Errorf("read tokens: %w", err)
	}
	result := make([]SourceToken, 0, len(rows))
	for _, row := range rows {
		id, err := requiredInt(row, "id")
		if err != nil {
			return nil, warnings, fmt.Errorf("read tokens.id: %w", err)
		}
		userID := optionalInt64(row["user_id"])
		result = append(result, SourceToken{ID: id, UserID: userID, Status: optionalInt(row["status"], 1), ExpiredTime: row["expired_time"], RemainQuota: row["remain_quota"], Group: row["group"], Key: row["key"]})
	}
	return result, warnings, nil
}

func loadQuota(ctx context.Context, tx pgx.Tx, schema string, warnings []string) ([]SourceQuota, []string, error) {
	table := "quota_data"
	columns, err := tableColumns(ctx, tx, schema, table)
	if err != nil {
		return nil, warnings, err
	}
	if len(columns) == 0 {
		table = "quota"
		columns, err = tableColumns(ctx, tx, schema, table)
		if err != nil {
			return nil, warnings, err
		}
	}
	if len(columns) == 0 {
		return nil, append(warnings, "source tables quota_data/quota are absent; quota staging is empty"), nil
	}
	if !columns["id"] {
		return nil, append(warnings, "source quota table is missing id; quota staging is empty"), nil
	}
	rows, err := queryRows(ctx, tx, schema, table, columns, []string{"id", "user_id", "model_name", "created_at", "token_used", "count", "quota", "use_group"})
	if err != nil {
		return nil, warnings, fmt.Errorf("read %s: %w", table, err)
	}
	result := make([]SourceQuota, 0, len(rows))
	for _, row := range rows {
		id, err := requiredInt(row, "id")
		if err != nil {
			return nil, warnings, fmt.Errorf("read quota_data.id: %w", err)
		}
		result = append(result, SourceQuota{ID: id, UserID: optionalInt64(row["user_id"]), Model: row["model_name"], CreatedAt: row["created_at"], TokenUsed: row["token_used"], Count: row["count"], Quota: row["quota"], Group: row["use_group"]})
	}
	return result, warnings, nil
}

func loadGroups(ctx context.Context, tx pgx.Tx, schema string, warnings []string) ([]SourceGroup, []string, error) {
	columns, err := tableColumns(ctx, tx, schema, "groups")
	if err != nil {
		return nil, warnings, err
	}
	if len(columns) == 0 {
		return nil, append(warnings, "source table groups is absent; group staging uses channel groups"), nil
	}
	rows, err := queryRows(ctx, tx, schema, "groups", columns, []string{"id", "name", "status"})
	if err != nil {
		return nil, warnings, fmt.Errorf("read groups: %w", err)
	}
	result := make([]SourceGroup, 0, len(rows))
	for index, row := range rows {
		id := row["id"]
		if id == "" {
			id = row["name"]
		}
		if id == "" {
			id = strconv.Itoa(index)
		}
		result = append(result, SourceGroup{ID: id, Name: row["name"], Status: row["status"]})
	}
	return result, warnings, nil
}

// Reasons a source deployment yielded no usable QuotaPerUnit. They are part of
// the migration record, so they are stable strings rather than free text.
const (
	quotaPerUnitOptionsTableAbsent = "options_table_absent"
	quotaPerUnitColumnsAbsent      = "options_table_missing_key_or_value_column"
	quotaPerUnitRowAbsent          = "quota_per_unit_option_absent"
	quotaPerUnitEmpty              = "quota_per_unit_option_empty"
	quotaPerUnitUnparsable         = "quota_per_unit_option_unparsable"
	quotaPerUnitNotPositive        = "quota_per_unit_option_not_positive"
)

// quotaPerUnitOptionKey is new-api's option key (model/option.go:597).
const quotaPerUnitOptionKey = "QuotaPerUnit"

// loadQuotaPerUnit reads the deployment's quota-to-currency ratio.
//
// This never falls back to new-api's compiled-in 500000: a deployment that
// overrode the option would be migrated at the wrong ratio, and the error
// would be invisible because both the wrong and the right number look like a
// plausible balance. Missing is reported as missing, per AGENTS.md section 2.
func loadQuotaPerUnit(ctx context.Context, tx pgx.Tx, schema string, warnings []string) (SourceQuotaPerUnit, []string, error) {
	columns, err := tableColumns(ctx, tx, schema, "options")
	if err != nil {
		return SourceQuotaPerUnit{}, warnings, err
	}
	if len(columns) == 0 {
		return SourceQuotaPerUnit{Reason: quotaPerUnitOptionsTableAbsent},
			append(warnings, "source table options is absent; quota_per_unit is missing and no balance may be converted"), nil
	}
	if !columns["key"] || !columns["value"] {
		return SourceQuotaPerUnit{Reason: quotaPerUnitColumnsAbsent},
			append(warnings, "source table options lacks key/value columns; quota_per_unit is missing and no balance may be converted"), nil
	}
	var raw string
	row := tx.QueryRow(ctx, `SELECT COALESCE(CAST("value" AS TEXT),'') FROM "`+schema+`"."options" WHERE "key"=$1`, quotaPerUnitOptionKey)
	switch err := row.Scan(&raw); {
	case errors.Is(err, pgx.ErrNoRows):
		return SourceQuotaPerUnit{Reason: quotaPerUnitRowAbsent},
			append(warnings, "source options has no QuotaPerUnit row; quota_per_unit is missing and no balance may be converted"), nil
	case err != nil:
		return SourceQuotaPerUnit{}, warnings, fmt.Errorf("read options.QuotaPerUnit: %w", err)
	}
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return SourceQuotaPerUnit{Raw: raw, Reason: quotaPerUnitEmpty},
			append(warnings, "source options.QuotaPerUnit is empty; quota_per_unit is missing and no balance may be converted"), nil
	}
	value := contracts.Decimal(trimmed)
	if !value.Valid() {
		return SourceQuotaPerUnit{Raw: raw, Reason: quotaPerUnitUnparsable},
			append(warnings, fmt.Sprintf("source options.QuotaPerUnit %q is not a decimal; quota_per_unit is missing and no balance may be converted", raw)), nil
	}
	if sign, ok := value.Sign(); !ok || sign <= 0 {
		return SourceQuotaPerUnit{Raw: raw, Reason: quotaPerUnitNotPositive},
			append(warnings, fmt.Sprintf("source options.QuotaPerUnit %q is not positive; quota_per_unit is missing and no balance may be converted", raw)), nil
	}
	return SourceQuotaPerUnit{Raw: raw, Value: value, Found: true}, warnings, nil
}

func requiredInt(row map[string]string, field string) (int64, error) {
	value := strings.TrimSpace(row[field])
	if value == "" {
		return 0, fmt.Errorf("%s is empty", field)
	}
	return strconv.ParseInt(value, 10, 64)
}
func optionalInt(value string, fallback int) int {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil {
		return fallback
	}
	return parsed
}
func optionalInt64(value string) int64 {
	if strings.TrimSpace(value) == "" {
		return 0
	}
	parsed, _ := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
	return parsed
}
func nowUTC() (t time.Time) { return time.Now().UTC() }

func validIdentifier(value string) bool {
	if value == "" {
		return false
	}
	for index, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || r == '_' || (index > 0 && r >= '0' && r <= '9') {
			continue
		}
		return false
	}
	return true
}
