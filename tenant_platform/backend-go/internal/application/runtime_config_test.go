package application

import (
	"bytes"
	"encoding/json"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
)

func TestBuildRuntimeConfigPreservesZeroAndLoadsActualGA(t *testing.T) {
	zeroInt := 0
	zeroFloat := 0.0
	stream := true
	provider := domain.LLMProvider{
		ID: 7, Revision: 3, ProviderType: domain.ProviderNativeOAI, Model: "gpt-test",
		APIKey:           "sk-real-upstream-key-must-not-leak",
		APIKeyCiphertext: []byte("ciphertext-must-not-leak"),
		SessionConfig: domain.GASessionConfig{
			MaxRetries: &zeroInt, Temperature: &zeroFloat, Stream: &stream,
		},
	}
	files, err := BuildRuntimeConfig(RuntimeConfigInput{
		Generation: 4, ProxyBaseURL: "http://127.0.0.1:8081",
		RoutingSnapshotID: "snapshot-4",
		Providers:         []RuntimeProviderBinding{{Provider: provider, Token: "capability-token-only"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range [][]byte{[]byte(provider.APIKey), provider.APIKeyCiphertext} {
		if bytes.Contains(files.JSON, forbidden) {
			t.Fatalf("runtime JSON leaked forbidden secret %q", forbidden)
		}
	}

	var document map[string]any
	if err := json.Unmarshal(files.JSON, &document); err != nil {
		t.Fatal(err)
	}
	config, ok := document["platform_native_oai_provider_7_config"].(map[string]any)
	if !ok {
		t.Fatalf("provider config missing: %s", files.JSON)
	}
	if config["max_retries"] != float64(0) || config["temperature"] != float64(0) {
		t.Fatalf("explicit zero lost: %v", config)
	}
	if config["apikey"] != "capability-token-only" {
		t.Fatalf("apikey = %v", config["apikey"])
	}

	configDir := t.TempDir()
	if err := WriteRuntimeConfigAtomic(configDir, files); err != nil {
		t.Fatal(err)
	}
	result := runActualGAConfigProbe(t, configDir, "platform_native_oai_provider_7_config")
	if result.ClassName != "NativeOAISession" || result.MaxRetries != 0 || result.Temperature != 0 {
		t.Fatalf("GA probe = %+v", result)
	}
}

func TestBuildRuntimeConfigCreatesStableMixin(t *testing.T) {
	providers := []RuntimeProviderBinding{
		{Provider: domain.LLMProvider{ID: 8, Revision: 1, ProviderType: domain.ProviderNativeClaude, Model: "claude-test"}, Token: "claude-token"},
		{Provider: domain.LLMProvider{ID: 2, Revision: 1, ProviderType: domain.ProviderNativeOAI, Model: "gpt-test"}, Token: "oai-token"},
	}
	files, err := BuildRuntimeConfig(RuntimeConfigInput{
		Generation: 1, ProxyBaseURL: "http://127.0.0.1:8081",
		RoutingSnapshotID: "snapshot-mixin", Providers: providers,
	})
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]json.RawMessage
	if err := json.Unmarshal(files.JSON, &document); err != nil {
		t.Fatal(err)
	}
	var mixin struct {
		LLMNos []string `json:"llm_nos"`
	}
	if err := json.Unmarshal(document["mixin_config"], &mixin); err != nil {
		t.Fatal(err)
	}
	want := []string{"provider-8", "provider-2"}
	if len(mixin.LLMNos) != len(want) || mixin.LLMNos[0] != want[0] || mixin.LLMNos[1] != want[1] {
		t.Fatalf("llm_nos = %v, want %v", mixin.LLMNos, want)
	}
}

func TestBuildRuntimeConfigRejectsDuplicateProviderAndMissingToken(t *testing.T) {
	provider := domain.LLMProvider{ID: 1, Revision: 1, ProviderType: domain.ProviderNativeOAI, Model: "gpt-test"}
	for name, providers := range map[string][]RuntimeProviderBinding{
		"duplicate":     {{Provider: provider, Token: "a"}, {Provider: provider, Token: "b"}},
		"missing token": {{Provider: provider}},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := BuildRuntimeConfig(RuntimeConfigInput{
				Generation: 1, ProxyBaseURL: "http://127.0.0.1:8081",
				RoutingSnapshotID: "snapshot", Providers: providers,
			})
			if err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
}

type gaConfigProbe struct {
	ClassName   string  `json:"class_name"`
	MaxRetries  int     `json:"max_retries"`
	Temperature float64 `json:"temperature"`
}

func runActualGAConfigProbe(t *testing.T, configDir, variable string) gaConfigProbe {
	t.Helper()
	python, err := exec.LookPath("python")
	if err != nil {
		t.Fatal(err)
	}
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	repoRoot := filepath.Clean(filepath.Join(filepath.Dir(thisFile), "..", "..", "..", ".."))
	script := `
import importlib
import json
import sys
sys.path.insert(0, sys.argv[1])
sys.path.insert(1, sys.argv[2])
mykey = importlib.import_module("mykey")
llmcore = importlib.import_module("llmcore")
name = sys.argv[3]
session = llmcore.resolve_session(name)
print(json.dumps({
    "class_name": session.__class__.__name__,
    "max_retries": session.max_retries,
    "temperature": session.temperature,
}))
`
	command := exec.Command(python, "-c", script, configDir, repoRoot, variable)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("GA config probe failed: %v\n%s", err, output)
	}
	lines := bytes.Split(bytes.TrimSpace(output), []byte{'\n'})
	var result gaConfigProbe
	if err := json.Unmarshal(bytes.TrimSpace(lines[len(lines)-1]), &result); err != nil {
		t.Fatalf("decode GA probe %q: %v", output, err)
	}
	return result
}
