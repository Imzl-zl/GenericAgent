package llmproxy

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"
	"unicode"
)

// Server validates capability tokens and streams native Provider traffic
// through one policy-enforcing reverse proxy.
type Server struct {
	cfg          Config
	validator    *Validator
	reverseProxy http.Handler
}

// NewServer validates cfg and constructs the shared policy and reverse proxy.
func NewServer(cfg Config) (*Server, error) {
	cfg = cfg.WithDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	validator, err := NewValidator(cfg.SigningKey, cfg.Revocations)
	if err != nil {
		return nil, err
	}
	// round9 审查: 生产(llm-proxy 进程/内嵌)必须配置在线 task 活跃性校验;
	// 未配置(测试)保持签名+撤销校验。
	if cfg.TaskChecker != nil {
		validator.WithTaskChecker(cfg.TaskChecker)
	}
	policy, err := NewNetworkPolicy(cfg.AllowedUpstreamCIDRs, cfg.AllowedHTTPHosts)
	if err != nil {
		return nil, err
	}
	cache, err := NewTransportCache(policy)
	if err != nil {
		return nil, err
	}
	return &Server{
		cfg: cfg, validator: validator, reverseProxy: newTransparentReverseProxy(cache),
	}, nil
}

// NewHTTPServer applies the same streaming-safe limits to every deployment.
func NewHTTPServer(addr string, handler http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      0,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}
}

// ParseNetworkPolicyList parses comma- or whitespace-separated deployment values.
func ParseNetworkPolicyList(raw string) []string {
	return strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || unicode.IsSpace(r)
	})
}

// Config returns the validated configuration.
func (s *Server) Config() Config { return s.cfg }

// Validator returns the token validator for focused tests.
func (s *Server) Validator() *Validator { return s.validator }

// Handler returns the HTTP handler tree.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.handleHealth)
	mux.HandleFunc("/v1/chat/completions", s.handleChatCompletions)
	mux.HandleFunc("/chat/completions", s.handleChatCompletions)
	mux.HandleFunc("/v1/responses", s.handleResponses)
	mux.HandleFunc("/v1/messages", s.handleMessages)
	mux.HandleFunc("/v1/images/generations", s.handleImageGenerations)
	mux.HandleFunc("/images/generations", s.handleImageGenerations)
	return mux
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}
