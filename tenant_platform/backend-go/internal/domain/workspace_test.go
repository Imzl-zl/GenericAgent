package domain

import "testing"

func TestWorkspaceKeyPersonal(t *testing.T) {
	key, err := PersonalWorkspaceKey(42)
	if err != nil {
		t.Fatalf("PersonalWorkspaceKey: %v", err)
	}
	if key != "personal:42" {
		t.Fatalf("key = %q, want personal:42", key)
	}
}

func TestWorkspaceKeyTeam(t *testing.T) {
	key, err := TeamWorkspaceKey(7)
	if err != nil {
		t.Fatalf("TeamWorkspaceKey: %v", err)
	}
	if key != "team:7" {
		t.Fatalf("key = %q, want team:7", key)
	}
}

func TestWorkspaceKeyRejectsNonPositiveIDs(t *testing.T) {
	if _, err := PersonalWorkspaceKey(0); err == nil {
		t.Fatal("PersonalWorkspaceKey(0) should fail")
	}
	if _, err := PersonalWorkspaceKey(-1); err == nil {
		t.Fatal("PersonalWorkspaceKey(-1) should fail")
	}
	if _, err := TeamWorkspaceKey(0); err == nil {
		t.Fatal("TeamWorkspaceKey(0) should fail")
	}
}

// 审查 R5-I: team 整数分支必须拒绝 0 与负整数(此前只拒绝 "0", 接受 team:-1)。
func TestValidateWorkspaceKeyRejectsNonPositiveTeamIDs(t *testing.T) {
	for _, bad := range []string{"team:-1", "team:0", "team:-999"} {
		if err := ValidateWorkspaceKey(bad); err == nil {
			t.Fatalf("ValidateWorkspaceKey(%q) should fail", bad)
		}
	}
	for _, good := range []string{"team:1", "team:42", "team:0007"} {
		if err := ValidateWorkspaceKey(good); err != nil {
			t.Fatalf("ValidateWorkspaceKey(%q) should pass: %v", good, err)
		}
	}
}

func TestRunnerKeyEqualsWorkspaceKey(t *testing.T) {
	ws, err := PersonalWorkspaceKey(99)
	if err != nil {
		t.Fatalf("workspace: %v", err)
	}
	rk, err := RunnerKeyForWorkspace(ws)
	if err != nil {
		t.Fatalf("RunnerKeyForWorkspace: %v", err)
	}
	if rk != ws {
		t.Fatalf("runner key = %q, want equal to workspace key %q", rk, ws)
	}
}

func TestRunnerKeyRejectsForeignFormat(t *testing.T) {
	if _, err := RunnerKeyForWorkspace("other:1"); err == nil {
		t.Fatal("RunnerKeyForWorkspace(foreign) should fail")
	}
	if _, err := RunnerKeyForWorkspace("personal:abc"); err == nil {
		t.Fatal("RunnerKeyForWorkspace(bad id) should fail")
	}
}

func TestWorkspaceKeyParsing(t *testing.T) {
	scope, id, err := ParseWorkspaceKey("personal:42")
	if err != nil {
		t.Fatalf("ParseWorkspaceKey: %v", err)
	}
	if scope != "personal" || id != 42 {
		t.Fatalf("scope=%q id=%d, want personal 42", scope, id)
	}
	if _, _, err := ParseWorkspaceKey("team:abc"); err == nil {
		t.Fatal("ParseWorkspaceKey(bad id) should fail")
	}
	if _, _, err := ParseWorkspaceKey("nonsense"); err == nil {
		t.Fatal("ParseWorkspaceKey(no colon) should fail")
	}
	if _, _, err := ParseWorkspaceKey("unknown:1"); err == nil {
		t.Fatal("ParseWorkspaceKey(unknown scope) should fail")
	}
}

func TestValidateWorkspaceKeyAcceptsTeamUUID(t *testing.T) {
	// 生产 team 主键为 UUID（PRD §5）：领域校验必须接受 team:<uuid>，
	// 否则 WorkspaceDirHash/RunnerKeyForWorkspace 无法用于真实团队工作区
	//（审查 Minor-1：旧 helper 只接受整数 team id）。
	for _, key := range []string{
		"team:3b1f6a2e-9d4c-4f8e-9b2a-1c3d5e7f9a0b",
		"team:7", // 兼容旧整数格式
		"personal:42",
	} {
		if err := ValidateWorkspaceKey(key); err != nil {
			t.Fatalf("ValidateWorkspaceKey(%q): %v", key, err)
		}
		if _, err := WorkspaceDirHash(key); err != nil {
			t.Fatalf("WorkspaceDirHash(%q): %v", key, err)
		}
	}
	for _, key := range []string{
		"team:", "team:abc", "personal:abc", "personal:0", "personal:-1",
		"other:1", "nonsense", "personal:",
		"team:3b1f6a2e-9d4c-4f8e-9b2a-1c3d5e7f9a0b-extra",
		"team:3b1f6a2e9d4c4f8e9b2a1c3d5e7f9a0b", // 非规范 UUID 格式
	} {
		if err := ValidateWorkspaceKey(key); err == nil {
			t.Fatalf("ValidateWorkspaceKey(%q) should fail", key)
		}
	}
}

func TestRunnerKeyForWorkspaceAcceptsTeamUUID(t *testing.T) {
	key := "team:3b1f6a2e-9d4c-4f8e-9b2a-1c3d5e7f9a0b"
	rk, err := RunnerKeyForWorkspace(key)
	if err != nil {
		t.Fatalf("RunnerKeyForWorkspace(team uuid): %v", err)
	}
	if rk != key {
		t.Fatalf("runner key = %q, want %q", rk, key)
	}
}

func TestWorkspaceKeyHashIsStableAndScoped(t *testing.T) {
	// 方案 §4: workspaces/<hash(workspace_key)> —— hash 必须稳定且区分工作区。
	a1, err := WorkspaceDirHash("personal:1")
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	a2, err := WorkspaceDirHash("personal:1")
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	b, err := WorkspaceDirHash("personal:2")
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	c, err := WorkspaceDirHash("team:1")
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	if a1 != a2 {
		t.Fatal("hash not stable for same workspace")
	}
	if a1 == b || a1 == c {
		t.Fatal("hash collision across distinct workspaces")
	}
	if len(a1) == 0 || len(a1) > 64 {
		t.Fatalf("unexpected hash length %d", len(a1))
	}
}
