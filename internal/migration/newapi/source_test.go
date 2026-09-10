package newapi

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// The source loader turns loosely-typed new-api columns into typed fields. The
// rules it must not break: required fields fail loudly, optional fields fall
// back without guessing, and the schema identifier never reaches SQL unchecked.

func TestValidIdentifierRejectsAnythingThatCouldEscapeIntoSQL(t *testing.T) {
	for _, valid := range []string{"public", "new_api", "s1", "_private", "Mixed_Case9"} {
		if !validIdentifier(valid) {
			t.Errorf("validIdentifier(%q) = false, want true", valid)
		}
	}
	// LoadSnapshot interpolates the schema name directly into the query text,
	// so every one of these must be rejected before it gets there.
	for _, invalid := range []string{
		"",
		"1leading_digit",
		"public; DROP TABLE channels",
		`public" OR "1"="1`,
		"public.channels",
		"has space",
		"trailing-dash",
		"quote'd",
	} {
		if validIdentifier(invalid) {
			t.Errorf("validIdentifier(%q) = true, want false", invalid)
		}
	}
}

func TestRequiredIntFailsInsteadOfDefaultingToZero(t *testing.T) {
	// A missing or unparsable id must never silently become account 0.
	for _, row := range []map[string]string{
		{},
		{"id": ""},
		{"id": "   "},
		{"id": "not-a-number"},
		{"id": "12.5"},
	} {
		if _, err := requiredInt(row, "id"); err == nil {
			t.Errorf("requiredInt(%#v) succeeded, want error", row)
		}
	}
	value, err := requiredInt(map[string]string{"id": " 42 "}, "id")
	if err != nil || value != 42 {
		t.Fatalf("requiredInt = (%d, %v), want (42, nil)", value, err)
	}
}

func TestOptionalIntUsesFallbackOnlyForAbsentOrUnparsableValues(t *testing.T) {
	cases := []struct {
		value    string
		fallback int
		want     int
	}{
		{"", 1, 1},
		{"   ", 1, 1},
		{"garbage", 1, 1},
		{"0", 1, 0},   // an explicit 0 must survive, not be replaced by the fallback
		{" 2 ", 1, 2}, // new-api stores these as text in some deployments
		{"-1", 1, -1},
	}
	for _, testCase := range cases {
		if got := optionalInt(testCase.value, testCase.fallback); got != testCase.want {
			t.Errorf("optionalInt(%q, %d) = %d, want %d", testCase.value, testCase.fallback, got, testCase.want)
		}
	}
}

func TestOptionalInt64TreatsUnparsableValuesAsZero(t *testing.T) {
	cases := map[string]int64{"": 0, "  ": 0, "garbage": 0, "7": 7, " 8 ": 8, "-3": -3}
	for value, want := range cases {
		if got := optionalInt64(value); got != want {
			t.Errorf("optionalInt64(%q) = %d, want %d", value, got, want)
		}
	}
}

func TestParseProxyReturnsEmptyRatherThanGuessingWhenSettingIsUnusable(t *testing.T) {
	// Losing channels.setting.proxy breaks codex refresh (overview 6.1), so the
	// only acceptable outcomes are the exact value or a clean empty string.
	if got := parseProxy(`{"proxy":"http://p.example.test:8080"}`); got != "http://p.example.test:8080" {
		t.Fatalf("parseProxy lost the configured proxy: %q", got)
	}
	for _, unusable := range []string{"", "   ", "not-json", "{", `{"proxy":""}`, `{"other":"x"}`, `[]`} {
		if got := parseProxy(unusable); got != "" {
			t.Errorf("parseProxy(%q) = %q, want empty", unusable, got)
		}
	}
}

func TestApplyRefusesToRunWithoutDatabaseOrCipher(t *testing.T) {
	// Apply is the only write path in this package. Both guards must fail
	// closed before a transaction is opened, so a misconfigured CLI cannot
	// half-write a run or store a credential in the clear.
	ctx := context.Background()
	err := Apply(ctx, nil, nil, Plan{}, "run-1")
	if err == nil || !strings.Contains(err.Error(), "target database is not configured") {
		t.Fatalf("Apply with a nil database: err = %v, want a target-database error", err)
	}

	// A pool created from a config alone does not dial, so this reaches the
	// cipher guard without touching a database.
	pool, err := pgxpool.New(ctx, "postgres://unused@127.0.0.1:1/unused")
	if err != nil {
		t.Fatalf("construct a non-dialing pool: %v", err)
	}
	defer pool.Close()
	if err := Apply(ctx, pool, nil, Plan{}, "run-1"); err == nil || !strings.Contains(err.Error(), "credential cipher is not configured") {
		t.Fatalf("Apply with a nil cipher: err = %v, want a cipher error", err)
	}
}
