package application

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/infrastructure/checkpoint"
	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/infrastructure/policy"
)

const CapabilityVersion = "foundation.v1"

// ErrTaskNotFound 是任务归属校验失败与任务不存在的统一哨兵(审查 I-4):
// 越权访问返回与不存在相同的 not-found 语义, 不泄露任务存在性。
var ErrTaskNotFound = fmt.Errorf("task not found")

// DefaultToolPolicyVersion 是任务执行使用的统一工具策略版本(审查 D1 去分级):
// 所有用户/会话使用同一全能力档, 不再按用户分配或动态升级。静态 policy
// manifest(foundation.v1.json) 是唯一真值, worker 侧按此过滤 TOOLS_SCHEMA。
const DefaultToolPolicyVersion = "foundation.session-files.v1"

// TaskService is the application-facing task API.
type TaskService interface {
	SubmitTask(ctx context.Context, cmd domain.SubmitTaskCommand) (domain.Task, error)
	// SubmitTaskWithInboundMessage 在同一事务内持久化入站消息行与任务
	// (round10 审查 B7): 消除任务/消息行二段写入的崩溃与并发窗口。
	SubmitTaskWithInboundMessage(ctx context.Context, cmd domain.SubmitTaskCommand, msg domain.Message) (domain.Task, domain.Message, error)
	// GetTask 返回任务; requesterUserID 必须为任务归属者(RequesterID),
	// 否则返回 not-found 语义错误(审查 I-4: 任务仅归属者可读)。
	GetTask(ctx context.Context, taskID string, requesterUserID int64) (domain.Task, error)
	CancelTask(ctx context.Context, taskID string, requesterUserID int64) (domain.Task, error)
	ClaimNextTask(ctx context.Context, sessionKey, platformInstanceID string) (domain.Task, bool, error)
	RecoverAfterRestart(ctx context.Context, platformInstanceID string) error
	// ReadResult 返回任务结果; requesterUserID 必须为任务归属者
	// (审查 I-4: 结果与任务同可见性)。
	ReadResult(ctx context.Context, taskID string, requesterUserID int64) (domain.ResultPayload, error)
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
	// 审查 I-4: 会话归属校验——用户只能向自己的 personal 会话或
	// 所属团队会话提交任务, 防止跨用户/跨团队越权写入。
	if err := s.validateSessionAccess(ctx, cmd.SessionKey, cmd.RequesterUserID); err != nil {
		return domain.Task{}, err
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

// SubmitTaskWithInboundMessage 原子提交入站消息行与任务(round10 审查 B7)。
func (s *taskService) SubmitTaskWithInboundMessage(ctx context.Context, cmd domain.SubmitTaskCommand, msg domain.Message) (domain.Task, domain.Message, error) {
	if _, err := s.registry.Resolve(CapabilityVersion, cmd.ToolPolicyVersion); err != nil {
		return domain.Task{}, domain.Message{}, fmt.Errorf("tool_policy_version: %w", err)
	}
	// 审查 I-4: 与 SubmitTask 一致的会话归属校验。
	if err := s.validateSessionAccess(ctx, cmd.SessionKey, cmd.RequesterUserID); err != nil {
		return domain.Task{}, domain.Message{}, err
	}
	// 软预检与 SubmitTask 一致(队列上限快速拒绝; 硬校验在事务内)。
	if s.perUserQueueLimit > 0 && cmd.RequesterUserID > 0 {
		queued, err := s.store.CountQueuedTasksByRequester(ctx, cmd.RequesterUserID)
		if err != nil {
			return domain.Task{}, domain.Message{}, fmt.Errorf("count queued: %w", err)
		}
		if queued >= s.perUserQueueLimit {
			return domain.Task{}, domain.Message{}, ErrPerUserQueueFull
		}
	}
	task, msgRow, err := s.store.SubmitTaskWithInboundMessage(ctx, cmd, msg)
	if err != nil {
		return domain.Task{}, domain.Message{}, err
	}
	if s.kick != nil {
		s.kick(ctx, task.SessionKey)
	}
	return task, msgRow, nil
}

// validateSessionAccess 校验 requesterUserID 是否有权向 sessionKey 提交任务
// (审查 I-4): personal:<uid> 必须等于本人; team:<tid> 必须是已批准成员;
// 其余格式一律拒绝。requester 缺失(<=0)视为未认证, 拒绝。
// Round16-P2: 统一用 domain.ValidateWorkspaceKey 严格解析——旧实现用
// fmt.Sscanf 宽松解析, `personal:123abc` 会被误解析为 uid=123 通过归属
// 校验(Sscanf 吞掉 trailing garbage 且 err=nil), 与 domain 严格校验
// (ParseInt 拒绝)语义分裂; team 整数 id 在 store 的 $1::uuid cast 处
// SQL 报错而非干净拒绝。
func (s *taskService) validateSessionAccess(ctx context.Context, sessionKey string, requesterUserID int64) error {
	if requesterUserID <= 0 {
		return fmt.Errorf("%w: requester identity required", domain.ErrSessionAccessDenied)
	}
	// Round16-P2: 入口统一严格校验, 与 WorkspaceDirHash/checkpoint 同源。
	// team:<旧整数格式> 由 ValidateWorkspaceKey 放行, 但 team 表 id 为 UUID,
	// 整数 id 的 workspace 行不可能存在——提交必因 workspace not found 失败,
	// 这里直接拒绝避免 store 层 $1::uuid cast 500。
	if err := domain.ValidateWorkspaceKey(sessionKey); err != nil {
		return fmt.Errorf("%w: invalid session key %q: %v", domain.ErrSessionAccessDenied, sessionKey, err)
	}
	// 审查 I-4: 提交门禁与 capability 在线校验一致——pending 用户的任务
	// 执行时会被 llmproxy 拒绝(IsTaskCapabilityActive 要求 approved),
	// 提交即拒绝, 避免产生必然失败的任务。
	approved, err := s.store.IsApprovedUser(ctx, requesterUserID)
	if err != nil {
		return fmt.Errorf("%w: user status check: %v", domain.ErrSessionAccessDenied, err)
	}
	if !approved {
		return fmt.Errorf("%w: requester %d is not approved", domain.ErrSessionAccessDenied, requesterUserID)
	}
	const personalPrefix = "personal:"
	const teamPrefix = "team:"
	switch {
	case strings.HasPrefix(sessionKey, personalPrefix):
		// ValidateWorkspaceKey 已保证 personal:<positive-int>, 此处用
		// ParseInt 严格解析(与 domain 同实现), 不再容忍 trailing garbage。
		uid, err := strconv.ParseInt(strings.TrimPrefix(sessionKey, personalPrefix), 10, 64)
		if err != nil || uid != requesterUserID {
			return fmt.Errorf("%w: personal session %q does not belong to requester %d", domain.ErrSessionAccessDenied, sessionKey, requesterUserID)
		}
		return nil
	case strings.HasPrefix(sessionKey, teamPrefix):
		teamID := strings.TrimPrefix(sessionKey, teamPrefix)
		if _, err := uuid.Parse(teamID); err != nil {
			return fmt.Errorf("%w: team session %q has invalid team id", domain.ErrSessionAccessDenied, sessionKey)
		}
		member, err := s.store.IsApprovedTeamMember(ctx, teamID, requesterUserID)
		if err != nil {
			return fmt.Errorf("%w: team membership check: %v", domain.ErrSessionAccessDenied, err)
		}
		if !member {
			return fmt.Errorf("%w: requester %d is not an approved member of team %q", domain.ErrSessionAccessDenied, requesterUserID, teamID)
		}
		return nil
	default:
		return fmt.Errorf("%w: unsupported session key %q", domain.ErrSessionAccessDenied, sessionKey)
	}
}

// taskAccessError 是任务归属校验失败的错误(not-found 语义, 不泄露存在性)。
func taskAccessError(taskID string) error {
	return fmt.Errorf("%w: %s", ErrTaskNotFound, taskID)
}

func (s *taskService) GetTask(ctx context.Context, taskID string, requesterUserID int64) (domain.Task, error) {
	task, err := s.store.GetTask(ctx, taskID)
	if err != nil {
		return domain.Task{}, err
	}
	// 审查 I-4: 任务仅归属者可读; 不匹配返回 not-found 语义。
	if requesterUserID <= 0 || task.RequesterID != requesterUserID {
		return domain.Task{}, taskAccessError(taskID)
	}
	return task, nil
}

func (s *taskService) CancelTask(ctx context.Context, taskID string, requesterUserID int64) (domain.Task, error) {
	// 审查 I-4: 归属前置校验, 与 GetTask/ReadResult 一致的 not-found 语义
	// (越权取消不泄露任务存在性); store.CancelTask 事务内校验保留为纵深防御。
	if _, err := s.GetTask(ctx, taskID, requesterUserID); err != nil {
		return domain.Task{}, err
	}
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

func (s *taskService) ReadResult(ctx context.Context, taskID string, requesterUserID int64) (domain.ResultPayload, error) {
	task, err := s.GetTask(ctx, taskID, requesterUserID)
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
