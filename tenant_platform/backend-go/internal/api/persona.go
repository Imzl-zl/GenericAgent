// Package api persona store handlers.
package api

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
)

type personaBody struct {
	Name         string `json:"name"`
	Description  string `json:"description"`
	SystemPrompt string `json:"system_prompt"`
	IsPublic     bool   `json:"is_public"`
}

func (s *Server) handleListPersonas(w http.ResponseWriter, r *http.Request) {
	tid := traceID()
	userID, ok := userIDFromContext(r.Context())
	if !ok {
		writeErr(w, http.StatusUnauthorized, "UNAUTHORIZED", "missing user context", tid)
		return
	}
	personas, err := s.personas.ListForUser(r.Context(), userID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "PERSONA_LIST_FAILED", err.Error(), tid)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"personas": personaListReply(personas)})
}

func (s *Server) handleCreatePersona(w http.ResponseWriter, r *http.Request) {
	tid := traceID()
	userID, ok := userIDFromContext(r.Context())
	if !ok {
		writeErr(w, http.StatusUnauthorized, "UNAUTHORIZED", "missing user context", tid)
		return
	}
	var body personaBody
	if err := decodeStrict(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "INVALID_JSON", err.Error(), tid)
		return
	}
	if err := validatePersonaBody(body); err != nil {
		writeErr(w, http.StatusBadRequest, "VALIDATION_ERROR", err.Error(), tid)
		return
	}
	p, err := s.personas.CreatePersona(r.Context(), userID, body.Name, body.Description, body.SystemPrompt, body.IsPublic)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "PERSONA_CREATE_FAILED", err.Error(), tid)
		return
	}
	writeJSON(w, http.StatusCreated, personaReply(p))
}

func (s *Server) handleGetPersona(w http.ResponseWriter, r *http.Request) {
	tid := traceID()
	userID, ok := userIDFromContext(r.Context())
	if !ok {
		writeErr(w, http.StatusUnauthorized, "UNAUTHORIZED", "missing user context", tid)
		return
	}
	id := r.PathValue("persona_id")
	if id == "" {
		writeErr(w, http.StatusBadRequest, "INVALID_ID", "persona_id is required", tid)
		return
	}
	p, err := s.personas.GetPersona(r.Context(), id, userID)
	if err != nil {
		writeErr(w, http.StatusNotFound, "NOT_FOUND", err.Error(), tid)
		return
	}
	writeJSON(w, http.StatusOK, personaReply(p))
}

func (s *Server) handleUpdatePersona(w http.ResponseWriter, r *http.Request) {
	tid := traceID()
	userID, ok := userIDFromContext(r.Context())
	if !ok {
		writeErr(w, http.StatusUnauthorized, "UNAUTHORIZED", "missing user context", tid)
		return
	}
	id := r.PathValue("persona_id")
	if id == "" {
		writeErr(w, http.StatusBadRequest, "INVALID_ID", "persona_id is required", tid)
		return
	}
	var body personaBody
	if err := decodeStrict(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "INVALID_JSON", err.Error(), tid)
		return
	}
	if err := validatePersonaBody(body); err != nil {
		writeErr(w, http.StatusBadRequest, "VALIDATION_ERROR", err.Error(), tid)
		return
	}
	p, err := s.personas.UpdatePersona(r.Context(), id, userID, body.Name, body.Description, body.SystemPrompt)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "PERSONA_UPDATE_FAILED", err.Error(), tid)
		return
	}
	writeJSON(w, http.StatusOK, personaReply(p))
}

func (s *Server) handleDeletePersona(w http.ResponseWriter, r *http.Request) {
	tid := traceID()
	userID, ok := userIDFromContext(r.Context())
	if !ok {
		writeErr(w, http.StatusUnauthorized, "UNAUTHORIZED", "missing user context", tid)
		return
	}
	id := r.PathValue("persona_id")
	if id == "" {
		writeErr(w, http.StatusBadRequest, "INVALID_ID", "persona_id is required", tid)
		return
	}
	if err := s.personas.DeletePersona(r.Context(), id, userID); err != nil {
		writeErr(w, http.StatusBadRequest, "PERSONA_DELETE_FAILED", err.Error(), tid)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": true})
}

func (s *Server) handleSubmitPersona(w http.ResponseWriter, r *http.Request) {
	tid := traceID()
	userID, ok := userIDFromContext(r.Context())
	if !ok {
		writeErr(w, http.StatusUnauthorized, "UNAUTHORIZED", "missing user context", tid)
		return
	}
	id := r.PathValue("persona_id")
	if id == "" {
		writeErr(w, http.StatusBadRequest, "INVALID_ID", "persona_id is required", tid)
		return
	}
	if err := s.personas.SubmitForReview(r.Context(), id, userID); err != nil {
		writeErr(w, http.StatusBadRequest, "PERSONA_SUBMIT_FAILED", err.Error(), tid)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"submitted": true})
}

func (s *Server) handleSetDefaultPersona(w http.ResponseWriter, r *http.Request) {
	tid := traceID()
	userID, ok := userIDFromContext(r.Context())
	if !ok {
		writeErr(w, http.StatusUnauthorized, "UNAUTHORIZED", "missing user context", tid)
		return
	}
	var body struct {
		PersonaID string `json:"persona_id"`
	}
	if err := decodeStrict(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "INVALID_JSON", err.Error(), tid)
		return
	}
	var err error
	if body.PersonaID == "" {
		err = s.personas.ClearDefault(r.Context(), userID)
	} else {
		err = s.personas.SetDefault(r.Context(), userID, body.PersonaID)
	}
	if err != nil {
		writeErr(w, http.StatusBadRequest, "DEFAULT_PERSONA_FAILED", err.Error(), tid)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"default_persona_id": body.PersonaID})
}

func (s *Server) handleAdminListPendingPersonas(w http.ResponseWriter, r *http.Request) {
	tid := traceID()
	personas, err := s.personas.ListPendingReview(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "PERSONA_LIST_FAILED", err.Error(), tid)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"personas": personaListReply(personas)})
}

type moderatePersonaBody struct {
	Note string `json:"note"`
}

func (s *Server) handleAdminApprovePersona(w http.ResponseWriter, r *http.Request) {
	s.handleModeratePersona(w, r, domain.PersonaApproved)
}

func (s *Server) handleAdminRejectPersona(w http.ResponseWriter, r *http.Request) {
	s.handleModeratePersona(w, r, domain.PersonaRejected)
}

func (s *Server) handleModeratePersona(w http.ResponseWriter, r *http.Request, status domain.PersonaStatus) {
	tid := traceID()
	id := r.PathValue("persona_id")
	if id == "" {
		writeErr(w, http.StatusBadRequest, "INVALID_ID", "persona_id is required", tid)
		return
	}
	var body moderatePersonaBody
	if err := decodeStrict(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "INVALID_JSON", err.Error(), tid)
		return
	}
	if err := s.personas.Moderate(r.Context(), id, s.devUserID, status, body.Note); err != nil {
		writeErr(w, http.StatusBadRequest, "PERSONA_MODERATE_FAILED", err.Error(), tid)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "status": string(status)})
}

func validatePersonaBody(b personaBody) error {
	if strings.TrimSpace(b.Name) == "" {
		return fmt.Errorf("name is required")
	}
	if strings.TrimSpace(b.SystemPrompt) == "" {
		return fmt.Errorf("system_prompt is required")
	}
	return nil
}

func personaReply(p domain.Persona) map[string]any {
	return map[string]any{
		"id":            p.ID,
		"author_id":     p.AuthorUserID,
		"name":          p.Name,
		"description":   p.Description,
		"system_prompt": p.SystemPrompt,
		"is_public":     p.IsPublic,
		"status":        string(p.Status),
		"admin_note":    nilOrString(p.AdminNote),
		"created_at":    p.CreatedAt.UTC().Format(time.RFC3339),
		"updated_at":    p.UpdatedAt.UTC().Format(time.RFC3339),
	}
}

func nilOrString(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func personaListReply(personas []domain.Persona) []map[string]any {
	out := make([]map[string]any, 0, len(personas))
	for _, p := range personas {
		out = append(out, personaReply(p))
	}
	return out
}
