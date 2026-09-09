package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestRunTenantCreateCommandRequiresTenantAndDatabase(t *testing.T) {
	t.Setenv("GATEWAY_DATABASE_URL", "")
	var out bytes.Buffer
	if err := runTenantCreateCommand(nil, &out); err == nil || !strings.Contains(err.Error(), "--tenant") {
		t.Fatalf("missing tenant flag: %v", err)
	}
	if err := runTenantCreateCommand([]string{"--tenant", "acme"}, &out); err == nil || !strings.Contains(err.Error(), "GATEWAY_DATABASE_URL") {
		t.Fatalf("missing database URL: %v", err)
	}
	if err := runTenantCreateCommand([]string{"--tenant", "acme", "extra"}, &out); err == nil || !strings.Contains(err.Error(), "unexpected") {
		t.Fatalf("stray argument accepted: %v", err)
	}
	if out.Len() != 0 {
		t.Fatalf("no token must be printed on failure: %s", out.String())
	}
}
