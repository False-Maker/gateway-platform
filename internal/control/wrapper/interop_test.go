package wrapper

import (
	"bytes"
	"context"
	"encoding/json"
	"os/exec"
	"testing"
	"time"

	"github.com/elucid/gateway-platform/pkg/contracts"
)

func TestPythonFixtureConsumerInteroperatesWithWrapperJSONContract(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 is not available")
	}
	job := contracts.WrapperJob{
		SchemaVersion:  contracts.SchemaVersion,
		JobID:          "interop-job",
		Provider:       "claude",
		Operation:      contracts.WrapperAuthorize,
		EncryptedInput: []byte{0, 1, 2, 3, 255},
	}
	lease := contracts.WrapperLease{
		SchemaVersion: contracts.SchemaVersion,
		JobID:         job.JobID,
		LeaseID:       "interop-lease",
		WorkerID:      "python-fixture",
		ExpiresAt:     time.Now().Add(time.Minute),
	}
	input, err := json.Marshal(struct {
		Job   contracts.WrapperJob   `json:"job"`
		Lease contracts.WrapperLease `json:"lease"`
	}{Job: job, Lease: lease})
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(context.Background(), "python3", "testdata/fixture_consumer.py")
	cmd.Stdin = bytes.NewReader(input)
	output, err := cmd.Output()
	if err != nil {
		t.Fatalf("python fixture: %v", err)
	}
	var completion contracts.WrapperCompletion
	if err := json.Unmarshal(output, &completion); err != nil {
		t.Fatalf("decode Python completion: %v; output=%s", err, output)
	}
	if err := completion.ValidateLease(lease); err != nil {
		t.Fatal(err)
	}
	if string(completion.EncryptedOutput) != string(job.EncryptedInput) {
		t.Fatalf("Python fixture changed opaque envelope: %v", completion.EncryptedOutput)
	}
}
