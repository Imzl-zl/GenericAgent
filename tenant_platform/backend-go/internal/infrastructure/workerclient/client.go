package workerclient

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	workerv1 "github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/gen/worker/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Sentinel values used by IsContextError without importing context into events.go tests loosely.
var (
	contextCanceled = context.Canceled
	contextDeadline = context.DeadlineExceeded
)

// Default unary deadlines for non-streaming calls when the caller context has no deadline.
const (
	defaultUnaryTimeout      = 30 * time.Second
	defaultCheckpointTimeout = 60 * time.Second
	defaultHealthTimeout     = 10 * time.Second
)

// WorkerClient is the typed platform-facing Worker API.
// One gRPC connection per Worker process; no retries in this layer.
type WorkerClient interface {
	StartSession(ctx context.Context, req *workerv1.StartSessionRequest) (*workerv1.StartSessionResponse, error)
	ExecuteTask(ctx context.Context, req *workerv1.ExecuteTaskRequest) (<-chan WorkerEvent, <-chan error)
	BeginCheckpoint(ctx context.Context, req *workerv1.BeginCheckpointRequest) (*workerv1.CheckpointReady, error)
	CancelTask(ctx context.Context, taskID string) error
	Health(ctx context.Context) (*workerv1.HealthResponse, error)
	Shutdown(ctx context.Context, reason string) error
}

// Client implements WorkerClient over a single gRPC connection.
type Client struct {
	raw workerv1.WorkerServiceClient
}

// New wraps an existing gRPC connection. The connection is owned by the caller;
// the client does not close it.
func New(conn grpc.ClientConnInterface) (*Client, error) {
	if conn == nil {
		return nil, errors.New("workerclient: nil connection")
	}
	return &Client{raw: workerv1.NewWorkerServiceClient(conn)}, nil
}

// NewFromClient constructs a Client from a generated WorkerServiceClient (tests/injection).
func NewFromClient(raw workerv1.WorkerServiceClient) (*Client, error) {
	if raw == nil {
		return nil, errors.New("workerclient: nil WorkerServiceClient")
	}
	return &Client{raw: raw}, nil
}

func withDeadline(ctx context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	if _, ok := ctx.Deadline(); ok {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, d)
}

func (c *Client) StartSession(ctx context.Context, req *workerv1.StartSessionRequest) (*workerv1.StartSessionResponse, error) {
	if req == nil {
		return nil, errors.New("workerclient: nil StartSessionRequest")
	}
	callCtx, cancel := withDeadline(ctx, defaultUnaryTimeout)
	defer cancel()
	resp, err := c.raw.StartSession(callCtx, req)
	if err != nil {
		return nil, wrapRPC("StartSession", err)
	}
	return resp, nil
}

// ExecuteTask starts a server-streaming task and returns:
//   - events: typed display events (chunk/tool_progress/terminal); closed exactly once
//   - errs: at most one non-nil error (malformed conversion or transport), then closed
//
// A terminal event is delivered on events and then both channels close with a nil error.
// Checkpoint payloads never appear on the event channel.
// The receive loop respects ctx cancellation; there is no retry.
func (c *Client) ExecuteTask(ctx context.Context, req *workerv1.ExecuteTaskRequest) (<-chan WorkerEvent, <-chan error) {
	events := make(chan WorkerEvent, 16)
	errs := make(chan error, 1)

	go func() {
		defer close(events)
		defer close(errs)

		if req == nil || req.GetTask() == nil {
			errs <- &MalformedEventError{Reason: "nil ExecuteTaskRequest.task"}
			return
		}

		stream, err := c.raw.ExecuteTask(ctx, req)
		if err != nil {
			errs <- &TransportError{Op: "ExecuteTask.open", Err: err}
			return
		}

		for {
			if err := ctx.Err(); err != nil {
				errs <- wrapContextOrTransport("ExecuteTask.recv", err)
				return
			}

			raw, err := stream.Recv()
			if err != nil {
				if errors.Is(err, io.EOF) {
					// Clean end without terminal is a protocol/transport fault for the platform.
					errs <- &TransportError{Op: "ExecuteTask.recv", Err: io.ErrUnexpectedEOF}
					return
				}
				errs <- wrapContextOrTransport("ExecuteTask.recv", err)
				return
			}

			ev, convErr := ConvertEvent(raw)
			if convErr != nil {
				errs <- convErr
				return
			}

			select {
			case events <- ev:
			case <-ctx.Done():
				errs <- wrapContextOrTransport("ExecuteTask.deliver", ctx.Err())
				return
			}

			if ev.IsTerminal() {
				// Terminal closes the stream for the single consumer; no further events.
				return
			}
		}
	}()

	return events, errs
}

func (c *Client) BeginCheckpoint(ctx context.Context, req *workerv1.BeginCheckpointRequest) (*workerv1.CheckpointReady, error) {
	if req == nil {
		return nil, errors.New("workerclient: nil BeginCheckpointRequest")
	}
	callCtx, cancel := withDeadline(ctx, defaultCheckpointTimeout)
	defer cancel()
	ready, err := c.raw.BeginCheckpoint(callCtx, req)
	if err != nil {
		return nil, wrapRPC("BeginCheckpoint", err)
	}
	// Token-preserving: surface server response as-is (server must echo token).
	return ready, nil
}

func (c *Client) CancelTask(ctx context.Context, taskID string) error {
	if taskID == "" {
		return errors.New("workerclient: empty taskID")
	}
	callCtx, cancel := withDeadline(ctx, defaultUnaryTimeout)
	defer cancel()
	_, err := c.raw.CancelTask(callCtx, &workerv1.CancelTaskRequest{TaskId: taskID})
	if err != nil {
		return wrapRPC("CancelTask", err)
	}
	return nil
}

func (c *Client) Health(ctx context.Context) (*workerv1.HealthResponse, error) {
	callCtx, cancel := withDeadline(ctx, defaultHealthTimeout)
	defer cancel()
	resp, err := c.raw.Health(callCtx, &workerv1.HealthRequest{})
	if err != nil {
		return nil, wrapRPC("Health", err)
	}
	return resp, nil
}

func (c *Client) Shutdown(ctx context.Context, reason string) error {
	callCtx, cancel := withDeadline(ctx, defaultUnaryTimeout)
	defer cancel()
	resp, err := c.raw.Shutdown(callCtx, &workerv1.ShutdownRequest{Reason: reason})
	if err != nil {
		return wrapRPC("Shutdown", err)
	}
	// accepted=false means the Worker's runner thread did not drain within its
	// grace period — the stop was not clean. Surface it so callers escalate to
	// a hard process/container kill instead of assuming graceful teardown.
	if !resp.GetAccepted() {
		return fmt.Errorf("worker shutdown not accepted (runner still draining); escalate to hard kill")
	}
	return nil
}

func wrapRPC(op string, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	if st, ok := status.FromError(err); ok {
		switch st.Code() {
		case codes.Canceled, codes.DeadlineExceeded:
			return err
		case codes.Unavailable, codes.ResourceExhausted, codes.Aborted, codes.Internal:
			return &TransportError{Op: op, Err: err}
		}
	}
	return fmt.Errorf("workerclient %s: %w", op, err)
}

func wrapContextOrTransport(op string, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	if st, ok := status.FromError(err); ok {
		switch st.Code() {
		case codes.Canceled:
			return context.Canceled
		case codes.DeadlineExceeded:
			return context.DeadlineExceeded
		}
	}
	return &TransportError{Op: op, Err: err}
}

// Ensure *Client implements WorkerClient.
var _ WorkerClient = (*Client)(nil)
