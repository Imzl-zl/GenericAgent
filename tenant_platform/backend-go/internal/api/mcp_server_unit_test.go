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
	s.server = domain.MCPServer{
		ID: 1, ServerKey: input.ServerKey, Name: input.Name, URL: input.URL,
		TimeoutSeconds: input.TimeoutSeconds, Enabled: false, Revision: 1,
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
		"timeout_seconds":30
	}`))
	response := httptest.NewRecorder()
	server.handleAdminCreateMCPServer(response, request)
	if response.Code != http.StatusCreated {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if strings.Contains(response.Body.String(), "headers") {
		t.Fatalf("response exposed unsupported headers fields: %s", response.Body.String())
	}
	var created map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created["server_key"] != "exa" || created["enabled"] != false {
		t.Fatalf("created=%v", created)
	}

	enable := httptest.NewRequest("POST", "/v1/admin/mcp-servers/1/enable", nil)
	enable.SetPathValue("mcp_server_id", "1")
	enableResponse := httptest.NewRecorder()
	server.handleAdminEnableMCPServer(enableResponse, enable)
	if enableResponse.Code != http.StatusOK || !store.server.Enabled {
		t.Fatalf("enable status=%d server=%+v", enableResponse.Code, store.server)
	}
}

func TestAdminMCPRejectsInvalidServerKeyAndAnyHeadersField(t *testing.T) {
	server := &Server{mcpServers: &memoryMCPStore{}}
	for name, body := range map[string]string{
		"invalid key":           `{"server_key":"bad id","name":"Bad","url":"https://example.com/mcp","timeout_seconds":30}`,
		"missing timeout":       `{"server_key":"ok","name":"Bad","url":"https://example.com/mcp"}`,
		"zero timeout":          `{"server_key":"ok","name":"Bad","url":"https://example.com/mcp","timeout_seconds":0}`,
		"empty headers":         `{"server_key":"ok","name":"Bad","url":"https://example.com/mcp","headers":{},"timeout_seconds":30}`,
		"authenticated headers": `{"server_key":"ok","name":"Bad","url":"https://example.com/mcp","headers":{"Authorization":"Bearer secret"},"timeout_seconds":30}`,
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
