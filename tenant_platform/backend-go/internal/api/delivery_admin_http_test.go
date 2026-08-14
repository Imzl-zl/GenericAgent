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

// fakeDeliveryAdminStore 是 DeliveryAdminStore 的内存实现(admin 死信端点
// 单测, 2026-08-14 审查 E2)。
type fakeDeliveryAdminStore struct {
	rows     []domain.DeliveryAdminRow
	requeue  map[string]bool // deliveryID → 是否死信(可重投)
	requeued []string
}

func (f *fakeDeliveryAdminStore) ListDeliveries(_ context.Context, status string, limit int) ([]domain.DeliveryAdminRow, error) {
	var out []domain.DeliveryAdminRow
	for _, r := range f.rows {
		if status == "" || string(r.Status) == status {
			out = append(out, r)
		}
	}
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (f *fakeDeliveryAdminStore) RequeueDeadLetterDelivery(_ context.Context, deliveryID string, _ time.Time) (bool, error) {
	if f.requeue[deliveryID] {
		f.requeue[deliveryID] = false
		f.requeued = append(f.requeued, deliveryID)
		return true, nil
	}
	return false, nil
}

func newDeliveryAdminTestServer(t *testing.T) *Server {
	t.Helper()
	s := &Server{
		adminToken: "admin-secret",
		deliveries: &fakeDeliveryAdminStore{
			requeue: map[string]bool{},
		},
		mux: http.NewServeMux(),
	}
	s.mux.HandleFunc("GET /v1/admin/deliveries", s.auth(s.handleAdminListDeliveries))
	s.mux.HandleFunc("POST /v1/admin/deliveries/{delivery_id}/requeue", s.auth(s.handleAdminRequeueDelivery))
	return s
}

func TestAdminListDeliveries(t *testing.T) {
	s := newDeliveryAdminTestServer(t)
	dl := s.deliveries.(*fakeDeliveryAdminStore)
	ts := time.Now().UTC()
	dl.rows = []domain.DeliveryAdminRow{
		{DeliveryID: "t1:task_failed", TaskID: "t1", DeliveryType: domain.DeliveryTaskFailed,
			Status: "dead_letter", ErrorCode: "SEND_FAILED", ErrorMessage: "boom", AttemptCount: 10, CreatedAt: ts},
		{DeliveryID: "t2:task_complete", TaskID: "t2", DeliveryType: domain.DeliveryTaskComplete,
			Status: "acked", CreatedAt: ts},
	}

	// 默认只列死信。
	req := httptest.NewRequest(http.MethodGet, "/v1/admin/deliveries", nil)
	req.Header.Set("X-Platform-Admin-Token", "admin-secret")
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		Deliveries []struct {
			DeliveryID   string `json:"delivery_id"`
			ErrorCode    string `json:"error_code"`
			AttemptCount int    `json:"attempt_count"`
		} `json:"deliveries"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Deliveries) != 1 || body.Deliveries[0].DeliveryID != "t1:task_failed" ||
		body.Deliveries[0].ErrorCode != "SEND_FAILED" || body.Deliveries[0].AttemptCount != 10 {
		t.Fatalf("unexpected list: %+v", body.Deliveries)
	}

	// 未带 AdminToken → 401。
	req2 := httptest.NewRequest(http.MethodGet, "/v1/admin/deliveries", nil)
	rec2 := httptest.NewRecorder()
	s.mux.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusUnauthorized {
		t.Fatalf("unauth status=%d", rec2.Code)
	}
}

func TestAdminRequeueDelivery(t *testing.T) {
	s := newDeliveryAdminTestServer(t)
	dl := s.deliveries.(*fakeDeliveryAdminStore)
	dl.requeue["t1:task_failed"] = true

	// 死信重投 → 200 requeued。
	req := httptest.NewRequest(http.MethodPost, "/v1/admin/deliveries/t1:task_failed/requeue", nil)
	req.Header.Set("X-Platform-Admin-Token", "admin-secret")
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if len(dl.requeued) != 1 || dl.requeued[0] != "t1:task_failed" {
		t.Fatalf("requeue not recorded: %+v", dl.requeued)
	}

	// 非死信/不存在 → 404。
	req2 := httptest.NewRequest(http.MethodPost, "/v1/admin/deliveries/t2:task_complete/requeue", nil)
	req2.Header.Set("X-Platform-Admin-Token", "admin-secret")
	rec2 := httptest.NewRecorder()
	s.mux.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusNotFound {
		t.Fatalf("non-dead-letter requeue status=%d body=%s", rec2.Code, rec2.Body.String())
	}

	// 未带 AdminToken → 401。
	req3 := httptest.NewRequest(http.MethodPost, "/v1/admin/deliveries/t1:task_failed/requeue", nil)
	rec3 := httptest.NewRecorder()
	s.mux.ServeHTTP(rec3, req3)
	if rec3.Code != http.StatusUnauthorized {
		t.Fatalf("unauth status=%d", rec3.Code)
	}
}
