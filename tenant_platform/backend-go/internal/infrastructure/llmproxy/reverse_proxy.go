package llmproxy

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
)

var nativeResponseHeaderAllowlist = map[string]struct{}{
	"Anthropic-Request-Id": {},
	"Cache-Control":        {},
	"Content-Encoding":     {},
	"Content-Length":       {},
	"Content-Type":         {},
	"Openai-Request-Id":    {},
	"Request-Id":           {},
	"Retry-After":          {},
	"Vary":                 {},
	"X-Request-Id":         {},
}

const sanitizedUpstreamErrorBody = "{\"code\":\"UPSTREAM_ERROR\",\"message\":\"upstream request failed\"}\n"

// maxUpstreamErrorBodyBytes 是保留的上游错误体读上限(安全截断)。
const maxUpstreamErrorBodyBytes = 1024

// cleanUpstreamErrorBody 清洗上游错误体: 剥离控制字符 + 安全截断(UTF-8
// 安全边界), 空结果返回 ""。错误体通常只含 message/code, 不含凭据。
func cleanUpstreamErrorBody(raw []byte) string {
	s := strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, string(raw))
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	const maxLen = 400
	if len(s) <= maxLen {
		return s
	}
	// 按 rune 截断, 不切断多字节字符。
	truncated := s[:maxLen]
	for len(truncated) > 0 && !utf8.ValidString(truncated) {
		truncated = truncated[:len(truncated)-1]
	}
	return truncated + "..."
}

// redactURL 去掉 query(可能含敏感参数), 只留 scheme/host/path。
func redactURL(u *url.URL) string {
	if u == nil {
		return ""
	}
	cp := *u
	cp.RawQuery = ""
	cp.Fragment = ""
	return cp.String()
}

type proxyRequestContext struct {
	Claims   CapabilityClaims
	Provider domain.LLMProvider
	Target   *url.URL
	RealKey  string
}

type proxyRequestContextKey struct{}

func attachProxyRequestContext(request *http.Request, value *proxyRequestContext) *http.Request {
	ctx := context.WithValue(request.Context(), proxyRequestContextKey{}, value)
	return request.WithContext(ctx)
}

func proxyContext(request *http.Request) (*proxyRequestContext, bool) {
	value, ok := request.Context().Value(proxyRequestContextKey{}).(*proxyRequestContext)
	return value, ok && value != nil
}

func newTransparentReverseProxy(cache *TransportCache) *httputil.ReverseProxy {
	return &httputil.ReverseProxy{
		Rewrite:        rewriteUpstreamRequest,
		Transport:      &routingRoundTripper{cache: cache},
		ModifyResponse: sanitizeUpstreamResponse,
		ErrorHandler:   handleProxyTransportError,
		FlushInterval:  -1,
	}
}

func rewriteUpstreamRequest(request *httputil.ProxyRequest) {
	requestContext, ok := proxyContext(request.Out)
	if !ok || requestContext.Target == nil {
		clear(request.Out.Header)
		request.Out.URL = &url.URL{}
		return
	}
	target := *requestContext.Target
	request.Out.URL = &target
	request.Out.Host = target.Host
	SanitizeAndInjectHeaders(
		request.Out.Header,
		request.In.Header,
		requestContext.Provider,
		requestContext.RealKey,
	)
}

type routingRoundTripper struct {
	cache *TransportCache
}

func (r *routingRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	requestContext, ok := proxyContext(request)
	if !ok {
		closeRequestBody(request)
		return nil, errors.New("proxy request context is missing")
	}
	transport, err := r.cache.RoundTripper(requestContext.Provider)
	if err != nil {
		closeRequestBody(request)
		return nil, err
	}
	return transport.RoundTrip(request)
}

func sanitizeUpstreamResponse(response *http.Response) error {
	rebuildAllowedResponseHeaders(response.Header)
	if response.StatusCode >= http.StatusOK && response.StatusCode < http.StatusMultipleChoices {
		return nil
	}
	// 2026-08-14 架构改进(可观测性): 上游错误体只进服务端日志(清洗截断),
	// 不透传给 GA——上游错误体可能含账号/配额等敏感信息(测试 mock 即含
	// account/quota), 原"不透传"设计是安全边界, 保留; 排障看 llm-proxy 日志。
	var errBody string
	if response.Body != nil {
		if raw, err := io.ReadAll(io.LimitReader(response.Body, maxUpstreamErrorBodyBytes)); err == nil {
			errBody = cleanUpstreamErrorBody(raw)
		}
		_ = response.Body.Close()
	}
	response.Body = io.NopCloser(strings.NewReader(sanitizedUpstreamErrorBody))
	response.ContentLength = int64(len(sanitizedUpstreamErrorBody))
	response.Header.Set("Content-Type", "application/json")
	response.Header.Set("Content-Length", strconv.Itoa(len(sanitizedUpstreamErrorBody)))
	response.Header.Del("Content-Encoding")

	if requestContext, ok := proxyContext(response.Request); ok {
		slog.Warn(
			"llmproxy: upstream returned non-success status",
			"provider_id", requestContext.Provider.ID,
			"provider_revision", requestContext.Provider.Revision,
			"status", response.StatusCode,
			"upstream", redactURL(response.Request.URL),
			"error_body", errBody,
		)
	}
	return nil
}

func rebuildAllowedResponseHeaders(headers http.Header) {
	for name := range headers {
		canonical := http.CanonicalHeaderKey(name)
		if _, allowed := nativeResponseHeaderAllowlist[canonical]; !allowed {
			delete(headers, name)
		}
	}
}

func handleProxyTransportError(w http.ResponseWriter, request *http.Request, err error) {
	if errors.Is(err, context.Canceled) || errors.Is(request.Context().Err(), context.Canceled) {
		return
	}
	status := http.StatusBadGateway
	code := "UPSTREAM_CONNECT_FAILED"
	if errors.Is(err, context.DeadlineExceeded) {
		status = http.StatusGatewayTimeout
		code = "UPSTREAM_TIMEOUT"
	}
	slog.Error("llmproxy: upstream transport failed", "code", code)
	writeError(w, status, code, "upstream request failed")
}
