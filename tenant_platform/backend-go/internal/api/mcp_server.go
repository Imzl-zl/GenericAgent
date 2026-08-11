package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
)

type mcpServerWriteBody struct {
	ServerKey      string            `json:"server_key"`
	Name           string            `json:"name"`
	URL            string            `json:"url"`
	TimeoutSeconds int               `json:"timeout_seconds"`
	Headers        map[string]string `json:"headers"`
	Transport      string            `json:"transport"`
	Command        string            `json:"command"`
	Args           []string          `json:"args"`
	Isolation      string            `json:"isolation"`
	MaxInstances   int               `json:"max_instances"`
}

func (b *mcpServerWriteBody) normalize() error {
	b.ServerKey = strings.TrimSpace(b.ServerKey)
	b.Name = strings.TrimSpace(b.Name)
	b.URL = strings.TrimSpace(b.URL)
	return domain.ValidateMCPServerInput(domain.MCPServerCreate{
		ServerKey: b.ServerKey, Name: b.Name, URL: b.URL,
		TimeoutSeconds: b.TimeoutSeconds, Headers: b.Headers,
		Transport: b.Transport,
		Command: b.Command, Args: b.Args, Isolation: b.Isolation,
		MaxInstances: b.MaxInstances,
	})
}

func (s *Server) handleAdminCreateMCPServer(w http.ResponseWriter, r *http.Request) {
	tid := traceID()
	var body mcpServerWriteBody
	if err := decodeStrict(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "INVALID_JSON", err.Error(), tid)
		return
	}
	if err := body.normalize(); err != nil {
		writeErr(w, http.StatusBadRequest, "VALIDATION_ERROR", err.Error(), tid)
		return
	}
	// 新建必须明文(掩码只出现在回显/更新保留语义中)。
	for key, value := range body.Headers {
		if strings.HasSuffix(value, "***") {
			writeErr(w, http.StatusBadRequest, "VALIDATION_ERROR", fmt.Sprintf("header %q must be a plaintext value on create (masked values are only valid on update)", key), tid)
			return
		}
	}
	server, err := s.mcpServers.CreateMCPServer(r.Context(), domain.MCPServerCreate{
		ServerKey: body.ServerKey, Name: body.Name, URL: body.URL,
		TimeoutSeconds: body.TimeoutSeconds, Headers: body.Headers,
		Transport: body.Transport,
		Command: body.Command, Args: body.Args, Isolation: body.Isolation,
		MaxInstances: body.MaxInstances,
	})
	if err != nil {
		writeMCPStoreError(w, err, "MCP_SERVER_CREATE_FAILED", tid)
		return
	}
	writeJSON(w, http.StatusCreated, mcpServerReply(server))
}

func (s *Server) handleAdminListMCPServers(w http.ResponseWriter, r *http.Request) {
	tid := traceID()
	servers, err := s.mcpServers.ListMCPServers(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "MCP_SERVER_LIST_FAILED", err.Error(), tid)
		return
	}
	out := make([]map[string]any, 0, len(servers))
	for _, server := range servers {
		out = append(out, mcpServerReply(server))
	}
	writeJSON(w, http.StatusOK, map[string]any{"servers": out})
}

func (s *Server) handleAdminUpdateMCPServer(w http.ResponseWriter, r *http.Request) {
	tid := traceID()
	id, ok := parseMCPServerID(w, r, tid)
	if !ok {
		return
	}
	var body mcpServerWriteBody
	if err := decodeStrict(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "INVALID_JSON", err.Error(), tid)
		return
	}
	if err := body.normalize(); err != nil {
		writeErr(w, http.StatusBadRequest, "VALIDATION_ERROR", err.Error(), tid)
		return
	}
	// 掩码合并(JSON 编辑契约): 提交值与当前存储值的掩码一致时保留原 key
	// (secret 惯例: 留空/掩码 = 不变, 明文 = 更新)。
	if len(body.Headers) > 0 {
		merged, mergeErr := s.mergeMaskedHeaders(r.Context(), id, body.Headers)
		if mergeErr != nil {
			writeErr(w, http.StatusInternalServerError, "MCP_SERVER_UPDATE_FAILED", mergeErr.Error(), tid)
			return
		}
		body.Headers = merged
	}
	server, err := s.mcpServers.UpdateMCPServer(r.Context(), id, domain.MCPServerUpdate{
		MCPServerCreate: domain.MCPServerCreate{
			ServerKey: body.ServerKey, Name: body.Name, URL: body.URL,
			TimeoutSeconds: body.TimeoutSeconds, Headers: body.Headers,
			Transport: body.Transport,
			Command: body.Command, Args: body.Args, Isolation: body.Isolation,
			MaxInstances: body.MaxInstances,
		},
	})
	if err != nil {
		writeMCPStoreError(w, err, "MCP_SERVER_UPDATE_FAILED", tid)
		return
	}
	writeJSON(w, http.StatusOK, mcpServerReply(server))
}

// mergeMaskedHeaders 合并提交的 headers 与当前存储值: 提交值等于当前值
// 的掩码(maskSecretValue)时保留当前明文, 否则采用提交值(新键/明文更新)。
func (s *Server) mergeMaskedHeaders(ctx context.Context, id int64, submitted map[string]string) (map[string]string, error) {
	servers, err := s.mcpServers.ListMCPServers(ctx)
	if err != nil {
		return nil, err
	}
	var current domain.MCPServer
	for _, srv := range servers {
		if srv.ID == id {
			current = srv
			break
		}
	}
	merged := make(map[string]string, len(submitted))
	for key, value := range submitted {
		if strings.HasSuffix(value, "***") && maskSecretValue(current.Headers[key]) == value {
			merged[key] = current.Headers[key]
		} else {
			merged[key] = value
		}
	}
	return merged, nil
}

func (s *Server) handleAdminDeleteMCPServer(w http.ResponseWriter, r *http.Request) {
	tid := traceID()
	id, ok := parseMCPServerID(w, r, tid)
	if !ok {
		return
	}
	if err := s.mcpServers.DeleteMCPServer(r.Context(), id); err != nil {
		writeMCPStoreError(w, err, "MCP_SERVER_DELETE_FAILED", tid)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleAdminEnableMCPServer(w http.ResponseWriter, r *http.Request) {
	s.handleAdminSetMCPServerEnabled(w, r, true)
}

func (s *Server) handleAdminDisableMCPServer(w http.ResponseWriter, r *http.Request) {
	s.handleAdminSetMCPServerEnabled(w, r, false)
}

func (s *Server) handleAdminSetMCPServerEnabled(w http.ResponseWriter, r *http.Request, enabled bool) {
	tid := traceID()
	id, ok := parseMCPServerID(w, r, tid)
	if !ok {
		return
	}
	server, err := s.mcpServers.SetMCPServerEnabled(r.Context(), id, enabled)
	if err != nil {
		writeMCPStoreError(w, err, "MCP_SERVER_STATE_FAILED", tid)
		return
	}
	writeJSON(w, http.StatusOK, mcpServerReply(server))
}

func writeMCPStoreError(w http.ResponseWriter, err error, code, tid string) {
	statusCode := http.StatusInternalServerError
	switch {
	case errors.Is(err, domain.ErrMCPServerNotFound):
		statusCode = http.StatusNotFound
	case errors.Is(err, domain.ErrMCPServerConflict):
		statusCode = http.StatusConflict
	}
	writeErr(w, statusCode, code, err.Error(), tid)
}

func parseMCPServerID(w http.ResponseWriter, r *http.Request, tid string) (int64, bool) {
	id, err := strconv.ParseInt(strings.TrimSpace(r.PathValue("mcp_server_id")), 10, 64)
	if err != nil || id <= 0 {
		writeErr(w, http.StatusBadRequest, "INVALID_MCP_SERVER_ID", "mcp_server_id must be positive", tid)
		return 0, false
	}
	return id, true
}

func mcpServerReply(server domain.MCPServer) map[string]any {
	out := map[string]any{
		"mcp_server_id":   server.ID,
		"server_key":      server.ServerKey,
		"name":            server.Name,
		"url":             server.URL,
		"timeout_seconds": server.TimeoutSeconds,
		"headers":         maskMCPHeaders(server.Headers),
		"transport":       server.Transport,
		"command":         server.Command,
		"args":            server.Args,
		"isolation":       server.Isolation,
		"max_instances":   server.MaxInstances,
		"enabled":         server.Enabled,
		"revision":        server.Revision,
		"created_at":      server.CreatedAt.UTC().Format(time.RFC3339),
		"updated_at":      server.UpdatedAt.UTC().Format(time.RFC3339),
	}
	if out["transport"] == "" {
		out["transport"] = domain.MCPTransportHTTP
	}
	if out["isolation"] == "" {
		out["isolation"] = domain.MCPIsolationShared
	}
	if out["max_instances"] == 0 {
		out["max_instances"] = domain.DefaultMCPMaxInstances
	}
	return out
}

// maskMCPHeaders 掩码 headers 值: 回显只保留值前缀 + ***, 明文 key 只写不读
// (secret 惯例: web/API 响应永不回显明文; 编辑时留空=不变、填写=更新)。
func maskMCPHeaders(headers map[string]string) map[string]string {
	if len(headers) == 0 {
		return nil
	}
	masked := make(map[string]string, len(headers))
	for k, v := range headers {
		masked[k] = maskSecretValue(v)
	}
	return masked
}

// maskSecretValue 保留值前 4 字符(展示可辨), 其余替换为 ***。
func maskSecretValue(value string) string {
	const maxVisible = 4
	if len(value) <= maxVisible {
		return "***"
	}
	return value[:maxVisible] + "***"
}

// --- 配额管理 API ---

type mcpQuotaWriteBody struct {
	OwnerKey   string `json:"owner_key"`
	ServerID   string `json:"server_id"`
	Period     string `json:"period"`
	LimitCount int64  `json:"limit_count"`
}

func (b *mcpQuotaWriteBody) normalize() (domain.MCPQuotaLimit, error) {
	limit := domain.MCPQuotaLimit{
		OwnerKey: strings.TrimSpace(b.OwnerKey),
		ServerID: strings.TrimSpace(b.ServerID),
		Period:   domain.MCPQuotaPeriod(strings.TrimSpace(b.Period)),
		LimitCount: b.LimitCount,
	}
	if err := limit.Validate(); err != nil {
		return domain.MCPQuotaLimit{}, err
	}
	return limit, nil
}

// handleAdminListMCPQuotas GET /v1/admin/mcp-quotas?owner_key=X
// owner_key 缺失时列出全部(管理员视图: 用户列表页逐用户配置)。
func (s *Server) handleAdminListMCPQuotas(w http.ResponseWriter, r *http.Request) {
	tid := traceID()
	ownerKey := strings.TrimSpace(r.URL.Query().Get("owner_key"))
	limits, err := s.mcpServers.ListMCPQuotaLimits(r.Context(), ownerKey)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "MCP_QUOTA_LIST_FAILED", err.Error(), tid)
		return
	}
	out := make([]map[string]any, 0, len(limits))
	for _, l := range limits {
		out = append(out, map[string]any{
			"owner_key":   l.OwnerKey,
			"server_id":   l.ServerID,
			"period":      l.Period,
			"limit_count": l.LimitCount,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"quotas": out})
}

// handleAdminUpsertMCPQuota PUT /v1/admin/mcp-quotas (upsert 限额)。
func (s *Server) handleAdminUpsertMCPQuota(w http.ResponseWriter, r *http.Request) {
	tid := traceID()
	var body mcpQuotaWriteBody
	if err := decodeStrict(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "INVALID_JSON", err.Error(), tid)
		return
	}
	limit, err := body.normalize()
	if err != nil {
		writeErr(w, http.StatusBadRequest, "VALIDATION_ERROR", err.Error(), tid)
		return
	}
	if err := s.mcpServers.SetMCPQuotaLimit(r.Context(), limit.OwnerKey, limit.ServerID, string(limit.Period), limit.LimitCount); err != nil {
		writeErr(w, http.StatusInternalServerError, "MCP_QUOTA_UPSERT_FAILED", err.Error(), tid)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"owner_key":   limit.OwnerKey,
		"server_id":   limit.ServerID,
		"period":      limit.Period,
		"limit_count": limit.LimitCount,
	})
}

// handleAdminDeleteMCPQuota DELETE /v1/admin/mcp-quotas?owner_key=&server_id=&period=
func (s *Server) handleAdminDeleteMCPQuota(w http.ResponseWriter, r *http.Request) {
	tid := traceID()
	q := r.URL.Query()
	ownerKey := strings.TrimSpace(q.Get("owner_key"))
	serverID := strings.TrimSpace(q.Get("server_id"))
	period := strings.TrimSpace(q.Get("period"))
	if ownerKey == "" || serverID == "" || period == "" {
		writeErr(w, http.StatusBadRequest, "INVALID_QUOTA_PARAMS", "owner_key, server_id and period are required", tid)
		return
	}
	limit := domain.MCPQuotaLimit{OwnerKey: ownerKey, ServerID: serverID, Period: domain.MCPQuotaPeriod(period), LimitCount: 1}
	if err := limit.Validate(); err != nil {
		writeErr(w, http.StatusBadRequest, "VALIDATION_ERROR", err.Error(), tid)
		return
	}
	if err := s.mcpServers.DeleteMCPQuotaLimit(r.Context(), ownerKey, serverID, period); err != nil {
		writeErr(w, http.StatusInternalServerError, "MCP_QUOTA_DELETE_FAILED", err.Error(), tid)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
