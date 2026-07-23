package workerclient_test

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/workerclient"
	workerv1 "github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/gen/worker/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func newClient(t *testing.T, srv *scriptedWorker) (workerclient.WorkerClient, func()) {
	t.Helper()
	conn, cleanup := startTestWorker(t, srv)
	client, err := workerclient.New(conn)
	if err != nil {
		cleanup()
		t.Fatalf("New: %v", err)
	}
	return client, cleanup
}

func collect(t *testing.T, events <-chan workerclient.WorkerEvent, errs <-chan error, timeout time.Duration) ([]workerclient.WorkerEvent, error) {
	t.Helper()
	deadline := time.After(timeout)
	var out []workerclient.WorkerEvent
	var firstErr error
	eventsOpen, errsOpen := true, true
	for eventsOpen || errsOpen {
		select {
		case <-deadline:
			t.Fatalf("collect timed out; got %d events, err=%v", len(out), firstErr)
		case ev, ok := <-events:
			if !ok {
				eventsOpen = false
				events = nil
				continue
			}
			out = append(out, ev)
		case err, ok := <-errs:
			if !ok {
				errsOpen = false
				errs = nil
				continue
			}
			if err != nil && firstErr == nil {
				firstErr = err
			}
		}
	}
	return out, firstErr
}

func TestWorkerClient_ExecuteTask_StreamingOrderAndTerminalClosesOnce(t *testing.T) {
	srv := &scriptedWorker{
		scripts: [][]streamAction{{
			{event: chunkEvent("t1", "hello", 1)},
			{event: chunkEvent("t1", " world", 1)},
			{event: terminalEvent("t1", workerv1.TerminalStatus_TASK_SUCCEEDED, "done")},
		}},
	}
	client, cleanup := newClient(t, srv)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	events, errs := client.ExecuteTask(ctx, &workerv1.ExecuteTaskRequest{
		Task: &workerv1.TaskEnvelope{TaskId: "t1", SessionKey: "personal:1", Prompt: "hi"},
	})

	got, err := collect(t, events, errs, 5*time.Second)
	if err != nil {
		t.Fatalf("unexpected stream error: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("want 3 events, got %d: %+v", len(got), got)
	}
	if !got[0].IsChunk() || got[0].Chunk.GetText() != "hello" {
		t.Fatalf("event0 want chunk hello, got %+v", got[0])
	}
	if !got[1].IsChunk() || got[1].Chunk.GetText() != " world" {
		t.Fatalf("event1 want chunk world, got %+v", got[1])
	}
	if !got[2].IsTerminal() || got[2].Terminal.GetStatus() != workerv1.TerminalStatus_TASK_SUCCEEDED {
		t.Fatalf("event2 want succeeded terminal, got %+v", got[2])
	}

	// Channels must be closed (collect already drained). Re-read should not block.
	select {
	case _, ok := <-events:
		if ok {
			t.Fatal("events channel still open after terminal")
		}
	default:
		// already closed and drained
	}

	// No checkpoint payload can appear on the display stream.
	for i, ev := range got {
		if ev.IsCheckpoint() {
			t.Fatalf("event %d illegally carries checkpoint payload: %+v", i, ev)
		}
	}
}

func TestWorkerClient_ExecuteTask_ContextCancellation(t *testing.T) {
	srv := &scriptedWorker{
		scripts: [][]streamAction{{
			{event: chunkEvent("t-cancel", "partial", 1)},
			{waitCancel: true},
		}},
	}
	client, cleanup := newClient(t, srv)
	defer cleanup()

	ctx, cancel := context.WithCancel(context.Background())
	events, errs := client.ExecuteTask(ctx, &workerv1.ExecuteTaskRequest{
		Task: &workerv1.TaskEnvelope{TaskId: "t-cancel", SessionKey: "personal:1", Prompt: "x"},
	})

	// First event arrives.
	select {
	case ev := <-events:
		if !ev.IsChunk() {
			t.Fatalf("want first chunk, got %+v", ev)
		}
	case err := <-errs:
		t.Fatalf("unexpected early err: %v", err)
	case <-time.After(3 * time.Second):
		t.Fatal("timeout waiting for first chunk")
	}

	cancel()

	got, err := collect(t, events, errs, 5*time.Second)
	if err == nil {
		t.Fatalf("want cancellation/transport error, got nil (events=%+v)", got)
	}
	if !errors.Is(err, context.Canceled) && status.Code(err) != codes.Canceled {
		// Accept wrapped cancel or gRPC canceled.
		if !errors.Is(err, context.DeadlineExceeded) {
			// still require cancel-related
			if st, ok := status.FromError(err); !ok || (st.Code() != codes.Canceled && st.Code() != codes.DeadlineExceeded) {
				// Client may surface a typed transport error wrapping cancel.
				if !workerclient.IsTransportError(err) && !workerclient.IsContextError(err) {
					t.Fatalf("want cancel/transport error, got %T %v", err, err)
				}
			}
		}
	}
}

func TestWorkerClient_ExecuteTask_MalformedEmptyPayload(t *testing.T) {
	srv := &scriptedWorker{
		scripts: [][]streamAction{{
			{event: chunkEvent("t-bad", "ok", 1)},
			{event: emptyPayloadEvent()},
		}},
	}
	client, cleanup := newClient(t, srv)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	events, errs := client.ExecuteTask(ctx, &workerv1.ExecuteTaskRequest{
		Task: &workerv1.TaskEnvelope{TaskId: "t-bad", SessionKey: "personal:1", Prompt: "x"},
	})
	got, err := collect(t, events, errs, 5*time.Second)
	if err == nil {
		t.Fatalf("want malformed event error, got nil (events=%+v)", got)
	}
	if !workerclient.IsMalformedEventError(err) {
		t.Fatalf("want IsMalformedEventError, got %T %v", err, err)
	}
	// Chunk before malformed should still have been delivered.
	if len(got) < 1 || !got[0].IsChunk() {
		t.Fatalf("want prior chunk delivered, got %+v", got)
	}
	// Event channel must close after error.
	select {
	case _, ok := <-events:
		if ok {
			t.Fatal("events still open after malformed error")
		}
	default:
	}
}

func TestWorkerClient_ExecuteTask_MalformedNilTerminal(t *testing.T) {
	srv := &scriptedWorker{
		scripts: [][]streamAction{{
			{event: nilTerminalEvent()},
		}},
	}
	client, cleanup := newClient(t, srv)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	events, errs := client.ExecuteTask(ctx, &workerv1.ExecuteTaskRequest{
		Task: &workerv1.TaskEnvelope{TaskId: "t-nil-term", SessionKey: "personal:1", Prompt: "x"},
	})
	_, err := collect(t, events, errs, 5*time.Second)
	if err == nil {
		t.Fatal("want malformed terminal error")
	}
	if !workerclient.IsMalformedEventError(err) {
		t.Fatalf("want IsMalformedEventError, got %T %v", err, err)
	}
}

func TestWorkerClient_ExecuteTask_MalformedUnspecifiedTerminalStatus(t *testing.T) {
	srv := &scriptedWorker{
		scripts: [][]streamAction{{
			{event: terminalEvent("t-unspec", workerv1.TerminalStatus_TERMINAL_STATUS_UNSPECIFIED, "nope")},
		}},
	}
	client, cleanup := newClient(t, srv)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	events, errs := client.ExecuteTask(ctx, &workerv1.ExecuteTaskRequest{
		Task: &workerv1.TaskEnvelope{TaskId: "t-unspec", SessionKey: "personal:1", Prompt: "x"},
	})
	_, err := collect(t, events, errs, 5*time.Second)
	if err == nil {
		t.Fatal("want malformed terminal status error")
	}
	if !workerclient.IsMalformedEventError(err) {
		t.Fatalf("want IsMalformedEventError, got %T %v", err, err)
	}
}

func TestWorkerClient_ExecuteTask_TransportDisconnect(t *testing.T) {
	srv := &scriptedWorker{
		scripts: [][]streamAction{{
			{event: chunkEvent("t-disc", "before-drop", 1)},
			transportFail(codes.Unavailable, "connection reset by peer"),
		}},
	}
	client, cleanup := newClient(t, srv)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	events, errs := client.ExecuteTask(ctx, &workerv1.ExecuteTaskRequest{
		Task: &workerv1.TaskEnvelope{TaskId: "t-disc", SessionKey: "personal:1", Prompt: "x"},
	})
	got, err := collect(t, events, errs, 5*time.Second)
	if err == nil {
		t.Fatalf("want transport error, got nil (events=%+v)", got)
	}
	if !workerclient.IsTransportError(err) {
		// Unavailable from gRPC should classify as transport.
		if status.Code(err) != codes.Unavailable && !errors.Is(err, io.EOF) {
			t.Fatalf("want transport/unavailable error, got %T %v", err, err)
		}
	}
	if len(got) != 1 || !got[0].IsChunk() {
		t.Fatalf("want single prior chunk, got %+v", got)
	}
}

func TestWorkerClient_BeginCheckpoint_TokenPreserving(t *testing.T) {
	srv := &scriptedWorker{
		checkpoint: &workerv1.CheckpointReady{
			// Leave token/task/staging empty so server helper echoes request fields,
			// and client must return the server response (token-preserving end-to-end).
			Checksum:     "sha256:abc123",
			ResultDigest: "sha256:def456",
		},
	}
	client, cleanup := newClient(t, srv)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	req := &workerv1.BeginCheckpointRequest{
		TaskId:          "task-cp-1",
		CheckpointToken: "tok-preserve-me",
		StagingRef:      "staging/task-cp-1.bundle.json",
		MaxBundleBytes:  1024 * 1024,
	}
	ready, err := client.BeginCheckpoint(ctx, req)
	if err != nil {
		t.Fatalf("BeginCheckpoint: %v", err)
	}
	if ready.GetTaskId() != "task-cp-1" {
		t.Fatalf("task_id: got %q", ready.GetTaskId())
	}
	if ready.GetCheckpointToken() != "tok-preserve-me" {
		t.Fatalf("checkpoint_token not preserved: got %q", ready.GetCheckpointToken())
	}
	if ready.GetStagingRef() != "staging/task-cp-1.bundle.json" {
		t.Fatalf("staging_ref not preserved: got %q", ready.GetStagingRef())
	}
	if ready.GetChecksum() != "sha256:abc123" {
		t.Fatalf("checksum: got %q", ready.GetChecksum())
	}
	if ready.GetResultDigest() != "sha256:def456" {
		t.Fatalf("result_digest: got %q", ready.GetResultDigest())
	}

	srv.mu.Lock()
	defer srv.mu.Unlock()
	if srv.lastCheckpoint == nil || srv.lastCheckpoint.GetCheckpointToken() != "tok-preserve-me" {
		t.Fatalf("server did not receive token: %+v", srv.lastCheckpoint)
	}
}

func TestWorkerClient_CancelTask_SendsTaskID(t *testing.T) {
	srv := &scriptedWorker{}
	client, cleanup := newClient(t, srv)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := client.CancelTask(ctx, "task-to-cancel"); err != nil {
		t.Fatalf("CancelTask: %v", err)
	}
	srv.mu.Lock()
	defer srv.mu.Unlock()
	if len(srv.cancelled) != 1 || srv.cancelled[0] != "task-to-cancel" {
		t.Fatalf("cancelled=%v", srv.cancelled)
	}
}

func TestWorkerClient_HealthAndShutdown(t *testing.T) {
	srv := &scriptedWorker{
		health: &workerv1.HealthResponse{
			WorkerInstanceId: "w-ready",
			SessionKey:       "personal:9",
			Ready:            true,
		},
	}
	client, cleanup := newClient(t, srv)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	h, err := client.Health(ctx)
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	if !h.GetReady() || h.GetWorkerInstanceId() != "w-ready" {
		t.Fatalf("health=%+v", h)
	}
	if err := client.Shutdown(ctx, "test-done"); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if !srv.shutdownOK {
		t.Fatal("shutdown not observed")
	}
}

func TestWorkerClient_StartSession(t *testing.T) {
	srv := &scriptedWorker{}
	client, cleanup := newClient(t, srv)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	resp, err := client.StartSession(ctx, &workerv1.StartSessionRequest{
		SessionKey: "personal:1",
		RuntimePolicy: &workerv1.RuntimePolicy{
			CapabilityVersion: "foundation.v1",
			PolicyDigest:      "sha256:test",
			MaxTurns:          4,
		},
	})
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	if resp.GetSessionKey() != "personal:1" || resp.GetWorkerInstanceId() == "" {
		t.Fatalf("resp=%+v", resp)
	}
}

func TestWorkerClient_ExecuteTask_NoCheckpointOnDisplayStream(t *testing.T) {
	// Explicitly assert the typed wrapper has no checkpoint field path that
	// a scheduler could mistake for BeginCheckpoint results.
	srv := &scriptedWorker{
		scripts: [][]streamAction{{
			{event: terminalEvent("t-only", workerv1.TerminalStatus_TASK_SUCCEEDED, "ok")},
		}},
	}
	client, cleanup := newClient(t, srv)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	events, errs := client.ExecuteTask(ctx, &workerv1.ExecuteTaskRequest{
		Task: &workerv1.TaskEnvelope{TaskId: "t-only", SessionKey: "personal:1", Prompt: "x"},
	})
	got, err := collect(t, events, errs, 5*time.Second)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(got) != 1 || !got[0].IsTerminal() {
		t.Fatalf("got %+v", got)
	}
	if got[0].IsCheckpoint() {
		t.Fatal("terminal event reported as checkpoint")
	}
	// Compile-time / API: CheckpointReady only via BeginCheckpoint.
	_ = workerclient.WorkerClient(nil)
}
