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
