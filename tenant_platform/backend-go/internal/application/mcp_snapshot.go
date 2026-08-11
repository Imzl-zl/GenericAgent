package application

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"

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
	for _, server := range servers {
		if server.ID <= 0 || server.Revision <= 0 || !server.Enabled {
			return RuntimeMCPSnapshot{}, fmt.Errorf("invalid enabled MCP server %d", server.ID)
		}
		transport := server.Transport
		if transport == "" {
			transport = domain.MCPTransportHTTP
		}
		// stdio transport 已随 mcp-gateway 退役移除(EPIC D5): 遗留行跳过
		// 不下发(不合成 gateway URL, 避免空 URL 下发 worker)。
		if transport != domain.MCPTransportHTTP {
			slog.Warn("mcp snapshot: stdio server skipped (transport removed)",
				"server_id", server.ServerKey)
			continue
		}
		fingerprint = append(fingerprint, mcpSnapshotFingerprint{
			ID: server.ID, ServerID: server.ServerKey, Revision: server.Revision,
		})
		runtimeServers = append(runtimeServers, RuntimeMCPServer{
			ServerID: server.ServerKey, Name: server.Name, URL: server.URL,
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
