package llmproxy

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
)

// defaultUpstreamTimeout bounds a single upstream forward call.
//
// Must exceed the Worker's GA Core read_timeout (configured to 120s in
// worker_credential.go buildMyKeyContent). Proxy timeout < GA Core timeout
// causes the Proxy to cancel the request while GA Core is still waiting,
// wasting upstream LLM quota (the request already reached the provider).
// Industry standard (Nginx proxy_read_timeout, Envoy timeout): proxy layer
// timeout > backend timeout. 150s = 120s GA Core read_timeout + 30s buffer
// for connection/TLS overhead.
const defaultUpstreamTimeout = 150 * time.Second

// Server is the LLM Proxy HTTP server. It validates capability_tokens,
// fetches the active provider from the platform store, decrypts the real key,
// and forwards approved requests upstream.
type Server struct {
	cfg       Config
	validator *Validator
	upstream  *Upstream
}

// NewServer validates cfg and constructs the validator + upstream.
func NewServer(cfg Config) (*Server, error) {
	cfg = cfg.WithDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	v, err := NewValidator(cfg.SigningKey, cfg.Revocations)
	if err != nil {
		return nil, err
	}
	// http.Client.Timeout is intentionally NOT set: the handler-level ctx
	// (context.WithTimeout(defaultUpstreamTimeout)) is the single source of
	// truth for request lifetime. A redundant client-level timeout would race
	// with ctx and could fire first, obscuring the real cancellation reason.
	//
	// The transport is tuned for one (or few) upstream LLM hosts: the Go
	// default MaxIdleConnsPerHost of 2 would force a fresh TCP+TLS handshake
	// for almost every concurrent Worker request. 32 idle conns per host
	// covers the expected concurrency with headroom (same rationale as
	// poller/client.go).
	u := NewUpstream(&http.Client{Transport: &http.Transport{
		MaxIdleConns:        64,
		MaxIdleConnsPerHost: 32,
		IdleConnTimeout:     90 * time.Second,
		TLSHandshakeTimeout: 10 * time.Second,
	}})
	return &Server{cfg: cfg, validator: v, upstream: u}, nil
}

// Config returns the validated configuration.
func (s *Server) Config() Config { return s.cfg }

// Validator returns the token validator for focused tests.
func (s *Server) Validator() *Validator { return s.validator }

// SetUpstream overrides the upstream forwarder (for tests).
func (s *Server) SetUpstream(u *Upstream) {
	if u != nil {
		s.upstream = u
	}
}

// Handler returns the HTTP handler tree.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.handleHealth)
	mux.HandleFunc("/v1/chat/completions", s.handleChatCompletions)
	mux.HandleFunc("/chat/completions", s.handleChatCompletions)
	mux.HandleFunc("/v1/messages", s.handleMessages)
	return mux
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

// currentProvider returns the active provider with decrypted API key.
func (s *Server) currentProvider(ctx context.Context) (domain.LLMProvider, error) {
	p, err := s.cfg.ProviderSource.GetDefaultProvider(ctx)
	if err != nil {
		return domain.LLMProvider{}, err
	}
	version, err := strconv.Atoi(p.APIKeyKeyVersion)
	if err != nil {
		return domain.LLMProvider{}, fmt.Errorf("parse key version: %w", err)
	}
	key, err := s.cfg.Cipher.Decrypt(p.APIKeyCiphertext, version)
	if err != nil {
		return domain.LLMProvider{}, err
	}
	p.APIKey = string(key)
	return p, nil
}
