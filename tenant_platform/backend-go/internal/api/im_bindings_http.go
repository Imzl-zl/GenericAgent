package api

import (
	"errors"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
)

// saveChannelBindingBody is the PUT /v1/me/im-bindings/{channel_type} body.
// app_id/app_secret 加密后存入 channel_configs.config_ciphertext(JSON),
// API 响应只回脱敏值。
type saveChannelBindingBody struct {
	AppID     string `json:"app_id"`
	AppSecret string `json:"app_secret"`
}

// handleSaveChannelBinding saves/updates credentials for a credential-
// configurable channel (feishu|dingtalk|qq). 保存即生效: 落库后触发 poller
// 热重载, 无独立连通性测试按钮(IM_CHANNEL_BINDING §4/§10 已决)。
func (s *Server) handleSaveChannelBinding(w http.ResponseWriter, r *http.Request) {
	tid := traceID()
	userID, ok := userIDFromContext(r.Context())
	if !ok {
		writeErr(w, http.StatusUnauthorized, "UNAUTHORIZED", "missing user context", tid)
		return
	}
	s.saveBinding(w, r, userID, tid)
}

// handleAdminSaveChannelBinding is the admin-auth version (developer user id).
func (s *Server) handleAdminSaveChannelBinding(w http.ResponseWriter, r *http.Request) {
	tid := traceID()
	s.saveBinding(w, r, s.adminUserID, tid)
}

func (s *Server) saveBinding(w http.ResponseWriter, r *http.Request, userID int64, tid string) {
	channelType := domain.ChannelType(strings.TrimSpace(r.PathValue("channel_type")))
	if !domain.IsValidChannelType(string(channelType)) || channelType == domain.ChannelWechat {
		writeErr(w, http.StatusBadRequest, "INVALID_CHANNEL_TYPE",
			"channel_type must be one of feishu|dingtalk|qq", tid)
		return
	}
	var body saveChannelBindingBody
	if err := decodeStrict(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "INVALID_JSON", err.Error(), tid)
		return
	}
	cfg, err := s.channelSvc.UpsertCredentials(r.Context(), userID, channelType, body.AppID, body.AppSecret)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeErr(w, http.StatusNotFound, "NOT_FOUND", err.Error(), tid)
			return
		}
		writeErr(w, http.StatusBadRequest, "BINDING_SAVE_FAILED", err.Error(), tid)
		return
	}
	writeJSON(w, http.StatusOK, s.bindingReply(cfg))
}

// handleUnbindChannel disables a credential-configurable channel binding and
// notifies the poller to disconnect (IM_CHANNEL_BINDING §4).
func (s *Server) handleUnbindChannel(w http.ResponseWriter, r *http.Request) {
	tid := traceID()
	userID, ok := userIDFromContext(r.Context())
	if !ok {
		writeErr(w, http.StatusUnauthorized, "UNAUTHORIZED", "missing user context", tid)
		return
	}
	s.unbindChannel(w, r, userID, tid)
}

// handleAdminUnbindChannel is the admin-auth version (developer user id).
func (s *Server) handleAdminUnbindChannel(w http.ResponseWriter, r *http.Request) {
	tid := traceID()
	s.unbindChannel(w, r, s.adminUserID, tid)
}

func (s *Server) unbindChannel(w http.ResponseWriter, r *http.Request, userID int64, tid string) {
	channelType := domain.ChannelType(strings.TrimSpace(r.PathValue("channel_type")))
	if !domain.IsValidChannelType(string(channelType)) || channelType == domain.ChannelWechat {
		writeErr(w, http.StatusBadRequest, "INVALID_CHANNEL_TYPE",
			"channel_type must be one of feishu|dingtalk|qq", tid)
		return
	}
	cfg, err := s.channelSvc.Unbind(r.Context(), userID, channelType)
	if err != nil {
		if errors.Is(err, domain.ErrChannelBindingNotFound) || errors.Is(err, pgx.ErrNoRows) {
			writeErr(w, http.StatusNotFound, "NOT_FOUND", "channel binding not found", tid)
			return
		}
		writeErr(w, http.StatusInternalServerError, "UNBIND_FAILED", err.Error(), tid)
		return
	}
	writeJSON(w, http.StatusOK, s.bindingReply(cfg))
}
