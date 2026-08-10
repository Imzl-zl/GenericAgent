package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
)

type fakeRuntimeSettingsStore struct {
	mu          sync.Mutex
	windowMS    int
	maxTurns    int
	streamMode  domain.IMStreamingMode
	updateErr   error
	afterUpdate func()
}

func (f *fakeRuntimeSettingsStore) GetIMInboundCoalesceWindowMS(context.Context) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.windowMS, nil
}

func (f *fakeRuntimeSettingsStore) UpdateIMInboundCoalesceWindowMS(_ context.Context, windowMS int, _ int64) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.windowMS = windowMS
	return f.windowMS, nil
}

func (f *fakeRuntimeSettingsStore) GetAgentMaxTurns(context.Context) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.maxTurns, nil
}

func (f *fakeRuntimeSettingsStore) UpdateAgentMaxTurns(_ context.Context, maxTurns int, _ int64) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.maxTurns = maxTurns
	return f.maxTurns, nil
}

func (f *fakeRuntimeSettingsStore) GetIMStreamingMode(context.Context) (domain.IMStreamingMode, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.streamMode == "" {
		return domain.DefaultIMStreamingMode, nil
	}
	return f.streamMode, nil
}

func (f *fakeRuntimeSettingsStore) UpdateIMStreamingMode(_ context.Context, mode domain.IMStreamingMode, _ int64) (domain.IMStreamingMode, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.updateErr != nil {
		return "", f.updateErr
	}
	f.streamMode = mode
	return f.streamMode, nil
}

type fakeIMAggregationRuntime struct{ windowMS int }

func (f *fakeIMAggregationRuntime) ConfigureInboundCoalescing(_ context.Context, windowMS int) error {
	f.windowMS = windowMS
	return nil
}

func TestAdminIMAggregationSettingsGetAndUpdate(t *testing.T) {
	settings := &fakeRuntimeSettingsStore{windowMS: 2500}
	runtime := &fakeIMAggregationRuntime{windowMS: 2500}
	srv, err := NewServer(ServerConfig{
		Service:              dashboardFakeTaskService{},
		Registry:             dashboardFakeRegistry{},
		RuntimeSettings:      settings,
		IMAggregationRuntime: runtime,
		AdminToken:           "test-admin token",
		AdminUserID:          9,
	})
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/v1/admin/settings/im-aggregation", nil)
	req.Header.Set("X-Platform-Admin-Token", "test-admin token")
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("get status=%d body=%s", rr.Code, rr.Body.String())
	}
	var got imAggregationSettingsReply
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.WindowMS != 2500 {
		t.Fatalf("window=%d", got.WindowMS)
	}

	body, _ := json.Marshal(updateIMAggregationSettingsBody{WindowMS: 1800})
	req = httptest.NewRequest(http.MethodPut, "/v1/admin/settings/im-aggregation", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Platform-Admin-Token", "test-admin token")
	rr = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("put status=%d body=%s", rr.Code, rr.Body.String())
	}
	if settings.windowMS != 1800 || runtime.windowMS != 1800 {
		t.Fatalf("settings=%d runtime=%d", settings.windowMS, runtime.windowMS)
	}
}

func TestAdminIMAggregationSettingsRejectInvalidWindow(t *testing.T) {
	srv, err := NewServer(ServerConfig{
		Service:         dashboardFakeTaskService{},
		Registry:        dashboardFakeRegistry{},
		RuntimeSettings: &fakeRuntimeSettingsStore{windowMS: 2500, maxTurns: 80},
		AdminToken:      "test-admin token",
		AdminUserID:     9,
	})
	if err != nil {
		t.Fatal(err)
	}

	body := []byte(`{"window_ms": 6000}`)
	req := httptest.NewRequest(http.MethodPut, "/v1/admin/settings/im-aggregation", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Platform-Admin-Token", "test-admin token")
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
}

func TestAdminAgentRuntimeSettingsGetAndUpdate(t *testing.T) {
	settings := &fakeRuntimeSettingsStore{windowMS: 2500, maxTurns: 80}
	srv, err := NewServer(ServerConfig{
		Service:         dashboardFakeTaskService{},
		Registry:        dashboardFakeRegistry{},
		RuntimeSettings: settings,
		AdminToken:      "test-admin token",
		AdminUserID:     9,
	})
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/v1/admin/settings/agent-runtime", nil)
	req.Header.Set("X-Platform-Admin-Token", "test-admin token")
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("get status=%d body=%s", rr.Code, rr.Body.String())
	}
	var got map[string]int
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got["max_turns"] != 80 {
		t.Fatalf("max_turns=%d", got["max_turns"])
	}

	req = httptest.NewRequest(http.MethodPut, "/v1/admin/settings/agent-runtime", bytes.NewBufferString(`{"max_turns":120}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Platform-Admin-Token", "test-admin token")
	rr = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("put status=%d body=%s", rr.Code, rr.Body.String())
	}
	if settings.maxTurns != 120 {
		t.Fatalf("stored max turns=%d", settings.maxTurns)
	}
}

func TestAdminRuntimeProfileConcurrentReadAndUpdate(t *testing.T) {
	settings := &fakeRuntimeSettingsStore{windowMS: 2500, maxTurns: 80}
	srv, err := NewServer(ServerConfig{
		Service:         dashboardFakeTaskService{},
		Registry:        dashboardFakeRegistry{},
		RuntimeSettings: settings,
		RuntimeProfile:  RuntimeProfile{AgentMaxTurns: 80},
		AdminToken:      "test-admin token",
		AdminUserID:     9,
	})
	if err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(2)
		go func(value int) {
			defer wg.Done()
			body, _ := json.Marshal(map[string]int{"max_turns": value})
			req := httptest.NewRequest(http.MethodPut, "/v1/admin/settings/agent-runtime", bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("X-Platform-Admin-Token", "test-admin token")
			srv.Handler().ServeHTTP(httptest.NewRecorder(), req)
		}(80 + i)
		go func() {
			defer wg.Done()
			req := httptest.NewRequest(http.MethodGet, "/v1/admin/dashboard/stats", nil)
			req.Header.Set("X-Platform-Admin-Token", "test-admin token")
			srv.Handler().ServeHTTP(httptest.NewRecorder(), req)
		}()
	}
	wg.Wait()
}

func TestAdminAgentRuntimeSettingsRejectInvalidMaxTurns(t *testing.T) {
	srv, err := NewServer(ServerConfig{
		Service:         dashboardFakeTaskService{},
		Registry:        dashboardFakeRegistry{},
		RuntimeSettings: &fakeRuntimeSettingsStore{windowMS: 2500, maxTurns: 80},
		AdminToken:      "test-admin token",
		AdminUserID:     9,
	})
	if err != nil {
		t.Fatal(err)
	}

	for _, value := range []int{9, 501} {
		body, _ := json.Marshal(map[string]int{"max_turns": value})
		req := httptest.NewRequest(http.MethodPut, "/v1/admin/settings/agent-runtime", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Platform-Admin-Token", "test-admin token")
		rr := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rr, req)
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("max_turns=%d status=%d body=%s", value, rr.Code, rr.Body.String())
		}
	}
}

func TestAdminIMStreamingSettingsGetAndUpdate(t *testing.T) {
	settings := &fakeRuntimeSettingsStore{windowMS: 2500, maxTurns: 80}
	srv, err := NewServer(ServerConfig{
		Service:         dashboardFakeTaskService{},
		Registry:        dashboardFakeRegistry{},
		RuntimeSettings: settings,
		AdminToken:      "test-admin token",
		AdminUserID:     9,
	})
	if err != nil {
		t.Fatal(err)
	}

	// 默认 streaming(设计: 私聊默认开)。
	req := httptest.NewRequest(http.MethodGet, "/v1/admin/settings/im-streaming", nil)
	req.Header.Set("X-Platform-Admin-Token", "test-admin token")
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("get status=%d body=%s", rr.Code, rr.Body.String())
	}
	var got map[string]string
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got["mode"] != string(domain.IMStreamingStreaming) {
		t.Fatalf("default mode=%q, want streaming", got["mode"])
	}

	// 更新为 final_only。
	req = httptest.NewRequest(http.MethodPut, "/v1/admin/settings/im-streaming", bytes.NewBufferString(`{"mode":"final_only"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Platform-Admin-Token", "test-admin token")
	rr = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("put status=%d body=%s", rr.Code, rr.Body.String())
	}
	if settings.streamMode != domain.IMStreamingFinalOnly {
		t.Fatalf("stored mode=%q, want final_only", settings.streamMode)
	}
}

func TestAdminIMStreamingSettingsRejectInvalidMode(t *testing.T) {
	srv, err := NewServer(ServerConfig{
		Service:         dashboardFakeTaskService{},
		Registry:        dashboardFakeRegistry{},
		RuntimeSettings: &fakeRuntimeSettingsStore{windowMS: 2500, maxTurns: 80},
		AdminToken:      "test-admin token",
		AdminUserID:     9,
	})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPut, "/v1/admin/settings/im-streaming", bytes.NewBufferString(`{"mode":"banana"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Platform-Admin-Token", "test-admin token")
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("invalid mode status=%d, want 400", rr.Code)
	}
}
