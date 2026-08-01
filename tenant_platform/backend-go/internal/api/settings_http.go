package api

import (
	"net/http"

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

const (
	documentPoolApplyApplied      = "applied"
	documentPoolApplyPendingRetry = "pending_retry"
)

type documentPoolSettingsReply struct {
	domain.DocumentPoolSettings
	DeploymentMaxActive int    `json:"deployment_max_active"`
	ApplyStatus         string `json:"apply_status,omitempty"`
}

type updateDocumentPoolSettingsBody struct {
	Enabled               bool   `json:"enabled"`
	MaxActive             int    `json:"max_active"`
	MinReady              int    `json:"min_ready"`
	JobIdleTTLSeconds     int    `json:"job_idle_ttl_seconds"`
	ReadyIdleTTLSeconds   int    `json:"ready_idle_ttl_seconds"`
	GlobalQueueLimit      int    `json:"global_queue_limit"`
	PerTenantQueueLimit   int    `json:"per_tenant_queue_limit"`
	PerTenantActiveLimit  int    `json:"per_tenant_active_limit"`
	JobTimeoutSeconds     int    `json:"job_timeout_seconds"`
	CommandTimeoutSeconds int    `json:"command_timeout_seconds"`
	ExpectedVersion       int64  `json:"expected_version"`
	Reason                string `json:"reason"`
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
	windowMS, err := s.runtimeSettings.UpdateIMInboundCoalesceWindowMS(r.Context(), body.WindowMS, s.devUserID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "UPDATE_IM_AGGREGATION_FAILED", err.Error(), tid)
		return
	}
	if s.imAggregationRuntime != nil {
		if err := s.imAggregationRuntime.ConfigureInboundCoalescing(r.Context(), windowMS); err != nil {
			writeErr(w, http.StatusBadGateway, "APPLY_IM_AGGREGATION_FAILED", err.Error(), tid)
			return
		}
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
	maxTurns, err := s.runtimeSettings.UpdateAgentMaxTurns(r.Context(), body.MaxTurns, s.devUserID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "UPDATE_AGENT_RUNTIME_FAILED", err.Error(), tid)
		return
	}
	s.updateRuntimeProfile(func(profile *RuntimeProfile) {
		profile.AgentMaxTurns = maxTurns
	})
	writeJSON(w, http.StatusOK, agentRuntimeSettingsReply{MaxTurns: maxTurns})
}
