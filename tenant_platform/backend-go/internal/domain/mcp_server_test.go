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
			wantErr: "command/args are only valid with transport",
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

// stdio transport: Worker 沙箱内进程宿主(2026-08 恢复, 主流 mcp.json 兼容)。
func TestValidateMCPServerInput_Stdio(t *testing.T) {
	cases := []struct {
		name    string
		input   MCPServerCreate
		wantErr string
	}{
		{
			name:  "valid stdio bare command",
			input: MCPServerCreate{ServerKey: "serena", Name: "Serena", Transport: "stdio", Command: "serena", Args: []string{"start-mcp-server", "--context=agent", "--project-from-cwd"}, TimeoutSeconds: 60},
		},
		{
			name:  "valid stdio absolute path",
			input: MCPServerCreate{ServerKey: "pandoc", Name: "Pandoc", Transport: "stdio", Command: "/opt/mcp-tools/mcp-pandoc", TimeoutSeconds: 60},
		},
		{
			name:  "stdio without args ok",
			input: MCPServerCreate{ServerKey: "tools", Name: "Tools", Transport: "stdio", Command: "mcp-server", TimeoutSeconds: 30},
		},
		{
			name:    "stdio requires command",
			input:   MCPServerCreate{ServerKey: "x", Name: "X", Transport: "stdio", TimeoutSeconds: 30},
			wantErr: "command is required",
		},
		{
			name:    "stdio with url rejected",
			input:   MCPServerCreate{ServerKey: "x", Name: "X", Transport: "stdio", Command: "serena", URL: "https://mcp.exa.ai/mcp", TimeoutSeconds: 30},
			wantErr: "url must be empty",
		},
		{
			name:    "stdio with headers rejected",
			input:   MCPServerCreate{ServerKey: "x", Name: "X", Transport: "stdio", Command: "serena", Headers: map[string]string{"Authorization": "Bearer x"}, TimeoutSeconds: 30},
			wantErr: "headers are not supported",
		},
		{
			name:    "command with spaces rejected",
			input:   MCPServerCreate{ServerKey: "bad", Name: "Bad", Transport: "stdio", Command: "serena -x", TimeoutSeconds: 60},
			wantErr: "command",
		},
		{
			name:    "command with shell metachars rejected",
			input:   MCPServerCreate{ServerKey: "bad", Name: "Bad", Transport: "stdio", Command: "serena;rm -rf /", TimeoutSeconds: 60},
			wantErr: "command",
		},
		{
			name:    "too many args rejected",
			input:   MCPServerCreate{ServerKey: "x", Name: "X", Transport: "stdio", Command: "serena", Args: make([]string, MaxMCPArgs+1), TimeoutSeconds: 30},
			wantErr: "args must not exceed",
		},
		{
			name:    "empty arg rejected",
			input:   MCPServerCreate{ServerKey: "x", Name: "X", Transport: "stdio", Command: "serena", Args: []string{""}, TimeoutSeconds: 30},
			wantErr: "each arg",
		},
		{
			name:    "unknown transport rejected",
			input:   MCPServerCreate{ServerKey: "x", Name: "X", Transport: "sse", URL: "https://x.ai/mcp", TimeoutSeconds: 30},
			wantErr: "transport must be",
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

// max_instances 边界对 http 同样生效(用 http 测)。
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

// stdio transport: 格式校验(fail-fast 防手误); 管理员可信, 不做命令白名单。
func TestValidateMCPServerInput_StdioFormatChecks(t *testing.T) {
	for _, command := range []string{"sh", "/bin/sh", "/usr/bin/bash", "serena", "/opt/mcp-tools/mcp-pandoc"} {
		input := MCPServerCreate{ServerKey: "x", Name: "X", Transport: "stdio", Command: command, TimeoutSeconds: 30}
		if err := ValidateMCPServerInput(input); err != nil {
			t.Fatalf("command %q should be accepted, got %v", command, err)
		}
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
