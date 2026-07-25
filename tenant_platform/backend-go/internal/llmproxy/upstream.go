package llmproxy

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
)

// MaxUpstreamResponseBytes bounds the upstream response body read into memory.
const MaxUpstreamResponseBytes = 8 * 1024 * 1024

// UpstreamRequest is a validated request to forward upstream.
type UpstreamRequest struct {
	Body []byte // raw OpenAI chat-completions JSON body
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

// Forwarder forwards validated request bodies to the real upstream
// OpenAI-compatible API. The real key is injected here and never logged.
type Forwarder struct {
	baseURL string
	apiKey  string
	client  *http.Client
	log     *log.Logger
}

// NewForwarder validates baseURL/apiKey and applies defaults.
func NewForwarder(baseURL, apiKey string, client *http.Client) (*Forwarder, error) {
	if baseURL == "" {
		return nil, fmt.Errorf("upstream base URL is required")
	}
	if apiKey == "" {
		return nil, fmt.Errorf("upstream API key is required")
	}
	if client == nil {
		client = http.DefaultClient
	}
	return &Forwarder{baseURL: baseURL, apiKey: apiKey, client: client, log: log.Default()}, nil
}

// SetLogger injects a logger. Used by tests to assert no key leakage.
func (f *Forwarder) SetLogger(l *log.Logger) {
	if l != nil {
		f.log = l
	}
}

// Forward posts body to upstream /chat/completions and returns the response.
// Upstream 429/5xx produce an UpstreamError (no silent success). The real
// key is sent only in the Authorization header and never appears in logs.
func (f *Forwarder) Forward(ctx context.Context, req UpstreamRequest) (UpstreamResponse, error) {
	url := strings.TrimRight(f.baseURL, "/") + "/chat/completions"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(req.Body))
	if err != nil {
		return UpstreamResponse{}, fmt.Errorf("build upstream request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+f.apiKey)

	resp, err := f.client.Do(httpReq)
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
		// Log only status/URL/byte count — never the key or full body.
		f.log.Printf("upstream error status=%d url=%s bytes=%d", resp.StatusCode, url, len(body))
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
