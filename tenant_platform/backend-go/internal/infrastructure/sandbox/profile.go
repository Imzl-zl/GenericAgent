// Package sandbox owns the fixed, deployment-reviewed Runner profile and the
// Docker lifecycle (create/inspect/destroy) for workspace-isolated GA Runners
// (spec §7 Runner 安全与生命周期).
//
// The profile is fixed at deployment time: channel messages, user settings and
// Agent tool calls can never select images, mounts, Docker flags, networks or
// Sophub permissions.
package sandbox

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// RunnerNetwork 是 Runner 加入网络的默认名。部署可用 GA_RUNNER_NETWORK
// 覆盖(round11 M2: compose 内部网络带项目名前缀, 多套部署隔离)。
const RunnerNetwork = "runner-control"

// runnerNetworkPattern 校验 docker 网络名(小写字母/数字/下划线/点/连字符,
// 首字符不能是点/连字符)。
var runnerNetworkPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]*$`)

// workspace subpaths mounted into the Runner (spec §4).
const (
	LegacyRoot           = "/ga/legacy" // GA 原生根(进程 workdir, 相对路径基准)
	LegacyMemoryMount    = "/ga/legacy/memory"
	LegacyTempMount      = "/ga/legacy/temp"
	RunnerStateMount     = "/ga/runner-state"
	RunnerConfigMount    = "/ga/runner-config" // 控制面材料(证书/策略), 只读
)

// RunnerControlPort 是 Runner 控制面 mTLS 监听端口(方案 §7)。
const RunnerControlPort = 9443

// RunnerOverlayMount 是 Runner 容器内 overlay(legacy 代码只读副本)的
// tmpfs 挂载点(审查 R4-I13): overlay 不落持久 state 卷, 每次容器启动重新
// 物化, 防止 Runner 篡改副本后经 checkpoint 持久化。
const RunnerOverlayMount = "/ga/overlay"

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
	// ShareGID 是共享卷共享组 gid: 目录/文件属组为 ShareGID(默认 10003),
	// Platform 经 compose group_add 后可与 Runner 读写同一工作区(方案 §7 共享卷)。
	ShareGID int
	// AllowMutableTag 允许非 digest 镜像引用(本地开发); 生产必须固定 digest。
	AllowMutableTag bool
	// AllowRunc 允许以默认 runc 运行时创建 Runner(仅限受信本地开发):
	// 生产(不可信 Runner)必须显式设置 Runtime="runsc"(gVisor); 未设置
	// Runtime 且未声明 AllowRunc 时拒绝启动(fail-closed, 审查 R4-I10)。
	AllowRunc bool
	SeccompProfile  string
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
		ShareGID:        10003,
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
	// Runner 运行时 fail-closed(审查 R4-I10): 不可信生产必须显式 runsc;
	// 空运行时(默认 docker/runc)只允许在显式 AllowRunc 开关下使用, 防止
	// 部署遗漏安全加固时静默降级为普通容器隔离。
	switch p.Runtime {
	case "runsc":
		// ok: gVisor 隔离, 生产推荐。
	case "":
		if !p.AllowRunc {
			return errors.New("runner runtime must be runsc for untrusted production; set AllowRunc (GA_RUNNER_ALLOW_RUNC) only for trusted local dev when using the default docker runtime")
		}
	default:
		return fmt.Errorf("unsupported runner runtime %q (want \"runsc\" or empty)", p.Runtime)
	}
	if strings.ContainsAny(p.Image, "{}${}") || strings.ContainsAny(p.Image, " \t") {
		return fmt.Errorf("unsafe image reference %q", p.Image)
	}
	// 生产必须固定 digest: 可变 tag 会在 Manager 重启/重新拉取后漂移,
	// inspect 校验也将失去可比对基准(方案 §7 固定 profile)。
	if !strings.Contains(p.Image, "@sha256:") && !p.AllowMutableTag {
		return fmt.Errorf("profile image must be a fixed digest reference, got %q (set AllowMutableTag only for local dev)", p.Image)
	}
	if len(p.Networks) != 1 {
		return fmt.Errorf("runner must join exactly one network, got %v", p.Networks)
	}
	if !runnerNetworkPattern.MatchString(p.Networks[0]) {
		return fmt.Errorf("unsafe runner network name %q", p.Networks[0])
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
	if p.ShareGID <= 0 {
		return errors.New("runner share gid must be positive")
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
