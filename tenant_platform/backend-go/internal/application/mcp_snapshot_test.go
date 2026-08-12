package application

import (
	"context"
	"testing"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
)

type fakeMCPSource struct {
	servers []domain.MCPServer
}

func (source *fakeMCPSource) ListEnabledMCPServers(context.Context) ([]domain.MCPServer, error) {
	return append([]domain.MCPServer(nil), source.servers...), nil
}

func (source *fakeMCPSource) GetWorkspaceOwner(context.Context, string) (int64, error) {
	return 42, nil
}

func TestResolveMCPSnapshotChangesWithRevision(t *testing.T) {
	source := &fakeMCPSource{servers: []domain.MCPServer{{
		ID: 1, ServerKey: "exa", Name: "Exa", URL: "https://mcp.exa.ai/mcp",
		TimeoutSeconds: 30, Enabled: true, Revision: 1,
	}}}
	s := &scheduler{cfg: SchedulerConfig{MCPServer: source}}

	first, err := s.resolveMCPSnapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Servers) != 1 {
		t.Fatalf("snapshot=%+v", first)
	}

	source.servers[0].Revision = 2
	second, err := s.resolveMCPSnapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if first.ID == second.ID {
		t.Fatalf("snapshot ID did not change: %s", first.ID)
	}
}

func TestDisabledMCPSnapshotIsStableWithoutSource(t *testing.T) {
	s := &scheduler{}
	first, err := s.resolveMCPSnapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.resolveMCPSnapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if first.ID != "" || second.ID != "" || len(first.Servers) != 0 {
		t.Fatalf("disabled snapshots: first=%+v second=%+v", first, second)
	}
}

func TestResolveMCPSnapshotTreatsNoEnabledServersAsDisabled(t *testing.T) {
	s := &scheduler{cfg: SchedulerConfig{MCPServer: &fakeMCPSource{}}}
	snapshot, err := s.resolveMCPSnapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.ID != "" || len(snapshot.Servers) != 0 {
		t.Fatalf("snapshot=%+v", snapshot)
	}
}

// stdio 遗留行(0049 时代数据)在快照中跳过: gateway 已退役, 不合成 URL。
func TestResolveMCPSnapshotSkipsStdioLegacyRows(t *testing.T) {
	source := &fakeMCPSource{servers: []domain.MCPServer{
		{ID: 1, ServerKey: "exa", Name: "Exa", URL: "https://mcp.exa.ai/mcp", TimeoutSeconds: 30, Enabled: true, Revision: 1},
		{ID: 2, ServerKey: "pandoc", Name: "Pandoc", Transport: "stdio", TimeoutSeconds: 60, Enabled: true, Revision: 1},
	}}
	s := &scheduler{cfg: SchedulerConfig{MCPServer: source}}
	snapshot, err := s.resolveMCPSnapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Servers) != 1 || snapshot.Servers[0].ServerID != "exa" {
		t.Fatalf("expected only http server in snapshot, got %+v", snapshot.Servers)
	}
}

func (source *fakeMCPSource) MCPQuotaAvailable(_ context.Context, _, _ string) (bool, error) {
	return true, nil
}
