package sandbox

import (
	"context"
	"errors"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// inspectOutput is the parsed subset of `docker inspect` used for the
// post-create verification (spec §7: 创建后必须校验).
type inspectOutput struct {
	ReadOnlyRootFS  bool
	// Running 是容器 State.Running(审查 R5-I7): 停止/退出的容器不得复用。
	Running         bool
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
	// TmpfsOpts 是 tmpfs 挂载点 → 选项字符串(rw,noexec,nosuid,nodev,...)。
	TmpfsOpts map[string]string
	Labels    map[string]string // com.genericagent.runner.* 归属标签
	// 资源限制(审查 I10: inspect 必须核对 cgroup 限额与附加组)。
	MemoryBytes int64
	CPUQuota    int64
	CPUPeriod   int64
	PIDsLimit   int64
	GroupAdd    []string
	// 补齐校验(审查 I9): CapAdd 必须为空; Env 固定键不得被覆盖;
	// Cmd 必须等于固定监听参数; AppArmor 不得为 unconfined。
	CapAdd          []string
	Env             []string
	Cmd             []string
	AppArmorProfile string
	// HostMounts 是 HostConfig.Mounts 的解析结果(审查 C1): Docker 26+ 的
	// volume-subpath 请求参数(卷名/Target/VolumeOptions.Subpath)只出现在
	// HostConfig.Mounts, 顶层 .Mounts 只有实际挂载结果(不含 Subpath)。
	// subpath 归属校验必须从 HostConfig 读取并按 Target 关联。
	HostMounts []hostMount
}

// hostMount 是 HostConfig.Mounts 中一项挂载的真实形状。
type hostMount struct {
	Type          string
	Source        string // 卷名(volume)/宿主路径(bind)
	Target        string
	ReadOnly      bool
	VolumeSubpath string // 仅 volume-subpath 挂载有值
}

type inspectMount struct {
	Type        string
	Source      string
	Destination string
	RW          bool
	// VolumeSubpath 是 Docker 26+ volume-subpath 挂载的卷内子路径
	// (审查: 必须精确匹配 <workspace-hash>/<sub>, 否则可能挂错工作区)。
	VolumeSubpath string
}

// Inspect 直接解析 docker inspect 的完整 JSON 输出(审查 C1: Docker 模板
// 对 map 字段不支持属性访问, 完整 JSON 解析路径稳定且含 HostConfig.Mounts)。

// ErrRunnerNotRunning 表示容器存在但已停止/退出(审查 R5-I7): EnsureRunner
// 不得复用死容器, 应销毁重建; 其他 inspect 错误仍 fail-closed。
var ErrRunnerNotRunning = errors.New("runner container is not running")

// Inspect verifies a created Runner against the fixed profile invariants:
// image reference, runtime, read-only rootfs, no privileged, cap_drop ALL,
// no-new-privileges, seccomp not disabled, exactly one network, no devices,
// and the exact five workspace mounts (type/source/destination/read-write).
// 注意: 不使用 `docker inspect --format` 模板——Docker 模板对 map 字段
// (Config/HostConfig) 必须用 {{index .Config "key"}} 语法, 属性访问
// .Config.ReadonlyRootfs 会报 "template parsing error"(审查 C1 实证);
// 直接解析 docker inspect 的完整 JSON 输出, 字段路径稳定且含 HostConfig.Mounts。
func (d *DockerCLI) Inspect(ctx context.Context, name string) error {
	stdout, stderr, exitCode, err := d.runner.Run(ctx, d.cfg.Binary, "inspect", name)
	if err != nil {
		return fmt.Errorf("docker inspect runner: %w", err)
	}
	if exitCode != 0 {
		return fmt.Errorf("docker inspect runner %s failed (%d): %s", name, exitCode, strings.TrimSpace(string(stderr)))
	}
	var raw []struct {
		State struct {
			Running bool `json:"Running"`
		} `json:"State"`
		Config struct {
			ReadonlyRootfs bool              `json:"ReadonlyRootfs"`
			User           string            `json:"User"`
			Labels         map[string]string `json:"Labels"`
			Image          string            `json:"Image"`
			Env            []string          `json:"Env"`
			Cmd            []string          `json:"Cmd"`
		} `json:"Config"`
		HostConfig struct {
			Privileged     bool                          `json:"Privileged"`
			CapDrop        []string                      `json:"CapDrop"`
			CapAdd         []string                      `json:"CapAdd"`
			SecurityOpt    []string                      `json:"SecurityOpt"`
			Runtime        string                        `json:"Runtime"`
			ReadonlyRootfs bool                          `json:"ReadonlyRootfs"`
			Devices        []struct{ PathOnHost string } `json:"Devices"`
			Tmpfs          map[string]string             `json:"Tmpfs"`
			Memory         int64                         `json:"Memory"`
			CpuQuota       int64                         `json:"CpuQuota"`
			CpuPeriod      int64                         `json:"CpuPeriod"`
			PidsLimit      int64                         `json:"PidsLimit"`
			GroupAdd       []string                      `json:"GroupAdd"`
			Mounts         []struct {
				Type          string `json:"Type"`
				Source        string `json:"Source"`
				Target        string `json:"Target"`
				ReadOnly      bool   `json:"ReadOnly"`
				VolumeOptions *struct {
					Subpath string `json:"Subpath"`
				} `json:"VolumeOptions"`
			} `json:"Mounts"`
		} `json:"HostConfig"`
		AppArmorProfile string `json:"AppArmorProfile"`
		NetworkSettings struct {
			Networks map[string]struct{} `json:"Networks"`
		} `json:"NetworkSettings"`
		Mounts []struct {
			Type          string `json:"Type"`
			Source        string `json:"Source"`
			Destination   string `json:"Destination"`
			RW            bool   `json:"RW"`
			VolumeOptions *struct {
				Subpath string `json:"Subpath"`
			} `json:"VolumeOptions"`
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
		Running:         info.State.Running,
		Privileged:      info.HostConfig.Privileged,
		CapDrop:         info.HostConfig.CapDrop,
		CapAdd:          info.HostConfig.CapAdd,
		NoNewPrivileges: containsSecurityOpt(info.HostConfig.SecurityOpt, "no-new-privileges"),
		Seccomp:         securityOptValue(info.HostConfig.SecurityOpt, "seccomp="),
		User:            info.Config.User,
		Image:           info.Config.Image,
		Env:             info.Config.Env,
		Cmd:             info.Config.Cmd,
		AppArmorProfile: info.AppArmorProfile,
		Runtime:         info.HostConfig.Runtime,
		Labels:          info.Config.Labels,
		MemoryBytes:     info.HostConfig.Memory,
		CPUQuota:        info.HostConfig.CpuQuota,
		CPUPeriod:       info.HostConfig.CpuPeriod,
		PIDsLimit:       info.HostConfig.PidsLimit,
		GroupAdd:        info.HostConfig.GroupAdd,
		TmpfsOpts:       make(map[string]string),
	}
	for _, hm := range info.HostConfig.Mounts {
		sub := ""
		if hm.VolumeOptions != nil {
			sub = hm.VolumeOptions.Subpath
		}
		out.HostMounts = append(out.HostMounts, hostMount{
			Type:          hm.Type,
			Source:        hm.Source,
			Target:        hm.Target,
			ReadOnly:      hm.ReadOnly,
			VolumeSubpath: sub,
		})
	}
	for name := range info.NetworkSettings.Networks {
		out.Networks = append(out.Networks, name)
	}
	sort.Strings(out.Networks)
	for _, m := range info.Mounts {
		subpath := ""
		if m.VolumeOptions != nil {
			subpath = m.VolumeOptions.Subpath
		}
		out.Mounts = append(out.Mounts, inspectMount{
			Type: m.Type, Source: m.Source, Destination: m.Destination, RW: m.RW,
			VolumeSubpath: subpath,
		})
	}
	// 审查 C1: volume-subpath 的请求参数(卷名/子路径)只出现在
	// HostConfig.Mounts, 顶层 .Mounts 不含 VolumeOptions.Subpath(Docker
	// 29.6.2 实测)。subpath 归属校验必须按 Target 从 HostConfig 关联。
	// 顶层解析保留 VolumeOptions 分支用于兼容旧 Docker(26 之前无
	// volume-subpath; 26+ 顶层该字段为空)——真正取值以 HostMounts 为准,
	// 找不到对应 Target 时留空, 由 validateInspect 的 volume 分支拒绝。
	hostByTarget := make(map[string]string, len(info.HostConfig.Mounts))
	for _, hm := range info.HostConfig.Mounts {
		sub := ""
		if hm.VolumeOptions != nil {
			sub = hm.VolumeOptions.Subpath
		}
		hostByTarget[hm.Target] = sub
	}
	for i := range out.Mounts {
		if out.Mounts[i].Type != "volume" {
			continue
		}
		if sub, ok := hostByTarget[out.Mounts[i].Destination]; ok {
			out.Mounts[i].VolumeSubpath = sub
		}
	}
	sort.Slice(out.Mounts, func(i, j int) bool { return out.Mounts[i].Destination < out.Mounts[j].Destination })
	for _, dev := range info.HostConfig.Devices {
		out.Devices = append(out.Devices, dev.PathOnHost)
	}
	for dest, opts := range info.HostConfig.Tmpfs {
		out.Tmpfs = append(out.Tmpfs, dest)
		out.TmpfsOpts[dest] = opts
	}
	sort.Strings(out.Tmpfs)
	// 审查 C1: 顶层 .Mounts 的解析值(实际挂载结果)保留在 out.Mounts,
	// subpath 关联统一在 validateInspect 的 associateVolumeSubpaths 完成,
	// 保证解析路径与测试直调路径共用同一逻辑。
	return validateInspect(out, d.cfg.Profile, d.cfg.WorkspacesRoot, d.cfg.WorkspaceVolume, d.expectedMountSources())
}

// expectedMountSources 返回每个固定挂载点对应的 workspace 子路径
// (校验 source 尾缀, 防止任意 bind/volume 源被挂到工作区路径)。
func (d *DockerCLI) expectedMountSources() map[string]string {
	return map[string]string{
		LegacyMemoryMount: "memory",
		LegacyTempMount:   "temp",
		RunnerStateMount:  "state",
		RunnerConfigMount: "config",
	}
}

// associateVolumeSubpaths 按 Target 把 HostConfig.Mounts 的 volume-subpath
// 关联到顶层挂载(审查 C1): Docker 26+ 的 volume-subpath 请求参数只在
// HostConfig.Mounts, 顶层 .Mounts 的 VolumeOptions.Subpath 恒为空。
// 返回副本, 不修改调用者。
func associateVolumeSubpaths(info inspectOutput) inspectOutput {
	out := info
	out.Mounts = append([]inspectMount(nil), info.Mounts...)
	hostByTarget := make(map[string]string, len(info.HostMounts))
	for _, hm := range info.HostMounts {
		hostByTarget[hm.Target] = hm.VolumeSubpath
	}
	for i := range out.Mounts {
		if out.Mounts[i].Type != "volume" {
			continue
		}
		if sub, ok := hostByTarget[out.Mounts[i].Destination]; ok {
			out.Mounts[i].VolumeSubpath = sub
		}
	}
	return out
}

// validateInspect enforces the post-create invariants on parsed inspect output.
// mountSubs: destination -> 期望的 workspace 子路径。workspacesRoot 与
// workspaceVolume 用于精确校验 source 归属(审查 I10: 不只查尾缀, 必须
// 匹配当前 workspace hash 的完整路径)。
func validateInspect(info inspectOutput, profile Profile, workspacesRoot, workspaceVolume string, mountSubs map[string]string) error {
	info = associateVolumeSubpaths(info)
	// 审查 R5-I7: 停止/退出(State.Running=false)的容器不得被复用——Runner
	// 崩溃退出后 EnsureRunner 必须销毁重建, 而不是把死容器当作可用 Worker。
	if !info.Running {
		return fmt.Errorf("%w", ErrRunnerNotRunning)
	}
	if !info.ReadOnlyRootFS {
		return fmt.Errorf("runner root filesystem is not read-only")
	}
	if info.Privileged {
		return fmt.Errorf("runner is privileged")
	}
	if !containsAll(info.CapDrop, "ALL") {
		return fmt.Errorf("runner missing cap_drop ALL: %v", info.CapDrop)
	}
	// 补齐校验(审查 I9): 创建时未 cap-add, 任何 CapAdd 都意味着 profile 漂移。
	if len(info.CapAdd) != 0 {
		return fmt.Errorf("runner has unexpected cap_add: %v", info.CapAdd)
	}
	if !info.NoNewPrivileges {
		return fmt.Errorf("runner missing no-new-privileges")
	}
	if info.Seccomp == "unconfined" {
		return fmt.Errorf("runner seccomp is unconfined")
	}
	// AppArmor 显式关闭同样拒绝(缺省 docker-default 可接受)。
	if info.AppArmorProfile == "unconfined" {
		return fmt.Errorf("runner apparmor is unconfined")
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
	// 补齐校验(审查 I9): 固定环境变量必须精确存在(防止被覆盖/缺失导致
	// mTLS 监听或工作区路径漂移); Cmd 必须等于固定监听参数。
	wantEnv := map[string]string{
		"GA_CONFIG_ROOT":      RunnerConfigMount,
		"GA_LEGACY_ROOT":      LegacyRoot,
		"GA_RUNTIME_DIR":      RunnerStateMount,
		"GA_POLICY_FILE":      RunnerConfigMount + "/policy.json",
		"GA_WORKER_LISTEN":    fmt.Sprintf("tcp:0.0.0.0:%d", RunnerControlPort),
		"GA_RUNNER_TLS_CERT":  RunnerConfigMount + "/server.crt",
		"GA_RUNNER_TLS_KEY":   RunnerConfigMount + "/server.key",
		"GA_RUNNER_TLS_CA":    RunnerConfigMount + "/ca.crt",
		"GA_WORKSPACE_MEMORY": LegacyMemoryMount,
		"GA_WORKSPACE_TEMP":   LegacyTempMount,
	}
	env := envMap(info.Env)
	for k, want := range wantEnv {
		if env[k] != want {
			return fmt.Errorf("runner env %s = %q, want %q", k, env[k], want)
		}
	}
	// GA_WORKSPACE_KEY / GA_RUNNER_GENERATION 是 per-request 值, 无法从
	// profile 推导; 至少要求存在且非空, generation 必须与容器 label 一致。
	if env["GA_WORKSPACE_KEY"] == "" || env["GA_RUNNER_GENERATION"] == "" {
		return fmt.Errorf("runner env GA_WORKSPACE_KEY/GA_RUNNER_GENERATION missing")
	}
	if gen, err := strconv.ParseUint(env["GA_RUNNER_GENERATION"], 10, 64); err != nil || gen == 0 {
		return fmt.Errorf("runner env GA_RUNNER_GENERATION invalid: %q", env["GA_RUNNER_GENERATION"])
	} else if info.Labels["com.genericagent.runner.generation"] != env["GA_RUNNER_GENERATION"] {
		return fmt.Errorf("runner env generation %q != label %q", env["GA_RUNNER_GENERATION"], info.Labels["com.genericagent.runner.generation"])
	}
	wantCmd := []string{"--listen", fmt.Sprintf("tcp:0.0.0.0:%d", RunnerControlPort)}
	if len(info.Cmd) != len(wantCmd) {
		return fmt.Errorf("runner cmd = %v, want %v", info.Cmd, wantCmd)
	}
	for i := range wantCmd {
		if info.Cmd[i] != wantCmd[i] {
			return fmt.Errorf("runner cmd = %v, want %v", info.Cmd, wantCmd)
		}
	}
	// 资源限额与附加组(审查 I10): cgroup 限制与共享组必须与固定 profile 一致。
	if profile.MemoryBytes > 0 && info.MemoryBytes != profile.MemoryBytes {
		return fmt.Errorf("runner memory = %d, want %d", info.MemoryBytes, profile.MemoryBytes)
	}
	if profile.CPUQuota > 0 && info.CPUQuota != profile.CPUQuota {
		return fmt.Errorf("runner cpu quota = %d, want %d", info.CPUQuota, profile.CPUQuota)
	}
	if profile.CPUPeriod > 0 && info.CPUPeriod != profile.CPUPeriod {
		return fmt.Errorf("runner cpu period = %d, want %d", info.CPUPeriod, profile.CPUPeriod)
	}
	if profile.PIDsLimit > 0 && info.PIDsLimit != profile.PIDsLimit {
		return fmt.Errorf("runner pids limit = %d, want %d", info.PIDsLimit, profile.PIDsLimit)
	}
	if profile.ShareGID > 0 && !exactlyOneGroup(info.GroupAdd, strconv.Itoa(profile.ShareGID)) {
		return fmt.Errorf("runner shared group must be exactly %d in GroupAdd %v", profile.ShareGID, info.GroupAdd)
	}
	// tmpfs 精确校验(审查): 只能有 /tmp 一个 tmpfs, 且必须含安全选项。
	if len(info.Tmpfs) != 2 || info.Tmpfs[0] != RunnerOverlayMount || info.Tmpfs[1] != "/tmp" {
		return fmt.Errorf("runner tmpfs = %v, want exactly [%s /tmp]", info.Tmpfs, RunnerOverlayMount)
	}
	for _, dest := range []string{"/tmp", RunnerOverlayMount} {
		for _, flag := range []string{"rw", "noexec", "nosuid", "nodev"} {
			if !containsTmpfsFlag(info.TmpfsOpts[dest], flag) {
				return fmt.Errorf("runner tmpfs %s missing %s in opts %q", dest, flag, info.TmpfsOpts[dest])
			}
		}
	}
	// workspace hash 从容器 label 读取(创建时写入), 用于精确校验挂载 source。
	workspaceHash := info.Labels["com.genericagent.runner.hash"]
	if !workspaceHashPattern.MatchString(workspaceHash) {
		return fmt.Errorf("runner missing or invalid workspace hash label: %q", workspaceHash)
	}
	// 固定四个挂载: memory/temp/state 读写, config 只读(审查: attachments
	// 冗余挂载已移除, 附件统一经工作区 temp/——方案 §6)。
	// 注意: info.Mounts 已按 Destination 字典序排序(Inspect 中 sort.Slice),
	// 本表必须保持与排序后完全相同的顺序, 否则真实 docker inspect 解析路径
	// 会误报。字典序: /ga/legacy/memory < /ga/legacy/temp < /ga/runner-config
	// < /ga/runner-state。
	expected := []struct {
		sub, dst string
		ro       bool
	}{
		{"memory", LegacyMemoryMount, false},
		{"temp", LegacyTempMount, false},
		{"config", RunnerConfigMount, true},
		{"state", RunnerStateMount, false},
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
		// source 归属精确校验(审查 I10): bind 必须为
		// <workspacesRoot>/<hash>/<sub>, volume 必须为
		// <workspaceVolume>/_data/<hash>/<sub>(daemon 命名空间)。
		sub := want.sub
		if wantSub, ok := mountSubs[want.dst]; ok {
			sub = wantSub
		}
		source := filepath.ToSlash(got.Source)
		switch got.Type {
		case "bind":
			// bind 挂载: source 必须是 <workspacesRoot>/<hash>/<sub> 精确全路径。
			wantSource := filepath.ToSlash(filepath.Join(workspacesRoot, workspaceHash, sub))
			if source != wantSource {
				return fmt.Errorf("runner mount[%d] %q source = %q, want %q", i, want.dst, source, wantSource)
			}
		case "volume":
			// Docker volume-subpath 挂载的 Source 恒为卷根绝对路径
			// (/var/lib/docker/volumes/<vol>/_data), 不含 subpath。精确校验
			// 卷名归属 + VolumeOptions.Subpath 必须等于 <hash>/<sub>(Docker
			// 26+; 审查: 防挂错工作区 subpath)。
			wantVolume := filepath.ToSlash(filepath.Join("/var/lib/docker/volumes", workspaceVolume, "_data"))
			if workspaceVolume == "" {
				return fmt.Errorf("runner mount[%d] %q is a volume but workspace volume is unset", i, want.dst)
			}
			if !strings.HasSuffix(source, wantVolume) && source != wantVolume {
				return fmt.Errorf("runner mount[%d] %q volume source = %q, want %q", i, want.dst, source, wantVolume)
			}
			wantSubpath := filepath.ToSlash(filepath.Join(workspaceHash, sub))
			if got.VolumeSubpath != wantSubpath {
				return fmt.Errorf("runner mount[%d] %q volume subpath = %q, want %q", i, want.dst, got.VolumeSubpath, wantSubpath)
			}
		default:
			return fmt.Errorf("runner mount[%d] %q type = %q, want bind|volume", i, want.dst, got.Type)
		}
		if got.RW == want.ro {
			state := "rw"
			if want.ro {
				state = "ro"
			}
			return fmt.Errorf("runner mount[%d] %q is %s, want %s", i, want.dst, state, state)
		}
	}
	// user 精确匹配(审查): 固定 UID:GID, 仅"非 root"不足以防属性漂移。
	wantUser := fmt.Sprintf("%d:%d", profile.UID, profile.GID)
	if info.User != wantUser {
		return fmt.Errorf("runner user = %q, want %q", info.User, wantUser)
	}
	return nil
}

// exactlyOneGroup 校验附加组列表恰好包含一个且等于 want。
func exactlyOneGroup(groups []string, want string) bool {
	return len(groups) == 1 && groups[0] == want
}

// containsTmpfsFlag 校验 tmpfs 选项字符串包含指定 flag(rw,noexec,nosuid,
// nodev,size=... 逗号分隔)。
func containsTmpfsFlag(opts, flag string) bool {
	for _, part := range strings.Split(opts, ",") {
		if strings.TrimSpace(part) == flag {
			return true
		}
	}
	return false
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

// envMap 把 docker inspect 的 Env 数组(K=V)转为 map, 供固定环境变量精确校验。
// 重复键按 docker 语义取最后一个(与容器运行时一致)。
func envMap(envs []string) map[string]string {
	out := make(map[string]string, len(envs))
	for _, e := range envs {
		if k, v, ok := strings.Cut(e, "="); ok {
			out[k] = v
		}
	}
	return out
}
