package api

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
)

type createLLMProviderBody struct {
	Name         string `json:"name"`
	ProviderType string `json:"provider_type"`
	BaseURL      string `json:"base_url"`
	Model        string `json:"model"`
	APIKey       string `json:"api_key"`
}

type updateLLMProviderBody struct {
	Name         string `json:"name"`
	ProviderType string `json:"provider_type"`
	BaseURL      string `json:"base_url"`
	Model        string `json:"model"`
	APIKey       string `json:"api_key"`
}

func (s *Server) handleAdminCreateLLMProvider(w http.ResponseWriter, r *http.Request) {
	tid := traceID()
	if s.llmProviders == nil || s.cipher == nil {
		writeErr(w, http.StatusServiceUnavailable, "NOT_CONFIGURED", "LLM provider management not configured", tid)
		return
	}
	var body createLLMProviderBody
	if err := decodeStrict(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "INVALID_JSON", err.Error(), tid)
		return
	}
	providerType, err := validateLLMProviderBody(body.Name, body.ProviderType, body.BaseURL, body.Model, body.APIKey)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "VALIDATION_ERROR", err.Error(), tid)
		return
	}
	ciphertext, version, err := s.cipher.Encrypt([]byte(body.APIKey))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "ENCRYPT_FAILED", err.Error(), tid)
		return
	}
	provider, err := s.llmProviders.CreateProvider(r.Context(), body.Name,
		providerType, body.BaseURL, body.Model, ciphertext, strconv.Itoa(version))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "PROVIDER_CREATE_FAILED", err.Error(), tid)
		return
	}
	writeJSON(w, http.StatusCreated, llmProviderReply(provider))
}

func (s *Server) handleAdminListLLMProviders(w http.ResponseWriter, r *http.Request) {
	tid := traceID()
	if s.llmProviders == nil {
		writeErr(w, http.StatusServiceUnavailable, "NOT_CONFIGURED", "LLM provider management not configured", tid)
		return
	}
	providers, err := s.llmProviders.ListProviders(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "LIST_PROVIDERS_FAILED", err.Error(), tid)
		return
	}
	out := make([]map[string]any, 0, len(providers))
	for _, p := range providers {
		out = append(out, llmProviderReply(p))
	}
	writeJSON(w, http.StatusOK, map[string]any{"providers": out})
}

func (s *Server) handleAdminGetLLMProvider(w http.ResponseWriter, r *http.Request) {
	tid := traceID()
	if s.llmProviders == nil {
		writeErr(w, http.StatusServiceUnavailable, "NOT_CONFIGURED", "LLM provider management not configured", tid)
		return
	}
	id, ok := parseProviderID(w, r, tid)
	if !ok {
		return
	}
	provider, err := s.llmProviders.GetProvider(r.Context(), id)
	if err != nil {
		writeErr(w, http.StatusNotFound, "PROVIDER_NOT_FOUND", err.Error(), tid)
		return
	}
	writeJSON(w, http.StatusOK, llmProviderReply(provider))
}

func (s *Server) handleAdminUpdateLLMProvider(w http.ResponseWriter, r *http.Request) {
	tid := traceID()
	if s.llmProviders == nil || s.cipher == nil {
		writeErr(w, http.StatusServiceUnavailable, "NOT_CONFIGURED", "LLM provider management not configured", tid)
		return
	}
	id, ok := parseProviderID(w, r, tid)
	if !ok {
		return
	}
	var body updateLLMProviderBody
	if err := decodeStrict(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "INVALID_JSON", err.Error(), tid)
		return
	}
	providerType, err := validateLLMProviderBody(body.Name, body.ProviderType, body.BaseURL, body.Model, body.APIKey)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "VALIDATION_ERROR", err.Error(), tid)
		return
	}
	ciphertext, version, err := s.cipher.Encrypt([]byte(body.APIKey))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "ENCRYPT_FAILED", err.Error(), tid)
		return
	}
	provider, err := s.llmProviders.UpdateProvider(r.Context(), id, body.Name,
		providerType, body.BaseURL, body.Model, ciphertext, strconv.Itoa(version))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "PROVIDER_UPDATE_FAILED", err.Error(), tid)
		return
	}
	writeJSON(w, http.StatusOK, llmProviderReply(provider))
}

func (s *Server) handleAdminDeleteLLMProvider(w http.ResponseWriter, r *http.Request) {
	tid := traceID()
	if s.llmProviders == nil {
		writeErr(w, http.StatusServiceUnavailable, "NOT_CONFIGURED", "LLM provider management not configured", tid)
		return
	}
	id, ok := parseProviderID(w, r, tid)
	if !ok {
		return
	}
	if err := s.llmProviders.DeleteProvider(r.Context(), id); err != nil {
		writeErr(w, http.StatusBadRequest, "PROVIDER_DELETE_FAILED", err.Error(), tid)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleAdminSetDefaultLLMProvider(w http.ResponseWriter, r *http.Request) {
	tid := traceID()
	if s.llmProviders == nil {
		writeErr(w, http.StatusServiceUnavailable, "NOT_CONFIGURED", "LLM provider management not configured", tid)
		return
	}
	id, ok := parseProviderID(w, r, tid)
	if !ok {
		return
	}
	if err := s.llmProviders.SetDefaultProvider(r.Context(), id); err != nil {
		writeErr(w, http.StatusBadRequest, "SET_DEFAULT_FAILED", err.Error(), tid)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"provider_id": id, "is_default": true})
}

func validateLLMProviderBody(name, providerType, baseURL, model, apiKey string) (domain.LLMProviderType, error) {
	if strings.TrimSpace(name) == "" {
		return "", fmt.Errorf("name is required")
	}
	if strings.TrimSpace(baseURL) == "" {
		return "", fmt.Errorf("base_url is required")
	}
	if strings.TrimSpace(model) == "" {
		return "", fmt.Errorf("model is required")
	}
	if strings.TrimSpace(apiKey) == "" {
		return "", fmt.Errorf("api_key is required")
	}
	t := domain.LLMProviderType(providerType)
	if t != domain.ProviderOpenAICompatible && t != domain.ProviderAnthropicMessages {
		return "", fmt.Errorf("provider_type must be one of %s|%s",
			domain.ProviderOpenAICompatible, domain.ProviderAnthropicMessages)
	}
	return t, nil
}

func parseProviderID(w http.ResponseWriter, r *http.Request, tid string) (int64, bool) {
	raw := r.PathValue("provider_id")
	id, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil || id <= 0 {
		writeErr(w, http.StatusBadRequest, "INVALID_PROVIDER_ID", "provider_id must be a positive integer", tid)
		return 0, false
	}
	return id, true
}

func llmProviderReply(p domain.LLMProvider) map[string]any {
	return map[string]any{
		"provider_id":   p.ID,
		"name":          p.Name,
		"provider_type": string(p.ProviderType),
		"base_url":      p.BaseURL,
		"model":         p.Model,
		"is_default":    p.IsDefault,
		"state":         p.State,
		"created_at":    p.CreatedAt.UTC().Format(time.RFC3339),
		"updated_at":    p.UpdatedAt.UTC().Format(time.RFC3339),
	}
}
