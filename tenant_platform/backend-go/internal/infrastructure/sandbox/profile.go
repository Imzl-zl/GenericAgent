// Package sandbox owns the fixed, deployment-reviewed Runner profile and the
// Docker lifecycle (create/inspect/destroy) for workspace-isolated GA Runners
// (spec §7 Runner 安全与生命周期).
//
// The profile is fixed at deployment time: channel messages, user settings and
// Agent tool calls can never select images, mounts, Docker flags, networks or
// Sophub permissions.
package sandbox

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// RunnerNetwork is the only network a Runner joins.
const RunnerNetwork = "runner-control"

// workspace subpaths mounted into the Runner (spec §4).
const (
	LegacyMemoryMount = "/ga/legacy/memory"
	LegacyTempMount   = "/ga/legacy/temp"
	RunnerStateMount  = "/ga/runner-state"
)

// WorkspaceMount is one deterministic mount derived from the workspace key.
type WorkspaceMount struct {
	Source      string // host path: workspaces/<hash>/<subpath>
	Destination string // fixed container path
	ReadOnly    bool
}

var workspaceHashPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)

// Profile is the fixed Runner profile. It is parameterised by deployment env
// only; business inputs can never alter it.
type Profile struct {
	Image         string // fixed digest reference
	Runtime       string // "" (docker) or "runsc"
	Networks      []string
	Mounts        []WorkspaceMount
	ReadOnlyRootFS bool
	CapDrop       []string
	NoNewPrivileges bool
	Privileged    bool
	MemoryBytes   int64
	CPUPeriod     int64
	CPUQuota      int64
	PIDsLimit     int64
	UID           int
	GID           int
	SeccompProfile string
}

// ValidProfile returns a profile with the mandatory hardened defaults.
func ValidProfile() Profile {
	return Profile{
		Image:           "ga-runner@sha256:0000000000000000000000000000000000000000000000000000000000000000",
		Networks:        []string{RunnerNetwork},
		ReadOnlyRootFS:  true,
		CapDrop:         []string{"ALL"},
		NoNewPrivileges: true,
		MemoryBytes:     1 << 30, // 1 GiB (GA_RUNNER_MEMORY_BYTES)
		CPUPeriod:       100000,
		CPUQuota:        100000,
		PIDsLimit:       128,
		UID:             10002,
		GID:             10002,
	}
}

// WorkspaceSources returns the three deterministic workspace subpath sources.
func (p Profile) WorkspaceSources() []string {
	return []string{"memory", "temp", "state"}
}

// IsValidWorkspaceHash reports whether hash is a 64-hex workspace dir hash and
// cannot traverse outside the workspaces root.
func (p Profile) IsValidWorkspaceHash(hash string) (bool, error) {
	if !workspaceHashPattern.MatchString(hash) {
		return false, fmt.Errorf("invalid workspace hash %q", hash)
	}
	return true, nil
}

// Validate enforces the fixed profile invariants (spec §7).
func (p Profile) Validate() error {
	if strings.TrimSpace(p.Image) == "" {
		return errors.New("profile image is required")
	}
	if strings.ContainsAny(p.Image, "{}${}") || strings.ContainsAny(p.Image, " \t") {
		return fmt.Errorf("unsafe image reference %q", p.Image)
	}
	if !containsExactly(p.Networks, RunnerNetwork) {
		return fmt.Errorf("runner must join exactly %s, got %v", RunnerNetwork, p.Networks)
	}
	if p.Privileged {
		return errors.New("privileged runners are forbidden")
	}
	if !p.ReadOnlyRootFS {
		return errors.New("runner root filesystem must be read-only")
	}
	if len(p.CapDrop) == 0 {
		return errors.New("cap_drop must be set (ALL recommended)")
	}
	if p.UID <= 0 || p.GID <= 0 {
		return errors.New("runner uid/gid must be non-root")
	}
	if p.MemoryBytes <= 0 || p.PIDsLimit <= 0 {
		return errors.New("memory and pids limits must be positive")
	}
	if p.CPUPeriod <= 0 || p.CPUQuota <= 0 {
		return errors.New("cpu period/quota must be positive")
	}
	for _, m := range p.Mounts {
		switch m.Destination {
		case LegacyMemoryMount, LegacyTempMount, RunnerStateMount:
			// ok: only the three workspace subpaths
		default:
			return fmt.Errorf("mount destination %q is not a workspace subpath", m.Destination)
		}
		if !strings.HasPrefix(m.Source, "workspaces/") && !strings.Contains(m.Source, "/workspaces/") {
			return fmt.Errorf("mount source %q must live under workspaces/", m.Source)
		}
	}
	return nil
}

// WorkspaceDirHash mirrors domain.WorkspaceDirHash (kept local to avoid a
// domain dependency from infrastructure; both are SHA-256 of the key).
func WorkspaceDirHash(workspaceKey string) string {
	sum := sha256.Sum256([]byte(workspaceKey))
	return hex.EncodeToString(sum[:])
}

func containsExactly(values []string, want string) bool {
	if len(values) != 1 {
		return false
	}
	return values[0] == want
}
