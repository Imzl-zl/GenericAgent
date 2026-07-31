package api

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
)

type sophubBindingBody struct {
	APIKey string `json:"api_key"`
}

type sophubImportBody struct {
	RemoteSOPID string `json:"remote_sop_id"`
}

type sopRejectBody struct {
	Note string `json:"note"`
}

func (s *Server) handleAdminGetSophubBinding(w http.ResponseWriter, r *http.Request) {
	status, err := s.sophub.GetBindingStatus(r.Context())
	if errors.Is(err, domain.ErrSophubNotConfigured) {
		writeJSON(w, http.StatusOK, sophubBindingStatusReply(domain.SophubBindingStatus{}))
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "SOPHUB_BINDING_FAILED", "Sophub binding lookup failed", traceID())
		return
	}
	writeJSON(w, http.StatusOK, sophubBindingStatusReply(status))
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
	writeJSON(w, http.StatusOK, sophubBindingStatusReply(status))
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

func (s *Server) handleAdminImportSOPCandidate(w http.ResponseWriter, r *http.Request) {
	tid := traceID()
	var body sophubImportBody
	if err := decodeStrict(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "INVALID_JSON", err.Error(), tid)
		return
	}
	candidate, err := s.sophub.ImportCandidate(r.Context(), strings.TrimSpace(body.RemoteSOPID))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "SOP_IMPORT_FAILED", err.Error(), tid)
		return
	}
	writeJSON(w, http.StatusCreated, sopCandidateReply(candidate))
}

func (s *Server) handleAdminListSOPCandidates(w http.ResponseWriter, r *http.Request) {
	tid := traceID()
	status := domain.SOPCandidateStatus(strings.TrimSpace(r.URL.Query().Get("status")))
	candidates, err := s.sophub.ListCandidates(r.Context(), status)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "SOP_CANDIDATE_LIST_FAILED", err.Error(), tid)
		return
	}
	out := make([]map[string]any, 0, len(candidates))
	for _, candidate := range candidates {
		out = append(out, sopCandidateReply(candidate))
	}
	writeJSON(w, http.StatusOK, map[string]any{"candidates": out})
}

func (s *Server) handleAdminApproveSOPCandidate(w http.ResponseWriter, r *http.Request) {
	tid := traceID()
	version, err := s.sophub.ApproveCandidate(r.Context(), r.PathValue("candidate_id"), s.devUserID)
	if err != nil {
		writeErr(w, http.StatusConflict, "SOP_APPROVE_FAILED", err.Error(), tid)
		return
	}
	writeJSON(w, http.StatusCreated, sopVersionReply(version))
}

func (s *Server) handleAdminRejectSOPCandidate(w http.ResponseWriter, r *http.Request) {
	tid := traceID()
	var body sopRejectBody
	if err := decodeStrict(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "INVALID_JSON", err.Error(), tid)
		return
	}
	if err := s.sophub.RejectCandidate(r.Context(), r.PathValue("candidate_id"), s.devUserID, body.Note); err != nil {
		writeErr(w, http.StatusConflict, "SOP_REJECT_FAILED", err.Error(), tid)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"rejected": true})
}

func (s *Server) handleAdminListSOPRegistry(w http.ResponseWriter, r *http.Request) {
	tid := traceID()
	items, err := s.sophub.ListRegistry(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "SOP_REGISTRY_LIST_FAILED", "SOP registry list failed", tid)
		return
	}
	out := make([]map[string]any, 0, len(items))
	for _, item := range items {
		reply := sopVersionReply(item.Version)
		reply["loaded"] = item.Loaded
		out = append(out, reply)
	}
	writeJSON(w, http.StatusOK, map[string]any{"sops": out})
}

func (s *Server) handleAdminLoadSOPVersion(w http.ResponseWriter, r *http.Request) {
	tid := traceID()
	entry, err := s.sophub.LoadVersion(r.Context(), r.PathValue("version_id"), s.devUserID)
	if err != nil {
		writeErr(w, http.StatusConflict, "SOP_LOAD_FAILED", err.Error(), tid)
		return
	}
	writeJSON(w, http.StatusOK, sopEntryReply(entry))
}

func (s *Server) handleAdminUnloadSOP(w http.ResponseWriter, r *http.Request) {
	tid := traceID()
	entry, err := s.sophub.Unload(r.Context(), r.PathValue("entry_id"), s.devUserID)
	if err != nil {
		writeErr(w, http.StatusConflict, "SOP_UNLOAD_FAILED", err.Error(), tid)
		return
	}
	writeJSON(w, http.StatusOK, sopEntryReply(entry))
}

func (s *Server) handleListLoadedSOPs(w http.ResponseWriter, r *http.Request) {
	tid := traceID()
	if _, ok := userIDFromContext(r.Context()); !ok {
		writeErr(w, http.StatusUnauthorized, "UNAUTHORIZED", "missing user context", tid)
		return
	}
	versions, err := s.sophub.ListLoaded(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "SOP_LIST_FAILED", "loaded SOP list failed", tid)
		return
	}
	out := make([]map[string]any, 0, len(versions))
	for _, version := range versions {
		out = append(out, loadedSOPReply(version))
	}
	writeJSON(w, http.StatusOK, map[string]any{"sops": out})
}

func sophubBindingStatusReply(status domain.SophubBindingStatus) map[string]any {
	reply := map[string]any{
		"configured":   status.Configured,
		"author_type":  status.AuthorType,
		"agent_uid":    status.AgentUID,
		"display_name": status.DisplayName,
	}
	if status.VerifiedAt != nil {
		reply["verified_at"] = status.VerifiedAt.UTC().Format(time.RFC3339)
	}
	if !status.UpdatedAt.IsZero() {
		reply["updated_at"] = status.UpdatedAt.UTC().Format(time.RFC3339)
	}
	return reply
}

func sopCandidateReply(candidate domain.SOPCandidate) map[string]any {
	reply := map[string]any{
		"candidate_id":  candidate.ID,
		"remote_sop_id": candidate.RemoteSOPID,
		"title":         candidate.Title,
		"description":   candidate.Description,
		"file_type":     candidate.FileType,
		"content":       candidate.Content,
		"source_digest": candidate.SourceDigest,
		"status":        candidate.Status,
		"review_note":   candidate.ReviewNote,
	}
	if candidate.ReviewedAt != nil {
		reply["reviewed_at"] = candidate.ReviewedAt.UTC().Format(time.RFC3339)
	}
	return reply
}

func sopVersionReply(version domain.SOPVersion) map[string]any {
	return map[string]any{
		"version_id":   version.ID,
		"entry_id":     version.EntryID,
		"candidate_id": version.CandidateID,
		"version":      version.Version,
		"title":        version.Title,
		"description":  version.Description,
		"content":      version.Content,
		"digest":       version.ContentDigest,
		"approved_at":  version.ApprovedAt.UTC().Format(time.RFC3339),
	}
}

func loadedSOPReply(version domain.SOPVersion) map[string]any {
	return map[string]any{
		"title":       version.Title,
		"description": version.Description,
		"content":     version.Content,
		"digest":      version.ContentDigest,
		"version":     version.Version,
	}
}

func sopEntryReply(entry domain.SOPEntry) map[string]any {
	reply := map[string]any{
		"entry_id":          entry.ID,
		"remote_sop_id":     entry.RemoteSOPID,
		"loaded_version_id": entry.LoadedVersionID,
	}
	if entry.LoadedAt != nil {
		reply["loaded_at"] = entry.LoadedAt.UTC().Format(time.RFC3339)
	}
	return reply
}

func parsePositiveQueryInt(w http.ResponseWriter, r *http.Request, name string, fallback, max int, tid string) (int, bool) {
	raw := strings.TrimSpace(r.URL.Query().Get(name))
	if raw == "" {
		return fallback, true
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value <= 0 || value > max {
		writeErr(w, http.StatusBadRequest, "VALIDATION_ERROR", name+" is invalid", tid)
		return 0, false
	}
	return value, true
}
