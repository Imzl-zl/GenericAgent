package policy

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func foundationPolicyPath(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	// .../backend-go/internal/infrastructure/policy/registry_test.go -> tenant_platform/contracts/policy
	root := filepath.Clean(filepath.Join(filepath.Dir(file), "..", "..", "..", ".."))
	p := filepath.Join(root, "contracts", "policy", "foundation.v1.json")
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("foundation policy missing at %s: %v", p, err)
	}
	return p
}

func TestLoadRegistry_FoundationDigestAndResolve(t *testing.T) {
	path := foundationPolicyPath(t)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	wantDigest := "sha256:" + hex.EncodeToString(sum[:])

	reg, err := LoadRegistry(path)
	if err != nil {
		t.Fatalf("LoadRegistry: %v", err)
	}
	if reg.Digest() != wantDigest {
		t.Fatalf("digest=%q want %q", reg.Digest(), wantDigest)
	}
	pol, err := reg.Resolve("foundation.v1", "foundation.no-host-tools.v1")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if pol.Version != "foundation.no-host-tools.v1" {
		t.Fatalf("version=%q", pol.Version)
	}
	if len(pol.AllowedTools) != 1 || pol.AllowedTools[0] != "update_working_checkpoint" {
		t.Fatalf("allowed_tools=%v", pol.AllowedTools)
	}
}

func TestLoadRegistry_RejectsUnknownAndCrossCapability(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "policy.json")
	content := `{
  "schema_version": "genericagent.capability-policy.v1",
  "capabilities": {
    "foundation.v1": {
      "tool_policies": {
        "foundation.no-host-tools.v1": { "allowed_tools": ["update_working_checkpoint"] }
      }
    },
    "other.v1": {
      "tool_policies": {
        "other.host-tools.v1": { "allowed_tools": ["shell"] }
      }
    }
  }
}`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	reg, err := LoadRegistry(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reg.Resolve("foundation.v1", "missing"); err == nil {
		t.Fatal("expected unknown tool policy error")
	}
	if _, err := reg.Resolve("foundation.v1", "other.host-tools.v1"); err == nil {
		t.Fatal("expected cross-capability error")
	} else if !strings.Contains(err.Error(), "belongs to capability") {
		t.Fatalf("cross-capability error wording: %v", err)
	}
	if _, err := reg.Resolve("nope.v1", "foundation.no-host-tools.v1"); err == nil {
		t.Fatal("expected unknown capability")
	}
}

func TestLoadRegistry_RejectsMalformed(t *testing.T) {
	dir := t.TempDir()
	cases := map[string]string{
		"bad-schema.json": `{"schema_version":"nope","capabilities":{"c":{"tool_policies":{"p":{"allowed_tools":["a"]}}}}}`,
		"empty-allow.json": `{
  "schema_version": "genericagent.capability-policy.v1",
  "capabilities": {"c":{"tool_policies":{"p":{"allowed_tools":[]}}}}
}`,
		"dup-policy.json": `{
  "schema_version": "genericagent.capability-policy.v1",
  "capabilities": {
    "a":{"tool_policies":{"same":{"allowed_tools":["x"]}}},
    "b":{"tool_policies":{"same":{"allowed_tools":["y"]}}}
  }
}`,
		"empty-name.json": `{
  "schema_version": "genericagent.capability-policy.v1",
  "capabilities": {"":{"tool_policies":{"p":{"allowed_tools":["x"]}}}}
}`,
	}
	for name, body := range cases {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadRegistry(p); err == nil {
			t.Fatalf("%s: expected error", name)
		}
	}
	if _, err := LoadRegistry(filepath.Join(dir, "missing.json")); err == nil {
		t.Fatal("missing file should fail")
	}
}
