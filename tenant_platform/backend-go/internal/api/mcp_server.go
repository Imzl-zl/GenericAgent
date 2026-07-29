package api

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
)

type mcpServerWriteBody struct {
	ServerKey      string `json:"server_key"`
	Name           string `json:"name"`
	URL            string `json:"url"`
	TimeoutSeconds int    `json:"timeout_seconds"`
}

func (b *mcpServerWriteBody) normalize() error {
	b.ServerKey = strings.TrimSpace(b.ServerKey)
	b.Name = strings.TrimSpace(b.Name)
	b.URL = strings.TrimSpace(b.URL)
	return domain.ValidateMCPServerInput(domain.MCPServerCreate{
		ServerKey: b.ServerKey, Name: b.Name, URL: b.URL,
		TimeoutSeconds: b.TimeoutSeconds,
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
	server, err := s.mcpServers.CreateMCPServer(r.Context(), domain.MCPServerCreate{
		ServerKey: body.ServerKey, Name: body.Name, URL: body.URL,
		TimeoutSeconds: body.TimeoutSeconds,
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
	server, err := s.mcpServers.UpdateMCPServer(r.Context(), id, domain.MCPServerUpdate{
		MCPServerCreate: domain.MCPServerCreate{
			ServerKey: body.ServerKey, Name: body.Name, URL: body.URL,
			TimeoutSeconds: body.TimeoutSeconds,
		},
	})
	if err != nil {
		writeMCPStoreError(w, err, "MCP_SERVER_UPDATE_FAILED", tid)
		return
	}
	writeJSON(w, http.StatusOK, mcpServerReply(server))
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
	return map[string]any{
		"mcp_server_id":   server.ID,
		"server_key":      server.ServerKey,
		"name":            server.Name,
		"url":             server.URL,
		"timeout_seconds": server.TimeoutSeconds,
		"enabled":         server.Enabled,
		"revision":        server.Revision,
		"created_at":      server.CreatedAt.UTC().Format(time.RFC3339),
		"updated_at":      server.UpdatedAt.UTC().Format(time.RFC3339),
	}
}
