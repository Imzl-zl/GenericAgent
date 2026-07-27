package api

import (
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
)

type providerWriteBody struct {
	Name            string                         `json:"name"`
	ProviderType    domain.LLMProviderType         `json:"provider_type"`
	BaseURL         string                         `json:"base_url"`
	Model           string                         `json:"model"`
	APIKey          *string                        `json:"api_key,omitempty"`
	SessionConfig   domain.GASessionConfig         `json:"session_config"`
	TransportConfig domain.ProviderTransportConfig `json:"transport_config"`
}

func (s *Server) handleAdminCreateLLMProvider(w http.ResponseWriter, r *http.Request) {
	tid := traceID()
	if s.llmProviders == nil || s.cipher == nil {
		writeErr(w, http.StatusServiceUnavailable, "NOT_CONFIGURED", "LLM provider management not configured", tid)
		return
	}
	var body providerWriteBody
	if err := decodeStrict(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "INVALID_JSON", err.Error(), tid)
		return
	}
	if err := body.validateAndNormalize(true); err != nil {
		writeErr(w, http.StatusBadRequest, "VALIDATION_ERROR", err.Error(), tid)
		return
	}
	ciphertext, keyVersion, err := s.encryptProviderKey(*body.APIKey)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "ENCRYPT_FAILED", err.Error(), tid)
		return
	}
	provider, err := s.llmProviders.CreateProvider(r.Context(), domain.LLMProviderCreate{
		Name: body.Name, ProviderType: body.ProviderType, BaseURL: body.BaseURL, Model: body.Model,
		APIKeyCiphertext: ciphertext, APIKeyKeyVersion: keyVersion,
		SessionConfig: body.SessionConfig, TransportConfig: body.TransportConfig,
	})
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
	var body providerWriteBody
	if err := decodeStrict(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "INVALID_JSON", err.Error(), tid)
		return
	}
	if err := body.validateAndNormalize(false); err != nil {
		writeErr(w, http.StatusBadRequest, "VALIDATION_ERROR", err.Error(), tid)
		return
	}
	rotateKey := body.APIKey != nil && strings.TrimSpace(*body.APIKey) != ""
	var ciphertext []byte
	var keyVersion string
	if rotateKey {
		var err error
		ciphertext, keyVersion, err = s.encryptProviderKey(*body.APIKey)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "ENCRYPT_FAILED", err.Error(), tid)
			return
		}
	}
	provider, err := s.llmProviders.UpdateProvider(r.Context(), id, domain.LLMProviderUpdate{
		LLMProviderCreate: domain.LLMProviderCreate{
			Name: body.Name, ProviderType: body.ProviderType, BaseURL: body.BaseURL, Model: body.Model,
			APIKeyCiphertext: ciphertext, APIKeyKeyVersion: keyVersion,
			SessionConfig: body.SessionConfig, TransportConfig: body.TransportConfig,
		},
		RotateAPIKey: rotateKey,
	})
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

func (s *Server) handleAdminDisableLLMProvider(w http.ResponseWriter, r *http.Request) {
	s.handleAdminSetLLMProviderState(w, r, domain.ProviderDisabled)
}

func (s *Server) handleAdminEnableLLMProvider(w http.ResponseWriter, r *http.Request) {
	s.handleAdminSetLLMProviderState(w, r, domain.ProviderActive)
}

func (s *Server) handleAdminSetLLMProviderState(
	w http.ResponseWriter,
	r *http.Request,
	state domain.LLMProviderState,
) {
	tid := traceID()
	if s.llmProviders == nil {
		writeErr(w, http.StatusServiceUnavailable, "NOT_CONFIGURED", "LLM provider management not configured", tid)
		return
	}
	id, ok := parseProviderID(w, r, tid)
	if !ok {
		return
	}
	provider, err := s.llmProviders.SetProviderState(r.Context(), id, state)
	if err != nil {
		writeErr(w, http.StatusConflict, "PROVIDER_STATE_FAILED", err.Error(), tid)
		return
	}
	writeJSON(w, http.StatusOK, llmProviderReply(provider))
}

func (b *providerWriteBody) validateAndNormalize(requireAPIKey bool) error {
	b.Name = strings.TrimSpace(b.Name)
	b.BaseURL = strings.TrimSpace(b.BaseURL)
	b.Model = strings.TrimSpace(b.Model)
	if b.Name == "" {
		return fmt.Errorf("name is required")
	}
	if b.Model == "" {
		return fmt.Errorf("model is required")
	}
	if b.ProviderType != domain.ProviderNativeOAI && b.ProviderType != domain.ProviderNativeClaude {
		return fmt.Errorf("provider_type must be one of %s|%s", domain.ProviderNativeOAI, domain.ProviderNativeClaude)
	}
	if err := validateProviderBaseURL(b.BaseURL); err != nil {
		return err
	}
	if requireAPIKey && (b.APIKey == nil || strings.TrimSpace(*b.APIKey) == "") {
		return fmt.Errorf("api_key is required")
	}
	if err := b.SessionConfig.Validate(b.ProviderType); err != nil {
		return fmt.Errorf("session_config: %w", err)
	}
	if b.TransportConfig.AuthMode == "" {
		b.TransportConfig.AuthMode = domain.ProviderAuthAuto
	}
	if err := b.TransportConfig.Validate(); err != nil {
		return fmt.Errorf("transport_config: %w", err)
	}
	return nil
}

func validateProviderBaseURL(raw string) error {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return fmt.Errorf("base_url must be an absolute http or https URL")
	}
	if parsed.User != nil || parsed.Fragment != "" || parsed.Opaque != "" {
		return fmt.Errorf("base_url must contain no credentials or fragment")
	}
	return nil
}

func (s *Server) encryptProviderKey(raw string) ([]byte, string, error) {
	ciphertext, version, err := s.cipher.Encrypt([]byte(strings.TrimSpace(raw)))
	if err != nil {
		return nil, "", err
	}
	return ciphertext, strconv.Itoa(version), nil
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
		"provider_id":      p.ID,
		"name":             p.Name,
		"provider_type":    string(p.ProviderType),
		"base_url":         p.BaseURL,
		"model":            p.Model,
		"session_config":   p.SessionConfig,
		"transport_config": p.TransportConfig,
		"revision":         p.Revision,
		"is_default":       p.IsDefault,
		"state":            p.State,
		"created_at":       p.CreatedAt.UTC().Format(time.RFC3339),
		"updated_at":       p.UpdatedAt.UTC().Format(time.RFC3339),
	}
}
