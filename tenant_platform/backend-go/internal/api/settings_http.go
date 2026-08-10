package api

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
)

type imAggregationSettingsReply struct {
	WindowMS int `json:"window_ms"`
}

type updateIMAggregationSettingsBody struct {
	WindowMS int `json:"window_ms"`
}

type agentRuntimeSettingsReply struct {
	MaxTurns int `json:"max_turns"`
}

type updateAgentRuntimeSettingsBody struct {
	MaxTurns int `json:"max_turns"`
}

func (s *Server) handleGetIMAggregationSettings(w http.ResponseWriter, r *http.Request) {
	tid := traceID()
	windowMS, err := s.runtimeSettings.GetIMInboundCoalesceWindowMS(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "GET_IM_AGGREGATION_FAILED", err.Error(), tid)
		return
	}
	writeJSON(w, http.StatusOK, imAggregationSettingsReply{WindowMS: windowMS})
}

// applyInboundCoalescingWithRetry 推送 IM 聚合设置到 Poller; 失败时后台
// 有界重试(审查 Minor: 消除 DB 已提交但 Poller 未应用的不一致窗口)。
// DB 是事实源: 每次重试前对账 DB 当前值, 已被更新则放弃旧值重试,
// 防止后台旧值覆盖用户新设置。返回是否立即成功。
func (s *Server) applyInboundCoalescingWithRetry(windowMS int) bool {
	if s.imAggregationRuntime == nil || s.runtimeSettings == nil {
		return true
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.imAggregationRuntime.ConfigureInboundCoalescing(ctx, windowMS); err == nil {
		return true
	}
	// 推送失败: 后台指数退避重试(1s/2s/4s), 期间 DB 值已持久化,
	// 客户端已收到 502; 重试收敛后无需客户端再操作。
	go func(targetMS int) {
		for attempt, delay := range []time.Duration{time.Second, 2 * time.Second, 4 * time.Second} {
			time.Sleep(delay)
			cur, err := s.runtimeSettings.GetIMInboundCoalesceWindowMS(context.Background())
			if err != nil || cur != targetMS {
				// DB 已推进到新值(用户再次更新), 放弃旧值重试。
				return
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			err = s.imAggregationRuntime.ConfigureInboundCoalescing(ctx, cur)
			cancel()
			if err == nil {
				slog.Info("api: inbound coalescing applied after retry", "window_ms", cur, "attempt", attempt+1)
				return
			}
		}
		slog.Error("api: inbound coalescing retries exhausted; poller out of sync with db", "window_ms", targetMS)
	}(windowMS)
	return false
}

func (s *Server) handleUpdateIMAggregationSettings(w http.ResponseWriter, r *http.Request) {
	tid := traceID()
	var body updateIMAggregationSettingsBody
	if err := decodeStrict(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "INVALID_JSON", err.Error(), tid)
		return
	}
	if err := domain.ValidateIMInboundCoalesceWindowMS(body.WindowMS); err != nil {
		writeErr(w, http.StatusBadRequest, "INVALID_WINDOW_MS", err.Error(), tid)
		return
	}
	windowMS, err := s.runtimeSettings.UpdateIMInboundCoalesceWindowMS(r.Context(), body.WindowMS, s.adminUserID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "UPDATE_IM_AGGREGATION_FAILED", err.Error(), tid)
		return
	}
	// DB 已提交; Poller 应用失败时后台有界重试收敛, 502 如实告知客户端。
	if !s.applyInboundCoalescingWithRetry(windowMS) {
		writeErr(w, http.StatusBadGateway, "APPLY_IM_AGGREGATION_FAILED",
			"settings saved; poller apply failed and will retry in background", tid)
		return
	}
	s.updateRuntimeProfile(func(profile *RuntimeProfile) {
		profile.IMInboundCoalesceWindowMS = windowMS
	})
	writeJSON(w, http.StatusOK, imAggregationSettingsReply{WindowMS: windowMS})
}

func (s *Server) handleGetAgentRuntimeSettings(w http.ResponseWriter, r *http.Request) {
	tid := traceID()
	maxTurns, err := s.runtimeSettings.GetAgentMaxTurns(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "GET_AGENT_RUNTIME_FAILED", err.Error(), tid)
		return
	}
	writeJSON(w, http.StatusOK, agentRuntimeSettingsReply{MaxTurns: maxTurns})
}

func (s *Server) handleUpdateAgentRuntimeSettings(w http.ResponseWriter, r *http.Request) {
	tid := traceID()
	var body updateAgentRuntimeSettingsBody
	if err := decodeStrict(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "INVALID_JSON", err.Error(), tid)
		return
	}
	if err := domain.ValidateAgentMaxTurns(body.MaxTurns); err != nil {
		writeErr(w, http.StatusBadRequest, "INVALID_MAX_TURNS", err.Error(), tid)
		return
	}
	maxTurns, err := s.runtimeSettings.UpdateAgentMaxTurns(r.Context(), body.MaxTurns, s.adminUserID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "UPDATE_AGENT_RUNTIME_FAILED", err.Error(), tid)
		return
	}
	s.updateRuntimeProfile(func(profile *RuntimeProfile) {
		profile.AgentMaxTurns = maxTurns
	})
	writeJSON(w, http.StatusOK, agentRuntimeSettingsReply{MaxTurns: maxTurns})
}

type imStreamingSettingsReply struct {
	Mode domain.IMStreamingMode `json:"mode"`
}

type updateIMStreamingSettingsBody struct {
	Mode domain.IMStreamingMode `json:"mode"`
}

func (s *Server) handleGetIMStreamingSettings(w http.ResponseWriter, r *http.Request) {
	tid := traceID()
	mode, err := s.runtimeSettings.GetIMStreamingMode(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "GET_IM_STREAMING_FAILED", err.Error(), tid)
		return
	}
	writeJSON(w, http.StatusOK, imStreamingSettingsReply{Mode: mode})
}

func (s *Server) handleUpdateIMStreamingSettings(w http.ResponseWriter, r *http.Request) {
	tid := traceID()
	var body updateIMStreamingSettingsBody
	if err := decodeStrict(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "INVALID_JSON", err.Error(), tid)
		return
	}
	if err := domain.ValidateIMStreamingMode(string(body.Mode)); err != nil {
		writeErr(w, http.StatusBadRequest, "INVALID_IM_STREAMING_MODE", err.Error(), tid)
		return
	}
	mode, err := s.runtimeSettings.UpdateIMStreamingMode(r.Context(), body.Mode, s.adminUserID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "UPDATE_IM_STREAMING_FAILED", err.Error(), tid)
		return
	}
	s.updateRuntimeProfile(func(profile *RuntimeProfile) {
		profile.IMStreamingMode = mode
	})
	writeJSON(w, http.StatusOK, imStreamingSettingsReply{Mode: mode})
}
