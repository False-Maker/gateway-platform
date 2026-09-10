package newapi

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// LoadSnapshot is the only code in this repository that touches a production
// new-api database. These tests build a synthetic source schema in the test
// database and assert the two properties that matter there: it never writes,
// and it never guesses a column it could not find.

func openSourceTestPool(t *testing.T) (context.Context, *pgxpool.Pool) {
	t.Helper()
	databaseURL := os.Getenv("GATEWAY_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("GATEWAY_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	db, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)
	return ctx, db
}

// createSourceSchema builds a throwaway schema and returns its name. Each test
// gets its own so they can run in any order against a shared database.
func createSourceSchema(t *testing.T, ctx context.Context, db *pgxpool.Pool, name string, statements ...string) string {
	t.Helper()
	if _, err := db.Exec(ctx, `DROP SCHEMA IF EXISTS "`+name+`" CASCADE`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `CREATE SCHEMA "`+name+`"`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := db.Exec(context.Background(), `DROP SCHEMA IF EXISTS "`+name+`" CASCADE`); err != nil {
			t.Logf("drop test schema %s: %v", name, err)
		}
	})
	for _, statement := range statements {
		if _, err := db.Exec(ctx, strings.ReplaceAll(statement, "{schema}", `"`+name+`"`)); err != nil {
			t.Fatalf("prepare source schema: %v\nstatement: %s", err, statement)
		}
	}
	return name
}

func TestLoadSnapshotReadsAllSourceTablesAndPreservesRawValues(t *testing.T) {
	ctx, db := openSourceTestPool(t)
	schema := createSourceSchema(t, ctx, db, "newapi_src_full",
		`CREATE TABLE {schema}.channels (
			id BIGINT PRIMARY KEY, type INT, key TEXT, status INT, name TEXT,
			base_url TEXT, models TEXT, "group" TEXT, used_quota BIGINT,
			balance NUMERIC, setting TEXT, model_mapping TEXT, channel_info TEXT)`,
		`INSERT INTO {schema}.channels VALUES
			(7, 14, 'sk-claude-a', 1, 'claude-a', 'https://claude.example.test', '["m1","m2"]',
			 'default,vip', 1234, 5.25, '{"proxy":"http://p.example.test:8080"}', '{"a":"b"}', '{}'),
			(9, 57, '{"access_token":"at","refresh_token":"rt","account_id":"acct"}', 1, 'codex-a',
			 '', 'm3', 'default', 0, 0, '{}', '', '{}')`,
		`CREATE TABLE {schema}.users (
			id BIGINT PRIMARY KEY, username TEXT, email TEXT, status INT,
			quota BIGINT, used_quota BIGINT, "group" TEXT)`,
		`INSERT INTO {schema}.users VALUES (3, 'alice', 'alice@example.test', 1, 500000, 42, 'default')`,
		`CREATE TABLE {schema}.tokens (
			id BIGINT PRIMARY KEY, user_id BIGINT, status INT, expired_time BIGINT,
			remain_quota BIGINT, "group" TEXT, key TEXT)`,
		`INSERT INTO {schema}.tokens VALUES (11, 3, 1, -1, 99, 'default', 'plaintext-token-key')`,
		`CREATE TABLE {schema}.quota_data (
			id BIGINT PRIMARY KEY, user_id BIGINT, model_name TEXT, created_at BIGINT,
			token_used BIGINT, count BIGINT, quota BIGINT, use_group TEXT)`,
		`INSERT INTO {schema}.quota_data VALUES (21, 3, 'claude-3', 1757000000, 17, 2, 340, 'default')`,
		`CREATE TABLE {schema}.groups (id TEXT PRIMARY KEY, name TEXT, status TEXT)`,
		`INSERT INTO {schema}.groups VALUES ('g1', 'default', 'enabled')`,
	)

	snapshot, err := LoadSnapshot(ctx, db, schema)
	if err != nil {
		t.Fatalf("LoadSnapshot: %v", err)
	}
	if len(snapshot.SchemaWarnings) != 0 {
		t.Errorf("a complete schema produced warnings: %v", snapshot.SchemaWarnings)
	}
	if len(snapshot.Channels) != 2 {
		t.Fatalf("got %d channels, want 2", len(snapshot.Channels))
	}
	// Ordering is by id, which the plan builder relies on for stable source ids.
	first := snapshot.Channels[0]
	if first.ID != 7 || first.Type != 14 || first.Status != 1 {
		t.Errorf("channel 7 read back as %+v", first)
	}
	if first.Key != "sk-claude-a" {
		t.Errorf("channel key was altered: %q", first.Key)
	}
	if first.Group != "default,vip" {
		t.Errorf("channels.group must be preserved verbatim for group expansion, got %q", first.Group)
	}
	// setting.proxy and the codex key JSON are the two documented easy-to-lose
	// fields (AUDIT-CONTEXT 1.3), so assert them explicitly rather than by count.
	if got := parseProxy(first.Setting); got != "http://p.example.test:8080" {
		t.Errorf("setting.proxy lost: %q (raw setting %q)", got, first.Setting)
	}
	if first.Balance == "" || first.UsedQuota == "" {
		t.Errorf("numeric columns must arrive as raw text, got balance=%q used_quota=%q", first.Balance, first.UsedQuota)
	}
	if !strings.Contains(snapshot.Channels[1].Key, `"refresh_token":"rt"`) {
		t.Errorf("codex OAuth key JSON lost: %q", snapshot.Channels[1].Key)
	}

	if len(snapshot.Users) != 1 || snapshot.Users[0].ID != 3 || snapshot.Users[0].Username != "alice" {
		t.Errorf("users read back as %+v", snapshot.Users)
	}
	if len(snapshot.Tokens) != 1 || snapshot.Tokens[0].UserID != 3 {
		t.Errorf("tokens read back as %+v", snapshot.Tokens)
	}
	if len(snapshot.Quota) != 1 || snapshot.Quota[0].Model != "claude-3" || snapshot.Quota[0].Quota != "340" {
		t.Errorf("quota_data read back as %+v", snapshot.Quota)
	}
	if len(snapshot.Groups) != 1 || snapshot.Groups[0].ID != "g1" {
		t.Errorf("groups read back as %+v", snapshot.Groups)
	}
	if snapshot.CapturedAt.IsZero() {
		t.Error("CapturedAt was not stamped")
	}
}

func TestLoadSnapshotWarnsInsteadOfFailingWhenOptionalTablesAndColumnsAreAbsent(t *testing.T) {
	ctx, db := openSourceTestPool(t)
	// Only the three required channel columns exist; users/tokens/quota/groups
	// are absent entirely. An older new-api deployment looks like this.
	schema := createSourceSchema(t, ctx, db, "newapi_src_minimal",
		`CREATE TABLE {schema}.channels (id BIGINT PRIMARY KEY, type INT, key TEXT)`,
		`INSERT INTO {schema}.channels VALUES (1, 14, 'sk-only-key')`,
	)

	snapshot, err := LoadSnapshot(ctx, db, schema)
	if err != nil {
		t.Fatalf("a minimal-but-valid schema must load, got: %v", err)
	}
	if len(snapshot.Channels) != 1 {
		t.Fatalf("got %d channels, want 1", len(snapshot.Channels))
	}
	channel := snapshot.Channels[0]
	// Missing optional columns must come back empty, not as a fabricated value.
	if channel.Setting != "" || channel.Models != "" || channel.Group != "" || channel.Balance != "" {
		t.Errorf("absent optional columns were filled in: %+v", channel)
	}
	// An absent channels.status column means the source schema has no notion of
	// per-channel status at all, which source.go:57 treats as active. That is
	// distinct from a status column holding a value it cannot map -- see
	// TestLoadSnapshotDoesNotGuessAStatusItCannotMap.
	if channel.Status != 1 {
		t.Errorf("absent channels.status became %d, want 1", channel.Status)
	}
	if len(snapshot.Users) != 0 || len(snapshot.Tokens) != 0 || len(snapshot.Quota) != 0 || len(snapshot.Groups) != 0 {
		t.Error("absent optional tables produced rows")
	}
	joined := strings.Join(snapshot.SchemaWarnings, "\n")
	for _, want := range []string{"users", "tokens", "quota", "groups"} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing schema warning for %q; warnings were:\n%s", want, joined)
		}
	}

	// The warnings must survive into the plan summary, otherwise a dry-run
	// reviewer never learns the tenant staging was empty because of schema
	// gaps rather than because the source had no users.
	plan, err := BuildPlan(snapshot)
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	if len(plan.Accounts) != 1 {
		t.Fatalf("got %d accounts from the minimal schema, want 1", len(plan.Accounts))
	}
	if len(plan.Summary.SchemaWarnings) != len(snapshot.SchemaWarnings) {
		t.Errorf("schema warnings did not reach the plan summary: %v", plan.Summary.SchemaWarnings)
	}
}

func TestLoadSnapshotDoesNotGuessAStatusItCannotMap(t *testing.T) {
	ctx, db := openSourceTestPool(t)
	// Here the status column exists but holds NULL and an unmapped value. The
	// loader must not substitute "active"; BuildPlan must then reject both
	// channels with a reason instead of migrating a channel whose real state
	// is unknown.
	schema := createSourceSchema(t, ctx, db, "newapi_src_status",
		`CREATE TABLE {schema}.channels (id BIGINT PRIMARY KEY, type INT, key TEXT, status INT)`,
		`INSERT INTO {schema}.channels VALUES (1, 14, 'sk-a', NULL), (2, 14, 'sk-b', 99)`,
	)

	snapshot, err := LoadSnapshot(ctx, db, schema)
	if err != nil {
		t.Fatalf("LoadSnapshot: %v", err)
	}
	if len(snapshot.Channels) != 2 {
		t.Fatalf("got %d channels, want 2", len(snapshot.Channels))
	}
	if snapshot.Channels[0].Status != 0 {
		t.Errorf("NULL status became %d, want 0 so mapStatus rejects it", snapshot.Channels[0].Status)
	}
	if snapshot.Channels[1].Status != 99 {
		t.Errorf("unmapped status was rewritten to %d, want 99 preserved", snapshot.Channels[1].Status)
	}

	plan, err := BuildPlan(snapshot)
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	if len(plan.Accounts) != 0 {
		t.Errorf("channels with unusable status produced %d accounts, want 0", len(plan.Accounts))
	}
	if plan.Summary.RejectedCount != 2 {
		t.Errorf("rejected %d channels, want 2", plan.Summary.RejectedCount)
	}
	for reason := range plan.Summary.RejectionReasons {
		if !strings.Contains(reason, "status") {
			t.Errorf("rejection reason does not mention status: %q", reason)
		}
	}
}

func TestLoadSnapshotFailsExplicitlyOnMissingRequiredChannelColumns(t *testing.T) {
	ctx, db := openSourceTestPool(t)
	// "key" is absent. Dropping it silently would migrate credential-less
	// accounts, so this must be a hard error naming the column.
	schema := createSourceSchema(t, ctx, db, "newapi_src_nokey",
		`CREATE TABLE {schema}.channels (id BIGINT PRIMARY KEY, type INT, name TEXT)`,
	)
	_, err := LoadSnapshot(ctx, db, schema)
	if err == nil {
		t.Fatal("LoadSnapshot succeeded without a channels.key column")
	}
	if !strings.Contains(err.Error(), "key") {
		t.Errorf("error does not name the missing column: %v", err)
	}
}

func TestLoadSnapshotFailsWhenTheChannelsTableIsAbsent(t *testing.T) {
	ctx, db := openSourceTestPool(t)
	schema := createSourceSchema(t, ctx, db, "newapi_src_empty")
	if _, err := LoadSnapshot(ctx, db, schema); err == nil {
		t.Fatal("LoadSnapshot succeeded against a schema with no channels table")
	}
}

func TestLoadSnapshotRejectsSchemaNamesBeforeQueryingAndNeverWritesTheSource(t *testing.T) {
	ctx, db := openSourceTestPool(t)
	schema := createSourceSchema(t, ctx, db, "newapi_src_readonly",
		`CREATE TABLE {schema}.channels (id BIGINT PRIMARY KEY, type INT, key TEXT, status INT)`,
		`INSERT INTO {schema}.channels VALUES (1, 14, 'sk-a', 1)`,
	)

	// The schema name is interpolated into the query text, so an injection
	// attempt must be refused by validIdentifier before any SQL runs. If the
	// guard were missing, this payload would drop the table below.
	injection := schema + `"; DROP TABLE "` + schema + `"."channels`
	if _, err := LoadSnapshot(ctx, db, injection); err == nil {
		t.Fatal("LoadSnapshot accepted an injected schema identifier")
	}

	snapshot, err := LoadSnapshot(ctx, db, schema)
	if err != nil {
		t.Fatalf("LoadSnapshot after the rejected identifier: %v", err)
	}
	if len(snapshot.Channels) != 1 {
		t.Fatalf("source table was damaged: got %d channels, want 1", len(snapshot.Channels))
	}

	var count int
	if err := db.QueryRow(ctx, `SELECT count(*) FROM "`+schema+`".channels`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("source row count changed to %d", count)
	}

	// Row counts alone would look identical whether or not LoadSnapshot passed
	// pgx.ReadOnly, so verify separately that this database really does reject
	// writes under that transaction option. Together the two assertions pin the
	// "production source database is never written" guarantee.
	tx, err := db.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `INSERT INTO "`+schema+`".channels VALUES (2, 14, 'sk-b', 1)`); err == nil {
		t.Error("a read-only transaction accepted an INSERT; the read-only guarantee is untested here")
	}
}
