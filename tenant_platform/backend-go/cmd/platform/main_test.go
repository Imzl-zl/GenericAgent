package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/application"
	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/infrastructure/policy"
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

func TestBuildWorkerRuntimeLoopbackDefault(t *testing.T) {
	boot := application.DevBootstrapConfig{LegacyRoot: t.TempDir()}
	rt, err := buildWorkerRuntime(boot)
	if err != nil {
		t.Fatalf("buildWorkerRuntime: %v", err)
	}
	if rt == nil {
		t.Fatal("expected runtime")
	}
}

func TestFinishPlatformWaitsForSchedulerCleanupOnSignal(t *testing.T) {
	done := make(chan error, 1)
	cleaned := make(chan struct{})
	go func() {
		time.Sleep(50 * time.Millisecond)
		close(cleaned)
		done <- context.Canceled
	}()
	if err := finishPlatform(context.Canceled, done, time.Second); err != nil {
		t.Fatal(err)
	}
	select {
	case <-cleaned:
	default:
		t.Fatal("finishPlatform returned before scheduler cleanup")
	}
}
