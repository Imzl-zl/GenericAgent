package sandbox

import (
	"testing"
)

func TestProfileRejectsInvalidNetworks(t *testing.T) {
	base := ValidProfile()
	base.Networks = []string{"application", "database"}
	if err := base.Validate(); err == nil {
		t.Fatal("profile with business networks must fail; runner may only join runner-control")
	}
}

func TestProfileAllowsConfiguredNetworkName(t *testing.T) {
	// round11 M2: compose 内部网络带项目名前缀, 部署经 GA_RUNNER_NETWORK 覆盖。
	base := ValidProfile()
	base.Runtime = "runsc"
	base.Networks = []string{"genericagent_runner-control"}
	if err := base.Validate(); err != nil {
		t.Fatalf("project-prefixed network must be accepted: %v", err)
	}
	for _, bad := range []string{"", "runner control", "runner;control", "-runner"} {
		base.Networks = []string{bad}
		if err := base.Validate(); err == nil {
			t.Fatalf("unsafe network name %q must fail", bad)
		}
	}
}

func TestProfileRejectsMountsOutsideWorkspaceSubpaths(t *testing.T) {
	base := ValidProfile()
	base.Mounts = []WorkspaceMount{
		{Source: "/host/anywhere", Destination: "/ga/legacy/memory", ReadOnly: false},
	}
	if err := base.Validate(); err == nil {
		t.Fatal("arbitrary host mounts must fail")
	}
}

func TestProfileRejectsDockerSocketMount(t *testing.T) {
	base := ValidProfile()
	base.Mounts = append(base.Mounts, WorkspaceMount{
		Source: "/var/run/docker.sock", Destination: "/var/run/docker.sock", ReadOnly: false,
	})
	if err := base.Validate(); err == nil {
		t.Fatal("docker socket mount must fail")
	}
}

func TestProfileRejectsPrivilegedOrMissingCapDrop(t *testing.T) {
	base := ValidProfile()
	base.Privileged = true
	if err := base.Validate(); err == nil {
		t.Fatal("privileged runner must fail")
	}

	base = ValidProfile()
	base.CapDrop = []string{}
	if err := base.Validate(); err == nil {
		t.Fatal("missing cap_drop must fail")
	}
}

func TestProfileRejectsNonReadOnlyRootFS(t *testing.T) {
	base := ValidProfile()
	base.ReadOnlyRootFS = false
	if err := base.Validate(); err == nil {
		t.Fatal("writable root fs must fail")
	}
}

func TestProfileRejectsEmptyImageOrUnsafeImageSelection(t *testing.T) {
	base := ValidProfile()
	base.Image = ""
	if err := base.Validate(); err == nil {
		t.Fatal("empty image must fail")
	}

	base = ValidProfile()
	base.Image = "{{user-controlled}}"
	if err := base.Validate(); err == nil {
		t.Fatal("user-controlled image must fail")
	}
}

// TestProfileRuntimeFailClosed(审查 R4-I10): 空运行时(默认 docker)必须
// 显式 AllowRunc 才允许; 未知运行时一律拒绝; runsc 始终允许。
func TestProfileRuntimeFailClosed(t *testing.T) {
	base := ValidProfile()
	if err := base.Validate(); err == nil {
		t.Fatal("empty runtime without AllowRunc must fail (fail-closed)")
	}

	base = ValidProfile()
	base.Runtime = "runc"
	if err := base.Validate(); err == nil {
		t.Fatal("explicit runc without AllowRunc must fail")
	}

	base = ValidProfile()
	base.Runtime = ""
	base.AllowRunc = true
	if err := base.Validate(); err != nil {
		t.Fatalf("empty runtime with AllowRunc must pass: %v", err)
	}

	base = ValidProfile()
	base.Runtime = "runsc"
	if err := base.Validate(); err != nil {
		t.Fatalf("runsc runtime must pass: %v", err)
	}

	base = ValidProfile()
	base.Runtime = "some-other-runtime"
	base.AllowRunc = true
	if err := base.Validate(); err == nil {
		t.Fatal("unknown runtime must fail even with AllowRunc")
	}
}

func TestWorkspaceMountsRejectNonWorkspaceSources(t *testing.T) {
	base := ValidProfile()
	if len(base.WorkspaceSources()) != 3 {
		t.Fatalf("expected 3 workspace sources, got %d", len(base.WorkspaceSources()))
	}
	ok, err := base.IsValidWorkspaceHash("0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")
	if err != nil || !ok {
		t.Fatalf("IsValidWorkspaceHash: ok=%v err=%v", ok, err)
	}
	if _, err := base.IsValidWorkspaceHash("../../etc"); err == nil {
		t.Fatal("path traversal workspace hash must fail")
	}
}
