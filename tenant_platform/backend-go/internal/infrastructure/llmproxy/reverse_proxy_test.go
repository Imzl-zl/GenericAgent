package llmproxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
)

type reverseProxyHarness struct {
	server         *Server
	proxy          *httptest.Server
	upstream       *httptest.Server
	providerSource *fakeProviderSource
}

func newReverseProxyHarness(
	t *testing.T,
	providerType domain.LLMProviderType,
	upstreamHandler http.Handler,
) *reverseProxyHarness {
	t.Helper()
	upstream := httptest.NewServer(upstreamHandler)
	model := "gpt-test"
	if providerType == domain.ProviderNativeClaude {
		model = "claude-test"
	}
	providerSource := &fakeProviderSource{
		provider: testProvider(providerType, upstream.URL, model, testUpstreamKey),
	}
	server, err := NewServer(Config{
		Listen:               "127.0.0.1:0",
		SigningKey:           []byte(testSigningKey),
		TokenTTL:             time.Hour,
		ProviderSource:       providerSource,
		Cipher:               &fakeCipher{wantVersion: 1},
		Revocations:          &fakeRevocationSource{revoked: make(map[[32]byte]bool)},
		UsageCounter:         &fakeUsageCounter{maxCalls: 1000},
		AllowedUpstreamCIDRs: []string{"127.0.0.0/8", "::1/128"},
		AllowedHTTPHosts:     []string{upstream.Listener.Addr().String()},
	})
	if err != nil {
		upstream.Close()
		t.Fatalf("NewServer: %v", err)
	}
	proxy := httptest.NewServer(server.Handler())
	t.Cleanup(proxy.Close)
	t.Cleanup(upstream.Close)
	return &reverseProxyHarness{
		server: server, proxy: proxy, upstream: upstream, providerSource: providerSource,
	}
}

func (h *reverseProxyHarness) issueToken(t *testing.T, spec CapabilitySpec) string {
	t.Helper()
	issuer, err := NewIssuer([]byte(testSigningKey), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	token, _, err := issuer.Issue(spec)
	if err != nil {
		t.Fatal(err)
	}
	return token
}

func (h *reverseProxyHarness) defaultToken(t *testing.T) string {
	t.Helper()
	provider := h.providerSource.provider
	return h.issueToken(t, CapabilitySpec{
		SessionKey:       "personal:42",
		ProviderID:       provider.ID,
		ProviderRevision: provider.Revision,
		ProviderType:     provider.ProviderType,
		Model:            provider.Model,
		PolicyVersion:    "p1",
		TaskID:           "task-1",
		RunnerGeneration: 1,
		Operation:        "llm.chat",
		Budget:           `{"max_turns":8}`,
	})
}

func proxyRequest(
	t *testing.T,
	ctx context.Context,
	client *http.Client,
	method string,
	url string,
	token string,
	body string,
) *http.Response {
	t.Helper()
	request, err := http.NewRequestWithContext(ctx, method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+token)
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	return response
}

func TestReverseProxyRoutesNativeProtocolsAndPreservesMetadata(t *testing.T) {
	tests := []struct {
		name             string
		providerType     domain.LLMProviderType
		authMode         domain.ProviderAuthMode
		inboundPath      string
		wantUpstreamPath string
		body             string
		wantAuthHeader   string
		wantAPIKey       string
	}{
		{
			name: "oai chat", providerType: domain.ProviderNativeOAI,
			inboundPath:      "/v1/chat/completions?trace=true",
			wantUpstreamPath: "/v1/chat/completions?trace=true",
			body:             `{"model":"gpt-test","messages":[]}`,
			wantAuthHeader:   "Bearer " + testUpstreamKey,
		},
		{
			name: "oai responses", providerType: domain.ProviderNativeOAI,
			inboundPath:      "/v1/responses",
			wantUpstreamPath: "/v1/responses",
			body:             `{"model":"gpt-test","input":"hello"}`,
			wantAuthHeader:   "Bearer " + testUpstreamKey,
		},
		{
			name: "claude x-api-key", providerType: domain.ProviderNativeClaude,
			authMode:         domain.ProviderAuthXAPIKey,
			inboundPath:      "/v1/messages",
			wantUpstreamPath: "/v1/messages",
			body:             `{"model":"claude-test","messages":[]}`,
			wantAPIKey:       testUpstreamKey,
		},
		{
			name: "claude bearer", providerType: domain.ProviderNativeClaude,
			authMode:         domain.ProviderAuthBearer,
			inboundPath:      "/v1/messages",
			wantUpstreamPath: "/v1/messages",
			body:             `{"model":"claude-test","messages":[]}`,
			wantAuthHeader:   "Bearer " + testUpstreamKey,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if got := r.URL.RequestURI(); got != test.wantUpstreamPath {
					t.Errorf("upstream URI = %q, want %q", got, test.wantUpstreamPath)
				}
				body, err := io.ReadAll(r.Body)
				if err != nil {
					t.Errorf("read upstream body: %v", err)
				}
				if string(body) != test.body {
					t.Errorf("upstream body = %q, want %q", body, test.body)
				}
				if got := r.Header.Get("Authorization"); got != test.wantAuthHeader {
					t.Errorf("Authorization = %q, want %q", got, test.wantAuthHeader)
				}
				if got := r.Header.Get("X-Api-Key"); got != test.wantAPIKey {
					t.Errorf("X-Api-Key = %q, want %q", got, test.wantAPIKey)
				}
				for name, want := range map[string]string{
					"User-Agent":          "ga-native-test",
					"Openai-Beta":         "responses=v1",
					"X-Stainless-Runtime": "node",
				} {
					if got := r.Header.Get(name); got != want {
						t.Errorf("%s = %q, want %q", name, got, want)
					}
				}
				if r.Header.Get("Cookie") != "" || r.Header.Get("X-Forwarded-For") != "" {
					t.Errorf("unsafe headers reached upstream: %v", r.Header)
				}
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Cache-Control", "no-cache")
				w.Header().Set("X-Request-Id", "request-123")
				w.Header().Set("Set-Cookie", "provider_session=secret")
				w.Header().Set("Server", "provider-internal")
				w.Header().Set("WWW-Authenticate", "provider-account")
				w.Header().Set("X-Provider-Secret", "must-strip")
				_, _ = w.Write([]byte(`{"ok":true}`))
			})
			harness := newReverseProxyHarness(t, test.providerType, upstream)
			harness.providerSource.provider.TransportConfig.AuthMode = test.authMode
			token := harness.defaultToken(t)
			request, err := http.NewRequest(http.MethodPost, harness.proxy.URL+test.inboundPath, strings.NewReader(test.body))
			if err != nil {
				t.Fatal(err)
			}
			request.Header.Set("Authorization", "Bearer "+token)
			request.Header.Set("X-Api-Key", "worker-supplied-key")
			request.Header.Set("Cookie", "capability-session")
			request.Header.Set("X-Forwarded-For", "127.0.0.1")
			request.Header.Set("User-Agent", "ga-native-test")
			request.Header.Set("OpenAI-Beta", "responses=v1")
			request.Header.Set("X-Stainless-Runtime", "node")
			response, err := harness.proxy.Client().Do(request)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			body, err := io.ReadAll(response.Body)
			if err != nil {
				t.Fatal(err)
			}
			if response.StatusCode != http.StatusOK || string(body) != `{"ok":true}` {
				t.Fatalf("status=%d body=%s", response.StatusCode, body)
			}
			for name, want := range map[string]string{
				"Content-Type":  "application/json",
				"Cache-Control": "no-cache",
				"X-Request-Id":  "request-123",
			} {
				if got := response.Header.Get(name); got != want {
					t.Errorf("response %s = %q, want %q", name, got, want)
				}
			}
			for _, name := range []string{"Set-Cookie", "Server", "WWW-Authenticate", "X-Provider-Secret"} {
				if got := response.Header.Get(name); got != "" {
					t.Errorf("unsafe response header %s = %q", name, got)
				}
			}
		})
	}
}

func TestSSEFirstEventArrivesBeforeUpstreamCompletes(t *testing.T) {
	tests := []struct {
		name         string
		providerType domain.LLMProviderType
		path         string
		body         string
	}{
		{
			name: "oai chat", providerType: domain.ProviderNativeOAI,
			path: "/v1/chat/completions", body: `{"model":"gpt-test","stream":true,"messages":[]}`,
		},
		{
			name: "oai responses", providerType: domain.ProviderNativeOAI,
			path: "/v1/responses", body: `{"model":"gpt-test","stream":true,"input":"hello"}`,
		},
		{
			name: "claude messages", providerType: domain.ProviderNativeClaude,
			path: "/v1/messages", body: `{"model":"claude-test","stream":true,"messages":[]}`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assertSSEStreamsBeforeCompletion(t, test.providerType, test.path, test.body)
		})
	}
}

func assertSSEStreamsBeforeCompletion(
	t *testing.T,
	providerType domain.LLMProviderType,
	path string,
	body string,
) {
	t.Helper()
	firstWritten := make(chan struct{})
	release := make(chan struct{})
	upstream := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Error("fixture response does not support flushing")
			return
		}
		_, _ = io.WriteString(w, "data: first\n\n")
		flusher.Flush()
		close(firstWritten)
		<-release
		_, _ = io.WriteString(w, "data: done\n\n")
		flusher.Flush()
	})
	harness := newReverseProxyHarness(t, providerType, upstream)
	token := harness.defaultToken(t)
	event := make(chan string, 1)
	go func() {
		response, err := proxyRequestNoFail(
			context.Background(), harness.proxy.Client(), harness.proxy.URL+path,
			token, body,
		)
		if err != nil {
			event <- "error: " + err.Error()
			return
		}
		defer response.Body.Close()
		line, err := bufio.NewReader(response.Body).ReadString('\n')
		if err != nil {
			event <- "error: " + err.Error()
			return
		}
		event <- line
	}()

	select {
	case <-firstWritten:
	case <-time.After(2 * time.Second):
		close(release)
		t.Fatal("upstream did not write first SSE event")
	}
	var got string
	streamedEarly := false
	select {
	case got = <-event:
		streamedEarly = true
	case <-time.After(time.Second):
	}
	close(release)
	if !streamedEarly {
		select {
		case got = <-event:
		case <-time.After(2 * time.Second):
			t.Fatal("proxy did not return after upstream completion")
		}
		t.Fatalf("first SSE event was buffered until upstream completion: %q", got)
	}
	if got != "data: first\n" {
		t.Fatalf("first SSE line = %q", got)
	}
}

func proxyRequestNoFail(
	ctx context.Context,
	client *http.Client,
	url string,
	token string,
	body string,
) (*http.Response, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Authorization", "Bearer "+token)
	return client.Do(request)
}

func TestReverseProxyClientCancellationReachesUpstream(t *testing.T) {
	entered := make(chan struct{})
	cancelled := make(chan struct{})
	forceRelease := make(chan struct{})
	defer close(forceRelease)
	upstream := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		_ = r.Body.Close()
		close(entered)
		select {
		case <-r.Context().Done():
			close(cancelled)
		case <-forceRelease:
		}
	})
	harness := newReverseProxyHarness(t, domain.ProviderNativeOAI, upstream)
	ctx, cancel := context.WithCancel(context.Background())
	token := harness.defaultToken(t)
	requestDone := make(chan error, 1)
	go func() {
		response, err := proxyRequestNoFail(
			ctx, harness.proxy.Client(), harness.proxy.URL+"/v1/chat/completions",
			token, `{"model":"gpt-test","messages":[]}`,
		)
		if response != nil {
			response.Body.Close()
		}
		requestDone <- err
	}()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("request did not reach upstream")
	}
	cancel()
	select {
	case <-cancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("client cancellation did not reach upstream context")
	}
	select {
	case err := <-requestDone:
		if err == nil {
			t.Fatal("cancelled client request unexpectedly succeeded")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancelled client request did not return")
	}
}

func TestCapabilityBindingRejectsStaleOrMismatchedProvider(t *testing.T) {
	tests := []struct {
		name       string
		mutate     func(*domain.LLMProvider, *CapabilitySpec)
		body       string
		wantStatus int
		wantCode   string
	}{
		{
			name:       "provider not found",
			mutate:     func(_ *domain.LLMProvider, spec *CapabilitySpec) { spec.ProviderID = 2 },
			body:       `{"model":"gpt-test"}`,
			wantStatus: http.StatusNotFound, wantCode: "PROVIDER_NOT_FOUND",
		},
		{
			name:       "provider disabled",
			mutate:     func(provider *domain.LLMProvider, _ *CapabilitySpec) { provider.State = "disabled" },
			body:       `{"model":"gpt-test"}`,
			wantStatus: http.StatusConflict, wantCode: "PROVIDER_DISABLED",
		},
		{
			name:       "revision mismatch",
			mutate:     func(provider *domain.LLMProvider, _ *CapabilitySpec) { provider.Revision++ },
			body:       `{"model":"gpt-test"}`,
			wantStatus: http.StatusConflict, wantCode: "PROVIDER_REVISION_MISMATCH",
		},
		{
			name:       "claim type mismatch",
			mutate:     func(_ *domain.LLMProvider, spec *CapabilitySpec) { spec.ProviderType = domain.ProviderNativeClaude },
			body:       `{"model":"gpt-test"}`,
			wantStatus: http.StatusConflict, wantCode: "PROVIDER_TYPE_MISMATCH",
		},
		{
			name:       "claim model mismatch",
			mutate:     func(_ *domain.LLMProvider, spec *CapabilitySpec) { spec.Model = "stale-model" },
			body:       `{"model":"stale-model"}`,
			wantStatus: http.StatusConflict, wantCode: "MODEL_MISMATCH",
		},
		{
			name:       "body model mismatch",
			mutate:     func(_ *domain.LLMProvider, _ *CapabilitySpec) {},
			body:       `{"model":"other-model"}`,
			wantStatus: http.StatusConflict, wantCode: "MODEL_MISMATCH",
		},
		{
			name:       "ambiguous duplicate model",
			mutate:     func(_ *domain.LLMProvider, _ *CapabilitySpec) {},
			body:       `{"model":"other-model","model":"gpt-test"}`,
			wantStatus: http.StatusBadRequest, wantCode: "ROUTE_NOT_ALLOWED",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var upstreamHits atomic.Int32
			harness := newReverseProxyHarness(t, domain.ProviderNativeOAI, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				upstreamHits.Add(1)
				_, _ = w.Write([]byte(`{}`))
			}))
			provider := &harness.providerSource.provider
			spec := CapabilitySpec{
				SessionKey: "personal:42", ProviderID: provider.ID, ProviderRevision: provider.Revision,
				ProviderType: provider.ProviderType, Model: provider.Model, PolicyVersion: "p1",
				TaskID: "task-1", RunnerGeneration: 1,
				Operation: "llm.chat",
				Budget:    `{"max_turns":8}`,
			}
			test.mutate(provider, &spec)
			token := harness.issueToken(t, spec)
			response := proxyRequest(
				t, context.Background(), harness.proxy.Client(), http.MethodPost,
				harness.proxy.URL+"/v1/chat/completions", token, test.body,
			)
			defer response.Body.Close()
			if response.StatusCode != test.wantStatus {
				body, _ := io.ReadAll(response.Body)
				t.Fatalf("status=%d want=%d body=%s", response.StatusCode, test.wantStatus, body)
			}
			var payload struct {
				Code string `json:"code"`
			}
			if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
				t.Fatal(err)
			}
			if payload.Code != test.wantCode {
				t.Fatalf("code=%q want=%q", payload.Code, test.wantCode)
			}
			if upstreamHits.Load() != 0 {
				t.Fatalf("mismatched capability reached upstream %d times", upstreamHits.Load())
			}
		})
	}
}

func TestCapabilityBindingAcceptsGAClaudeOneMillionContextModel(t *testing.T) {
	for _, test := range []struct {
		name       string
		configured string
		outbound   string
	}{
		{name: "lowercase marker", configured: "claude-sonnet-4[1m]", outbound: "claude-sonnet-4"},
		{name: "uppercase marker", configured: "claude-sonnet-4[1M]", outbound: "claude-sonnet-4"},
	} {
		t.Run(test.name, func(t *testing.T) {
			harness := newReverseProxyHarness(t, domain.ProviderNativeClaude, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(`{}`))
			}))
			provider := &harness.providerSource.provider
			provider.Model = test.configured
			token := harness.issueToken(t, CapabilitySpec{
				SessionKey: "personal:42", ProviderID: provider.ID, ProviderRevision: provider.Revision,
				ProviderType: provider.ProviderType, Model: provider.Model, PolicyVersion: "p1",
				TaskID: "task-1", RunnerGeneration: 1,
				Operation: "llm.chat",
				Budget:    `{"max_turns":8}`,
			})
			response := proxyRequest(
				t, context.Background(), harness.proxy.Client(), http.MethodPost,
				harness.proxy.URL+"/v1/messages", token, `{"model":"`+test.outbound+`","messages":[]}`,
			)
			defer response.Body.Close()
			if response.StatusCode != http.StatusOK {
				body, _ := io.ReadAll(response.Body)
				t.Fatalf("status=%d body=%s", response.StatusCode, body)
			}
		})
	}
}

func TestReverseProxySanitizesUpstreamErrorsWithoutReplay(t *testing.T) {
	for _, status := range []int{http.StatusTooManyRequests, http.StatusInternalServerError} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var hits atomic.Int32
			harness := newReverseProxyHarness(t, domain.ProviderNativeOAI, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				hits.Add(1)
				w.Header().Set("Retry-After", "12")
				w.Header().Set("OpenAI-Request-Id", "upstream-request")
				w.Header().Set("Set-Cookie", "provider-secret")
				w.WriteHeader(status)
				_, _ = w.Write([]byte(`{"account":"secret@example.com","quota":"private"}`))
			}))
			response := proxyRequest(
				t, context.Background(), harness.proxy.Client(), http.MethodPost,
				harness.proxy.URL+"/v1/chat/completions", harness.defaultToken(t),
				`{"model":"gpt-test","messages":[]}`,
			)
			defer response.Body.Close()
			body, err := io.ReadAll(response.Body)
			if err != nil {
				t.Fatal(err)
			}
			if response.StatusCode != status {
				t.Fatalf("status=%d want=%d body=%s", response.StatusCode, status, body)
			}
			if !strings.Contains(string(body), "UPSTREAM_ERROR") || strings.Contains(string(body), "secret@example.com") {
				t.Fatalf("unsanitized upstream body: %s", body)
			}
			if response.Header.Get("Retry-After") != "12" || response.Header.Get("OpenAI-Request-Id") != "upstream-request" {
				t.Fatalf("allowed error metadata missing: %v", response.Header)
			}
			if response.Header.Get("Set-Cookie") != "" {
				t.Fatalf("unsafe response cookie forwarded: %v", response.Header.Values("Set-Cookie"))
			}
			if hits.Load() != 1 {
				t.Fatalf("upstream request count=%d want=1", hits.Load())
			}
		})
	}
}

func TestReverseProxyRejectsBodyAboveLimitBeforeUpstream(t *testing.T) {
	var hits atomic.Int32
	harness := newReverseProxyHarness(t, domain.ProviderNativeOAI, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte(`{}`))
	}))
	body := `{"model":"gpt-test","padding":"` + strings.Repeat("x", MaxWorkerRequestBytes) + `"}`
	response := proxyRequest(
		t, context.Background(), harness.proxy.Client(), http.MethodPost,
		harness.proxy.URL+"/v1/chat/completions", harness.defaultToken(t), body,
	)
	defer response.Body.Close()
	if response.StatusCode != http.StatusRequestEntityTooLarge {
		payload, _ := io.ReadAll(response.Body)
		t.Fatalf("status=%d want=413 body=%s", response.StatusCode, payload)
	}
	if hits.Load() != 0 {
		t.Fatalf("oversized request reached upstream %d times", hits.Load())
	}
}

// TestSanitizeImageResponseOverLimit: 生图响应超 32MiB 上限 → 502
// IMAGE_RESPONSE_TOO_LARGE(安全审查项: 响应体无既有上限)。
func TestSanitizeImageResponseOverLimit(t *testing.T) {
	resp := &http.Response{
		StatusCode:    http.StatusOK,
		ContentLength: maxImageResponseBytes + 1,
		Body:          io.NopCloser(strings.NewReader("huge")),
		Header:        make(http.Header),
		Request:       &http.Request{},
	}
	ctx := context.WithValue(resp.Request.Context(), proxyRequestContextKey{}, &proxyRequestContext{
		Target: &url.URL{Path: "/v1/images/generations"},
	})
	resp.Request = resp.Request.WithContext(ctx)
	if err := sanitizeUpstreamResponse(resp); err != nil {
		t.Fatalf("sanitizeUpstreamResponse: %v", err)
	}
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "IMAGE_RESPONSE_TOO_LARGE") {
		t.Fatalf("body = %q", string(body))
	}
}

// TestSanitizeImageResponseChunkedCounting: chunked(Content-Length=-1)
// 生图响应由 imageResponseGuard 流式计数——超限时 Read 返回中断错误,
// 上游超限体不透传(审查 W3 双闸②)。
func TestSanitizeImageResponseChunkedCounting(t *testing.T) {
	oversized := make([]byte, maxImageResponseBytes+4096)
	resp := &http.Response{
		StatusCode:    http.StatusOK,
		ContentLength: -1,
		Body:          io.NopCloser(bytes.NewReader(oversized)),
		Header:        make(http.Header),
		Request:       &http.Request{},
	}
	ctx := context.WithValue(resp.Request.Context(), proxyRequestContextKey{}, &proxyRequestContext{
		Target: &url.URL{Path: "/v1/images/generations"},
	})
	resp.Request = resp.Request.WithContext(ctx)
	if err := sanitizeUpstreamResponse(resp); err != nil {
		t.Fatalf("sanitizeUpstreamResponse: %v", err)
	}
	guard, ok := resp.Body.(*imageResponseGuard)
	if !ok {
		t.Fatalf("body = %T, want *imageResponseGuard", resp.Body)
	}
	buf := make([]byte, 64*1024)
	total := 0
	var readErr error
	for {
		n, err := guard.Read(buf)
		total += n
		if err != nil {
			readErr = err
			break
		}
	}
	if readErr != errImageResponseTooLarge {
		t.Fatalf("read err = %v, want errImageResponseTooLarge (total=%d)", readErr, total)
	}
	if total > int(maxImageResponseBytes)+64*1024 {
		t.Fatalf("total read = %d, exceeded limit", total)
	}
}

// TestSanitizeImageResponseCustomBasePrefix: provider base URL 带自定义
// 前缀时(/proxy/v1/images/generations)上限仍生效——HasSuffix 判断(审查 W3)。
func TestSanitizeImageResponseCustomBasePrefix(t *testing.T) {
	resp := &http.Response{
		StatusCode:    http.StatusOK,
		ContentLength: maxImageResponseBytes + 1,
		Body:          io.NopCloser(strings.NewReader("huge")),
		Header:        make(http.Header),
		Request:       &http.Request{},
	}
	ctx := context.WithValue(resp.Request.Context(), proxyRequestContextKey{}, &proxyRequestContext{
		Target: &url.URL{Path: "/proxy/v1/images/generations"},
	})
	resp.Request = resp.Request.WithContext(ctx)
	if err := sanitizeUpstreamResponse(resp); err != nil {
		t.Fatalf("sanitizeUpstreamResponse: %v", err)
	}
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "IMAGE_RESPONSE_TOO_LARGE") {
		t.Fatalf("body = %q", string(body))
	}
}

// TestSanitizeChatResponseIgnoresLimit: 非生图路由不受 32MiB 上限约束。
func TestSanitizeChatResponseIgnoresLimit(t *testing.T) {
	resp := &http.Response{
		StatusCode:    http.StatusOK,
		ContentLength: maxImageResponseBytes + 1,
		Body:          io.NopCloser(strings.NewReader("ok")),
		Header:        make(http.Header),
		Request:       &http.Request{},
	}
	ctx := context.WithValue(resp.Request.Context(), proxyRequestContextKey{}, &proxyRequestContext{
		Target: &url.URL{Path: "/v1/chat/completions"},
	})
	resp.Request = resp.Request.WithContext(ctx)
	if err := sanitizeUpstreamResponse(resp); err != nil {
		t.Fatalf("sanitizeUpstreamResponse: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
}
