package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestRunImportCommandDryRunUsesImportServiceAndRedactsCredential(t *testing.T) {
	t.Setenv("GATEWAY_CREDENTIAL_KEY", base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x27}, 32)))
	file := t.TempDir() + "/import.json"
	payload := `{"source_system":"cli-test","source_id":"grok-1","provider":"grok","auth_mode":"api_key","static_key":"cli-secret","metadata":{"group":"cli"}}`
	if err := os.WriteFile(file, []byte(payload), 0o600); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := runImportCommand([]string{"--file", file, "--dry-run"}, &output); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(output.String(), "cli-secret") {
		t.Fatalf("import output leaked credential: %s", output.String())
	}
	var result importCommandResult
	if err := json.Unmarshal(output.Bytes(), &result); err != nil {
		t.Fatalf("output=%q err=%v", output.String(), err)
	}
	if result.Provider != "grok" || result.Platform != "grok" || result.Group != "cli" || result.CredentialKind != "static" || !result.DryRun || result.AccountID == "" {
		t.Fatalf("unexpected import result=%#v", result)
	}
	if result.CredentialVersion != 0 || result.FenceEpoch != 0 {
		t.Fatalf("dry-run unexpectedly persisted versions: %#v", result)
	}
}
