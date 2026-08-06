package domain

import "testing"

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
