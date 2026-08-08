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
			wantErr: "command/args are only valid for stdio",
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

func TestValidateMCPServerInput_Stdio(t *testing.T) {
	cases := []struct {
		name    string
		input   MCPServerCreate
		wantErr string
	}{
		{
			name:  "valid stdio, no url",
			input: MCPServerCreate{ServerKey: "pandoc", Name: "Pandoc", Transport: MCPTransportStdio, Command: "/opt/mcp-tools/mcp-pandoc", TimeoutSeconds: 60},
		},
		{
			name:  "valid stdio with args",
			input: MCPServerCreate{ServerKey: "pandoc", Name: "Pandoc", Transport: MCPTransportStdio, Command: "/opt/mcp-tools/mcp-pandoc", Args: []string{"--stdio"}, TimeoutSeconds: 60},
		},
		{
			name:    "stdio rejects url (gateway route is synthesized)",
			input:   MCPServerCreate{ServerKey: "pandoc", Name: "Pandoc", Transport: MCPTransportStdio, Command: "/opt/mcp-tools/mcp-pandoc", URL: "http://mcp-gateway:8083/v1/mcp/pandoc", TimeoutSeconds: 60},
			wantErr: "url must be empty for stdio",
		},
		{
			name:    "stdio requires whitelist command prefix",
			input:   MCPServerCreate{ServerKey: "bad", Name: "Bad", Transport: MCPTransportStdio, Command: "/bin/sh", TimeoutSeconds: 60},
			wantErr: "absolute path under /opt/mcp-tools/",
		},
		{
			name:    "stdio command must be a single token",
			input:   MCPServerCreate{ServerKey: "bad", Name: "Bad", Transport: MCPTransportStdio, Command: "/opt/mcp-tools/mcp-pandoc --stdio", TimeoutSeconds: 60},
			wantErr: "no whitespace or NUL",
		},
		{
			name:    "stdio rejects NUL in args",
			input:   MCPServerCreate{ServerKey: "bad", Name: "Bad", Transport: MCPTransportStdio, Command: "/opt/mcp-tools/x", Args: []string{"a\x00b"}, TimeoutSeconds: 60},
			wantErr: "no NUL",
		},
		{
			name:    "max_instances bounds",
			input:   MCPServerCreate{ServerKey: "pandoc", Name: "Pandoc", Transport: MCPTransportStdio, Command: "/opt/mcp-tools/mcp-pandoc", MaxInstances: 17, TimeoutSeconds: 60},
			wantErr: "max_instances must be between 1 and 16",
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

func TestMCPServerGatewayURL(t *testing.T) {
	cases := []struct {
		base, key, want string
	}{
		{"http://mcp-gateway:8083", "pandoc", "http://mcp-gateway:8083/v1/mcp/pandoc"},
		{"http://mcp-gateway:8083/", "pandoc", "http://mcp-gateway:8083/v1/mcp/pandoc"},
		{"  http://mcp-gateway:8083  ", "pandoc", "http://mcp-gateway:8083/v1/mcp/pandoc"},
	}
	for _, tc := range cases {
		if got := MCPServerGatewayURL(tc.base, tc.key); got != tc.want {
			t.Fatalf("MCPServerGatewayURL(%q, %q) = %q, want %q", tc.base, tc.key, got, tc.want)
		}
	}
}
