package llmproxy

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
)

const (
	defaultConnectTimeout        = 10 * time.Second
	defaultResponseHeaderTimeout = 120 * time.Second
	maxCachedTransports          = 64
)

// TransportCache reuses connection pools per Provider and effective transport settings.
type TransportCache struct {
	policy       *NetworkPolicy
	mu           sync.Mutex
	cache        map[string]*policyRoundTripper
	providerKeys map[int64]string
}

// NewTransportCache constructs a bounded cache under one immutable network policy.
func NewTransportCache(policy *NetworkPolicy) (*TransportCache, error) {
	if policy == nil || policy.resolver == nil {
		return nil, errors.New("network policy is required")
	}
	return &TransportCache{
		policy: policy, cache: make(map[string]*policyRoundTripper),
		providerKeys: make(map[int64]string),
	}, nil
}

// RoundTripper returns the Provider's cached, policy-enforcing transport.
func (c *TransportCache) RoundTripper(provider domain.LLMProvider) (http.RoundTripper, error) {
	if provider.ID <= 0 {
		return nil, errors.New("provider id must be positive")
	}
	if err := provider.TransportConfig.Validate(); err != nil {
		return nil, fmt.Errorf("provider transport config: %w", err)
	}
	base, err := url.Parse(provider.BaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse provider base URL: %w", err)
	}
	connectTimeout, responseHeaderTimeout, tlsVerify, err := effectiveTransportSettings(provider.TransportConfig)
	if err != nil {
		return nil, err
	}
	validationCtx, cancel := context.WithTimeout(context.Background(), connectTimeout)
	defer cancel()
	if err := c.policy.ValidateURL(validationCtx, base); err != nil {
		return nil, fmt.Errorf("provider target policy: %w", err)
	}
	proxyURL, err := parseTransportProxy(provider.TransportConfig.ProxyURL)
	if err != nil {
		return nil, err
	}
	if proxyURL != nil {
		if err := c.policy.ValidateURL(validationCtx, proxyURL); err != nil {
			return nil, fmt.Errorf("provider proxy policy: %w", err)
		}
	}
	settingsHash, err := transportCacheKey(provider.TransportConfig, connectTimeout, responseHeaderTimeout, tlsVerify)
	if err != nil {
		return nil, err
	}
	key := fmt.Sprintf("%d:%s", provider.ID, settingsHash)

	c.mu.Lock()
	defer c.mu.Unlock()
	if previousKey := c.providerKeys[provider.ID]; previousKey != "" && previousKey != key {
		if previous := c.cache[previousKey]; previous != nil {
			previous.transport.CloseIdleConnections()
			delete(c.cache, previousKey)
		}
	}
	if cached := c.cache[key]; cached != nil {
		return cached, nil
	}
	if len(c.cache) >= maxCachedTransports {
		for existingKey, cached := range c.cache {
			cached.transport.CloseIdleConnections()
			delete(c.cache, existingKey)
		}
		clear(c.providerKeys)
	}
	if !tlsVerify {
		slog.Warn("llmproxy: upstream TLS verification disabled", "provider_id", provider.ID)
	}
	dialer := &net.Dialer{Timeout: connectTimeout, KeepAlive: 30 * time.Second}
	transport := &http.Transport{
		Proxy:                 proxyFunction(proxyURL),
		DialContext:           c.policy.dialContext(dialer),
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          64,
		MaxIdleConnsPerHost:   32,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: responseHeaderTimeout,
		DisableCompression:    true,
		TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: !tlsVerify},
	}
	wrapped := &policyRoundTripper{transport: transport, policy: c.policy}
	c.cache[key] = wrapped
	c.providerKeys[provider.ID] = key
	return wrapped, nil
}

type policyRoundTripper struct {
	transport *http.Transport
	policy    *NetworkPolicy
}

func (r *policyRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	if request == nil {
		return nil, errors.New("upstream request is required")
	}
	if request.URL == nil {
		closeRequestBody(request)
		return nil, errors.New("upstream request URL is required")
	}
	if err := r.policy.ValidateURL(request.Context(), request.URL); err != nil {
		closeRequestBody(request)
		return nil, fmt.Errorf("upstream request policy: %w", err)
	}
	return r.transport.RoundTrip(request)
}

func closeRequestBody(request *http.Request) {
	if request.Body != nil {
		_ = request.Body.Close()
	}
}

func (p *NetworkPolicy) dialContext(dialer *net.Dialer) func(context.Context, string, string) (net.Conn, error) {
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, fmt.Errorf("parse dial address: %w", err)
		}
		addresses, err := p.resolveAllowed(ctx, host)
		if err != nil {
			return nil, err
		}
		var dialErr error
		for _, ip := range addresses {
			connection, err := dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
			if err == nil {
				return connection, nil
			}
			dialErr = errors.Join(dialErr, err)
		}
		return nil, fmt.Errorf("dial validated upstream addresses: %w", dialErr)
	}
}

func effectiveTransportSettings(config domain.ProviderTransportConfig) (time.Duration, time.Duration, bool, error) {
	connectTimeout, err := timeoutFromSeconds("connect_timeout_seconds", config.ConnectTimeoutSeconds, defaultConnectTimeout)
	if err != nil {
		return 0, 0, false, err
	}
	responseHeaderTimeout, err := timeoutFromSeconds(
		"response_header_timeout_seconds", config.ResponseHeaderTimeoutSeconds, defaultResponseHeaderTimeout,
	)
	if err != nil {
		return 0, 0, false, err
	}
	tlsVerify := true
	if config.TLSVerify != nil {
		tlsVerify = *config.TLSVerify
	}
	return connectTimeout, responseHeaderTimeout, tlsVerify, nil
}

func timeoutFromSeconds(field string, value *int, fallback time.Duration) (time.Duration, error) {
	if value == nil {
		return fallback, nil
	}
	const maxTimeoutSeconds = int64(1<<63-1) / int64(time.Second)
	seconds := int64(*value)
	if seconds <= 0 || seconds > maxTimeoutSeconds {
		return 0, fmt.Errorf("%s is outside supported duration range", field)
	}
	return time.Duration(seconds) * time.Second, nil
}

func parseTransportProxy(raw *string) (*url.URL, error) {
	if raw == nil {
		return nil, nil
	}
	parsed, err := url.Parse(*raw)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return nil, errors.New("provider proxy URL must be absolute HTTP or HTTPS")
	}
	if parsed.User != nil || parsed.Fragment != "" || parsed.Opaque != "" {
		return nil, errors.New("provider proxy URL must contain no credentials or fragment")
	}
	if (parsed.Path != "" && parsed.Path != "/") || parsed.RawQuery != "" {
		return nil, errors.New("provider proxy URL must contain no path or query")
	}
	return parsed, nil
}

func proxyFunction(proxyURL *url.URL) func(*http.Request) (*url.URL, error) {
	if proxyURL == nil {
		return nil
	}
	return http.ProxyURL(proxyURL)
}

func transportCacheKey(
	config domain.ProviderTransportConfig,
	connectTimeout time.Duration,
	responseHeaderTimeout time.Duration,
	tlsVerify bool,
) (string, error) {
	proxyURL := ""
	if config.ProxyURL != nil {
		proxyURL = *config.ProxyURL
	}
	encoded, err := json.Marshal(struct {
		AuthMode                   domain.ProviderAuthMode `json:"auth_mode"`
		ProxyURL                   string                  `json:"proxy_url"`
		TLSVerify                  bool                    `json:"tls_verify"`
		ConnectTimeoutNanoseconds  int64                   `json:"connect_timeout_nanoseconds"`
		ResponseTimeoutNanoseconds int64                   `json:"response_timeout_nanoseconds"`
	}{
		AuthMode: config.EffectiveAuthMode(), ProxyURL: proxyURL, TLSVerify: tlsVerify,
		ConnectTimeoutNanoseconds:  int64(connectTimeout),
		ResponseTimeoutNanoseconds: int64(responseHeaderTimeout),
	})
	if err != nil {
		return "", fmt.Errorf("marshal transport settings: %w", err)
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}
