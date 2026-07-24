package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/policy"
)

func TestResolvePolicyPathSurvivesWorkerWorkingDirectoryChange(t *testing.T) {
	originalWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	policyRelative := filepath.Join("..", "..", "..", "contracts", "policy", "foundation.v1.json")
	resolved, err := resolvePolicyPath(policyRelative)
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(resolved) {
		t.Fatalf("resolved policy path is not absolute: %s", resolved)
	}
	if err := os.Chdir(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(originalWD) })
	if _, err := policy.LoadRegistry(resolved); err != nil {
		t.Fatalf("load after worker cwd change: %v", err)
	}
}
