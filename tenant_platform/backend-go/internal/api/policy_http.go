// Package api — admin-configurable command registry + tool policy HTTP handlers
// (migration 0004). These endpoints let admins manage commands and tool policies
// at runtime without recompiling the platform.
package api

import (
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
)

type commandReply struct {
	ID        int64                  `json:"id"`
	Command   string                 `json:"command"`
	Action    domain.CommandAction   `json:"action"`
	Handler   string                 `json:"handler"`
	HelpText  string                 `json:"help_text"`
	Enabled   bool                   `json:"enabled"`
	SortOrder int                    `json:"sort_order"`
	UpdatedAt string                 `json:"updated_at"`
}

type updateCommandBody struct {
	Action    domain.CommandAction `json:"action"`
	HelpText  string               `json:"help_text"`
	Enabled   bool                 `json:"enabled"`
	SortOrder int                  `json:"sort_order"`
}

func (s *Server) handleListCommands(w http.ResponseWriter, r *http.Request) {
	tid := traceID()
	cmds, err := s.policies.ListAllCommands(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "COMMAND_LIST_FAILED", err.Error(), tid)
		return
	}
	out := make([]commandReply, 0, len(cmds))
	for _, c := range cmds {
		out = append(out, toCommandReply(c))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleUpdateCommand(w http.ResponseWriter, r *http.Request) {
	tid := traceID()
	idStr := r.PathValue("command_id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil || id <= 0 {
		writeErr(w, http.StatusBadRequest, "INVALID_COMMAND_ID", "command_id must be a positive integer", tid)
		return
	}
	var body updateCommandBody
	if err := decodeStrict(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "INVALID_JSON", err.Error(), tid)
		return
	}
	if body.Action != domain.CommandIntercept && body.Action != domain.CommandPassthrough {
		writeErr(w, http.StatusBadRequest, "INVALID_ACTION", "action must be 'intercept' or 'passthrough'", tid)
		return
	}
	cmd, err := s.policies.UpdateCommand(r.Context(), id, body.Action, body.HelpText,
		body.Enabled, body.SortOrder, s.devUserID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "COMMAND_UPDATE_FAILED", err.Error(), tid)
		return
	}
	// Trigger cache invalidation so the new command config takes effect
	// immediately for the current process.
	if s.router != nil {
		s.router.InvalidateCommandCache()
	}
	writeJSON(w, http.StatusOK, toCommandReply(cmd))
}

func toCommandReply(c domain.PlatformCommand) commandReply {
	return commandReply{
		ID:        c.ID,
		Command:   c.Command,
		Action:    c.Action,
		Handler:   c.Handler,
		HelpText:  c.HelpText,
		Enabled:   c.Enabled,
		SortOrder: c.SortOrder,
		UpdatedAt: c.UpdatedAt.UTC().Format("2006-01-02T15:04:05Z"),
	}
}

type toolPolicyReply struct {
	ID           int64    `json:"id"`
	Version      string   `json:"version"`
	AllowedTools []string `json:"allowed_tools"`
	Description  string   `json:"description"`
	Enabled      bool     `json:"enabled"`
	CreatedAt    string   `json:"created_at"`
	UpdatedAt    string   `json:"updated_at"`
}

type createToolPolicyBody struct {
	Version      string   `json:"version"`
	AllowedTools []string `json:"allowed_tools"`
	Description  string   `json:"description"`
}

func (s *Server) handleListToolPolicies(w http.ResponseWriter, r *http.Request) {
	tid := traceID()
	policies, err := s.policies.ListToolPolicies(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "POLICY_LIST_FAILED", err.Error(), tid)
		return
	}
	out := make([]toolPolicyReply, 0, len(policies))
	for _, p := range policies {
		out = append(out, toToolPolicyReply(p))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleCreateToolPolicy(w http.ResponseWriter, r *http.Request) {
	tid := traceID()
	var body createToolPolicyBody
	if err := decodeStrict(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "INVALID_JSON", err.Error(), tid)
		return
	}
	if body.Version == "" {
		writeErr(w, http.StatusBadRequest, "INVALID_VERSION", "version is required", tid)
		return
	}
	if len(body.AllowedTools) == 0 {
		writeErr(w, http.StatusBadRequest, "INVALID_TOOLS", "allowed_tools must not be empty", tid)
		return
	}
	p, err := s.policies.CreateToolPolicy(r.Context(), body.Version, body.Description,
		body.AllowedTools, s.devUserID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "POLICY_CREATE_FAILED", err.Error(), tid)
		return
	}
	writeJSON(w, http.StatusCreated, toToolPolicyReply(p))
}

func toToolPolicyReply(p domain.ToolPolicy) toolPolicyReply {
	tools := p.AllowedTools
	if tools == nil {
		tools = []string{}
	}
	return toolPolicyReply{
		ID:           p.ID,
		Version:      p.Version,
		AllowedTools: tools,
		Description:  p.Description,
		Enabled:      p.Enabled,
		CreatedAt:    p.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"),
		UpdatedAt:    p.UpdatedAt.UTC().Format("2006-01-02T15:04:05Z"),
	}
}

type updateUserToolPolicyBody struct {
	Version string `json:"version"`
}

func (s *Server) handleUpdateUserToolPolicy(w http.ResponseWriter, r *http.Request) {
	tid := traceID()
	idStr := r.PathValue("user_id")
	userID, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil || userID <= 0 {
		writeErr(w, http.StatusBadRequest, "INVALID_USER_ID", "user_id must be a positive integer", tid)
		return
	}
	var body updateUserToolPolicyBody
	if err := decodeStrict(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "INVALID_JSON", err.Error(), tid)
		return
	}
	if body.Version == "" {
		writeErr(w, http.StatusBadRequest, "INVALID_VERSION", "version is required", tid)
		return
	}
	if err := s.policies.UpdateUserToolPolicy(r.Context(), userID, body.Version, s.devUserID); err != nil {
		writeErr(w, http.StatusInternalServerError, "USER_POLICY_UPDATE_FAILED", err.Error(), tid)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"user_id":            userID,
		"tool_policy_version": body.Version,
	})
}

var _ = json.Marshal // keep import if future handlers need direct marshaling
