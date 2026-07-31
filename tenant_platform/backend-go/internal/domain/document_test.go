package domain

import (
	"encoding/json"
	"testing"
)

func TestDocumentJobStatusContract(t *testing.T) {
	valid := []DocumentJobStatus{
		DocumentJobQueued, DocumentJobStarting, DocumentJobRunning,
		DocumentJobSucceeded, DocumentJobFailed, DocumentJobCancelled, DocumentJobExpired,
	}
	for _, status := range valid {
		if !status.IsValid() {
			t.Fatalf("expected %q to be valid", status)
		}
	}
	for _, status := range []DocumentJobStatus{DocumentJobQueued, DocumentJobStarting, DocumentJobRunning} {
		if status.IsTerminal() {
			t.Fatalf("expected %q to be non-terminal", status)
		}
	}
	for _, status := range []DocumentJobStatus{DocumentJobSucceeded, DocumentJobFailed, DocumentJobCancelled, DocumentJobExpired} {
		if !status.IsTerminal() {
			t.Fatalf("expected %q to be terminal", status)
		}
	}
	if DocumentJobStatus("unknown").IsValid() {
		t.Fatal("unknown job status must be invalid")
	}
}

func TestDocumentCommandAndInstanceStatusContract(t *testing.T) {
	for _, status := range []DocumentCommandStatus{
		DocumentCommandPending, DocumentCommandExecuting, DocumentCommandSucceeded,
		DocumentCommandFailed, DocumentCommandExpired,
	} {
		if !status.IsValid() {
			t.Fatalf("expected command status %q to be valid", status)
		}
	}
	for _, status := range []DocumentInstanceStatus{
		DocumentInstanceReady, DocumentInstanceAllocated, DocumentInstanceCreating,
		DocumentInstanceRunning, DocumentInstanceDestroying, DocumentInstanceDestroyed,
		DocumentInstanceLost,
	} {
		if !status.IsValid() {
			t.Fatalf("expected instance status %q to be valid", status)
		}
	}
	if DocumentCommandStatus("unknown").IsValid() || DocumentInstanceStatus("unknown").IsValid() {
		t.Fatal("unknown command/instance statuses must be invalid")
	}
}

func TestDocumentOperationRequestJSONContract(t *testing.T) {
	request := DocumentOperationRequest{
		SchemaVersion: 1,
		Operation:     "extract_text",
		Parameters:    json.RawMessage(`{"page":9007199254740993}`),
	}
	encoded, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	const want = `{"schema_version":1,"operation":"extract_text","parameters":{"page":9007199254740993}}`
	if string(encoded) != want {
		t.Fatalf("encoded operation=%s want=%s", encoded, want)
	}
}
