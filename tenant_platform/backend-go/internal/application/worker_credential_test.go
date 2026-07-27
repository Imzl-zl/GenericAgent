package application

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/infrastructure/llmproxy"
)

type fakeLLMProviderSource struct{}

func (fakeLLMProviderSource) GetDefaultProvider(ctx context.Context) (domain.LLMProvider, error) {
	return domain.LLMProvider{
		ID:           1,
		Revision:     1,
		ProviderType: domain.ProviderNativeOAI,
		Model:        "gpt-test",
	}, nil
}

func TestWriteTokenOnlyMyKey_ContainsTokenAndProxyNotRealKey(t *testing.T) {
	dir := t.TempDir()
	const (
		proxyAddr  = "http://127.0.0.1:8081"
		realKey    = "sk-real-upstream-key-must-not-leak-1234567890"
		signingKey = "test-signing-key-at-least-32-bytes"
	)
	issuer, err := llmproxy.NewIssuer([]byte(signingKey), 3600)
	if err != nil {
		t.Fatal(err)
	}
	token, _, err := issuer.Issue(llmproxy.CapabilitySpec{
		SessionKey: "session-xyz", ProviderID: 1, ProviderRevision: 1,
		ProviderType: domain.ProviderNativeOAI, Model: "gpt-test",
		PolicyVersion: "foundation.no-host-tools.v1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := writeTokenOnlyMyKey(dir, proxyAddr, token); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "mykey.py")
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	s := string(content)
	if !strings.Contains(s, token) {
		t.Error("mykey.py missing capability_token")
	}
	if !strings.Contains(s, proxyAddr) {
		t.Error("mykey.py missing Proxy addr")
	}
	if strings.Contains(s, realKey) {
		t.Fatal("REAL UPSTREAM KEY LEAKED INTO mykey.py")
	}
	if strings.Contains(s, signingKey) {
		t.Fatal("HMAC signing key leaked into mykey.py")
	}
	if !strings.Contains(s, "platform-default") {
		t.Error("mykey.py missing platform-default name marker")
	}
	// Validate file mode (POSIX only; Windows ignores WriteFile perm bits).
	if runtime.GOOS != "windows" {
		if info, err := os.Stat(path); err == nil && info.Mode().Perm() != 0o600 {
			t.Errorf("mykey.py mode=%o want 0600", info.Mode().Perm())
		}
	}
}

func TestWriteTokenOnlyMyKey_OverwritesExistingFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mykey.py")
	// Pre-existing user-provided mykey.py with a real key (security red line:
	// the platform MUST overwrite this, never trust it).
	if err := os.WriteFile(path, []byte("native_oai_config = {'apikey': 'sk-real-user-key'}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeTokenOnlyMyKey(dir, "http://127.0.0.1:8081", "cap-token-overwrite"); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	s := string(content)
	if strings.Contains(s, "sk-real-user-key") {
		t.Fatal("platform failed to overwrite user-provided mykey.py (real key survived)")
	}
	if !strings.Contains(s, "cap-token-overwrite") {
		t.Error("mykey.py was not overwritten with capability_token")
	}
}

func TestScheduler_IssueAndWriteCredential_NoIssuerReturnsEmpty(t *testing.T) {
	s := &scheduler{}
	jti, err := s.issueAndWriteCredential(context.Background(), "session-1")
	if err != nil {
		t.Fatal(err)
	}
	if jti != "" {
		t.Fatalf("jti=%q want empty when TokenIssuer is nil", jti)
	}
}

func TestScheduler_IssueAndWriteCredential_WritesTokenAndReturnsJTI(t *testing.T) {
	dir := t.TempDir()
	issuer, err := llmproxy.NewIssuer([]byte("test-signing-key-at-least-32-bytes"), 3600)
	if err != nil {
		t.Fatal(err)
	}
	s := &scheduler{cfg: SchedulerConfig{
		TokenIssuer:        issuer,
		LLMProvider:        fakeLLMProviderSource{},
		LLMProxyAddr:       "http://127.0.0.1:9999",
		ConfigRoot:         dir,
		ModelPolicyVersion: "test.v1",
	}}
	jti, err := s.issueAndWriteCredential(context.Background(), "session-issue-test")
	if err != nil {
		t.Fatal(err)
	}
	if jti == "" {
		t.Fatal("jti empty")
	}
	content, err := os.ReadFile(filepath.Join(dir, "mykey.py"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(content), "http://127.0.0.1:9999") {
		t.Error("mykey.py missing Proxy addr")
	}
}
