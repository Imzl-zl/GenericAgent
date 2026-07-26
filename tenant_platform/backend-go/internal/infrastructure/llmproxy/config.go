// Package llmproxy implements the LLM Proxy: the sole holder of the real
// upstream LLM key. It validates short-lived session capability_tokens and
// forwards approved requests to the configured upstream provider.
package llmproxy

import (
	"fmt"
	"net"
	"time"
)

// DefaultTokenTTL is the default capability_token lifetime.
const DefaultTokenTTL = 1 * time.Hour

// MinSigningKeyLen is the minimum HMAC signing key length in bytes.
const MinSigningKeyLen = 16

// TokenCipher decrypts provider API keys stored in the platform database.
// The key version is persisted alongside the ciphertext so the platform can
// rotate keys without re-encrypting every provider.
type TokenCipher interface {
	Decrypt(ciphertext []byte, keyVersion int) ([]byte, error)
}

// Config holds LLM Proxy runtime configuration. The real upstream key is
// fetched from the provider store and decrypted with the cipher; it is never
// part of this static config.
type Config struct {
	Listen         string
	SigningKey     []byte
	TokenTTL       time.Duration
	ProviderSource ProviderSource
	Cipher         TokenCipher
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
	if c.ProviderSource == nil {
		return fmt.Errorf("provider source is required")
	}
	if c.Cipher == nil {
		return fmt.Errorf("cipher is required")
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
