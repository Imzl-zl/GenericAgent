package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/application"
)

func TestBuildDocumentManagerConfigRequiresImmutableRuntimePolicy(t *testing.T) {
	root := canonicalTestDir(t)
	valid := documentManagerOptions{
		databaseURL:         "postgres://example",
		owner:               "manager-1",
		workRoot:            root,
		runtimeBinary:       "docker",
		image:               "alpine@sha256:d9e853e87e55526f6b2917df91a2115c36dd7c696a35be12163d44e6e2a4b6bc",
		seccompProfile:      "builtin",
		uid:                 1000,
		gid:                 1000,
		memoryBytes:         128 << 20,
		cpuPeriod:           100000,
		cpuQuota:            50000,
		pidsLimit:           64,
		tmpfsBytes:          64 << 20,
		deploymentMaxActive: 1,
		claimLease:          time.Second,
		heartbeatInterval:   100 * time.Millisecond,
		pollInterval:        100 * time.Millisecond,
		commandPollInterval: 100 * time.Millisecond,
		shutdownTimeout:     time.Second,
	}
	managerCfg, _, err := buildDocumentManagerConfig(valid)
	if err != nil {
		t.Fatalf("valid config: %v", err)
	}
	if _, ok := managerCfg.Compiler.(application.FixedDocumentOperationCompiler); !ok {
		t.Fatalf("compiler=%T, want FixedDocumentOperationCompiler", managerCfg.Compiler)
	}

	tests := []struct {
		name string
		edit func(*documentManagerOptions)
		want string
	}{
		{"database", func(o *documentManagerOptions) { o.databaseURL = "" }, "database"},
		{"owner", func(o *documentManagerOptions) { o.owner = " " }, "owner"},
		{"tagged image", func(o *documentManagerOptions) { o.image = "alpine:3.20" }, "image"},
		{"root uid", func(o *documentManagerOptions) { o.uid = 0 }, "non-root"},
		{"hard max", func(o *documentManagerOptions) { o.deploymentMaxActive = 0 }, "deployment"},
		{"lease", func(o *documentManagerOptions) { o.claimLease = 0 }, "claim"},
		{"heartbeat", func(o *documentManagerOptions) { o.heartbeatInterval = o.claimLease }, "heartbeat"},
		{"relative root", func(o *documentManagerOptions) { o.workRoot = "relative" }, "work root"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			opts := valid
			tt.edit(&opts)
			_, _, err := buildDocumentManagerConfig(opts)
			if err == nil || !strings.Contains(strings.ToLower(err.Error()), tt.want) {
				t.Fatalf("err=%v want substring %q", err, tt.want)
			}
		})
	}
}

func TestParseDocumentManagerArgsReadsComposeRuntimeFlags(t *testing.T) {
	t.Setenv("DOCUMENT_MANAGER_ALLOW_ROOTFUL_RUNTIME", "true")
	t.Setenv("DOCUMENT_MANAGER_ALLOW_MUTABLE_IMAGE", "true")
	t.Setenv("DOCUMENT_MANAGER_SHUTDOWN_TIMEOUT", "30s")

	opts, err := parseDocumentManagerArgs(nil)
	if err != nil {
		t.Fatal(err)
	}
	if !opts.allowRootfulRuntime || !opts.allowMutableImage {
		t.Fatalf("compose opt-ins not parsed: %+v", opts)
	}
	if opts.shutdownTimeout != 30*time.Second {
		t.Fatalf("shutdown timeout=%s want 30s", opts.shutdownTimeout)
	}
}

func TestParseDocumentManagerArgsUsesEnvDatabaseURL(t *testing.T) {
	root := canonicalTestDir(t)
	t.Setenv("DATABASE_URL", "postgres://from-env")
	opts, err := parseDocumentManagerArgs([]string{
		"--work-root", root,
		"--image", "alpine@sha256:d9e853e87e55526f6b2917df91a2115c36dd7c696a35be12163d44e6e2a4b6bc",
		"--seccomp-profile", "builtin",
	})
	if err != nil {
		t.Fatal(err)
	}
	if opts.databaseURL != "postgres://from-env" || opts.workRoot != root || opts.owner == "" {
		t.Fatalf("opts=%+v", opts)
	}
}

func canonicalTestDir(t *testing.T) string {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Clean(root)
}

func TestMainDoesNotRequireGlobalFlagMutation(t *testing.T) {
	oldArgs := os.Args
	t.Cleanup(func() { os.Args = oldArgs })
	os.Args = []string{"document-manager", "--help"}
	if _, err := parseDocumentManagerArgs([]string{"--help"}); err == nil {
		t.Fatal("expected help to return flag error")
	}
}
