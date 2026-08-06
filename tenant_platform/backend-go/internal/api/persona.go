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

// updatePersonaBody is the request body for edit operations. Unlike
// personaBody it omits is_public: visibility is controlled by the moderation
// lifecycle (create/submit/approve/reject), not by editing.
type updatePersonaBody struct {
	Name         string `json:"name"`
	Description  string `json:"description"`
	SystemPrompt string `json:"system_prompt"`
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
	if err := validatePersonaBody(body.Name, body.SystemPrompt); err != nil {
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
	if err := validatePersonaBody(body.Name, body.SystemPrompt); err != nil {
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

// handleAdminListPersonas lists every persona for pool management. An optional
// ?status= query filters by lifecycle status (private/pending/approved/rejected).
// ?mine=true restricts the list to personas authored by the admin (dev user id).
func (s *Server) handleAdminListPersonas(w http.ResponseWriter, r *http.Request) {
	tid := traceID()
	if r.URL.Query().Get("mine") == "true" {
		personas, err := s.personas.ListByAuthor(r.Context(), s.adminUserID)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "PERSONA_LIST_FAILED", err.Error(), tid)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"personas": personaListReply(personas)})
		return
	}
	status := strings.TrimSpace(r.URL.Query().Get("status"))
	if status != "" && !domain.IsValidPersonaStatus(domain.PersonaStatus(status)) {
		writeErr(w, http.StatusBadRequest, "VALIDATION_ERROR", "invalid status filter", tid)
		return
	}
	personas, err := s.personas.ListAll(r.Context(), status)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "PERSONA_LIST_FAILED", err.Error(), tid)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"personas": personaListReply(personas)})
}

// handleAdminCreatePersona lets the admin author a persona under the platform
// dev user id. Public personas are published straight to the pool (approved)
// since the admin is the moderation authority.
func (s *Server) handleAdminCreatePersona(w http.ResponseWriter, r *http.Request) {
	tid := traceID()
	var body personaBody
	if err := decodeStrict(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "INVALID_JSON", err.Error(), tid)
		return
	}
	if err := validatePersonaBody(body.Name, body.SystemPrompt); err != nil {
		writeErr(w, http.StatusBadRequest, "VALIDATION_ERROR", err.Error(), tid)
		return
	}
	p, err := s.personas.AdminCreatePersona(r.Context(), s.adminUserID, body.Name, body.Description, body.SystemPrompt, body.IsPublic)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "PERSONA_CREATE_FAILED", err.Error(), tid)
		return
	}
	writeJSON(w, http.StatusCreated, personaReply(p))
}

// handleAdminUpdatePersona edits any persona in the pool regardless of author.
func (s *Server) handleAdminUpdatePersona(w http.ResponseWriter, r *http.Request) {
	tid := traceID()
	id := r.PathValue("persona_id")
	if id == "" {
		writeErr(w, http.StatusBadRequest, "INVALID_ID", "persona_id is required", tid)
		return
	}
	var body updatePersonaBody
	if err := decodeStrict(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "INVALID_JSON", err.Error(), tid)
		return
	}
	if err := validatePersonaBody(body.Name, body.SystemPrompt); err != nil {
		writeErr(w, http.StatusBadRequest, "VALIDATION_ERROR", err.Error(), tid)
		return
	}
	p, err := s.personas.AdminUpdatePersona(r.Context(), id, s.adminUserID, body.Name, body.Description, body.SystemPrompt)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "PERSONA_UPDATE_FAILED", err.Error(), tid)
		return
	}
	writeJSON(w, http.StatusOK, personaReply(p))
}

// handleAdminDeletePersona removes any persona from the pool regardless of
// author. Users who had it as their default are cleared via ON DELETE SET NULL.
func (s *Server) handleAdminDeletePersona(w http.ResponseWriter, r *http.Request) {
	tid := traceID()
	id := r.PathValue("persona_id")
	if id == "" {
		writeErr(w, http.StatusBadRequest, "INVALID_ID", "persona_id is required", tid)
		return
	}
	if err := s.personas.AdminDeletePersona(r.Context(), id); err != nil {
		writeErr(w, http.StatusBadRequest, "PERSONA_DELETE_FAILED", err.Error(), tid)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": true})
}

// handleAdminSetDefaultPersona sets or clears the admin's own default persona
// (applied to the admin's bound bot), using the platform dev user id.
func (s *Server) handleAdminSetDefaultPersona(w http.ResponseWriter, r *http.Request) {
	tid := traceID()
	var body struct {
		PersonaID string `json:"persona_id"`
	}
	if err := decodeStrict(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "INVALID_JSON", err.Error(), tid)
		return
	}
	var err error
	if body.PersonaID == "" {
		err = s.personas.ClearDefault(r.Context(), s.adminUserID)
	} else {
		err = s.personas.SetDefault(r.Context(), s.adminUserID, body.PersonaID)
	}
	if err != nil {
		writeErr(w, http.StatusBadRequest, "DEFAULT_PERSONA_FAILED", err.Error(), tid)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"default_persona_id": body.PersonaID})
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
	if err := s.personas.Moderate(r.Context(), id, s.adminUserID, status, body.Note); err != nil {
		writeErr(w, http.StatusBadRequest, "PERSONA_MODERATE_FAILED", err.Error(), tid)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "status": string(status)})
}

func validatePersonaBody(name, systemPrompt string) error {
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("name is required")
	}
	if strings.TrimSpace(systemPrompt) == "" {
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
