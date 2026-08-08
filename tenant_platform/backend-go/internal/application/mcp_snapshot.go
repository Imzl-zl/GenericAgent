package application

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strings"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
)

type mcpSnapshotFingerprint struct {
	ID       int64  `json:"id"`
	ServerID string `json:"server_id"`
	Revision int64  `json:"revision"`
}

func (s *scheduler) resolveMCPSnapshot(ctx context.Context) (RuntimeMCPSnapshot, error) {
	if s.cfg.MCPServer == nil {
		return disabledMCPSnapshot(), nil
	}
	servers, err := s.cfg.MCPServer.ListEnabledMCPServers(ctx)
	if err != nil {
		return RuntimeMCPSnapshot{}, fmt.Errorf("list enabled MCP servers: %w", err)
	}
	if len(servers) == 0 {
		return disabledMCPSnapshot(), nil
	}
	sort.Slice(servers, func(i, j int) bool { return servers[i].ID < servers[j].ID })
	fingerprint := make([]mcpSnapshotFingerprint, 0, len(servers))
	runtimeServers := make([]RuntimeMCPServer, 0, len(servers))
	gatewayBase := strings.TrimRight(strings.TrimSpace(s.cfg.MCPGatewayBaseURL), "/")
	for _, server := range servers {
		if server.ID <= 0 || server.Revision <= 0 || !server.Enabled {
			return RuntimeMCPSnapshot{}, fmt.Errorf("invalid enabled MCP server %d", server.ID)
		}
		transport := server.Transport
		if transport == "" {
			transport = domain.MCPTransportHTTP
		}
		url := strings.TrimSpace(server.URL)
		if transport == domain.MCPTransportStdio {
			// stdio 由 mcp-gateway 托管: URL 由合成函数统一生成
			// (与 proxy resolve 同函数), 管理员无需填写 gateway 地址。
			// gateway 未配置时 fail-closed 不下发该 server。
			if gatewayBase == "" {
				slog.Warn("mcp snapshot: stdio server skipped, MCPGatewayBaseURL not configured",
					"server_id", server.ServerKey)
				continue
			}
			url = domain.MCPServerGatewayURL(gatewayBase, server.ServerKey)
		}
		fingerprint = append(fingerprint, mcpSnapshotFingerprint{
			ID: server.ID, ServerID: server.ServerKey, Revision: server.Revision,
		})
		runtimeServers = append(runtimeServers, RuntimeMCPServer{
			ServerID: server.ServerKey, Name: server.Name, URL: url,
			TimeoutSeconds: server.TimeoutSeconds,
		})
	}
	if len(runtimeServers) == 0 {
		return disabledMCPSnapshot(), nil
	}
	encoded, err := json.Marshal(fingerprint)
	if err != nil {
		return RuntimeMCPSnapshot{}, fmt.Errorf("marshal MCP snapshot fingerprint: %w", err)
	}
	digest := sha256.Sum256(encoded)
	return RuntimeMCPSnapshot{
		ID: "sha256:" + hex.EncodeToString(digest[:]), Servers: runtimeServers,
	}, nil
}

func disabledMCPSnapshot() RuntimeMCPSnapshot {
	return RuntimeMCPSnapshot{Servers: []RuntimeMCPServer{}}
}
