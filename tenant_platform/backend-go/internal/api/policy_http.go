// Package api — admin-configurable platform command registry HTTP handlers
// (migration 0004).
//
// 审查 D1(去分级): 工具策略不再按用户动态分配, 工具能力统一由静态 policy
// manifest(foundation.v1.json) 决定; tool_policies 的 CRUD/用户分配端点与
// 存储层已移除, 本文件仅保留平台命令管理端点。
package api

import (
	"net/http"
	"strconv"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
)

type commandReply struct {
	ID        int64                `json:"id"`
	Command   string               `json:"command"`
	Action    domain.CommandAction `json:"action"`
	Handler   string               `json:"handler"`
	HelpText  string               `json:"help_text"`
	Enabled   bool                 `json:"enabled"`
	SortOrder int                  `json:"sort_order"`
	UpdatedAt string               `json:"updated_at"`
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
		body.Enabled, body.SortOrder, s.adminUserID)
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
