// Package api user-owned channel binding handlers.
package api

import (
	"encoding/json"
	"net/http"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
)

// handleGetOwnBot returns the current user's wechat channel config (the
// historical /v1/users/me/bots contract; wechat is the only 1:1 channel).
func (s *Server) handleGetOwnBot(w http.ResponseWriter, r *http.Request) {
	tid := traceID()
	userID, ok := userIDFromContext(r.Context())
	if !ok {
		writeErr(w, http.StatusUnauthorized, "UNAUTHORIZED", "missing user context", tid)
		return
	}
	bot, err := s.channelSvc.GetChannelConfig(r.Context(), userID, domain.ChannelWechat)
	if err != nil {
		writeErr(w, http.StatusNotFound, "NOT_FOUND", err.Error(), tid)
		return
	}
	writeJSON(w, http.StatusOK, channelConfigReply(bot))
}

// handleGetOwnBindings lists every channel binding of the authenticated user
// (IM_CHANNEL_BINDING §4): [{channel_type, state, bound_at, meta}] where meta
// carries the masked app_id (wechat: ilink_bot_id + masked ilink user).
func (s *Server) handleGetOwnBindings(w http.ResponseWriter, r *http.Request) {
	tid := traceID()
	userID, ok := userIDFromContext(r.Context())
	if !ok {
		writeErr(w, http.StatusUnauthorized, "UNAUTHORIZED", "missing user context", tid)
		return
	}
	s.writeBindings(w, r, userID, tid)
}

// handleAdminGetOwnBindings is the admin-auth version (developer user id).
func (s *Server) handleAdminGetOwnBindings(w http.ResponseWriter, r *http.Request) {
	tid := traceID()
	s.writeBindings(w, r, s.adminUserID, tid)
}

func (s *Server) writeBindings(w http.ResponseWriter, r *http.Request, userID int64, tid string) {
	cfgs, err := s.channelSvc.ListBindings(r.Context(), userID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "LIST_BINDINGS_FAILED", err.Error(), tid)
		return
	}
	out := make([]map[string]any, 0, len(cfgs))
	for _, c := range cfgs {
		out = append(out, s.bindingReply(c))
	}
	writeJSON(w, http.StatusOK, out)
}

// bindingReply 序列化单个渠道绑定; meta 只回脱敏值, 永不回明文凭据。
func (s *Server) bindingReply(c domain.ChannelConfig) map[string]any {
	meta := map[string]any{}
	switch c.ChannelType {
	case domain.ChannelWechat:
		meta["ilink_bot_id"] = c.IlinkBotID
		if c.IlinkUserID != "" {
			meta["channel_account_id"] = maskAccountID(c.IlinkUserID)
		}
	default:
		if appID := s.maskedAppIDFromConfig(c.ConfigCiphertext, c.ConfigKeyVersion); appID != "" {
			meta["app_id"] = appID
		}
	}
	reply := map[string]any{
		"channel_type": c.ChannelType,
		"state":        string(c.State),
		"bound_at":     c.CreatedAt.UTC().Format("2006-01-02T15:04:05Z07:00"),
		"meta":         meta,
	}
	if c.UpdatedAt.After(c.CreatedAt) {
		reply["updated_at"] = c.UpdatedAt.UTC().Format("2006-01-02T15:04:05Z07:00")
	}
	return reply
}

// maskedAppIDFromConfig 解密渠道配置 JSON 并返回脱敏 app_id; 解密失败时
// 返回空串(状态展示不因密钥问题失败, 密文本身不泄露)。
func (s *Server) maskedAppIDFromConfig(ct []byte, version int) string {
	if s.cipher == nil || len(ct) == 0 {
		return ""
	}
	plain, err := s.cipher.Decrypt(ct, version)
	if err != nil {
		return ""
	}
	var cfg struct {
		AppID string `json:"app_id"`
	}
	if err := json.Unmarshal(plain, &cfg); err != nil || cfg.AppID == "" {
		return ""
	}
	return maskAccountID(cfg.AppID)
}

// maskAccountID 脱敏展示渠道账号: 保留前 3 后 3, 中间以 * 遮蔽。
func maskAccountID(id string) string {
	if len(id) <= 8 {
		return "****"
	}
	return id[:3] + "****" + id[len(id)-3:]
}
