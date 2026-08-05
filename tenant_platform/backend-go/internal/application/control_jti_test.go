package application

import "testing"

// round11 审查 I4: 控制 RPC 使用独立签发的 control JTI, 而非任意 LLM JTI。
func TestControlJTIForPrefersControlToken(t *testing.T) {
	set := workerCredentialSet{JTIs: []string{"llm-1", "sophub-1"}, ControlJTI: "control-1"}
	if got := controlJTIFor(set); got != "control-1" {
		t.Fatalf("controlJTIFor = %q, want control-1", got)
	}
}

func TestControlJTIForFallsBackWithoutControlToken(t *testing.T) {
	// loopback/测试无 control token: 回退 firstJTI(空凭据集下为空)。
	set := workerCredentialSet{JTIs: []string{"llm-1"}}
	if got := controlJTIFor(set); got != "llm-1" {
		t.Fatalf("controlJTIFor fallback = %q, want llm-1", got)
	}
	if got := controlJTIFor(workerCredentialSet{}); got != "" {
		t.Fatalf("controlJTIFor empty = %q, want empty", got)
	}
}
