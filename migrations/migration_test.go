package migrations

import (
	"os"
	"strings"
	"testing"
)

func TestInitialSchemaContainsP0TablesAndConstraints(t *testing.T) {
	data, err := os.ReadFile("001_initial.sql")
	if err != nil {
		t.Fatal(err)
	}
	schema := string(data)
	for _, table := range []string{"accounts", "credentials", "request_attempts", "usage_ledger", "migration_runs", "migration_records", "quota_snapshot_outbox"} {
		if !strings.Contains(schema, "CREATE TABLE IF NOT EXISTS "+table) {
			t.Errorf("missing table %s", table)
		}
	}
	for _, constraint := range []string{"event_id TEXT NOT NULL UNIQUE", "attempt_id TEXT NOT NULL UNIQUE", "terminal_event_id TEXT UNIQUE", "fence_epoch BIGINT NOT NULL"} {
		if !strings.Contains(schema, constraint) {
			t.Errorf("missing schema invariant %q", constraint)
		}
	}
	if strings.Contains(schema, "access_token TEXT") {
		t.Fatal("credentials must not persist plaintext access tokens")
	}
}
