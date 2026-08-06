package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/application"
	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
)

type inviteDeleteService struct {
	deletedCodes []string
	deletedCount int64
	deleteErr    error
	revokedCode  string
}

func (s *inviteDeleteService) GenerateInviteCode(context.Context, int64) (string, domain.InviteCode, error) {
	return "", domain.InviteCode{}, nil
}

func (s *inviteDeleteService) RevokeInviteCode(_ context.Context, code string) error {
	s.revokedCode = code
	return nil
}

func (s *inviteDeleteService) ListInviteCodes(context.Context) ([]domain.InviteCode, error) {
	return nil, nil
}

func (s *inviteDeleteService) DeleteInviteCodes(_ context.Context, codes []string) (int64, error) {
	s.deletedCodes = append([]string(nil), codes...)
	return s.deletedCount, s.deleteErr
}

func (s *inviteDeleteService) RegisterWithInvite(context.Context, string, string, string) (domain.User, string, error) {
	return domain.User{}, "", nil
}

func (s *inviteDeleteService) Login(context.Context, string, string) (domain.User, string, error) {
	return domain.User{}, "", nil
}

func (s *inviteDeleteService) ValidateSession(context.Context, string) (int64, error) {
	return 0, nil
}

func inviteDeleteServer(t *testing.T, invite *inviteDeleteService) *Server {
	t.Helper()
	srv, err := NewServer(ServerConfig{
		Service:   dashboardFakeTaskService{},
		Registry:  dashboardFakeRegistry{},
		Invite:    invite,
		AdminToken:  "test-admin token",
		AdminUserID: 9,
	})
	if err != nil {
		t.Fatal(err)
	}
	return srv
}

func TestAdminDeleteInviteCodes(t *testing.T) {
	invite := &inviteDeleteService{deletedCount: 2}
	srv := inviteDeleteServer(t, invite)
	body, _ := json.Marshal(map[string]any{"codes": []string{"CODE-A", "CODE-B"}})
	req := httptest.NewRequest(http.MethodDelete, "/v1/admin/invite-codes", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Platform-Admin-Token", "test-admin token")
	rr := httptest.NewRecorder()

	srv.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if len(invite.deletedCodes) != 2 || invite.deletedCodes[0] != "CODE-A" || invite.deletedCodes[1] != "CODE-B" {
		t.Fatalf("deleted codes=%v", invite.deletedCodes)
	}
	var response struct {
		Deleted int64 `json:"deleted"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Deleted != 2 {
		t.Fatalf("deleted=%d, want 2", response.Deleted)
	}
}

func TestAdminDeleteInviteCodesRejectsUnknownFields(t *testing.T) {
	invite := &inviteDeleteService{}
	srv := inviteDeleteServer(t, invite)
	req := httptest.NewRequest(http.MethodDelete, "/v1/admin/invite-codes", bytes.NewBufferString(`{"codes":["CODE-A"],"force":true}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Platform-Admin-Token", "test-admin token")
	rr := httptest.NewRecorder()

	srv.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if len(invite.deletedCodes) != 0 {
		t.Fatalf("service must not be called, got %v", invite.deletedCodes)
	}
}

func TestAdminDeleteInviteCodesRejectsTrailingJSON(t *testing.T) {
	invite := &inviteDeleteService{}
	srv := inviteDeleteServer(t, invite)
	req := httptest.NewRequest(http.MethodDelete, "/v1/admin/invite-codes", bytes.NewBufferString(`{"codes":["CODE-A"]}{"codes":["CODE-B"]}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Platform-Admin-Token", "test-admin token")
	rr := httptest.NewRecorder()

	srv.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if len(invite.deletedCodes) != 0 {
		t.Fatalf("service must not be called, got %v", invite.deletedCodes)
	}
}

func TestAdminDeleteInviteCodesMapsStoreFailureToServerError(t *testing.T) {
	invite := &inviteDeleteService{deleteErr: errors.New("database unavailable")}
	srv := inviteDeleteServer(t, invite)
	req := httptest.NewRequest(http.MethodDelete, "/v1/admin/invite-codes", bytes.NewBufferString(`{"codes":["CODE-A"]}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Platform-Admin-Token", "test-admin token")
	rr := httptest.NewRecorder()

	srv.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if bytes.Contains(rr.Body.Bytes(), []byte("database unavailable")) {
		t.Fatalf("response exposed storage error: %s", rr.Body.String())
	}
}

func TestAdminDeleteInviteCodesRejectsEmptySelection(t *testing.T) {
	invite := &inviteDeleteService{deleteErr: application.ErrInviteCodesRequired}
	srv := inviteDeleteServer(t, invite)
	req := httptest.NewRequest(http.MethodDelete, "/v1/admin/invite-codes", bytes.NewBufferString(`{"codes":[]}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Platform-Admin-Token", "test-admin token")
	rr := httptest.NewRecorder()

	srv.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
}

func TestAdminRevokeInviteCodeRouteRemainsCompatible(t *testing.T) {
	invite := &inviteDeleteService{}
	srv := inviteDeleteServer(t, invite)
	req := httptest.NewRequest(http.MethodDelete, "/v1/admin/invite-codes/CODE-A", nil)
	req.Header.Set("X-Platform-Admin-Token", "test-admin token")
	rr := httptest.NewRecorder()

	srv.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if invite.revokedCode != "CODE-A" {
		t.Fatalf("revoked code=%q", invite.revokedCode)
	}
}
