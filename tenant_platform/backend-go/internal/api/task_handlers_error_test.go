package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"path/filepath"
	"runtime"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/application"
	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/infrastructure/policy"
)

// memoryTaskStore 是 TaskService 所需 TaskStore 端口的内存实现, 仅接线
// SubmitTask 错误注入, 其余方法返回未使用错误(Round16-P1 错误分级测试)。
type memoryTaskStore struct {
	submitErr error
}

func (m *memoryTaskStore) IsApprovedTeamMember(ctx context.Context, teamID string, userID int64) (bool, error) {
	return false, errors.New("unused")
}
func (m *memoryTaskStore) IsApprovedUser(ctx context.Context, userID int64) (bool, error) {
	return true, nil
}
func (m *memoryTaskStore) SubmitTask(ctx context.Context, cmd domain.SubmitTaskCommand) (domain.Task, error) {
	return domain.Task{}, m.submitErr
}
func (m *memoryTaskStore) SubmitTaskWithInboundMessage(ctx context.Context, cmd domain.SubmitTaskCommand, msg domain.Message) (domain.Task, domain.Message, error) {
	return domain.Task{}, domain.Message{}, m.submitErr
}
func (m *memoryTaskStore) GetTask(ctx context.Context, taskID string) (domain.Task, error) {
	return domain.Task{}, errors.New("unused")
}
func (m *memoryTaskStore) CancelTask(ctx context.Context, taskID string, requesterUserID int64) (domain.Task, bool, error) {
	return domain.Task{}, false, errors.New("unused")
}
func (m *memoryTaskStore) ClaimNextTask(ctx context.Context, sessionKey, platformInstanceID string, claimLease time.Duration) (domain.Task, bool, error) {
	return domain.Task{}, false, errors.New("unused")
}
func (m *memoryTaskStore) RecoverAfterRestart(ctx context.Context, platformInstanceID string) (int, error) {
	return 0, errors.New("unused")
}
func (m *memoryTaskStore) CompleteSucceeded(ctx context.Context, taskID, platformInstanceID, snapshotID, fileRef, checksum, resultRef, resultDigest string, resultBytes int, deliveryFiles []domain.DeliveryFile) (domain.Task, error) {
	return domain.Task{}, errors.New("unused")
}
func (m *memoryTaskStore) CompleteFailedTerminal(ctx context.Context, taskID, owner string, status domain.TaskStatus, deliveryType domain.DeliveryType, code, message, traceID string) (domain.Task, error) {
	return domain.Task{}, errors.New("unused")
}
func (m *memoryTaskStore) ListOwnedActiveTasks(ctx context.Context, platformInstanceID string) ([]domain.Task, error) {
	return nil, errors.New("unused")
}
func (m *memoryTaskStore) HeartbeatClaim(ctx context.Context, taskID, platformInstanceID string, claimLease time.Duration) error {
	return errors.New("unused")
}
func (m *memoryTaskStore) ListClaimableSessionKeys(ctx context.Context, limit, perRequesterRunningLimit int) ([]string, error) {
	return nil, errors.New("unused")
}
func (m *memoryTaskStore) MarkDispatchStarted(ctx context.Context, taskID, platformInstanceID, workerInstanceID string, freshSession bool) (domain.Task, error) {
	return domain.Task{}, errors.New("unused")
}
func (m *memoryTaskStore) MarkRunning(ctx context.Context, taskID, platformInstanceID string) (domain.Task, error) {
	return domain.Task{}, errors.New("unused")
}
func (m *memoryTaskStore) RecordChunkEvent(ctx context.Context, taskID string, byteCount int, digest string) error {
	return errors.New("unused")
}
func (m *memoryTaskStore) RecordHeartbeat(ctx context.Context, taskID string) error {
	return errors.New("unused")
}
func (m *memoryTaskStore) MarkTaskStreamFinal(ctx context.Context, taskID string) (*time.Time, error) {
	return nil, errors.New("unused")
}
func (m *memoryTaskStore) CountRunningTasks(ctx context.Context) (int, error) {
	return 0, errors.New("unused")
}
func (m *memoryTaskStore) RequeueTask(ctx context.Context, taskID, platformInstanceID string) error {
	return errors.New("unused")
}
func (m *memoryTaskStore) CountQueuedTasksByRequester(ctx context.Context, requesterUserID int64) (int, error) {
	return 0, errors.New("unused")
}
func (m *memoryTaskStore) ResetWorkspaceForNewSession(ctx context.Context, sessionKey, conversationKey string) (int, error) {
	return 0, errors.New("unused")
}
func (m *memoryTaskStore) WorkspaceIsFresh(ctx context.Context, sessionKey, conversationKey string) (bool, error) {
	return false, errors.New("unused")
}
func (m *memoryTaskStore) SetTaskCapabilityJTIs(ctx context.Context, taskID, platformInstanceID string, jtis []string) error {
	return errors.New("unused")
}

// TestSubmitTaskErrorMapping 验证 Round16-P1: SubmitTask 错误按语义分级
// (429/403/404), 旧实现全 500 SUBMIT_FAILED 导致客户端无法区分可恢复
// 与配置错误。
func TestSubmitTaskErrorMapping(t *testing.T) {
	_, file, _, _ := runtime.Caller(0)
	polPath := filepath.Clean(filepath.Join(filepath.Dir(file), "..", "..", "..", "contracts", "policy", "foundation.v1.json"))
	reg, err := policy.LoadRegistry(polPath)
	if err != nil {
		t.Fatalf("LoadRegistry: %v", err)
	}
	cases := []struct {
		name       string
		err        error
		wantStatus int
		wantCode   string
	}{
		{"queue full maps to 429", domain.ErrPerUserQueueFull, http.StatusTooManyRequests, "QUEUE_FULL"},
		{"session access denied maps to 403", domain.ErrSessionAccessDenied, http.StatusForbidden, "ACCESS_DENIED"},
		{"workspace not found maps to 404", domain.ErrWorkspaceNotFound, http.StatusNotFound, "WORKSPACE_NOT_FOUND"},
		{"unknown error stays 500", errors.New("boom"), http.StatusInternalServerError, "SUBMIT_FAILED"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := &memoryTaskStore{submitErr: tc.err}
			svc, err := application.NewTaskService(application.TaskServiceConfig{
				Store:              store,
				Registry:           reg,
				PlatformInstanceID: "test-platform",
				ClaimLease:         time.Second,
			})
			if err != nil {
				t.Fatalf("NewTaskService: %v", err)
			}
			srv := &Server{svc: svc}
			req := httptest.NewRequest(http.MethodPost, "/v1/sessions/personal:1/tasks",
				strings.NewReader(`{"prompt":"hi","message_id":"m-1","source_instance_id":"s-1","source":"web","persona_snapshot":[]}`))
			req.SetPathValue("session_key", "personal:1")
			req = req.WithContext(context.WithValue(req.Context(), ctxUserIDKey, int64(1)))
			rec := httptest.NewRecorder()
			srv.handleCreateTask(rec, req)
			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d (body %s)", rec.Code, tc.wantStatus, rec.Body.String())
			}
			var body struct {
				Code string `json:"code"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode body: %v", err)
			}
			if body.Code != tc.wantCode {
				t.Fatalf("code = %q, want %q", body.Code, tc.wantCode)
			}
		})
	}
}
