package sophub

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
)

const (
	OfficialBaseURL        = "https://fudankw.cn/sophub"
	maxSophubResponseBytes = 128 * 1024
	defaultTimeout         = 10 * time.Second
)

type Client struct {
	baseURL    string
	httpClient *http.Client
}

func NewClient() *Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	return newClient(OfficialBaseURL, &http.Client{Transport: transport, Timeout: defaultTimeout})
}

func newClient(baseURL string, httpClient *http.Client) *Client {
	configured := *httpClient
	if configured.Timeout <= 0 {
		configured.Timeout = defaultTimeout
	}
	configured.CheckRedirect = func(*http.Request, []*http.Request) error {
		return fmt.Errorf("Sophub redirects are not allowed")
	}
	return &Client{baseURL: strings.TrimRight(baseURL, "/"), httpClient: &configured}
}

func (client *Client) Verify(ctx context.Context, apiKey string) (domain.SophubIdentity, error) {
	var identity domain.SophubIdentity
	err := client.getJSON(ctx, apiKey, "/api/me", nil, &identity)
	return identity, err
}

func (client *Client) Search(ctx context.Context, apiKey, query string, page, pageSize int) (domain.SophubSearchResult, error) {
	if page <= 0 || pageSize <= 0 || pageSize > 100 {
		return domain.SophubSearchResult{}, fmt.Errorf("Sophub pagination is invalid")
	}
	values := url.Values{}
	values.Set("q", query)
	values.Set("page", strconv.Itoa(page))
	values.Set("page_size", strconv.Itoa(pageSize))
	var result domain.SophubSearchResult
	err := client.getJSON(ctx, apiKey, "/api/sops", values, &result)
	return result, err
}

func (client *Client) GetSOP(ctx context.Context, apiKey, remoteSOPID string) (domain.SophubRemoteSOP, error) {
	remoteSOPID = strings.TrimSpace(remoteSOPID)
	if remoteSOPID == "" || len([]byte(remoteSOPID)) > domain.MaxSOPRemoteIDBytes {
		return domain.SophubRemoteSOP{}, fmt.Errorf("remote SOP id is invalid")
	}
	var sop domain.SophubRemoteSOP
	err := client.getJSON(ctx, apiKey, "/api/sops/"+url.PathEscape(remoteSOPID), nil, &sop)
	return sop, err
}

func (client *Client) getJSON(ctx context.Context, apiKey, path string, query url.Values, target any) error {
	if strings.TrimSpace(apiKey) == "" {
		return fmt.Errorf("Sophub API key is required")
	}
	endpoint := client.baseURL + path
	if len(query) > 0 {
		endpoint += "?" + query.Encode()
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return fmt.Errorf("build Sophub request: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+apiKey)
	request.Header.Set("Accept", "application/json")
	response, err := client.httpClient.Do(request)
	if err != nil {
		return fmt.Errorf("Sophub request failed")
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("Sophub returned HTTP %d", response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxSophubResponseBytes+1))
	if err != nil {
		return fmt.Errorf("read Sophub response")
	}
	if len(body) > maxSophubResponseBytes {
		return fmt.Errorf("Sophub response is too large")
	}
	if err := json.Unmarshal(body, target); err != nil {
		return fmt.Errorf("Sophub returned invalid JSON")
	}
	return nil
}
