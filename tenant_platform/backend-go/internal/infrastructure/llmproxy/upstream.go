package llmproxy

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
)

// MaxUpstreamResponseBytes bounds the upstream response body read into memory.
const MaxUpstreamResponseBytes = 8 * 1024 * 1024

// UpstreamRequest is a validated request to forward upstream.
type UpstreamRequest struct {
	Body []byte // raw request body in the provider's native format
}

// UpstreamResponse is the upstream's non-streaming response.
type UpstreamResponse struct {
	StatusCode int
	Body       []byte
}

// UpstreamError is returned when the upstream responds 429 or 5xx. It is
// observable and retryable; the Proxy never silently succeeds on these.
type UpstreamError struct {
	Code int
	Body []byte
}

func (e *UpstreamError) Error() string {
	return fmt.Sprintf("upstream returned %d: %s", e.Code, truncateForLog(e.Body, 256))
}

// Upstream forwards validated request bodies to the real upstream using the
// provider's protocol. The real key is injected here and never logged.
type Upstream struct {
	client *http.Client
	log    *log.Logger
}

// NewUpstream creates an Upstream with the given HTTP client.
func NewUpstream(client *http.Client) *Upstream {
	if client == nil {
		client = http.DefaultClient
	}
	return &Upstream{client: client, log: log.Default()}
}

// SetLogger injects a logger. Used by tests to assert no key leakage.
func (u *Upstream) SetLogger(l *log.Logger) {
	if l != nil {
		u.log = l
	}
}

// Forward posts the body to the upstream endpoint matching the provider type.
// Upstream 429/5xx produce an UpstreamError (no silent success).
func (u *Upstream) Forward(ctx context.Context, p domain.LLMProvider, req UpstreamRequest) (UpstreamResponse, error) {
	switch p.ProviderType {
	case domain.ProviderOpenAICompatible:
		return u.forwardOpenAI(ctx, p, req)
	case domain.ProviderAnthropicMessages:
		return u.forwardAnthropic(ctx, p, req)
	default:
		return UpstreamResponse{}, fmt.Errorf("unsupported provider type: %s", p.ProviderType)
	}
}

func (u *Upstream) forwardOpenAI(ctx context.Context, p domain.LLMProvider, req UpstreamRequest) (UpstreamResponse, error) {
	url := strings.TrimRight(p.BaseURL, "/") + "/chat/completions"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(req.Body))
	if err != nil {
		return UpstreamResponse{}, fmt.Errorf("build upstream request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+p.APIKey)
	return u.do(httpReq)
}

func (u *Upstream) forwardAnthropic(ctx context.Context, p domain.LLMProvider, req UpstreamRequest) (UpstreamResponse, error) {
	url := strings.TrimRight(p.BaseURL, "/") + "/v1/messages"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(req.Body))
	if err != nil {
		return UpstreamResponse{}, fmt.Errorf("build upstream request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("x-api-key", p.APIKey)
	httpReq.Header.Set("anthropic-version", "2023-06-01")
	return u.do(httpReq)
}

func (u *Upstream) do(httpReq *http.Request) (UpstreamResponse, error) {
	resp, err := u.client.Do(httpReq)
	if err != nil {
		return UpstreamResponse{}, fmt.Errorf("upstream request: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, MaxUpstreamResponseBytes+1))
	if err != nil {
		return UpstreamResponse{}, fmt.Errorf("read upstream response: %w", err)
	}
	if len(body) > MaxUpstreamResponseBytes {
		return UpstreamResponse{StatusCode: resp.StatusCode, Body: body[:MaxUpstreamResponseBytes]},
			fmt.Errorf("upstream response exceeds %d bytes", MaxUpstreamResponseBytes)
	}
	out := UpstreamResponse{StatusCode: resp.StatusCode, Body: body}
	if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
		u.log.Printf("upstream error status=%d url=%s bytes=%d", resp.StatusCode, httpReq.URL.String(), len(body))
		return out, &UpstreamError{Code: resp.StatusCode, Body: body}
	}
	return out, nil
}

func truncateForLog(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "...(truncated)"
}
