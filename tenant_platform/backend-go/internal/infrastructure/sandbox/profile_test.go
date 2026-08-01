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
