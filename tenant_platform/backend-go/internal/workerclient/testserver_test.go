package workerclient_test

import (
	"context"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	workerv1 "github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/gen/worker/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

const bufSize = 1 << 20

// scriptedWorker is an in-process WorkerService used only by client unit tests.
type scriptedWorker struct {
	workerv1.UnimplementedWorkerServiceServer

	mu sync.Mutex

	health      *workerv1.HealthResponse
	startResp   *workerv1.StartSessionResponse
	startErr    error
	checkpoint  *workerv1.CheckpointReady
	checkpointE error
	cancelResp  *workerv1.CancelTaskResponse
	shutdownOK  bool

	// execute script: ordered actions per call. Each call consumes one script.
	scripts [][]streamAction
	// last BeginCheckpoint request for token assertions
	lastCheckpoint *workerv1.BeginCheckpointRequest
	// cancel observations
	cancelled []string
	// last ExecuteTask request
	lastTask *workerv1.ExecuteTaskRequest
}

type streamAction struct {
	// event is sent as a normal WorkerEvent (may be empty payload for malformed tests).
	event *workerv1.WorkerEvent
	// sleep before acting
	delay time.Duration
	// hang until ctx is done
	waitCancel bool
	// close stream with this error (nil => clean EOF after events)
	endErr error
	// if true, end the stream after this action without further events
	end bool
}

func (s *scriptedWorker) StartSession(ctx context.Context, req *workerv1.StartSessionRequest) (*workerv1.StartSessionResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.startErr != nil {
		return nil, s.startErr
	}
	if s.startResp != nil {
		return s.startResp, nil
	}
	return &workerv1.StartSessionResponse{
		SessionKey:       req.GetSessionKey(),
		WorkerInstanceId: "test-worker-1",
	}, nil
}

func (s *scriptedWorker) ExecuteTask(req *workerv1.ExecuteTaskRequest, stream workerv1.WorkerService_ExecuteTaskServer) error {
	s.mu.Lock()
	s.lastTask = req
	var actions []streamAction
	if len(s.scripts) > 0 {
		actions = s.scripts[0]
		s.scripts = s.scripts[1:]
	}
	s.mu.Unlock()

	ctx := stream.Context()
	for _, a := range actions {
		if a.delay > 0 {
			select {
			case <-time.After(a.delay):
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		if a.waitCancel {
			<-ctx.Done()
			return ctx.Err()
		}
		if a.event != nil {
			if err := stream.Send(a.event); err != nil {
				return err
			}
		}
		if a.endErr != nil {
			return a.endErr
		}
		if a.end {
			return nil
		}
	}
	return nil
}

func (s *scriptedWorker) BeginCheckpoint(ctx context.Context, req *workerv1.BeginCheckpointRequest) (*workerv1.CheckpointReady, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastCheckpoint = req
	if s.checkpointE != nil {
		return nil, s.checkpointE
	}
	if s.checkpoint != nil {
		// Preserve token/task/staging from request when scripted response leaves them empty.
		out := *s.checkpoint
		if out.TaskId == "" {
			out.TaskId = req.GetTaskId()
		}
		if out.CheckpointToken == "" {
			out.CheckpointToken = req.GetCheckpointToken()
		}
		if out.StagingRef == "" {
			out.StagingRef = req.GetStagingRef()
		}
		return &out, nil
	}
	return &workerv1.CheckpointReady{
		TaskId:          req.GetTaskId(),
		CheckpointToken: req.GetCheckpointToken(),
		StagingRef:      req.GetStagingRef(),
		Checksum:        "sha256:deadbeef",
		ResultDigest:    "sha256:cafebabe",
	}, nil
}

func (s *scriptedWorker) CancelTask(ctx context.Context, req *workerv1.CancelTaskRequest) (*workerv1.CancelTaskResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cancelled = append(s.cancelled, req.GetTaskId())
	if s.cancelResp != nil {
		return s.cancelResp, nil
	}
	return &workerv1.CancelTaskResponse{Accepted: true}, nil
}

func (s *scriptedWorker) Health(ctx context.Context, req *workerv1.HealthRequest) (*workerv1.HealthResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.health != nil {
		return s.health, nil
	}
	return &workerv1.HealthResponse{
		WorkerInstanceId: "test-worker-1",
		Ready:            true,
	}, nil
}

func (s *scriptedWorker) Shutdown(ctx context.Context, req *workerv1.ShutdownRequest) (*workerv1.ShutdownResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.shutdownOK = true
	return &workerv1.ShutdownResponse{Accepted: true}, nil
}

// startTestWorker spins an in-process gRPC server and returns a dialed conn.
func startTestWorker(t *testing.T, srv *scriptedWorker) (*grpc.ClientConn, func()) {
	t.Helper()
	lis := bufconn.Listen(bufSize)
	gs := grpc.NewServer()
	workerv1.RegisterWorkerServiceServer(gs, srv)
	go func() {
		_ = gs.Serve(lis)
	}()

	dialer := func(context.Context, string) (net.Conn, error) {
		return lis.Dial()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := grpc.DialContext(ctx, "bufnet",
		grpc.WithContextDialer(dialer),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithBlock(),
	)
	if err != nil {
		gs.Stop()
		t.Fatalf("dial bufnet: %v", err)
	}
	cleanup := func() {
		_ = conn.Close()
		gs.Stop()
		_ = lis.Close()
	}
	return conn, cleanup
}

// helpers to build scripted events
func chunkEvent(taskID, text string, turn uint32) *workerv1.WorkerEvent {
	return &workerv1.WorkerEvent{
		Payload: &workerv1.WorkerEvent_Chunk{
			Chunk: &workerv1.Chunk{TaskId: taskID, Text: text, Turn: turn},
		},
	}
}

func terminalEvent(taskID string, status workerv1.TerminalStatus, msg string) *workerv1.WorkerEvent {
	return &workerv1.WorkerEvent{
		Payload: &workerv1.WorkerEvent_Terminal{
			Terminal: &workerv1.Terminal{
				TaskId:      taskID,
				Status:      status,
				UserMessage: msg,
			},
		},
	}
}

func emptyPayloadEvent() *workerv1.WorkerEvent {
	return &workerv1.WorkerEvent{}
}

func nilTerminalEvent() *workerv1.WorkerEvent {
	return &workerv1.WorkerEvent{
		Payload: &workerv1.WorkerEvent_Terminal{Terminal: nil},
	}
}

func transportFail(code codes.Code, msg string) streamAction {
	return streamAction{endErr: status.Error(code, msg), end: true}
}

// silence unused import when only server helpers used
var _ = io.EOF
