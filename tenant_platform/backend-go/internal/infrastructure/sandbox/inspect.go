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
	Seccomp         string // "" = docker 默认 seccomp; "unconfined" 被拒绝
	Networks        []string
	Mounts          []inspectMount
	User            string
	Image           string // 创建时使用的镜像引用(Config.Image)
	Runtime         string // HostConfig.Runtime("" = 默认 runc)
	Devices         []string
	Tmpfs           []string
}

type inspectMount struct {
	Type        string
	Source      string
	Destination string
	RW          bool
}

// inspectFormat selects only the fields the verification needs.
const inspectFormat = `[{"Config":{"ReadonlyRootfs":{{json .Config.ReadonlyRootfs}},"User":{{json .Config.User}},"Labels":{{json .Config.Labels}},"Image":{{json .Config.Image}}},"HostConfig":{"Privileged":{{json .HostConfig.Privileged}},"CapDrop":{{json .HostConfig.CapDrop}},"SecurityOpt":{{json .HostConfig.SecurityOpt}},"Runtime":{{json .HostConfig.Runtime}},"ReadonlyRootfs":{{json .HostConfig.ReadonlyRootfs}},"Devices":{{json .HostConfig.Devices}},"Tmpfs":{{json .HostConfig.Tmpfs}}},"NetworkSettings":{"Networks":{{json .NetworkSettings.Networks}}},"Mounts":{{json .Mounts}}}]`

// Inspect verifies a created Runner against the fixed profile invariants:
// image reference, runtime, read-only rootfs, no privileged, cap_drop ALL,
// no-new-privileges, seccomp not disabled, exactly one network, no devices,
// and the exact five workspace mounts (type/source/destination/read-write).
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
			Image          string            `json:"Image"`
		} `json:"Config"`
		HostConfig struct {
			Privileged      bool     `json:"Privileged"`
			CapDrop         []string `json:"CapDrop"`
			SecurityOpt     []string `json:"SecurityOpt"`
			Runtime         string   `json:"Runtime"`
			ReadonlyRootfs  bool     `json:"ReadonlyRootfs"`
			Devices         []struct{ PathOnHost string } `json:"Devices"`
			Tmpfs           map[string]string `json:"Tmpfs"`
		} `json:"HostConfig"`
		NetworkSettings struct {
			Networks map[string]struct{} `json:"Networks"`
		} `json:"NetworkSettings"`
		Mounts []struct {
			Type        string `json:"Type"`
			Source      string `json:"Source"`
			Destination string `json:"Destination"`
			RW          bool   `json:"RW"`
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
		ReadOnlyRootFS:  info.HostConfig.ReadonlyRootfs,
		Privileged:      info.HostConfig.Privileged,
		CapDrop:         info.HostConfig.CapDrop,
		NoNewPrivileges: containsSecurityOpt(info.HostConfig.SecurityOpt, "no-new-privileges"),
		Seccomp:         securityOptValue(info.HostConfig.SecurityOpt, "seccomp="),
		User:            info.Config.User,
		Image:           info.Config.Image,
		Runtime:         info.HostConfig.Runtime,
	}
	for name := range info.NetworkSettings.Networks {
		out.Networks = append(out.Networks, name)
	}
	sort.Strings(out.Networks)
	for _, m := range info.Mounts {
		out.Mounts = append(out.Mounts, inspectMount{
			Type: m.Type, Source: m.Source, Destination: m.Destination, RW: m.RW,
		})
	}
	sort.Slice(out.Mounts, func(i, j int) bool { return out.Mounts[i].Destination < out.Mounts[j].Destination })
	for _, dev := range info.HostConfig.Devices {
		out.Devices = append(out.Devices, dev.PathOnHost)
	}
	for dest := range info.HostConfig.Tmpfs {
		out.Tmpfs = append(out.Tmpfs, dest)
	}
	sort.Strings(out.Tmpfs)
	return validateInspect(out, d.cfg.Profile)
}

// validateInspect enforces the post-create invariants on parsed inspect output.
func validateInspect(info inspectOutput, profile Profile) error {
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
	if info.Seccomp == "unconfined" {
		return fmt.Errorf("runner seccomp is unconfined")
	}
	if len(info.Networks) != 1 || info.Networks[0] != RunnerNetwork {
		return fmt.Errorf("runner networks = %v, want only %s", info.Networks, RunnerNetwork)
	}
	// 镜像引用必须与部署配置完全一致(拒绝 tag 漂移/替换)。
	if info.Image != profile.Image {
		return fmt.Errorf("runner image = %q, want %q", info.Image, profile.Image)
	}
	// Runtime: profile 指定(如 runsc)必须精确匹配; 未指定时接受空或默认 runc。
	wantRuntime := profile.Runtime
	if wantRuntime == "" {
		wantRuntime = "runc"
	}
	if info.Runtime != wantRuntime {
		return fmt.Errorf("runner runtime = %q, want %q", info.Runtime, wantRuntime)
	}
	if len(info.Devices) != 0 {
		return fmt.Errorf("runner devices = %v, want none", info.Devices)
	}
	// 固定五个挂载: memory/temp/state 读写, config/attachments 只读。
	expected := []struct {
		sub, dst string
		ro       bool
	}{
		{"memory", LegacyMemoryMount, false},
		{"temp", LegacyTempMount, false},
		{"state", RunnerStateMount, false},
		{"config", RunnerConfigMount, true},
		{"attachments", RunnerAttachmentsMount, true},
	}
	if len(info.Mounts) != len(expected) {
		return fmt.Errorf("runner mounts = %d, want exactly %d: %+v", len(info.Mounts), len(expected), info.Mounts)
	}
	for i, want := range expected {
		got := info.Mounts[i]
		if got.Destination != want.dst {
			return fmt.Errorf("runner mount[%d] destination = %q, want %q", i, got.Destination, want.dst)
		}
		if got.Type == "" || got.Source == "" {
			return fmt.Errorf("runner mount[%d] %q missing type/source", i, want.dst)
		}
		if got.RW == want.ro {
			state := "rw"
			if want.ro {
				state = "ro"
			}
			return fmt.Errorf("runner mount[%d] %q is %s, want %s", i, want.dst, state, state)
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

// securityOptValue 返回匹配前缀的 security-opt 值(如 seccomp=xxx 的 xxx)。
func securityOptValue(opts []string, prefix string) string {
	for _, o := range opts {
		if strings.HasPrefix(o, prefix) {
			return strings.TrimPrefix(o, prefix)
		}
	}
	return ""
}

func containsAll(values []string, want string) bool {
	for _, v := range values {
		if v == want {
			return true
		}
	}
	return false
}
