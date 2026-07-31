package document

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestDockerCLISmoke(t *testing.T) {
	if os.Getenv("GA_DOCUMENT_DOCKER_SMOKE") != "1" {
		t.Skip("set GA_DOCUMENT_DOCKER_SMOKE=1 to run the rootless document image smoke test")
	}
	image := strings.TrimSpace(os.Getenv("GA_DOCUMENT_SMOKE_IMAGE"))
	if image == "" {
		t.Fatal("GA_DOCUMENT_SMOKE_IMAGE must be the built document image repository@sha256 digest")
	}
	root := t.TempDir()
	name := fmt.Sprintf("ga-document-smoke-%d", os.Getpid())
	slot := filepath.Join(root, name)
	if err := os.Mkdir(slot, 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := DockerConfig{
		Binary:         envOrDefault("GA_DOCUMENT_RUNTIME_BINARY", "docker"),
		Image:          image,
		WorkRoot:       root,
		SeccompProfile: envOrDefault("GA_DOCUMENT_SECCOMP_PROFILE", "builtin"),
		UID:            1000,
		GID:            1000,
		MemoryBytes:    128 << 20,
		CPUPeriod:      100000,
		CPUQuota:       50000,
		PIDsLimit:      64,
		TmpfsBytes:     64 << 20,
		Command:        []string{"/usr/local/bin/ga-document-tool", "idle"},
	}
	runtime, err := NewDockerCLI(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	container, err := runtime.CreateAndStart(ctx, ContainerSpec{Name: name, SlotPath: slot})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cleanupCancel()
		_ = runtime.Destroy(cleanupCtx, name)
	})
	if container.ID == "" {
		t.Fatal("container ID is empty")
	}

	inspected := inspectSmokeContainer(t, ctx, cfg.Binary, name)
	if inspected.Config.User != "1000:1000" || !inspected.HostConfig.ReadonlyRootfs || inspected.HostConfig.NetworkMode != "none" {
		t.Fatalf("identity/rootfs/network=%+v", inspected)
	}
	if !slices.Contains(inspected.HostConfig.CapDrop, "ALL") ||
		!slices.Contains(inspected.HostConfig.SecurityOpt, "no-new-privileges:true") ||
		!slices.Contains(inspected.HostConfig.SecurityOpt, "seccomp="+cfg.SeccompProfile) {
		t.Fatalf("capabilities/security=%+v", inspected.HostConfig)
	}
	if inspected.HostConfig.Memory != cfg.MemoryBytes || inspected.HostConfig.CPUPeriod != cfg.CPUPeriod || inspected.HostConfig.CPUQuota != cfg.CPUQuota || inspected.HostConfig.PIDsLimit != cfg.PIDsLimit {
		t.Fatalf("resource limits=%+v", inspected.HostConfig)
	}
	if inspected.HostConfig.Tmpfs["/tmp"] != "rw,noexec,nosuid,nodev,size=67108864" || len(inspected.Mounts) != 0 {
		t.Fatalf("tmpfs=%+v mounts=%+v", inspected.HostConfig.Tmpfs, inspected.Mounts)
	}

	first, err := runtime.ExecInput(ctx, name, []string{
		"/usr/local/bin/ga-document-tool", "export-docx", "--input", "-", "--output", "-",
	}, []byte(`{"schema_version":1,"title":"Smoke One","content":"first document command"}`), 8<<20)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := zip.NewReader(bytes.NewReader(first.Stdout), int64(len(first.Stdout))); err != nil {
		t.Fatalf("first document output is not a DOCX zip: %v", err)
	}
	second, err := runtime.ExecInput(ctx, name, []string{
		"/usr/local/bin/ga-document-tool", "export-docx", "--input", "-", "--output", "-",
	}, []byte(`{"schema_version":1,"title":"Smoke Two","content":"second command in the same container"}`), 8<<20)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := zip.NewReader(bytes.NewReader(second.Stdout), int64(len(second.Stdout))); err != nil {
		t.Fatalf("second document output is not a DOCX zip: %v", err)
	}
	if _, err := runtime.Exec(ctx, name, []string{"/bin/sh", "-c", "id"}); err == nil {
		t.Fatal("scratch document image unexpectedly contains a shell")
	}
	if err := runtime.Destroy(ctx, name); err != nil {
		t.Fatal(err)
	}
	if err := runtime.Destroy(ctx, name); err != nil {
		t.Fatalf("second destroy was not idempotent: %v", err)
	}
}

type smokeInspect struct {
	Config struct {
		User string `json:"User"`
	} `json:"Config"`
	HostConfig struct {
		ReadonlyRootfs bool              `json:"ReadonlyRootfs"`
		NetworkMode    string            `json:"NetworkMode"`
		CapDrop        []string          `json:"CapDrop"`
		SecurityOpt    []string          `json:"SecurityOpt"`
		Memory         int64             `json:"Memory"`
		CPUPeriod      int64             `json:"CpuPeriod"`
		CPUQuota       int64             `json:"CpuQuota"`
		PIDsLimit      int64             `json:"PidsLimit"`
		Tmpfs          map[string]string `json:"Tmpfs"`
	} `json:"HostConfig"`
	Mounts []struct {
		Source      string `json:"Source"`
		Destination string `json:"Destination"`
		RW          bool   `json:"RW"`
	} `json:"Mounts"`
}

func inspectSmokeContainer(t *testing.T, ctx context.Context, binary, name string) smokeInspect {
	t.Helper()
	result, err := (osCommandRunner{}).Run(ctx, binary, "inspect", name)
	if err != nil {
		t.Fatal(err)
	}
	if result.exitCode != 0 {
		t.Fatal(commandError("inspect smoke container", result))
	}
	var inspected []smokeInspect
	if err := json.Unmarshal(result.stdout, &inspected); err != nil {
		t.Fatal(err)
	}
	if len(inspected) != 1 {
		t.Fatalf("inspect returned %d containers", len(inspected))
	}
	return inspected[0]
}

func envOrDefault(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}
