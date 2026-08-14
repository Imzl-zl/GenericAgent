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
		// Phase B 托管形态(安全审查项, 方案 §2): 生图响应无既有上限——
		// MaxWorkerRequestBytes 仅限请求体, DisableCompression 大 JSON 原样
		// 传输。20MiB 图片 b64 后 ≈27MB + JSON 开销, 上限 32MiB 留余量。
		// 双闸: ①Content-Length 前置拒绝(同步 JSON 响应恒有); ②chunked
		// (Content-Length=-1) 流式计数, 超限中断连接 fail-closed(审查 W3)。
		// path 用 HasSuffix 判断——provider base URL 可带自定义前缀
		// (/proxy/v1/images/generations), 固定前缀匹配会静默失效。
		if requestContext, ok := proxyContext(response.Request); ok && requestContext.Target != nil {
			path := requestContext.Target.Path
			if isImageGenerationsPath(path) {
				if response.ContentLength > maxImageResponseBytes {
					_ = response.Body.Close()
					slog.Warn("llmproxy: image response exceeds size limit",
						"content_length", response.ContentLength, "limit", maxImageResponseBytes)
					response.Body = io.NopCloser(strings.NewReader(sanitizedImageTooLargeBody))
					response.StatusCode = http.StatusBadGateway
					response.ContentLength = int64(len(sanitizedImageTooLargeBody))
					response.Header.Set("Content-Type", "application/json")
					response.Header.Set("Content-Length", strconv.Itoa(len(sanitizedImageTooLargeBody)))
				} else if response.ContentLength < 0 {
					response.Body = &imageResponseGuard{src: response.Body, limit: maxImageResponseBytes}
				}
			}
		}
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

// maxImageResponseBytes 生图响应体上限(安全审查项): 20MiB 交付上限 b64
// 膨胀 ~1.37 + JSON 结构开销, 32MiB 留余量。双闸: ①同步 JSON 响应按
// Content-Length 前置判定; ②chunked(Content-Length=-1) 由
// imageResponseGuard 流式计数, 超限中断连接 fail-closed(审查 W3)。
const maxImageResponseBytes = 32 * 1024 * 1024

// isImageGenerationsPath 判断上游目标路径是否为生图端点。用 HasSuffix
// 而非固定前缀匹配——provider base URL 可带自定义前缀(如
// https://host/proxy/v1), 此时 target.Path = /proxy/v1/images/generations。
func isImageGenerationsPath(path string) bool {
	return strings.HasSuffix(path, "/images/generations")
}

// errImageResponseTooLarge 是 chunked 生图响应超限时的连接中断信号。
var errImageResponseTooLarge = errors.New("image response exceeds size limit")

// imageResponseGuard 流式计数 body 读取; 累计超过上限即关闭上游 body 并
// 返回 errImageResponseTooLarge——ReverseProxy 拷贝循环中止, 客户端收到
// 截断的 chunked 响应(ConnectionError/JSON 解析失败), 上游超限体不透传。
// GA 侧 20MiB 落盘前检查是交付安全的第二道闸; 本层拦带宽/内存峰值。
type imageResponseGuard struct {
	src    io.ReadCloser
	limit  int64
	read   int64
	closed bool
}

func (g *imageResponseGuard) Read(p []byte) (int, error) {
	if g.closed {
		return 0, io.EOF
	}
	n, err := g.src.Read(p)
	if n > 0 {
		g.read += int64(n)
		if g.read > g.limit {
			_ = g.src.Close()
			g.closed = true
			return 0, errImageResponseTooLarge
		}
	}
	return n, err
}

func (g *imageResponseGuard) Close() error {
	if g.closed {
		return nil
	}
	g.closed = true
	return g.src.Close()
}

const sanitizedImageTooLargeBody = `{"code":"IMAGE_RESPONSE_TOO_LARGE","message":"image response exceeds size limit"}`

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
