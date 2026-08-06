package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/application"
	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/infrastructure/policy"
	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/infrastructure/postgres"
)

func apiFixture(t *testing.T) (*Server, string, *postgres.Store) {
	t.Helper()
	pool := postgres.OpenTestPool(t)
	store, err := postgres.NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	_, file, _, _ := runtime.Caller(0)
	polPath := filepath.Clean(filepath.Join(filepath.Dir(file), "..", "..", "..", "contracts", "policy", "foundation.v1.json"))
	reg, err := policy.LoadRegistry(polPath)
	if err != nil {
		t.Fatal(err)
	}
	dev, err := store.EnsureAdminContext(context.Background(), 9, "api-dev")
	if err != nil {
		t.Fatal(err)
	}
	svc, err := application.NewTaskService(application.TaskServiceConfig{
		Store: store, Registry: reg, PlatformInstanceID: "api-inst", ClaimLease: time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	srv, err := NewServer(ServerConfig{
		Service:   svc,
		Registry:  reg,
		TaskStats: store,
		// 审查 I-4: 任务端点只认用户 Bearer; 测试注入固定身份 fake。
		Invite: &fixedInviteService{userID: 9},
		RuntimeProfile: RuntimeProfile{
			ClaimLeaseSeconds:       60,
			TokenTTLSeconds:         3600,
			TokenRefreshSkewSeconds: 300,
			MaxTaskWallClockSeconds: 2700,
			TaskTimeoutSeconds:      0,
			TaskIdleTimeoutSeconds:  300,
			MaxRunningTasks:         16,
			PerRequesterRunningLimit:   4,
			PerUserQueueLimit:       8,
		},
		AdminToken: "test-admin token", AdminUserID: 9, SessionKey: dev.SessionKey,
	})
	if err != nil {
		t.Fatal(err)
	}
	return srv, dev.SessionKey, store
}

// fixedInviteService 是测试用固定身份 invite(审查 I-4: 任务端点 userAuth
// 所需): 按 token 映射到 userID, 未命中返回错误。
type fixedInviteService struct {
	userID int64
	tokens map[string]int64
}

func (s *fixedInviteService) GenerateInviteCode(context.Context, int64) (string, domain.InviteCode, error) {
	return "", domain.InviteCode{}, nil
}

func (s *fixedInviteService) RevokeInviteCode(context.Context, string) error { return nil }

func (s *fixedInviteService) DeleteInviteCodes(context.Context, []string) (int64, error) {
	return 0, nil
}

func (s *fixedInviteService) ListInviteCodes(context.Context) ([]domain.InviteCode, error) {
	return nil, nil
}

func (s *fixedInviteService) RegisterWithInvite(context.Context, string, string, string) (domain.User, string, error) {
	return domain.User{}, "", nil
}

func (s *fixedInviteService) Login(context.Context, string, string) (domain.User, string, error) {
	return domain.User{}, "", nil
}

func (s *fixedInviteService) ValidateSession(_ context.Context, token string) (int64, error) {
	if s.tokens != nil {
		if uid, ok := s.tokens[token]; ok {
			return uid, nil
		}
		return 0, fmt.Errorf("unknown token")
	}
	return s.userID, nil
}

// bearerHeader 返回任务端点测试所需的用户 Bearer 头(审查 I-4)。
func bearerHeader(r *http.Request) *http.Request {
	r.Header.Set("Authorization", "Bearer test-user-token")
	return r
}

// TestTaskOwnershipEnforced 验证审查 I-4 归属模型: 任务仅创建者可读/取结果/
// 取消; 其他用户访问返回 404(not-found 语义, 不泄露存在性); 越权向他人
// personal 会话提交被拒。
func TestTaskOwnershipEnforced(t *testing.T) {
	srv, _, store := apiFixture(t)
	srv.invite = &fixedInviteService{tokens: map[string]int64{"token-a": 9, "token-b": 77}}
	// 用户 B(77) 需存在且有自己的 workspace 才能提交任务。
	if _, err := store.EnsureAdminContext(context.Background(), 77, "user-b"); err != nil {
		t.Fatal(err)
	}
	body := map[string]any{
		"message_id": "m-owner", "source_instance_id": "si", "prompt": "owner task", "source": "web",
		"persona_snapshot": []string{"p"},
	}
	raw, _ := json.Marshal(body)
	post := func(token, path string) *httptest.ResponseRecorder {
		rr := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(raw))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+token)
		srv.Handler().ServeHTTP(rr, req)
		return rr
	}
	get := func(token, path string) *httptest.ResponseRecorder {
		rr := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		srv.Handler().ServeHTTP(rr, req)
		return rr
	}
	// 用户 A(9) 创建任务到自己的 personal 会话。
	rr := post("token-a", "/v1/sessions/personal:9/tasks")
	if rr.Code != http.StatusAccepted {
		t.Fatalf("owner create: status=%d body=%s", rr.Code, rr.Body.String())
	}
	var resp map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &resp)
	taskID, _ := resp["task_id"].(string)
	if taskID == "" {
		t.Fatalf("no task id: %v", resp)
	}
	// 用户 A 可读/取结果(无结果时 400 而非 404——任务存在且归属)。
	if rr = get("token-a", "/v1/tasks/"+taskID); rr.Code != http.StatusOK {
		t.Fatalf("owner get: %d %s", rr.Code, rr.Body.String())
	}
	// 用户 B(77) 不可读(404, 不泄露)。
	if rr = get("token-b", "/v1/tasks/"+taskID); rr.Code != http.StatusNotFound {
		t.Fatalf("other get: want 404 got %d %s", rr.Code, rr.Body.String())
	}
	// 用户 B 不可取结果(404)。
	if rr = get("token-b", "/v1/tasks/"+taskID+"/result"); rr.Code != http.StatusNotFound {
		t.Fatalf("other result: want 404 got %d %s", rr.Code, rr.Body.String())
	}
	// 用户 B 不可取消(404)。
	if rr = post("token-b", "/v1/tasks/"+taskID+"/cancel"); rr.Code != http.StatusNotFound {
		t.Fatalf("other cancel: want 404 got %d %s", rr.Code, rr.Body.String())
	}
	// 用户 B 不可向用户 A 的 personal 会话提交任务。
	if rr = post("token-b", "/v1/sessions/personal:9/tasks"); rr.Code == http.StatusAccepted {
		t.Fatal("cross-user session submit must be rejected")
	}
	// 用户 B 可向自己的 personal 会话提交。
	if rr = post("token-b", "/v1/sessions/personal:77/tasks"); rr.Code != http.StatusAccepted {
		t.Fatalf("own session submit: %d %s", rr.Code, rr.Body.String())
	}
}

func TestHealthzAndAuth(t *testing.T) {
	srv, sk, _ := apiFixture(t)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rr.Code != 200 {
		t.Fatalf("health %d", rr.Code)
	}
	body := map[string]any{
		"message_id": "m1", "source_instance_id": "si", "prompt": "hi", "source": "web",
		"persona_snapshot": []string{"p"},
	}
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/v1/sessions/"+sk+"/tasks", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	rr = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("want 401 got %d", rr.Code)
	}
}

func TestSubmitRejectsUnknownToolPolicyField(t *testing.T) {
	srv, sk, _ := apiFixture(t)
	body := map[string]any{
		"message_id": "m2", "source_instance_id": "si", "prompt": "hi", "source": "web",
		"persona_snapshot": []string{"p"}, "tool_policy_version": "other.host-tools.v1",
	}
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/v1/sessions/"+sk+"/tasks", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	bearerHeader(req)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code == http.StatusAccepted {
		t.Fatal("must reject unknown tool_policy_version field (去分级后字段已移除)")
	}
	var errBody map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &errBody)
	if errBody["code"] == nil {
		t.Fatalf("expected error code: %s", rr.Body.String())
	}
}

func TestSubmit202AndGetTask(t *testing.T) {
	srv, sk, _ := apiFixture(t)
	body := map[string]any{
		"message_id": "m3", "source_instance_id": "si", "prompt": "hello api", "source": "web",
		"persona_snapshot": []string{"p"},
	}
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/v1/sessions/"+sk+"/tasks", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	bearerHeader(req)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusAccepted {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var resp map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &resp)
	taskID, _ := resp["task_id"].(string)
	if taskID == "" || resp["status"] != "queued" {
		t.Fatalf("resp=%v", resp)
	}
	req = httptest.NewRequest(http.MethodGet, "/v1/tasks/"+taskID, nil)
	bearerHeader(req)
	rr = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("get %d", rr.Code)
	}
}

func TestResultRejectsPathLikeRef(t *testing.T) {
	srv, _, _ := apiFixture(t)
	req := httptest.NewRequest(http.MethodGet, "/v1/tasks/does-not-exist/result?result_ref=C:%5Csecrets", nil)
	bearerHeader(req)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code == 200 {
		t.Fatal("path-like ref must be rejected")
	}
}

type dashboardFakeTaskService struct{}

type dashboardFakeRegistry struct{}

type dashboardFakeUserService struct {
	pending  int
	approved int
}

type dashboardFakeTaskStats struct{ running int }

func (dashboardFakeTaskService) SubmitTask(context.Context, domain.SubmitTaskCommand) (domain.Task, error) {
	return domain.Task{}, nil
}
func (dashboardFakeTaskService) SubmitTaskWithInboundMessage(context.Context, domain.SubmitTaskCommand, domain.Message) (domain.Task, domain.Message, error) {
	return domain.Task{}, domain.Message{}, nil
}
func (dashboardFakeTaskService) GetTask(context.Context, string, int64) (domain.Task, error) {
	return domain.Task{}, nil
}
func (dashboardFakeTaskService) CancelTask(context.Context, string, int64) (domain.Task, error) {
	return domain.Task{}, nil
}
func (dashboardFakeTaskService) ClaimNextTask(context.Context, string, string) (domain.Task, bool, error) {
	return domain.Task{}, false, nil
}
func (dashboardFakeTaskService) RecoverAfterRestart(context.Context, string) error { return nil }
func (dashboardFakeTaskService) ReadResult(context.Context, string, int64) (domain.ResultPayload, error) {
	return domain.ResultPayload{}, nil
}

func (dashboardFakeRegistry) Digest() string { return "sha256:test" }
func (dashboardFakeRegistry) Resolve(string, string) (policy.ToolPolicy, error) {
	return policy.ToolPolicy{Version: "foundation.no-host-tools.v1", AllowedTools: []string{"read"}}, nil
}

func (f dashboardFakeUserService) CreateUser(context.Context, string, string) (domain.User, error) {
	return domain.User{}, nil
}
func (f dashboardFakeUserService) ApproveUser(context.Context, int64) (domain.User, error) {
	return domain.User{}, nil
}
func (f dashboardFakeUserService) BlockUser(context.Context, int64) (domain.User, error) {
	return domain.User{}, nil
}
func (f dashboardFakeUserService) ListPendingUsers(context.Context) ([]domain.User, error) {
	return nil, nil
}
func (f dashboardFakeUserService) CountPendingUsers(context.Context) (int, error) {
	return f.pending, nil
}
func (f dashboardFakeUserService) CountApprovedUsers(context.Context) (int, error) {
	return f.approved, nil
}

func (f dashboardFakeTaskStats) CountRunningTasks(context.Context) (int, error) {
	return f.running, nil
}

func TestAdminDashboardStatsExposeRuntimeProfileAndRunningTasks(t *testing.T) {
	srv, err := NewServer(ServerConfig{
		Service:   dashboardFakeTaskService{},
		Users:     dashboardFakeUserService{pending: 2, approved: 5},
		TaskStats: dashboardFakeTaskStats{running: 3},
		Registry:  dashboardFakeRegistry{},
		RuntimeProfile: RuntimeProfile{
			ClaimLeaseSeconds:         60,
			TokenTTLSeconds:           3600,
			TokenRefreshSkewSeconds:   300,
			MaxTaskWallClockSeconds:   2700,
			TaskTimeoutSeconds:        0,
			TaskIdleTimeoutSeconds:    300,
			MaxRunningTasks:           16,
			PerRequesterRunningLimit:     4,
			PerUserQueueLimit:         8,
			IMInboundCoalesceWindowMS: 2500,
		},
		AdminToken:  "test-admin token",
		AdminUserID: 9,
	})
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/v1/admin/dashboard/stats", nil)
	req.Header.Set("X-Platform-Admin-Token", "test-admin token")
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("dashboard status=%d body=%s", rr.Code, rr.Body.String())
	}
	var stats dashboardStatsResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &stats); err != nil {
		t.Fatal(err)
	}
	if stats.PendingUsers != 2 || stats.ApprovedUsers != 5 || stats.RunningTasks != 3 {
		t.Fatalf("stats=%+v", stats)
	}
	if stats.RuntimeProfile.MaxTaskWallClockSeconds != 2700 || stats.RuntimeProfile.TaskIdleTimeoutSeconds != 300 {
		t.Fatalf("runtime profile=%+v", stats.RuntimeProfile)
	}
	if stats.RuntimeProfile.TokenTTLSeconds != 3600 || stats.RuntimeProfile.PerUserQueueLimit != 8 {
		t.Fatalf("runtime profile=%+v", stats.RuntimeProfile)
	}
	if stats.RuntimeProfile.IMInboundCoalesceWindowMS != 2500 {
		t.Fatalf("runtime profile=%+v", stats.RuntimeProfile)
	}
}
