package application

import (
	"context"
	"os"
	"path/filepath"
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

func TestWriteTokenOnlyRuntimeConfigContainsTokenAndProxyNotRealKey(t *testing.T) {
	dir := t.TempDir()
	const (
		proxyAddr = "http://127.0.0.1:8081"
		realKey   = "sk-real-upstream-key-must-not-leak-1234567890"
		token     = "capability-token-only"
	)
	provider := domain.LLMProvider{
		ID: 1, Revision: 1, ProviderType: domain.ProviderNativeOAI,
		Model: "gpt-test", APIKey: realKey,
	}
	files, err := BuildRuntimeConfig(RuntimeConfigInput{
		Generation: 1, ProxyBaseURL: proxyAddr, RoutingSnapshotID: "snapshot",
		Providers: []RuntimeProviderBinding{{Provider: provider, Token: token}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteRuntimeConfigAtomic(dir, files); err != nil {
		t.Fatal(err)
	}
	runtimeJSON, err := os.ReadFile(filepath.Join(dir, runtimeConfigFilename))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(runtimeJSON), token) || !strings.Contains(string(runtimeJSON), proxyAddr) {
		t.Fatalf("runtime JSON missing token or Proxy: %s", runtimeJSON)
	}
	if strings.Contains(string(runtimeJSON), realKey) {
		t.Fatal("real upstream key leaked into runtime JSON")
	}
	loader, err := os.ReadFile(filepath.Join(dir, myKeyLoaderFilename))
	if err != nil {
		t.Fatal(err)
	}
	if string(loader) != MyKeyLoader || strings.Contains(string(loader), token) {
		t.Fatalf("mykey.py is not the fixed token-free loader: %s", loader)
	}
}

func TestWriteRuntimeConfigOverwritesExistingMyKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, myKeyLoaderFilename)
	if err := os.WriteFile(path, []byte("native_oai_config = {'apikey': 'sk-real-user-key'}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	provider := domain.LLMProvider{ID: 1, Revision: 1, ProviderType: domain.ProviderNativeOAI, Model: "gpt-test"}
	files, err := BuildRuntimeConfig(RuntimeConfigInput{
		Generation: 1, ProxyBaseURL: "http://127.0.0.1:8081", RoutingSnapshotID: "snapshot",
		Providers: []RuntimeProviderBinding{{Provider: provider, Token: "cap-token-overwrite"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteRuntimeConfigAtomic(dir, files); err != nil {
		t.Fatal(err)
	}
	loader, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(loader) != MyKeyLoader || strings.Contains(string(loader), "sk-real-user-key") {
		t.Fatalf("unsafe mykey.py survived: %s", loader)
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
	content, err := os.ReadFile(filepath.Join(dir, runtimeConfigFilename))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(content), "http://127.0.0.1:9999") {
		t.Error("runtime JSON missing Proxy addr")
	}
}
