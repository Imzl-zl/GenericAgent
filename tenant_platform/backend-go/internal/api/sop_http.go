package api

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
)

type sophubBindingBody struct {
	APIKey string `json:"api_key"`
}

type sophubBindingStatusReply struct {
	Configured  bool   `json:"configured"`
	DisplayName string `json:"display_name,omitempty"`
	AuthorType  string `json:"author_type,omitempty"`
	AgentUID    string `json:"agent_uid,omitempty"`
}

func toSophubBindingStatusReply(status domain.SophubBindingStatus) sophubBindingStatusReply {
	return sophubBindingStatusReply{
		Configured:  status.Configured,
		DisplayName: status.DisplayName,
		AuthorType:  status.AuthorType,
		AgentUID:    status.AgentUID,
	}
}

func (s *Server) handleAdminGetSophubBinding(w http.ResponseWriter, r *http.Request) {
	tid := traceID()
	status, err := s.sophub.GetBindingStatus(r.Context())
	if err != nil {
		// 审查: 未绑定是正常初始状态, 返回 200 configured:false 而不是 502
		// (与 SophubBindingStatus 契约一致, 前端可据此渲染"去绑定"入口)。
		if errors.Is(err, domain.ErrSophubNotConfigured) {
			writeJSON(w, http.StatusOK, sophubBindingStatusReply{Configured: false})
			return
		}
		writeErr(w, http.StatusBadGateway, "SOPHUB_STATUS_FAILED", err.Error(), tid)
		return
	}
	writeJSON(w, http.StatusOK, toSophubBindingStatusReply(status))
}

func (s *Server) handleAdminBindSophub(w http.ResponseWriter, r *http.Request) {
	tid := traceID()
	var body sophubBindingBody
	if err := decodeStrict(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "INVALID_JSON", err.Error(), tid)
		return
	}
	status, err := s.sophub.Bind(r.Context(), body.APIKey, s.devUserID)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "SOPHUB_BIND_FAILED", err.Error(), tid)
		return
	}
	writeJSON(w, http.StatusOK, toSophubBindingStatusReply(status))
}

func (s *Server) handleAdminSearchSophub(w http.ResponseWriter, r *http.Request) {
	tid := traceID()
	page, ok := parsePositiveQueryInt(w, r, "page", 1, 100000, tid)
	if !ok {
		return
	}
	pageSize, ok := parsePositiveQueryInt(w, r, "page_size", 24, 100, tid)
	if !ok {
		return
	}
	result, err := s.sophub.Search(r.Context(), strings.TrimSpace(r.URL.Query().Get("q")), page, pageSize)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "SOPHUB_SEARCH_FAILED", err.Error(), tid)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// parsePositiveQueryInt 解析正整数查询参数(带范围与默认值)。
func parsePositiveQueryInt(w http.ResponseWriter, r *http.Request, name string, def, max int, tid string) (int, bool) {
	raw := strings.TrimSpace(r.URL.Query().Get(name))
	if raw == "" {
		return def, true
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 || n > max {
		writeErr(w, http.StatusBadRequest, "INVALID_QUERY", name+" must be a positive integer", tid)
		return 0, false
	}
	return n, true
}
