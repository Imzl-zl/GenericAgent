package checkpoint

import (
	"strings"
	"testing"
)

// TestValidateBundleIdentityRejectsMissingSessionKey 验证 bundle 缺 session_key
// 时必须拒绝(审查 R4-M14: 不允许跳过身份绑定)。
func TestValidateBundleIdentityRejectsMissingSessionKey(t *testing.T) {
	bundle := map[string]any{
		"task_id":           "t1",
		"runner_generation": float64(1),
	}
	err := validateBundleIdentity(bundle, "t1", "personal:1", 1)
	if err == nil || !strings.Contains(err.Error(), "session_key") {
		t.Fatalf("expected session_key rejection, got %v", err)
	}
}

// TestValidateBundleIdentityRejectsMismatchedSessionKey 验证 bundle session_key
// 与任务 session 不一致时拒绝。
func TestValidateBundleIdentityRejectsMismatchedSessionKey(t *testing.T) {
	bundle := map[string]any{
		"task_id":           "t1",
		"session_key":       "personal:999",
		"runner_generation": float64(1),
	}
	err := validateBundleIdentity(bundle, "t1", "personal:1", 1)
	if err == nil || !strings.Contains(err.Error(), `"personal:999"`) {
		t.Fatalf("expected session mismatch rejection, got %v", err)
	}
}

// TestValidateBundleIdentityRejectsMissingGeneration 验证 bundle 缺
// runner_generation 时必须拒绝(审查 R4-M14)。
func TestValidateBundleIdentityRejectsMissingGeneration(t *testing.T) {
	bundle := map[string]any{
		"task_id":     "t1",
		"session_key": "personal:1",
	}
	err := validateBundleIdentity(bundle, "t1", "personal:1", 1)
	if err == nil || !strings.Contains(err.Error(), "runner_generation") {
		t.Fatalf("expected generation rejection, got %v", err)
	}
}

// TestValidateBundleIdentityRejectsStaleGeneration 验证 bundle 声明旧
// generation 时拒绝(旧 Runner 不得把陈旧 bundle 冒充新快照提交)。
func TestValidateBundleIdentityRejectsStaleGeneration(t *testing.T) {
	bundle := map[string]any{
		"task_id":           "t1",
		"session_key":       "personal:1",
		"runner_generation": float64(1),
	}
	err := validateBundleIdentity(bundle, "t1", "personal:1", 2)
	if err == nil || !strings.Contains(err.Error(), "runner_generation") {
		t.Fatalf("expected stale generation rejection, got %v", err)
	}
}

// TestValidateBundleIdentityAcceptsMatching 验证身份全部匹配时通过。
func TestValidateBundleIdentityAcceptsMatching(t *testing.T) {
	bundle := map[string]any{
		"task_id":           "t1",
		"session_key":       "personal:1",
		"runner_generation": float64(3),
	}
	if err := validateBundleIdentity(bundle, "t1", "personal:1", 3); err != nil {
		t.Fatalf("expected accept, got %v", err)
	}
}
