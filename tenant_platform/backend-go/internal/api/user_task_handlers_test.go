package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
)

// userTaskFakeUserService embeds the dashboard fake (satisfies the full
// UserService interface) and overrides the two self-service task methods.
type userTaskFakeUserService struct {
	dashboardFakeUserService
	tasks []domain.Task
	stats map[domain.TaskStatus]int
}

func (f *userTaskFakeUserService) ListMyTasks(_ context.Context, _ int64, _ int) ([]domain.Task, error) {
	return f.tasks, nil
}

func (f *userTaskFakeUserService) CountMyTaskStats(_ context.Context, _ int64) (map[domain.TaskStatus]int, error) {
	return f.stats, nil
}

func sampleTask(id string, status domain.TaskStatus) domain.Task {
	now := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	at := now
	return domain.Task{
		ID:          id,
		SessionKey:  "personal:1",
		RequesterID: 1,
		Source:      domain.SourceWeb,
		Status:      status,
		CreatedAt:   at,
		TerminalAt:  &at,
	}
}

func TestListMyTasksReturnsCompactEnvelope(t *testing.T) {
	svc := &userTaskFakeUserService{
		tasks: []domain.Task{
			sampleTask("task-1", domain.TaskSucceeded),
			sampleTask("task-2", domain.TaskQueued),
		},
	}
	srv := &Server{users: svc}

	req := httptest.NewRequest(http.MethodGet, "/v1/users/me/tasks?limit=10", nil)
	req = req.WithContext(context.WithValue(req.Context(), ctxUserIDKey, int64(1)))
	rec := httptest.NewRecorder()
	srv.handleListMyTasks(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Tasks []map[string]any `json:"tasks"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Tasks) != 2 {
		t.Fatalf("want 2 tasks, got %d", len(body.Tasks))
	}
	if body.Tasks[0]["task_id"] != "task-1" || body.Tasks[0]["status"] != "succeeded" {
		t.Fatalf("first task envelope wrong: %v", body.Tasks[0])
	}
	if _, ok := body.Tasks[0]["source"]; !ok {
		t.Fatal("source field missing")
	}
	if _, ok := body.Tasks[0]["created_at"]; !ok {
		t.Fatal("created_at field missing")
	}
	if _, ok := body.Tasks[0]["terminal_at"]; !ok {
		t.Fatal("terminal_at field missing for terminal task")
	}
}

func TestListMyTasksRejectsInvalidLimit(t *testing.T) {
	srv := &Server{users: &userTaskFakeUserService{}}
	req := httptest.NewRequest(http.MethodGet, "/v1/users/me/tasks?limit=abc", nil)
	req = req.WithContext(context.WithValue(req.Context(), ctxUserIDKey, int64(1)))
	rec := httptest.NewRecorder()
	srv.handleListMyTasks(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	req = httptest.NewRequest(http.MethodGet, "/v1/users/me/tasks?limit=0", nil)
	req = req.WithContext(context.WithValue(req.Context(), ctxUserIDKey, int64(1)))
	rec = httptest.NewRecorder()
	srv.handleListMyTasks(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for limit=0", rec.Code)
	}
}

func TestMyTaskStatsFillsZerosAndSumsRunning(t *testing.T) {
	svc := &userTaskFakeUserService{
		stats: map[domain.TaskStatus]int{
			domain.TaskQueued:    1,
			domain.TaskStarting:  2,
			domain.TaskRunning:   3,
			domain.TaskSucceeded: 4,
		},
	}
	srv := &Server{users: svc}
	req := httptest.NewRequest(http.MethodGet, "/v1/users/me/task-stats", nil)
	req = req.WithContext(context.WithValue(req.Context(), ctxUserIDKey, int64(1)))
	rec := httptest.NewRecorder()
	srv.handleMyTaskStats(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	var body map[string]int
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	// running = starting + running 合计。
	if body["running"] != 5 {
		t.Fatalf("running = %d, want 5", body["running"])
	}
	if body["queued"] != 1 || body["succeeded"] != 4 {
		t.Fatalf("queued/succeeded wrong: %v", body)
	}
	// 未出现的状态必须填零。
	if body["failed"] != 0 || body["cancelled"] != 0 || body["interrupted"] != 0 {
		t.Fatalf("zero-fill wrong: %v", body)
	}
	if body["total"] != 10 {
		t.Fatalf("total = %d, want 10", body["total"])
	}
}

func TestUserTaskRoutesRequireUserAuth(t *testing.T) {
	srv := &Server{users: &userTaskFakeUserService{}, invite: &fixedInviteService{}, mux: http.NewServeMux()}
	srv.registerUserTaskRoutes()

	for _, path := range []string{"/v1/users/me/tasks", "/v1/users/me/task-stats"} {
		req := httptest.NewRequest(http.MethodGet, path, nil) // 无 Authorization
		rec := httptest.NewRecorder()
		srv.mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s without token: status = %d, want 401", path, rec.Code)
		}
	}
}
