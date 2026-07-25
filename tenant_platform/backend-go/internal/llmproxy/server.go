package llmproxy

import (
	"encoding/json"
	"net/http"
	"time"
)

// defaultUpstreamTimeout bounds a single upstream forward call.
const defaultUpstreamTimeout = 60 * time.Second

// Server is the LLM Proxy HTTP server. It validates capability_tokens and
// forwards approved requests upstream. The real upstream key lives only in
// the Forwarder.
type Server struct {
	cfg       Config
	validator *Validator
	forwarder *Forwarder
}

// NewServer validates cfg and constructs the validator + forwarder.
func NewServer(cfg Config) (*Server, error) {
	cfg = cfg.WithDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	v, err := NewValidator(cfg.SigningKey)
	if err != nil {
		return nil, err
	}
	f, err := NewForwarder(cfg.UpstreamBaseURL, cfg.UpstreamAPIKey, &http.Client{Timeout: defaultUpstreamTimeout})
	if err != nil {
		return nil, err
	}
	return &Server{cfg: cfg, validator: v, forwarder: f}, nil
}

// Config returns the validated configuration.
func (s *Server) Config() Config { return s.cfg }

// Validator returns the token validator (used by tests and the revoke path).
func (s *Server) Validator() *Validator { return s.validator }

// SetForwarder overrides the upstream forwarder (for tests).
func (s *Server) SetForwarder(f *Forwarder) {
	if f != nil {
		s.forwarder = f
	}
}

// Handler returns the HTTP handler tree.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.handleHealth)
	mux.HandleFunc("/v1/chat/completions", s.handleChatCompletions)
	mux.HandleFunc("/chat/completions", s.handleChatCompletions)
	mux.HandleFunc("/internal/revoke", s.handleRevoke)
	return mux
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}
