package application

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/infrastructure/checkpoint"
	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/infrastructure/policy"
)

const CapabilityVersion = "foundation.v1"

// TaskService is the application-facing task API.
type TaskService interface {
	SubmitTask(ctx context.Context, cmd domain.SubmitTaskCommand) (domain.Task, error)
	GetTask(ctx context.Context, taskID string) (domain.Task, error)
	CancelTask(ctx context.Context, taskID string, requesterUserID int64) (domain.Task, error)
	ClaimNextTask(ctx context.Context, sessionKey, platformInstanceID string) (domain.Task, bool, error)
	RecoverAfterRestart(ctx context.Context, platformInstanceID string) error
	ReadResult(ctx context.Context, taskID string) (domain.ResultPayload, error)
}

// TaskServiceConfig wires store, policy, coordinator, and claim lease.
type TaskServiceConfig struct {
	Store              TaskStore
	Registry           policy.Registry
	Coordinator        checkpoint.Coordinator
	PlatformInstanceID string
	ClaimLease         time.Duration
	// PerUserQueueLimit caps the number of queued tasks a single requester
	// may have. Zero disables the check (dev/test only). The hard check is
	// enforced inside Store.SubmitTask's transaction to avoid TOCTOU races;
	// this field is the soft pre-check for fast rejection of obvious floods.
	PerUserQueueLimit int
	// Kick is optional; called after durable mutations that may unblock work.
	Kick func(ctx context.Context, sessionKey string)
	// CancelWorker is optional; invoked when durable cancel requires Worker RPC.
	CancelWorker func(ctx context.Context, task domain.Task) error
}

type taskService struct {
	store              TaskStore
	registry           policy.Registry
	coord              checkpoint.Coordinator
	platformInstanceID string
	claimLease         time.Duration
	perUserQueueLimit  int
	kick               func(ctx context.Context, sessionKey string)
	cancelWorker       func(ctx context.Context, task domain.Task) error
}

// NewTaskService constructs the service. Coordinator may be nil for unit tests that never ReadResult.
func NewTaskService(cfg TaskServiceConfig) (TaskService, error) {
	if cfg.Store == nil {
		return nil, fmt.Errorf("store is required")
	}
	if cfg.Registry == nil {
		return nil, fmt.Errorf("policy registry is required")
	}
	if strings.TrimSpace(cfg.PlatformInstanceID) == "" {
		return nil, fmt.Errorf("platform instance id is required")
	}
	if cfg.ClaimLease <= 0 {
		return nil, fmt.Errorf("claim lease must be positive")
	}
	return &taskService{
		store:              cfg.Store,
		registry:           cfg.Registry,
		coord:              cfg.Coordinator,
		platformInstanceID: cfg.PlatformInstanceID,
		claimLease:         cfg.ClaimLease,
		perUserQueueLimit:  cfg.PerUserQueueLimit,
		kick:               cfg.Kick,
		cancelWorker:       cfg.CancelWorker,
	}, nil
}

// ErrPerUserQueueFull is re-exported from domain so callers in the application
// layer can match against it without importing domain directly in some paths.
var ErrPerUserQueueFull = domain.ErrPerUserQueueFull

func (s *taskService) SubmitTask(ctx context.Context, cmd domain.SubmitTaskCommand) (domain.Task, error) {
	if _, err := s.registry.Resolve(CapabilityVersion, cmd.ToolPolicyVersion); err != nil {
		return domain.Task{}, fmt.Errorf("tool_policy_version: %w", err)
	}
	// Soft pre-check to fast-reject obvious floods without entering a tx.
	// The hard check inside Store.SubmitTask prevents TOCTOU.
	if s.perUserQueueLimit > 0 && cmd.RequesterUserID > 0 {
		queued, err := s.store.CountQueuedTasksByRequester(ctx, cmd.RequesterUserID)
		if err != nil {
			return domain.Task{}, fmt.Errorf("count queued: %w", err)
		}
		if queued >= s.perUserQueueLimit {
			return domain.Task{}, ErrPerUserQueueFull
		}
	}
	task, err := s.store.SubmitTask(ctx, cmd)
	if err != nil {
		return domain.Task{}, err
	}
	if s.kick != nil {
		s.kick(ctx, task.SessionKey)
	}
	return task, nil
}

func (s *taskService) GetTask(ctx context.Context, taskID string) (domain.Task, error) {
	return s.store.GetTask(ctx, taskID)
}

func (s *taskService) CancelTask(ctx context.Context, taskID string, requesterUserID int64) (domain.Task, error) {
	task, needWorker, err := s.store.CancelTask(ctx, taskID, requesterUserID)
	if err != nil {
		return domain.Task{}, err
	}
	if needWorker && s.cancelWorker != nil {
		if err := s.cancelWorker(ctx, task); err != nil {
			// Durable cancel_requested_at is already set; surface error but keep state.
			return task, fmt.Errorf("worker cancel: %w", err)
		}
	}
	if s.kick != nil {
		s.kick(ctx, task.SessionKey)
	}
	return task, nil
}

func (s *taskService) ClaimNextTask(ctx context.Context, sessionKey, platformInstanceID string) (domain.Task, bool, error) {
	if platformInstanceID == "" {
		platformInstanceID = s.platformInstanceID
	}
	return s.store.ClaimNextTask(ctx, sessionKey, platformInstanceID, s.claimLease)
}

func (s *taskService) RecoverAfterRestart(ctx context.Context, platformInstanceID string) error {
	if platformInstanceID == "" {
		platformInstanceID = s.platformInstanceID
	}
	_, err := s.store.RecoverAfterRestart(ctx, platformInstanceID)
	return err
}

func (s *taskService) ReadResult(ctx context.Context, taskID string) (domain.ResultPayload, error) {
	task, err := s.store.GetTask(ctx, taskID)
	if err != nil {
		return domain.ResultPayload{}, err
	}
	if task.ResultRef == "" || task.ResultDigest == "" {
		return domain.ResultPayload{}, fmt.Errorf("task %s has no committed result", taskID)
	}
	if s.coord == nil {
		return domain.ResultPayload{}, fmt.Errorf("checkpoint coordinator not configured")
	}
	return s.coord.ReadResult(ctx, task.ResultRef, task.ResultDigest)
}
