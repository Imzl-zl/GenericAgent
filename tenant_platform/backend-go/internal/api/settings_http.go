package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"

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

func (s *Server) handleGetDocumentPoolSettings(w http.ResponseWriter, r *http.Request) {
	tid := traceID()
	settings, err := s.runtimeSettings.GetDocumentPoolSettings(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "GET_DOCUMENT_POOL_SETTINGS_FAILED", err.Error(), tid)
		return
	}
	if err := domain.ValidateDocumentPoolSettings(settings, s.documentPoolDeploymentMaxActive); err != nil {
		writeErr(w, http.StatusInternalServerError, "INVALID_PERSISTED_DOCUMENT_POOL_SETTINGS", err.Error(), tid)
		return
	}
	if err := domain.ValidateDocumentPoolSettingsReason(settings.Reason); err != nil {
		writeErr(w, http.StatusInternalServerError, "INVALID_PERSISTED_DOCUMENT_POOL_SETTINGS", err.Error(), tid)
		return
	}
	writeJSON(w, http.StatusOK, documentPoolSettingsReply{
		DocumentPoolSettings: settings,
		DeploymentMaxActive:  s.documentPoolDeploymentMaxActive,
	})
}

func (s *Server) handleUpdateDocumentPoolSettings(w http.ResponseWriter, r *http.Request) {
	tid := traceID()
	var body updateDocumentPoolSettingsBody
	if err := decodeStrict(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "INVALID_JSON", err.Error(), tid)
		return
	}
	reason := strings.TrimSpace(body.Reason)
	if body.ExpectedVersion <= 0 {
		writeErr(w, http.StatusBadRequest, "INVALID_DOCUMENT_POOL_SETTINGS", "expected_version must be positive", tid)
		return
	}
	if err := domain.ValidateDocumentPoolSettingsReason(reason); err != nil {
		writeErr(w, http.StatusBadRequest, "INVALID_DOCUMENT_POOL_SETTINGS", err.Error(), tid)
		return
	}
	settings := domain.DocumentPoolSettings{
		Enabled: body.Enabled, MaxActive: body.MaxActive, MinReady: body.MinReady,
		JobIdleTTLSeconds: body.JobIdleTTLSeconds, ReadyIdleTTLSeconds: body.ReadyIdleTTLSeconds,
		GlobalQueueLimit: body.GlobalQueueLimit, PerTenantQueueLimit: body.PerTenantQueueLimit,
		PerTenantActiveLimit: body.PerTenantActiveLimit, JobTimeoutSeconds: body.JobTimeoutSeconds,
		CommandTimeoutSeconds: body.CommandTimeoutSeconds,
	}
	if err := domain.ValidateDocumentPoolSettings(settings, s.documentPoolDeploymentMaxActive); err != nil {
		writeErr(w, http.StatusBadRequest, "INVALID_DOCUMENT_POOL_SETTINGS", err.Error(), tid)
		return
	}
	stored, err := s.runtimeSettings.UpdateDocumentPoolSettings(r.Context(), settings, body.ExpectedVersion, s.devUserID, reason)
	if errors.Is(err, domain.ErrDocumentPoolSettingsConflict) {
		writeErr(w, http.StatusConflict, "DOCUMENT_POOL_SETTINGS_CONFLICT", err.Error(), tid)
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "UPDATE_DOCUMENT_POOL_SETTINGS_FAILED", err.Error(), tid)
		return
	}
	applyContext := context.WithoutCancel(r.Context())
	if err := s.documentPoolSettingsRuntime.ApplyDocumentPoolSettings(applyContext, stored); err != nil {
		slog.ErrorContext(applyContext, "document pool settings persisted but runtime apply failed; reconciliation will retry",
			"version", stored.Version, "error", err)
		writeJSON(w, http.StatusAccepted, documentPoolSettingsReply{
			DocumentPoolSettings: stored,
			DeploymentMaxActive:  s.documentPoolDeploymentMaxActive,
			ApplyStatus:          documentPoolApplyPendingRetry,
		})
		return
	}
	writeJSON(w, http.StatusOK, documentPoolSettingsReply{
		DocumentPoolSettings: stored,
		DeploymentMaxActive:  s.documentPoolDeploymentMaxActive,
		ApplyStatus:          documentPoolApplyApplied,
	})
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
