package llmproxy

import (
	"strings"
	"testing"
	"time"
)

// TestIssueControlTokenClaims verifies the dedicated control capability has
// its own audience and operation (round11 I4: control RPCs must not reuse
// LLM/Sophub capability tokens).
func TestIssueControlTokenClaims(t *testing.T) {
	issuer, err := NewIssuer([]byte("test-signing-key-at-least-32-bytes"), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	token, claims, err := issuer.IssueControlToken("personal:1", "task-1", 3, 0)
	if err != nil {
		t.Fatal(err)
	}
	if claims.Operation != ControlOperation {
		t.Fatalf("operation = %q, want %q", claims.Operation, ControlOperation)
	}
	if !claims.VerifyAudience(ControlAudience, true) {
		t.Fatalf("audience %v does not include %q", claims.Audience, ControlAudience)
	}
	for _, a := range claims.Audience {
		if a == CapabilityAudience {
			t.Fatalf("control token must not carry the LLM proxy audience: %v", claims.Audience)
		}
	}
	if claims.TaskID != "task-1" || claims.RunnerGeneration != 3 {
		t.Fatalf("control claims task/generation binding missing: %+v", claims)
	}
	if !strings.HasPrefix(token, "ey") {
		t.Fatalf("token is not a JWT: %.20s", token)
	}
}
