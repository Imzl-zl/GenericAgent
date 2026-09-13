package llmproxy

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"regexp"
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

// clientActionableUpstreamError 判定上游错误体是否属于"客户端可自行修正的参数/
// 校验类"错误——只有这类文本才被透传给 GA(见 clientActionableErrorBody)。
//
// 背景(2026-09-13 实测): image_gen 的参数协商靠错误文本点名参数("quality is
// not supported by text image queue")才能裁剪重试。本代理默认清洗上游错误体
// (安全边界: 上游体可能含账号/配额等敏感信息, 见 2026-08-14 注释与
// TestReverseProxySanitizesUpstreamErrorsWithoutReplay), 结果 GA 只看到
// UPSTREAM_ERROR → 协商永远不生效 → 模型只能盲重试, 实测 4 次后对用户说
// "图片服务不可用"。折中: **白名单**(而非黑名单)只放行"某参数不被支持/取值
// 非法"这类客户端能自行修正的文本, 其余(账号/配额/鉴权/内部错误)一律维持
// 清洗后的通用错误体。
var clientActionableUpstreamError = regexp.MustCompile(
	`(?i)is not supported|not supported by|unsupported|unknown parameter|` +
		`unrecognized (request )?(argument|parameter|field)|invalid parameter|` +
		`invalid_request|invalid value|must be one of|not allowed`)

// sensitiveUpstreamField 是上游体里出现就一律不透传的敏感字段名(纵深防御:
// 即便命中白名单话术, 也不把整段上游体发出去)。
var sensitiveUpstreamField = regexp.MustCompile(
	`(?i)(account|quota|balance|credit|api[-_]?key|secret|bearer|authorization)`)

// clientActionableErrorBody 从上游 4xx 错误体里**提取参数类错误消息并重建**
// 成干净的 {code,message} 体透传。返回 "" 表示不透传(维持通用清洗体)。
//
// 关键设计: **不原样转发**上游体, 而是只取 message 字段重建——这样上游体里
// 与 message 并列的账号/配额/凭据字段(安全审查担心的泄露面)根本不会出去,
// 同时 image_gen 凭文本点名参数自愈所需的信息完整保留。
func clientActionableErrorBody(errBody string) string {
	if errBody == "" {
		return ""
	}
	msg := ""
	var parsed map[string]any
	if json.Unmarshal([]byte(errBody), &parsed) == nil {
		if e, ok := parsed["error"].(map[string]any); ok {
			msg, _ = e["message"].(string)
		}
		if msg == "" {
			msg, _ = parsed["message"].(string)
		}
	} else {
		// 非 JSON(纯文本错误): 整段就是消息
		msg = errBody
	}
	msg = strings.TrimSpace(msg)
	if msg == "" || !clientActionableUpstreamError.MatchString(msg) {
		return "" // 非参数类 → 不透传
	}
	if sensitiveUpstreamField.MatchString(msg) {
		return "" // 消息里都带敏感词 → 宁可不透传
	}
	msg = cleanUpstreamErrorBody([]byte(msg))
	if msg == "" {
		return ""
	}
	out, err := json.Marshal(map[string]string{"code": "UPSTREAM_ERROR", "message": msg})
	if err != nil {
		return ""
	}
	return string(out)
}

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
	//
	// 2026-09-13 窄口径例外(见 clientActionableUpstreamError 注释): **生图端点
	// 的 4xx 参数/校验类错误**必须透传, 否则 image_gen 的错误文本驱动协商在
	// 托管形态下永远拿不到参数名(实测导致模型盲重试 4 次后放弃)。安全边界
	// 不破: 仅生图端点 + 仅 4xx + 仅命中白名单话术; 5xx/chat/账号配额类
	// 仍然清洗, 且有测试钉住两面。
	var errBody string
	if response.Body != nil {
		if raw, err := io.ReadAll(io.LimitReader(response.Body, maxUpstreamErrorBodyBytes)); err == nil {
			errBody = cleanUpstreamErrorBody(raw)
		}
		_ = response.Body.Close()
	}
	forwardedBody := sanitizedUpstreamErrorBody
	if isImageGenerationsPath(response.Request.URL.Path) &&
		response.StatusCode >= 400 && response.StatusCode < 500 {
		if actionable := clientActionableErrorBody(errBody); actionable != "" {
			forwardedBody = actionable
		}
	}
	response.Body = io.NopCloser(strings.NewReader(forwardedBody))
	response.ContentLength = int64(len(forwardedBody))
	response.Header.Set("Content-Type", "application/json")
	response.Header.Set("Content-Length", strconv.Itoa(len(forwardedBody)))
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
