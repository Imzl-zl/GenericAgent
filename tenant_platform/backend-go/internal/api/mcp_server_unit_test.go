package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
)

type memoryMCPStore struct {
	server    domain.MCPServer
	createErr error
	updateErr error
	stateErr  error
	deleteErr error
}

func (s *memoryMCPStore) CreateMCPServer(_ context.Context, input domain.MCPServerCreate) (domain.MCPServer, error) {
	if s.createErr != nil {
		return domain.MCPServer{}, s.createErr
	}
	// 与真实 postgres store 同款: 校验唯一实现在 domain(被 store 调用)。
	if err := domain.ValidateMCPServerInput(input); err != nil {
		return domain.MCPServer{}, err
	}
	s.server = domain.MCPServer{
		ID: 1, ServerKey: input.ServerKey, Name: input.Name, URL: input.URL,
		TimeoutSeconds: input.TimeoutSeconds, Headers: input.Headers, Enabled: false, Revision: 1,
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	return s.server, nil
}

func (s *memoryMCPStore) ListMCPServers(context.Context) ([]domain.MCPServer, error) {
	if s.server.ID == 0 {
		return []domain.MCPServer{}, nil
	}
	return []domain.MCPServer{s.server}, nil
}

func (s *memoryMCPStore) UpdateMCPServer(_ context.Context, id int64, input domain.MCPServerUpdate) (domain.MCPServer, error) {
	if s.updateErr != nil {
		return domain.MCPServer{}, s.updateErr
	}
	if s.server.ID != id {
		return domain.MCPServer{}, domain.ErrMCPServerNotFound
	}
	s.server.ServerKey, s.server.Name, s.server.URL = input.ServerKey, input.Name, input.URL
	s.server.TimeoutSeconds = input.TimeoutSeconds
	s.server.Headers = input.Headers
	s.server.Revision++
	return s.server, nil
}

func (s *memoryMCPStore) SetMCPServerEnabled(_ context.Context, id int64, enabled bool) (domain.MCPServer, error) {
	if s.stateErr != nil {
		return domain.MCPServer{}, s.stateErr
	}
	if s.server.ID != id {
		return domain.MCPServer{}, domain.ErrMCPServerNotFound
	}
	if s.server.Enabled != enabled {
		s.server.Enabled = enabled
		s.server.Revision++
	}
	return s.server, nil
}

func (s *memoryMCPStore) DeleteMCPServer(_ context.Context, id int64) error {
	if s.deleteErr != nil {
		return s.deleteErr
	}
	if s.server.ID != id {
		return domain.ErrMCPServerNotFound
	}
	s.server = domain.MCPServer{}
	return nil
}

func TestAdminMCPCreateDoesNotExposeHeadersAndSupportsEnable(t *testing.T) {
	store := &memoryMCPStore{}
	server := &Server{mcpServers: store}
	request := httptest.NewRequest("POST", "/v1/admin/mcp-servers", strings.NewReader(`{
		"server_key":"exa","name":"Exa","url":"https://mcp.exa.ai/mcp",
		"headers":{"Authorization":"Bearer tvly-secret-key-12345"},
		"timeout_seconds":30
	}`))
	response := httptest.NewRecorder()
	server.handleAdminCreateMCPServer(response, request)
	if response.Code != http.StatusCreated {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var created map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created["server_key"] != "exa" || created["enabled"] != false {
		t.Fatalf("created=%v", created)
	}
	// 新契约(EPIC D4'): headers 可配置, 回显掩码——明文 key 只写不读。
	if h, ok := created["headers"].(map[string]any); !ok || h["Authorization"] != "Bear***" {
		t.Fatalf("headers must be masked in reply: %v", created["headers"])
	}

	enable := httptest.NewRequest("POST", "/v1/admin/mcp-servers/1/enable", nil)
	enable.SetPathValue("mcp_server_id", "1")
	enableResponse := httptest.NewRecorder()
	server.handleAdminEnableMCPServer(enableResponse, enable)
	if enableResponse.Code != http.StatusOK || !store.server.Enabled {
		t.Fatalf("enable status=%d server=%+v", enableResponse.Code, store.server)
	}
}

func TestAdminMCPRejectsInvalidServerKeyAndHeaders(t *testing.T) {
	server := &Server{mcpServers: &memoryMCPStore{}}
	for name, body := range map[string]string{
		"invalid key":      `{"server_key":"bad id","name":"Bad","url":"https://example.com/mcp","timeout_seconds":30}`,
		"missing timeout":  `{"server_key":"ok","name":"Bad","url":"https://example.com/mcp"}`,
		"zero timeout":     `{"server_key":"ok","name":"Bad","url":"https://example.com/mcp","timeout_seconds":0}`,
		"reserved header":  `{"server_key":"ok","name":"Bad","url":"https://example.com/mcp","headers":{"Host":"evil"},"timeout_seconds":30}`,
		"empty header key": `{"server_key":"ok","name":"Bad","url":"https://example.com/mcp","headers":{"":"v"},"timeout_seconds":30}`,
	} {
		t.Run(name, func(t *testing.T) {
			request := httptest.NewRequest("POST", "/v1/admin/mcp-servers", strings.NewReader(body))
			response := httptest.NewRecorder()
			server.handleAdminCreateMCPServer(response, request)
			if response.Code != http.StatusBadRequest {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
		})
	}
	// 新契约(D4'): 合法凭据头(Authorization/x-api-key)接受并掩码回显。
	okBody := `{"server_key":"ok","name":"Bad","url":"https://example.com/mcp","headers":{"Authorization":"Bearer secret"},"timeout_seconds":30}`
	request := httptest.NewRequest("POST", "/v1/admin/mcp-servers", strings.NewReader(okBody))
	response := httptest.NewRecorder()
	server.handleAdminCreateMCPServer(response, request)
	if response.Code != http.StatusCreated {
		t.Fatalf("valid headers rejected: status=%d body=%s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), "Bear***") {
		t.Fatalf("valid headers must be masked in reply: %s", response.Body.String())
	}
}

func TestAdminMCPUpdateUsesRequestedTimeout(t *testing.T) {
	store := &memoryMCPStore{server: domain.MCPServer{
		ID: 1, ServerKey: "exa", Name: "Exa", URL: "https://mcp.exa.ai/mcp",
		TimeoutSeconds: 30, Revision: 1, CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}}
	server := &Server{mcpServers: store}
	request := httptest.NewRequest("PUT", "/v1/admin/mcp-servers/1", strings.NewReader(`{
		"server_key":"exa","name":"Exa","url":"https://mcp.exa.ai/mcp","timeout_seconds":45
	}`))
	request.SetPathValue("mcp_server_id", "1")
	response := httptest.NewRecorder()

	server.handleAdminUpdateMCPServer(response, request)

	if response.Code != http.StatusOK || store.server.TimeoutSeconds != 45 {
		t.Fatalf("status=%d timeout=%d body=%s", response.Code, store.server.TimeoutSeconds, response.Body.String())
	}
}

func TestAdminMCPDeleteAndDisableHandlers(t *testing.T) {
	store := &memoryMCPStore{server: domain.MCPServer{
		ID: 1, ServerKey: "exa", Name: "Exa", URL: "https://mcp.exa.ai/mcp",
		TimeoutSeconds: 30, Enabled: true, Revision: 2, CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}}
	server := &Server{mcpServers: store}

	disable := httptest.NewRequest("POST", "/v1/admin/mcp-servers/1/disable", nil)
	disable.SetPathValue("mcp_server_id", "1")
	disableResponse := httptest.NewRecorder()
	server.handleAdminDisableMCPServer(disableResponse, disable)
	if disableResponse.Code != http.StatusOK || store.server.Enabled {
		t.Fatalf("disable status=%d server=%+v", disableResponse.Code, store.server)
	}

	remove := httptest.NewRequest("DELETE", "/v1/admin/mcp-servers/1", nil)
	remove.SetPathValue("mcp_server_id", "1")
	removeResponse := httptest.NewRecorder()
	server.handleAdminDeleteMCPServer(removeResponse, remove)
	if removeResponse.Code != http.StatusNoContent || store.server.ID != 0 {
		t.Fatalf("delete status=%d server=%+v", removeResponse.Code, store.server)
	}
}

func TestAdminMCPMapsStoreErrors(t *testing.T) {
	internalErr := errors.New("database unavailable")
	validBody := `{"server_key":"exa","name":"Exa","url":"https://mcp.exa.ai/mcp","timeout_seconds":30}`

	for name, storeErr := range map[string]struct {
		err  error
		want int
	}{
		"not found": {err: domain.ErrMCPServerNotFound, want: http.StatusNotFound},
		"conflict":  {err: domain.ErrMCPServerConflict, want: http.StatusConflict},
		"internal":  {err: internalErr, want: http.StatusInternalServerError},
	} {
		t.Run("create "+name, func(t *testing.T) {
			server := &Server{mcpServers: &memoryMCPStore{createErr: storeErr.err}}
			response := httptest.NewRecorder()
			server.handleAdminCreateMCPServer(response, httptest.NewRequest("POST", "/v1/admin/mcp-servers", strings.NewReader(validBody)))
			if response.Code != storeErr.want {
				t.Fatalf("status=%d want=%d body=%s", response.Code, storeErr.want, response.Body.String())
			}
		})
	}

	t.Run("update internal", func(t *testing.T) {
		server := &Server{mcpServers: &memoryMCPStore{updateErr: internalErr}}
		request := httptest.NewRequest("PUT", "/v1/admin/mcp-servers/1", strings.NewReader(validBody))
		request.SetPathValue("mcp_server_id", "1")
		response := httptest.NewRecorder()
		server.handleAdminUpdateMCPServer(response, request)
		if response.Code != http.StatusInternalServerError {
			t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
		}
	})

	for name, invoke := range map[string]func(*Server, *httptest.ResponseRecorder){
		"delete": func(server *Server, response *httptest.ResponseRecorder) {
			request := httptest.NewRequest("DELETE", "/v1/admin/mcp-servers/1", nil)
			request.SetPathValue("mcp_server_id", "1")
			server.handleAdminDeleteMCPServer(response, request)
		},
		"state": func(server *Server, response *httptest.ResponseRecorder) {
			request := httptest.NewRequest("POST", "/v1/admin/mcp-servers/1/disable", nil)
			request.SetPathValue("mcp_server_id", "1")
			server.handleAdminDisableMCPServer(response, request)
		},
	} {
		t.Run(name+" internal", func(t *testing.T) {
			store := &memoryMCPStore{server: domain.MCPServer{ID: 1}, deleteErr: internalErr, stateErr: internalErr}
			server := &Server{mcpServers: store}
			response := httptest.NewRecorder()
			invoke(server, response)
			if response.Code != http.StatusInternalServerError {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
		})
	}
}

func (s *memoryMCPStore) SetMCPQuotaLimit(_ context.Context, ownerKey, serverID, period string, limitCount int64) error {
	return nil
}

func (s *memoryMCPStore) ListMCPQuotaLimits(_ context.Context, ownerKey string) ([]domain.MCPQuotaLimit, error) {
	return nil, nil
}

func (s *memoryMCPStore) DeleteMCPQuotaLimit(_ context.Context, ownerKey, serverID, period string) error {
	return nil
}

// 掩码合并(JSON 编辑契约): Update 提交掩码值(与当前掩码一致)时保留原 key,
// 明文值更新; Create 提交掩码值拒绝(必须明文)。
func TestAdminMCPUpdatePreservesMaskedHeaders(t *testing.T) {
	store := &memoryMCPStore{server: domain.MCPServer{
		ID: 1, ServerKey: "tavily", Name: "Tavily", URL: "https://mcp.tavily.com/mcp/",
		TimeoutSeconds: 30, Headers: map[string]string{"Authorization": "Bearer tvly-secret-12345"},
		Revision: 1, CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}}
	server := &Server{mcpServers: store}
	// 掩码值保留 + 新明文键更新。
	request := httptest.NewRequest("PUT", "/v1/admin/mcp-servers/1", strings.NewReader(`{
		"server_key":"tavily","name":"Tavily","url":"https://mcp.tavily.com/mcp/",
		"headers":{"Authorization":"Bear***","x-api-key":"exa-new-key"},"timeout_seconds":30
	}`))
	request.SetPathValue("mcp_server_id", "1")
	response := httptest.NewRecorder()
	server.handleAdminUpdateMCPServer(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if store.server.Headers["Authorization"] != "Bearer tvly-secret-12345" {
		t.Fatalf("masked header must be preserved, got %q", store.server.Headers["Authorization"])
	}
	if store.server.Headers["x-api-key"] != "exa-new-key" {
		t.Fatalf("new plaintext header must be set, got %q", store.server.Headers["x-api-key"])
	}
}

func TestAdminMCPCreateRejectsMaskedHeaders(t *testing.T) {
	server := &Server{mcpServers: &memoryMCPStore{}}
	request := httptest.NewRequest("POST", "/v1/admin/mcp-servers", strings.NewReader(`{
		"server_key":"ok","name":"Bad","url":"https://example.com/mcp",
		"headers":{"Authorization":"Bear***"},"timeout_seconds":30
	}`))
	response := httptest.NewRecorder()
	server.handleAdminCreateMCPServer(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("masked header on create must be rejected: status=%d body=%s", response.Code, response.Body.String())
	}
}

// TestAdminMCPUpdateRejectsStaleMaskedHeaders(Y3 回归): 提交的掩码值与当前
// 存储值掩码不一致(陈旧回显/并发编辑/伪造)必须 400 拒绝, 不得把掩码字符串
// 原样落库成为真实 header(否则注入上游后认证必失败)。新键提交掩码值同样拒绝。
func TestAdminMCPUpdateRejectsStaleMaskedHeaders(t *testing.T) {
	store := &memoryMCPStore{server: domain.MCPServer{
		ID: 1, ServerKey: "tavily", Name: "Tavily", URL: "https://mcp.tavily.com/mcp/",
		TimeoutSeconds: 30, Headers: map[string]string{"Authorization": "Bearer tvly-secret-12345"},
		Revision: 1, CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}}
	server := &Server{mcpServers: store}
	// 陈旧掩码: 真实值掩码是 "Bear***", 提交 "xxxx***" 不匹配 → 400, 且不落库。
	stale := httptest.NewRequest("PUT", "/v1/admin/mcp-servers/1", strings.NewReader(`{
		"server_key":"tavily","name":"Tavily","url":"https://mcp.tavily.com/mcp/",
		"headers":{"Authorization":"xxxx***"},"timeout_seconds":30
	}`))
	stale.SetPathValue("mcp_server_id", "1")
	response := httptest.NewRecorder()
	server.handleAdminUpdateMCPServer(response, stale)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("stale masked header must be rejected: status=%d body=%s", response.Code, response.Body.String())
	}
	if store.server.Headers["Authorization"] != "Bearer tvly-secret-12345" {
		t.Fatalf("stale masked value must not overwrite stored secret, got %q", store.server.Headers["Authorization"])
	}

	// 新键提交掩码值(该键当前不存在)→ 400, 不得写入。
	newKey := httptest.NewRequest("PUT", "/v1/admin/mcp-servers/1", strings.NewReader(`{
		"server_key":"tavily","name":"Tavily","url":"https://mcp.tavily.com/mcp/",
		"headers":{"x-api-key":"abcd***"},"timeout_seconds":30
	}`))
	newKey.SetPathValue("mcp_server_id", "1")
	response = httptest.NewRecorder()
	server.handleAdminUpdateMCPServer(response, newKey)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("masked value for a new key must be rejected: status=%d body=%s", response.Code, response.Body.String())
	}
	if _, exists := store.server.Headers["x-api-key"]; exists {
		t.Fatalf("masked value for new key must not be stored, got %q", store.server.Headers["x-api-key"])
	}
}

// failingListStore 在 ListMCPServers 上注入基础设施故障(模拟 DB 不可用),
// 其余操作委托给 memoryMCPStore。
type failingListStore struct {
	*memoryMCPStore
	listErr error
}

func (f *failingListStore) ListMCPServers(context.Context) ([]domain.MCPServer, error) {
	return nil, f.listErr
}

// TestAdminMCPUpdateStoreFailureIs500(2026-08 审查): 掩码合并前的存储读取
// 失败是基础设施故障 → 500, 不得降级为 400 VALIDATION_ERROR(否则客户端
// 会把 DB 故障当成自己的输入错误, 无限重试)。
func TestAdminMCPUpdateStoreFailureIs500(t *testing.T) {
	store := &failingListStore{
		memoryMCPStore: &memoryMCPStore{server: domain.MCPServer{
			ID: 1, ServerKey: "tavily", Name: "Tavily", URL: "https://mcp.tavily.com/mcp/",
			TimeoutSeconds: 30, Headers: map[string]string{"Authorization": "Bearer tvly-secret-12345"},
			Revision: 1, CreatedAt: time.Now(), UpdatedAt: time.Now(),
		}},
		listErr: errors.New("db unavailable"),
	}
	server := &Server{mcpServers: store}
	req := httptest.NewRequest("PUT", "/v1/admin/mcp-servers/1", strings.NewReader(`{
		"server_key":"tavily","name":"Tavily","url":"https://mcp.tavily.com/mcp/",
		"headers":{"x-api-key":"abcd***"},"timeout_seconds":30
	}`))
	req.SetPathValue("mcp_server_id", "1")
	response := httptest.NewRecorder()
	server.handleAdminUpdateMCPServer(response, req)
	if response.Code != http.StatusInternalServerError {
		t.Fatalf("store failure must map to 500, got %d body=%s", response.Code, response.Body.String())
	}
	if body := response.Body.String(); !strings.Contains(body, "MCP_SERVER_UPDATE_FAILED") {
		t.Fatalf("body = %s, want MCP_SERVER_UPDATE_FAILED", body)
	}
}
