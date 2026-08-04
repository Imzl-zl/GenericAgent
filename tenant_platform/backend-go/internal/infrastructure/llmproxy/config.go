// Package llmproxy implements the LLM Proxy: the sole holder of the real
// upstream LLM key. It validates short-lived session capability_tokens and
// forwards approved requests to the configured upstream provider.
package llmproxy

import (
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"
)

const (
	DefaultTokenTTL            = time.Hour
	DefaultRevocationRetention = DefaultTokenTTL + time.Minute
	MinSigningKeyLen           = 32
)

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
	Listen               string
	SigningKey           []byte
	TokenTTL             time.Duration
	ProviderSource       ProviderSource
	Cipher               TokenCipher
	Revocations          CapabilityRevocationSource
	// TaskChecker 在线联查 capability 绑定的 task/lease/成员状态(round9
	// 审查): 非 nil 时每次调用前执行, 成员移除/接管即时生效。
	TaskChecker          TaskCapabilityChecker
	// UsageCounter 按 JTI 原子计量 capability 调用次数(审查 R4-I9)。
	// 非 nil 时 llm.chat 转发前消费预算(max_turns), 超额拒绝。
	UsageCounter         CapabilityUsageCounter
	AllowedUpstreamCIDRs []string
	AllowedHTTPHosts     []string
}

// WithDefaults applies zero-value defaults.
func (c Config) WithDefaults() Config {
	if c.TokenTTL == 0 {
		c.TokenTTL = DefaultTokenTTL
	}
	return c
}

// Validate enforces non-empty, well-formed listen configuration.
// 默认开发态 loopback; 容器部署(llm-proxy 供内部 Runner 经 runner-control
// 网络访问)显式设置 0.0.0.0:8081(审查 C2: 内部服务必须监听非 loopback)。
func (c Config) Validate() error {
	if c.Listen == "" {
		return fmt.Errorf("listen address is required")
	}
	if !isValidListenAddr(c.Listen) {
		return fmt.Errorf("invalid listen address %q", c.Listen)
	}
	if c.ProviderSource == nil {
		return fmt.Errorf("provider source is required")
	}
	if c.Cipher == nil {
		return fmt.Errorf("cipher is required")
	}
	if c.Revocations == nil {
		return fmt.Errorf("capability revocation source is required")
	}
	if len(c.SigningKey) < MinSigningKeyLen {
		return fmt.Errorf("signing key must be at least %d bytes", MinSigningKeyLen)
	}
	if c.TokenTTL <= 0 {
		return fmt.Errorf("token ttl must be positive")
	}
	if _, err := NewNetworkPolicy(c.AllowedUpstreamCIDRs, c.AllowedHTTPHosts); err != nil {
		return err
	}
	return nil
}

func (c Config) NetworkPolicy() (*NetworkPolicy, error) {
	return NewNetworkPolicy(c.AllowedUpstreamCIDRs, c.AllowedHTTPHosts)
}

// isValidListenAddr 校验监听地址可解析: 允许 loopback(默认)与显式配置的
// 通配/具体地址(容器内部服务, 审查 C2); 拒绝空 host、无效端口与非法格式。
func isValidListenAddr(addr string) bool {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if host == "" || strings.ContainsAny(host, " \t/") {
		return false
	}
	p, err := strconv.Atoi(port)
	if err != nil || p < 0 || p > 65535 {
		return false
	}
	return true
}
