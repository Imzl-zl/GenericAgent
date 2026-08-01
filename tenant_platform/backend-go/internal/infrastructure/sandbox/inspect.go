package sandbox

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// inspectOutput is the parsed subset of `docker inspect` used for the
// post-create verification (spec §7: 创建后必须校验).
type inspectOutput struct {
	ReadOnlyRootFS  bool
	Privileged      bool
	CapDrop         []string
	NoNewPrivileges bool
	Networks        []string
	Mounts          []string
	User            string
}

// inspectFormat selects only the fields the verification needs.
const inspectFormat = `[{"Config":{"ReadonlyRootfs":{{json .Config.ReadonlyRootfs}},"User":{{json .Config.User}},"Labels":{{json .Config.Labels}}},"HostConfig":{"Privileged":{{json .HostConfig.Privileged}},"CapDrop":{{json .HostConfig.CapDrop}},"SecurityOpt":{{json .HostConfig.SecurityOpt}}},"NetworkSettings":{"Networks":{{json .NetworkSettings.Networks}}},"Mounts":{{json .Mounts}}}]`

// Inspect verifies a created Runner against the fixed profile invariants.
func (d *DockerCLI) Inspect(ctx context.Context, name string) error {
	stdout, stderr, exitCode, err := d.runner.Run(ctx, d.cfg.Binary,
		"inspect", "--format", inspectFormat, name)
	if err != nil {
		return fmt.Errorf("docker inspect runner: %w", err)
	}
	if exitCode != 0 {
		return fmt.Errorf("docker inspect runner %s failed (%d): %s", name, exitCode, strings.TrimSpace(string(stderr)))
	}
	var raw []struct {
		Config struct {
			ReadonlyRootfs bool              `json:"ReadonlyRootfs"`
			User           string            `json:"User"`
			Labels         map[string]string `json:"Labels"`
		} `json:"Config"`
		HostConfig struct {
			Privileged bool     `json:"Privileged"`
			CapDrop    []string `json:"CapDrop"`
			SecurityOpt []string `json:"SecurityOpt"`
		} `json:"HostConfig"`
		NetworkSettings struct {
			Networks map[string]struct{} `json:"Networks"`
		} `json:"NetworkSettings"`
		Mounts []struct {
			Destination string `json:"Destination"`
		} `json:"Mounts"`
	}
	if err := json.Unmarshal(stdout, &raw); err != nil {
		return fmt.Errorf("parse docker inspect output: %w", err)
	}
	if len(raw) != 1 {
		return fmt.Errorf("docker inspect returned %d containers, want 1", len(raw))
	}
	info := raw[0]

	out := inspectOutput{
		ReadOnlyRootFS:  info.Config.ReadonlyRootfs,
		Privileged:      info.HostConfig.Privileged,
		CapDrop:         info.HostConfig.CapDrop,
		NoNewPrivileges: containsSecurityOpt(info.HostConfig.SecurityOpt, "no-new-privileges"),
		User:            info.Config.User,
	}
	for name := range info.NetworkSettings.Networks {
		out.Networks = append(out.Networks, name)
	}
	sort.Strings(out.Networks)
	for _, m := range info.Mounts {
		out.Mounts = append(out.Mounts, m.Destination)
	}
	sort.Strings(out.Mounts)
	return validateInspect(out)
}

// validateInspect enforces the post-create invariants on parsed inspect output.
func validateInspect(info inspectOutput) error {
	if !info.ReadOnlyRootFS {
		return fmt.Errorf("runner root filesystem is not read-only")
	}
	if info.Privileged {
		return fmt.Errorf("runner is privileged")
	}
	if !containsAll(info.CapDrop, "ALL") {
		return fmt.Errorf("runner missing cap_drop ALL: %v", info.CapDrop)
	}
	if !info.NoNewPrivileges {
		return fmt.Errorf("runner missing no-new-privileges")
	}
	if len(info.Networks) != 1 || info.Networks[0] != RunnerNetwork {
		return fmt.Errorf("runner networks = %v, want only %s", info.Networks, RunnerNetwork)
	}
	expectedMounts := []string{LegacyMemoryMount, LegacyTempMount, RunnerStateMount}
	sort.Strings(expectedMounts)
	if len(info.Mounts) != len(expectedMounts) {
		return fmt.Errorf("runner mounts = %v, want exactly %v", info.Mounts, expectedMounts)
	}
	for i := range expectedMounts {
		if info.Mounts[i] != expectedMounts[i] {
			return fmt.Errorf("runner mounts = %v, want exactly %v", info.Mounts, expectedMounts)
		}
	}
	if !isNonRootUser(info.User) {
		return fmt.Errorf("runner user = %q, want non-root uid:gid", info.User)
	}
	return nil
}

// isNonRootUser 校验容器 user 是非 root uid:gid。
func isNonRootUser(user string) bool {
	parts := strings.Split(user, ":")
	if len(parts) != 2 {
		return false
	}
	uid, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || uid <= 0 {
		return false
	}
	gid, err := strconv.ParseInt(parts[1], 10, 64)
	return err == nil && gid > 0
}

func containsSecurityOpt(opts []string, want string) bool {
	for _, o := range opts {
		if strings.Contains(o, want) {
			return true
		}
	}
	return false
}

func containsAll(values []string, want string) bool {
	for _, v := range values {
		if v == want {
			return true
		}
	}
	return false
}
