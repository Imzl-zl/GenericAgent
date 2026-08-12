package api

import (
	"errors"
	"net/http"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
)

// writeStoreError 把 store/service 层错误映射为 HTTP 状态码(错误域分类,
// 见 tenant_platform/docs/FAILURE_POLICY.zh-CN.md):
//
//   - 业务拒绝哨兵(errors.Is 命中 domain 哨兵)→ 4xx——客户端可修正的
//     输入/状态问题(不存在 404、冲突 409), 重试无意义;
//   - 其余一律 500——基础设施故障(DB 不可用等), 不得降级为客户端错误
//     (2026-08 审查: 历史实现把 store 错误统一映射 400, 客户端会把
//     DB 故障当成自己的输入错误无限重试)。
//
// 新增业务哨兵时在此登记分支, handler 侧只需调用本函数。
func writeStoreError(w http.ResponseWriter, err error, code, tid string) {
	status := http.StatusInternalServerError
	switch {
	case errors.Is(err, domain.ErrValidation),
		errors.Is(err, domain.ErrInviteCodeInvalid):
		status = http.StatusBadRequest
	case errors.Is(err, domain.ErrMCPServerNotFound),
		errors.Is(err, domain.ErrUserNotFound),
		errors.Is(err, domain.ErrProviderNotFound),
		errors.Is(err, domain.ErrInviteNotFound),
		errors.Is(err, domain.ErrChannelBindingNotFound):
		status = http.StatusNotFound
	case errors.Is(err, domain.ErrMCPServerConflict),
		errors.Is(err, domain.ErrUsernameExists),
		errors.Is(err, domain.ErrProviderStateConflict):
		status = http.StatusConflict
	}
	writeErr(w, status, code, err.Error(), tid)
}
