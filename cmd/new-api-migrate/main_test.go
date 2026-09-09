package main

import (
	"context"
	"strings"
	"testing"
)

func TestRunRequiresSourceDatabaseURL(t *testing.T) {
	err := run(context.Background(), nil, func(string) string { return "" })
	if err == nil || !strings.Contains(err.Error(), "source database URL is required") {
		t.Fatalf("error = %v", err)
	}
}

func TestRunApplyRequiresTargetAndKeyBeforeConnecting(t *testing.T) {
	getenv := func(name string) string {
		if name == "GATEWAY_NEW_API_DATABASE_URL" { return "postgres://source.invalid/db" }
		return ""
	}
	err := run(context.Background(), []string{"--apply"}, getenv)
	if err == nil || !strings.Contains(err.Error(), "target database URL is required") {
		t.Fatalf("error = %v", err)
	}
}
