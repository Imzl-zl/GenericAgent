package domain

import (
	"strings"
	"testing"
)

func TestValidateMCPServerInput_HTTP(t *testing.T) {
	cases := []struct {
		name    string
		input   MCPServerCreate
		wantErr string
	}{
		{
			name:  "valid http",
			input: MCPServerCreate{ServerKey: "exa", Name: "Exa", URL: "https://mcp.exa.ai/mcp", TimeoutSeconds: 30},
		},
		{
			name:  "transport defaults to http",
			input: MCPServerCreate{ServerKey: "exa", Name: "Exa", URL: "http://localhost:8000/mcp", TimeoutSeconds: 30},
		},
		{
			name:    "http rejects stdio fields",
			input:   MCPServerCreate{ServerKey: "exa", Name: "Exa", URL: "https://x.ai/mcp", TimeoutSeconds: 30, Command: "/opt/mcp-tools/x"},
			wantErr: "command/args are not supported",
		},
		{
			name:    "http requires url",
			input:   MCPServerCreate{ServerKey: "exa", Name: "Exa", URL: "", TimeoutSeconds: 30},
			wantErr: "url must be an absolute http or https URL",
		},
		{
			name:    "http rejects credentials in url",
			input:   MCPServerCreate{ServerKey: "exa", Name: "Exa", URL: "https://user:pass@x.ai/mcp", TimeoutSeconds: 30},
			wantErr: "no credentials or fragment",
		},
		{
			name:    "timeout bounds",
			input:   MCPServerCreate{ServerKey: "exa", Name: "Exa", URL: "https://x.ai/mcp", TimeoutSeconds: 301},
			wantErr: "timeout_seconds",
		},
		{
			name:    "workspace isolation not yet supported",
			input:   MCPServerCreate{ServerKey: "exa", Name: "Exa", URL: "https://x.ai/mcp", TimeoutSeconds: 30, Isolation: MCPIsolationWorkspace},
			wantErr: "not supported yet",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateMCPServerInput(tc.input)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want containing %q", err, tc.wantErr)
			}
		})
	}
}

// stdio transport 已随 mcp-gateway 退役整体移除(EPIC D5): 所有 stdio 输入一律
// fail-closed 拒绝——包括旧契约里“合法”的 whitelist command/args 组合。
func TestValidateMCPServerInput_StdioRemoved(t *testing.T) {
	cases := []struct {
		name  string
		input MCPServerCreate
	}{
		{
			name:  "valid stdio shape (old contract) rejected",
			input: MCPServerCreate{ServerKey: "pandoc", Name: "Pandoc", Transport: "stdio", Command: "/opt/mcp-tools/mcp-pandoc", TimeoutSeconds: 60},
		},
		{
			name:  "stdio with args rejected",
			input: MCPServerCreate{ServerKey: "pandoc", Name: "Pandoc", Transport: "stdio", Command: "/opt/mcp-tools/mcp-pandoc", Args: []string{"--stdio"}, TimeoutSeconds: 60},
		},
		{
			name:  "stdio with url rejected",
			input: MCPServerCreate{ServerKey: "pandoc", Name: "Pandoc", Transport: "stdio", Command: "/opt/mcp-tools/mcp-pandoc", URL: "http://mcp-gateway:8083/v1/mcp/pandoc", TimeoutSeconds: 60},
		},
		{
			name:  "stdio outside whitelist rejected",
			input: MCPServerCreate{ServerKey: "bad", Name: "Bad", Transport: "stdio", Command: "/bin/sh", TimeoutSeconds: 60},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateMCPServerInput(tc.input)
			if err == nil || !strings.Contains(err.Error(), "stdio") {
				t.Fatalf("err = %v, want stdio rejection", err)
			}
		})
	}
}

// max_instances 边界对 http 同样生效(stdio 已移除, 用 http 测)。
func TestValidateMCPServerInput_MaxInstancesBoundsHTTP(t *testing.T) {
	input := MCPServerCreate{ServerKey: "exa", Name: "Exa", URL: "https://x.ai/mcp", MaxInstances: 17, TimeoutSeconds: 30}
	err := ValidateMCPServerInput(input)
	if err == nil || !strings.Contains(err.Error(), "max_instances must be between 1 and 16") {
		t.Fatalf("err = %v, want max_instances bounds", err)
	}
}


func TestValidateMCPServerInput_Headers(t *testing.T) {
	base := MCPServerCreate{ServerKey: "tavily", Name: "Tavily", URL: "https://mcp.tavily.com/mcp/", TimeoutSeconds: 30}
	cases := []struct {
		name    string
		headers map[string]string
		wantErr string
	}{
		{name: "authorization header ok", headers: map[string]string{"Authorization": "Bearer tvly-secret"}},
		{name: "x-api-key header ok", headers: map[string]string{"x-api-key": "exa-secret"}},
		{name: "multiple headers ok", headers: map[string]string{"Authorization": "Bearer x", "X-Custom": "y"}},
		{name: "empty headers ok", headers: nil},
		{name: "host header rejected", headers: map[string]string{"Host": "evil.example.com"}, wantErr: "reserved"},
		{name: "content-length rejected", headers: map[string]string{"Content-Length": "0"}, wantErr: "reserved"},
		{name: "connection rejected", headers: map[string]string{"Connection": "close"}, wantErr: "reserved"},
		{name: "transfer-encoding rejected", headers: map[string]string{"Transfer-Encoding": "chunked"}, wantErr: "reserved"},
		{name: "empty key rejected", headers: map[string]string{"": "v"}, wantErr: "header name"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			input := base
			input.Headers = tc.headers
			err := ValidateMCPServerInput(input)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want containing %q", err, tc.wantErr)
			}
		})
	}
}

// stdio transport 已随 mcp-gateway 退役整体移除: 校验必须 fail-closed 拒绝。
func TestValidateMCPServerInput_RejectsStdio(t *testing.T) {
	input := MCPServerCreate{
		ServerKey: "pandoc", Name: "Pandoc",
		Transport: "stdio", Command: "/opt/mcp-tools/mcp-pandoc",
		TimeoutSeconds: 30,
	}
	err := ValidateMCPServerInput(input)
	if err == nil || !strings.Contains(err.Error(), "stdio") {
		t.Fatalf("expected stdio rejection, got %v", err)
	}
}

// QuotaLimit 模型: 周期常量与限额结构(任务 3/4 引用面)。
func TestMCPQuotaModel(t *testing.T) {
	if MCPQuotaPeriodDay != "day" || MCPQuotaPeriodMonth != "month" {
		t.Fatalf("quota period constants wrong: %q %q", MCPQuotaPeriodDay, MCPQuotaPeriodMonth)
	}
	limit := MCPQuotaLimit{OwnerKey: "user-1", ServerID: "tavily", Period: MCPQuotaPeriodDay, LimitCount: 100}
	if limit.OwnerKey != "user-1" || limit.ServerID != "tavily" || limit.Period != MCPQuotaPeriodDay || limit.LimitCount != 100 {
		t.Fatalf("quota limit fields wrong: %+v", limit)
	}
	if err := limit.Validate(); err != nil {
		t.Fatalf("valid quota limit rejected: %v", err)
	}
	bad := MCPQuotaLimit{OwnerKey: "", ServerID: "tavily", Period: MCPQuotaPeriodDay, LimitCount: 100}
	if err := bad.Validate(); err == nil {
		t.Fatalf("empty owner_key should be rejected")
	}
	bad2 := MCPQuotaLimit{OwnerKey: "u", ServerID: "s", Period: "year", LimitCount: 1}
	if err := bad2.Validate(); err == nil {
		t.Fatalf("invalid period should be rejected")
	}
}
