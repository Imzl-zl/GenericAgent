// Package llmproxy implements the LLM Proxy: the sole holder of the real
// upstream LLM key. It validates short-lived session capability_tokens and
// forwards approved requests to the upstream OpenAI-compatible API.
package llmproxy

import (
	"fmt"
	"net"
	"net/url"
	"time"
)

// DefaultTokenTTL is the default capability_token lifetime.
const DefaultTokenTTL = 1 * time.Hour

// MinSigningKeyLen is the minimum HMAC signing key length in bytes.
const MinSigningKeyLen = 16

// Config holds LLM Proxy runtime configuration. The real upstream key is
// injected via host environment and never logged.
type Config struct {
	Listen          string
	UpstreamBaseURL string
	UpstreamAPIKey  string
	SigningKey      []byte
	TokenTTL        time.Duration
}

// WithDefaults applies zero-value defaults.
func (c Config) WithDefaults() Config {
	if c.TokenTTL == 0 {
		c.TokenTTL = DefaultTokenTTL
	}
	return c
}

// Validate enforces non-empty, well-formed, loopback-bound configuration.
func (c Config) Validate() error {
	if c.Listen == "" {
		return fmt.Errorf("listen address is required")
	}
	if !isLoopbackAddr(c.Listen) {
		return fmt.Errorf("listen address must be loopback: %s", c.Listen)
	}
	if c.UpstreamBaseURL == "" {
		return fmt.Errorf("upstream base URL is required")
	}
	u, err := url.Parse(c.UpstreamBaseURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return fmt.Errorf("upstream base URL is invalid: %s", c.UpstreamBaseURL)
	}
	if c.UpstreamAPIKey == "" {
		return fmt.Errorf("upstream API key is required (host env)")
	}
	if len(c.SigningKey) < MinSigningKeyLen {
		return fmt.Errorf("signing key must be at least %d bytes", MinSigningKeyLen)
	}
	if c.TokenTTL <= 0 {
		return fmt.Errorf("token ttl must be positive")
	}
	return nil
}

func isLoopbackAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
