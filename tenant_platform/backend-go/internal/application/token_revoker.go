package application

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// revokeTimeout bounds a single best-effort token revocation call.
const revokeTimeout = 5 * time.Second

// TokenRevoker revokes a capability_token by JTI. The platform calls this
// when a Worker session ends (process cleanup) so the token cannot be reused.
type TokenRevoker interface {
	Revoke(ctx context.Context, jti string) error
}

// httpTokenRevoker calls the LLM Proxy's /internal/revoke endpoint.
type httpTokenRevoker struct {
	proxyAddr string
	client    *http.Client
}

// NewHTTPTokenRevoker constructs an HTTP-backed revoker targeting the LLM
// Proxy at proxyAddr. Returns a non-nil interface even for empty addr (the
// revocations become no-ops, surfaced via logged errors).
func NewHTTPTokenRevoker(proxyAddr string) TokenRevoker {
	if strings.TrimSpace(proxyAddr) == "" {
		return noopTokenRevoker{}
	}
	return newHTTPTokenRevoker(proxyAddr)
}

func newHTTPTokenRevoker(proxyAddr string) *httpTokenRevoker {
	return &httpTokenRevoker{
		proxyAddr: strings.TrimRight(proxyAddr, "/"),
		client:    &http.Client{Timeout: revokeTimeout},
	}
}

func (r *httpTokenRevoker) Revoke(ctx context.Context, jti string) error {
	if jti == "" {
		return nil
	}
	body, err := json.Marshal(map[string]string{"jti": jti})
	if err != nil {
		return fmt.Errorf("marshal revoke body: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.proxyAddr+"/internal/revoke", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build revoke request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := r.client.Do(req)
	if err != nil {
		return fmt.Errorf("revoke request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("revoke returned %d", resp.StatusCode)
	}
	return nil
}

// noopTokenRevoker is the zero-value fallback when revocation is not wired
// (e.g., unit tests with an injected Worker that never holds a real token).
type noopTokenRevoker struct{}

func (noopTokenRevoker) Revoke(context.Context, string) error { return nil }
