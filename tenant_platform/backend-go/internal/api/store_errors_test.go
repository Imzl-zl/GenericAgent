package api

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
)

// TestWriteStoreErrorClassifiesBusinessRejections(2026-08 审查, 错误域分类):
// 业务拒绝哨兵 → 4xx(客户端可修正), 基础设施错误 → 500。此映射是 admin
// 写操作统一错误语义的单一真值源——新增哨兵必须在此登记。
func TestWriteStoreErrorClassifiesBusinessRejections(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"validation → 400", fmt.Errorf("%w: username is required", domain.ErrValidation), http.StatusBadRequest},
		{"invite code invalid → 400", domain.ErrInviteCodeInvalid, http.StatusBadRequest},
		{"user not found → 404", domain.ErrUserNotFound, http.StatusNotFound},
		{"provider not found → 404", domain.ErrProviderNotFound, http.StatusNotFound},
		{"invite not found → 404", domain.ErrInviteNotFound, http.StatusNotFound},
		{"mcp server not found → 404", domain.ErrMCPServerNotFound, http.StatusNotFound},
		{"channel binding not found → 404", domain.ErrChannelBindingNotFound, http.StatusNotFound},
		{"username exists → 409", domain.ErrUsernameExists, http.StatusConflict},
		{"provider state conflict → 409", domain.ErrProviderStateConflict, http.StatusConflict},
		{"mcp server conflict → 409", domain.ErrMCPServerConflict, http.StatusConflict},
		{"wrapped business sentinel survives %w", fmt.Errorf("db layer: %w", domain.ErrUsernameExists), http.StatusConflict},
		{"bare infra error → 500", errors.New("connection refused"), http.StatusInternalServerError},
		{"wrapped infra error → 500", fmt.Errorf("query failed: %w", errors.New("pool closed")), http.StatusInternalServerError},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			writeStoreError(rec, tc.err, "TEST_FAILED", "tid-1")
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d (body=%s)", rec.Code, tc.want, rec.Body.String())
			}
			if body := rec.Body.String(); !strings.Contains(body, "TEST_FAILED") {
				t.Fatalf("body = %s, want code TEST_FAILED", body)
			}
		})
	}
}
