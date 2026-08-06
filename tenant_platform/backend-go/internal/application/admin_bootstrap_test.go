package application

import (
	"context"
	"strings"
	"testing"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/infrastructure/postgres"
)

func TestEnsureAdminContext_RequiresFlag(t *testing.T) {
	pool := postgres.OpenTestPool(t)
	store, err := postgres.NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	_, err = EnsureAdminContext(context.Background(), store, AdminBootstrapConfig{
		Enabled:  false,
		UserID:   1,
		AdminToken: "tok",
	})
	if err == nil || !strings.Contains(err.Error(), "--dev-loopback") {
		t.Fatalf("expected flag rejection: %v", err)
	}
}

func TestEnsureAdminContext_CreatesApprovedPersonalWorkspace(t *testing.T) {
	pool := postgres.OpenTestPool(t)
	store, err := postgres.NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	dev, err := EnsureAdminContext(ctx, store, AdminBootstrapConfig{
		Enabled:  true,
		UserID:   7,
		Username: "dev7",
		AdminToken: "admin token",
	})
	if err != nil {
		t.Fatal(err)
	}
	if dev.SessionKey != "personal:7" {
		t.Fatalf("session=%s", dev.SessionKey)
	}
	var status, marker string
	if err := pool.QueryRow(ctx, `SELECT status, bootstrap_marker FROM users WHERE id=7`).Scan(&status, &marker); err != nil {
		t.Fatal(err)
	}
	if status != "approved" || marker != "dev-loopback" {
		t.Fatalf("user status=%s marker=%s", status, marker)
	}
	var kind string
	var team any
	var vol any
	var wmarker string
	if err := pool.QueryRow(ctx, `
SELECT kind, team_id, volume_id, bootstrap_marker FROM workspaces WHERE session_key=$1
`, dev.SessionKey).Scan(&kind, &team, &vol, &wmarker); err != nil {
		t.Fatal(err)
	}
	if kind != "personal" || team != nil || vol != nil || wmarker != "dev-loopback" {
		t.Fatalf("workspace kind=%s team=%v vol=%v marker=%s", kind, team, vol, wmarker)
	}

	dev2, err := EnsureAdminContext(ctx, store, AdminBootstrapConfig{
		Enabled: true, UserID: 7, Username: "dev7-renamed", AdminToken: "admin token",
	})
	if err != nil {
		t.Fatal(err)
	}
	if dev2.WorkspaceID != dev.WorkspaceID {
		t.Fatal("workspace id changed on bootstrap refresh")
	}

	if _, err := pool.Exec(ctx, `
INSERT INTO users (id, username, status, bootstrap_marker) VALUES (8, 'other', 'pending', NULL)
`); err != nil {
		t.Fatal(err)
	}
	_, err = EnsureAdminContext(ctx, store, AdminBootstrapConfig{
		Enabled: true, UserID: 8, AdminToken: "tok",
	})
	if err == nil {
		t.Fatal("expected refusal to promote non-bootstrap user")
	}
}
