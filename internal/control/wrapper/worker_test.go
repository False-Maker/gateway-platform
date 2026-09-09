package wrapper

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/elucid/gateway-platform/pkg/contracts"
	"github.com/redis/go-redis/v9"
)

func TestWorkerProcessesFixtureAndStopsOnContextCancel(t *testing.T) {
	mini := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	queue := Queue{Redis: rdb}
	job := contracts.WrapperJob{
		SchemaVersion:  contracts.SchemaVersion,
		JobID:          "worker-job",
		Provider:       "codex",
		Operation:      contracts.WrapperRefresh,
		AccountID:      "account-worker",
		FenceEpoch:     4,
		EncryptedInput: []byte{1, 2, 3, 4},
	}
	if _, err := queue.Enqueue(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- (Worker{Queue: queue, WorkerID: "worker-test", LeaseTTLSeconds: 30, PollInterval: time.Millisecond, Executor: FixtureExecutor{}}).Run(ctx)
	}()

	var terminal []byte
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); {
		var err error
		terminal, err = queue.Terminal(context.Background(), job.JobID)
		if err == nil {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if len(terminal) == 0 {
		t.Fatal("worker did not write terminal result")
	}
	var envelope terminalEnvelope
	if err := json.Unmarshal(terminal, &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Status != "completed" || envelope.Completion == nil || string(envelope.Completion.EncryptedOutput) != string(job.EncryptedInput) {
		t.Fatalf("unexpected fixture terminal: %#v", envelope)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("worker cancellation: %v", err)
	}
}

func TestWorkerCancellationDuringFixtureLeavesLeaseForReclaim(t *testing.T) {
	mini := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	queue := Queue{Redis: rdb}
	job := contracts.WrapperJob{SchemaVersion: contracts.SchemaVersion, JobID: "cancel-job", Provider: "codex", Operation: contracts.WrapperRefresh, AccountID: "account-cancel", FenceEpoch: 1, EncryptedInput: []byte("opaque")}
	if _, err := queue.Enqueue(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- (Worker{Queue: queue, WorkerID: "worker-cancel", LeaseTTLSeconds: 1, PollInterval: time.Millisecond, Executor: FixtureExecutor{Delay: time.Minute}}).Run(ctx)
	}()
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); {
		pending, err := rdb.XPending(context.Background(), StreamKey, GroupName).Result()
		if err == nil && pending.Count == 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	pending, err := rdb.XPending(context.Background(), StreamKey, GroupName).Result()
	if err != nil {
		t.Fatalf("pending query: %v", err)
	}
	if pending.Count != 1 {
		t.Fatalf("pending=%d, worker did not claim", pending.Count)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("worker cancellation: %v", err)
	}
	if _, err := queue.Terminal(context.Background(), job.JobID); !errors.Is(err, redis.Nil) {
		t.Fatalf("cancelled worker wrote terminal: %v", err)
	}
}

type failingExecutor struct{}

func (failingExecutor) Execute(context.Context, contracts.WrapperJob) ([]byte, error) {
	return nil, errors.New("fixture executor failed")
}

func TestWorkerFailsFixtureJobThroughQueueContract(t *testing.T) {
	mini := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	queue := Queue{Redis: rdb}
	job := contracts.WrapperJob{SchemaVersion: contracts.SchemaVersion, JobID: "failed-job", Provider: "claude", Operation: contracts.WrapperAuthorize, EncryptedInput: []byte("opaque")}
	if _, err := queue.Enqueue(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- (Worker{Queue: queue, WorkerID: "worker-fail", LeaseTTLSeconds: 30, PollInterval: time.Millisecond, Executor: failingExecutor{}}).Run(ctx)
	}()
	var terminal []byte
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); {
		var err error
		terminal, err = queue.Terminal(context.Background(), job.JobID)
		if err == nil {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("worker cancellation: %v", err)
	}
	if len(terminal) == 0 {
		t.Fatal("worker did not write failure terminal")
	}
	var envelope terminalEnvelope
	if err := json.Unmarshal(terminal, &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Status != "failed" || envelope.Failure == nil || envelope.Failure.Code != "executor_failed" {
		t.Fatalf("unexpected failure terminal: %#v", envelope)
	}
}

type classifiedExecutor struct{}

func (classifiedExecutor) Execute(context.Context, contracts.WrapperJob) ([]byte, error) {
	return nil, executionError(ErrorUnsupportedJob, false, contracts.ErrUnsupportedCapability)
}

func TestWorkerPreservesExecutorFailureClassification(t *testing.T) {
	mini := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	queue := Queue{Redis: rdb}
	job := contracts.WrapperJob{SchemaVersion: contracts.SchemaVersion, JobID: "classified-job", Provider: "claude", Operation: contracts.WrapperAuthorize, EncryptedInput: []byte("opaque")}
	if _, err := queue.Enqueue(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- (Worker{Queue: queue, WorkerID: "worker-classified", LeaseTTLSeconds: 30, PollInterval: time.Millisecond, Executor: classifiedExecutor{}}).Run(ctx)
	}()
	terminal := waitForTerminalEnvelope(t, queue, job.JobID)
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("worker cancellation: %v", err)
	}
	if terminal.Failure == nil || terminal.Failure.Code != ErrorUnsupportedJob || terminal.Failure.Retryable {
		t.Fatalf("unexpected classified failure: %#v", terminal)
	}
}

func waitForTerminalEnvelope(t *testing.T, queue Queue, jobID string) terminalEnvelope {
	t.Helper()
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); {
		raw, err := queue.Terminal(context.Background(), jobID)
		if err == nil {
			var terminal terminalEnvelope
			if err := json.Unmarshal(raw, &terminal); err != nil {
				t.Fatal(err)
			}
			return terminal
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("terminal was not written for %s", jobID)
	return terminalEnvelope{}
}
