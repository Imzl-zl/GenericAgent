package sandbox

import (
	"errors"
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// commandRunner abstracts docker binary invocation for tests.
type commandRunner interface {
	Run(ctx context.Context, binary string, args ...string) ([]byte, []byte, int, error)
}

type osCommandRunner struct{}

func (osCommandRunner) Run(ctx context.Context, binary string, args ...string) ([]byte, []byte, int, error) {
	cmd := exec.CommandContext(ctx, binary, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	exitCode := 0
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else {
			return nil, nil, -1, err
		}
	}
	return stdout.Bytes(), stderr.Bytes(), exitCode, nil
}

// DockerConfig is the fixed deployment-time Runner configuration.
type DockerConfig struct {
	Binary              string
	Profile             Profile
	WorkspacesRoot      string // daemon-visible workspace root: workspaces/<hash>/
	ContainerNamePrefix string
	// ManagerID 是 sandbox-manager 实例标识, 写入 Runner label 供孤儿回收归属判断。
	ManagerID string
	// WorkspaceVolume 是 daemon 命名空间内的 named volume 名(Compose 部署);
	// 非空时用 volume-subpath 挂载工作区子目录, 空则按 WorkspacesRoot 用
	// bind mount(裸机部署, Manager 与 daemon 同主机)。
	WorkspaceVolume string
}

// RunnerSpec is the validated, server-side derived creation request.
// None of these fields come from business input; only from the authenticated
// Platform control plane (workspace_key + Runner lease generation) and the
// fixed deployment profile (spec §7).
type RunnerSpec struct {
	WorkspaceKey   string // 控制面 workspace key(注入容器身份 env)
	WorkspaceHash  string // 64-hex, derived from workspace_key
	Generation     uint64
	Image          string            // fixed digest
	MemoryTemplate string            // 镜像内只读模板路径,空则跳过初始化
	Env            []string          // 控制面透传环境(LLM proxy 地址等; 安全参数由 Manager 固定)
	ConfigFiles    map[string][]byte // 写入 config/ 的文件: 证书/策略(名称必须为安全 basename)
}

// Runner is a created Runner container handle.
type Runner struct {
	ContainerID string
	Name        string
}

// DockerCLI owns docker create/start/inspect/destroy for Runners.
type DockerCLI struct {
	cfg    DockerConfig
	runner commandRunner
}

// NewDockerCLI validates config and returns a CLI.
func NewDockerCLI(cfg DockerConfig) (*DockerCLI, error) {
	if err := cfg.Profile.Validate(); err != nil {
		return nil, fmt.Errorf("invalid runner profile: %w", err)
	}
	if strings.TrimSpace(cfg.Binary) == "" {
		cfg.Binary = "docker"
	}
	if strings.TrimSpace(cfg.WorkspacesRoot) == "" {
		return nil, fmt.Errorf("DockerConfig.WorkspacesRoot is required")
	}
	if strings.TrimSpace(cfg.ContainerNamePrefix) == "" {
		cfg.ContainerNamePrefix = "ga-runner"
	}
	return &DockerCLI{cfg: cfg, runner: osCommandRunner{}}, nil
}

// CreateAndStart creates and starts a Runner container with the fixed profile.
// The only mounts are the five workspace subpaths; the container joins only
// runner-control; docker.sock is never mounted.
// protectedRunnerEnvKeys 是 Manager 固定的容器环境变量集合(方案 §7:
// 安全参数由 Manager 固定)。控制面透传 env 不得覆盖这些键——Docker 对
// 重复 --env 以最后一个为准, 若允许透传覆盖, 空值 GA_RUNNER_TLS_* 会让
// Worker 以 insecure gRPC 监听, 破坏 mTLS 控制面(审查)。
var protectedRunnerEnvKeys = map[string]struct{}{
	"GA_CONFIG_ROOT":       {},
	"GA_LEGACY_ROOT":       {},
	"GA_RUNTIME_DIR":       {},
	"GA_POLICY_FILE":       {},
	"GA_WORKER_LISTEN":     {},
	"GA_RUNNER_TLS_CERT":   {},
	"GA_RUNNER_TLS_KEY":    {},
	"GA_RUNNER_TLS_CA":     {},
	"GA_WORKSPACE_MEMORY":  {},
	"GA_WORKSPACE_TEMP":    {},
	"GA_WORKSPACE_KEY":     {},
	"GA_RUNNER_GENERATION": {},
}

// runnerEnvKey 返回 env 条目 "K=V" 的键部分(无 '=' 的条目视为非法)。
func runnerEnvKey(e string) (string, error) {
	k, _, ok := strings.Cut(e, "=")
	if !ok || strings.TrimSpace(k) == "" {
		return "", fmt.Errorf("runner env entry %q is not K=V", e)
	}
	return k, nil
}

func (d *DockerCLI) CreateAndStart(ctx context.Context, spec RunnerSpec) (Runner, error) {
	if ok, err := d.cfg.Profile.IsValidWorkspaceHash(spec.WorkspaceHash); !ok {
		return Runner{}, fmt.Errorf("invalid workspace hash: %w", err)
	}
	if spec.Generation == 0 {
		return Runner{}, fmt.Errorf("runner generation must be positive")
	}
	if err := d.cfg.Profile.Validate(); err != nil {
		return Runner{}, fmt.Errorf("invalid runner profile: %w", err)
	}
	if spec.Image != "" && strings.ContainsAny(spec.Image, "{}${} \t") {
		return Runner{}, fmt.Errorf("unsafe image reference %q", spec.Image)
	}
	image := spec.Image
	if image == "" {
		image = d.cfg.Profile.Image
	}

	name := d.RunnerName(spec.WorkspaceHash, spec.Generation)
	p := d.cfg.Profile

	// 预置工作区目录(创建容器前),memory 为空时从模板初始化。
	dirs, err := prepareWorkspaceDirs(d.cfg.WorkspacesRoot, spec.WorkspaceHash, spec.MemoryTemplate, p.UID, p.GID, p.ShareGID)
	if err != nil {
		return Runner{}, fmt.Errorf("prepare workspace dirs: %w", err)
	}
	// 控制面材料(短期 mTLS 证书/策略清单)原子写入 config/g<generation>
	// 并只读挂载(审查 C1/I6: 按 generation 隔离, 旧代清理不误删新配置)。
	if err := writeConfigFiles(d.cfg.WorkspacesRoot, spec.WorkspaceHash, spec.Generation, spec.ConfigFiles, p.UID, p.GID, p.ShareGID); err != nil {
		return Runner{}, fmt.Errorf("write runner config files: %w", err)
	}

	args := []string{
		"create",
		"--name", name,
		"--label", "com.genericagent.runner=true",
		"--label", "com.genericagent.runner.hash=" + spec.WorkspaceHash,
		"--label", "com.genericagent.runner.generation=" + strconv.FormatUint(spec.Generation, 10),
		"--label", "com.genericagent.runner.created=" + strconv.FormatInt(time.Now().Unix(), 10),
		// 审查 F7: 容器写入 Manager 实例 label, 销毁/复用前按归属校验, 防止
		// 持有控制面凭据的受损 Platform 删除其他部署中命名规则相同的容器。
		"--label", "com.genericagent.runner.manager=" + d.cfg.ManagerID,
		"--read-only",
		"--network", p.Networks[0],
		"--cap-drop", "ALL",
		"--security-opt", "no-new-privileges:true",
		"--user", strconv.Itoa(p.UID) + ":" + strconv.Itoa(p.GID),
		"--group-add", strconv.Itoa(p.ShareGID),
		"--memory", strconv.FormatInt(p.MemoryBytes, 10),
		"--cpu-period", strconv.FormatInt(p.CPUPeriod, 10),
		"--cpu-quota", strconv.FormatInt(p.CPUQuota, 10),
		"--pids-limit", strconv.FormatInt(p.PIDsLimit, 10),
		"--tmpfs", "/tmp:rw,noexec,nosuid,nodev,size=64m",
		// 审查 R4-I13: overlay(legacy 代码副本)放入容器 tmpfs, 不落持久
		// state 卷——每次容器启动重新物化并做 digest 校验, 防止 Runner 在
		// 会话内篡改副本后经 checkpoint 持久化。tmpfs 大小时 128m(模板+
		// assets 约数十 MB)。
		"--tmpfs", RunnerOverlayMount+":rw,noexec,nosuid,nodev,size=128m",
		// 进程 workdir 必须是 GA 根(/ga/legacy), 使 handler.cwd='./temp' 解析
		// 到工作区 temp(审查: 之前用 temp 挂载点做 workdir 会让相对路径
		// 错位到 temp/temp, 破坏 GA 原生路径语义)。
		"--workdir", LegacyRoot,
	}
	// 工作区挂载: Compose 部署用 named volume + volume-subpath(daemon 可解析),
	// 裸机用 bind source(Manager 与 daemon 同主机时等价)。
	// 审查 C3: state/committed 与 state/results 以只读子挂载遮蔽顶层 rw
	// state 挂载, Runner 不得删除/替换已提交快照与结果文件(Platform 写
	// committed/results 不受影响——挂载 ro 是容器侧视图)。
	subpaths := []struct {
		sub, dst string
		ro       bool
	}{
		{"memory", LegacyMemoryMount, false},
		{"temp", LegacyTempMount, false},
		{"state", RunnerStateMount, false},
		{"state/committed", RunnerStateMount + "/committed", true},
		{"state/results", RunnerStateMount + "/results", true},
		{"config/g" + strconv.FormatUint(spec.Generation, 10), RunnerConfigMount, true},
	}
	for _, m := range subpaths {
		mount := m
		var flag string
		if d.cfg.WorkspaceVolume != "" {
			flag = fmt.Sprintf("type=volume,source=%s,destination=%s,volume-subpath=%s/%s",
				d.cfg.WorkspaceVolume, mount.dst, spec.WorkspaceHash, mount.sub)
		} else {
			flag = fmt.Sprintf("type=bind,source=%s,destination=%s",
				filepath.Join(dirs.Workspace, mount.sub), mount.dst)
		}
		if mount.ro {
			flag += ",readonly"
		}
		args = append(args, "--mount", flag)
	}
	// 固定运行环境(方案 §7): Worker 必需变量 + 容器内 mTLS 监听参数。
	// 安全参数由 Manager 固定, 业务环境(LLM/Sophub 代理地址)由控制面透传。
	args = append(args,
		"--env", "GA_CONFIG_ROOT="+RunnerConfigMount,
		"--env", "GA_LEGACY_ROOT=/ga/legacy",
		"--env", "GA_RUNTIME_DIR="+RunnerStateMount,
		"--env", "GA_POLICY_FILE="+RunnerConfigMount+"/policy.json",
		"--env", "GA_WORKER_LISTEN=tcp:0.0.0.0:9443",
		"--env", "GA_RUNNER_TLS_CERT="+RunnerConfigMount+"/server.crt",
		"--env", "GA_RUNNER_TLS_KEY="+RunnerConfigMount+"/server.key",
		"--env", "GA_RUNNER_TLS_CA="+RunnerConfigMount+"/ca.crt",
		"--env", "GA_WORKSPACE_MEMORY="+LegacyMemoryMount,
		"--env", "GA_WORKSPACE_TEMP="+LegacyTempMount,
		"--env", "GA_OVERLAY_ROOT="+RunnerOverlayMount,
	)
	// 容器不可变身份(方案 §7): workspace key 与 lease generation 由 Manager
	// 固定注入, Worker 校验 StartSession/ExecuteTask 请求必须与之匹配, 防
	// 止误路由/迟到的控制面请求在错误工作区挂载中执行。值来自已认证的
	// Platform 控制面, 不含 '=' / NUL / 换行, 可安全作为 --env K=V。
	if strings.ContainsAny(spec.WorkspaceKey, "=\x00\n") {
		return Runner{}, fmt.Errorf("unsafe workspace key in runner env")
	}
	args = append(args,
		"--env", "GA_WORKSPACE_KEY="+spec.WorkspaceKey,
		"--env", "GA_RUNNER_GENERATION="+strconv.FormatUint(spec.Generation, 10),
	)
	for _, e := range spec.Env {
		if strings.TrimSpace(e) == "" || strings.Contains(e, "\x00") {
			return Runner{}, fmt.Errorf("invalid runner env entry")
		}
		// fail-closed(审查): 拒绝覆盖固定安全变量, 而不是依赖追加顺序。
		key, err := runnerEnvKey(e)
		if err != nil {
			return Runner{}, err
		}
		if _, protected := protectedRunnerEnvKeys[key]; protected {
			return Runner{}, fmt.Errorf("runner env entry %q overrides fixed security variable %s", e, key)
		}
		args = append(args, "--env", e)
	}
	if p.Runtime != "" {
		args = append(args, "--runtime", p.Runtime)
	}
	if p.SeccompProfile != "" {
		args = append(args, "--security-opt", "seccomp="+p.SeccompProfile)
	}
	// 显式命令参数覆盖镜像 CMD: 固定 mTLS TCP 监听(镜像默认 unix socket
	// 仅供本地冒烟), 与 Platform 拨号端点 runner-control 内 DNS:9443 一致。
	args = append(args, image, "--listen", "tcp:0.0.0.0:"+strconv.Itoa(RunnerControlPort))

	stdout, stderr, exitCode, err := d.runner.Run(ctx, d.cfg.Binary, args...)
	if err != nil {
		return Runner{}, fmt.Errorf("docker create runner: %w", err)
	}
	if exitCode != 0 {
		return Runner{}, fmt.Errorf("docker create runner failed (%d): %s", exitCode, strings.TrimSpace(string(stderr)))
	}
	containerID := strings.TrimSpace(string(stdout))
	if containerID == "" {
		return Runner{}, fmt.Errorf("docker create returned empty container id")
	}

	_, stderr, exitCode, err = d.runner.Run(ctx, d.cfg.Binary, "start", name)
	if err != nil || exitCode != 0 {
		// 审查 R5-I7: create 成功但 start 失败时, 立即按容器 ID 清理已创建
		// 容器(best-effort, 与主错误合并)——否则遗留 Created/stopped 容器,
		// 同 generation 的后续 Ensure 可能把它误当可复用 Runner。
		rmErr := d.destroyByID(ctx, containerID)
		return Runner{}, errors.Join(
			fmt.Errorf("docker start runner %s failed (%d): %s", name, exitCode, strings.TrimSpace(string(stderr))),
			rmErr,
		)
	}
	return Runner{ContainerID: containerID, Name: name}, nil
}

// destroyByID 按容器 ID 删除容器(best-effort): 已不存在视为成功。
func (d *DockerCLI) destroyByID(ctx context.Context, containerID string) error {
	if strings.TrimSpace(containerID) == "" {
		return nil
	}
	_, stderr, exitCode, err := d.runner.Run(ctx, d.cfg.Binary, "rm", "-f", containerID)
	if err != nil {
		return fmt.Errorf("docker rm runner %s: %w", containerID, err)
	}
	if exitCode != 0 && !strings.Contains(string(stderr), "No such container") {
		return fmt.Errorf("docker rm runner %s failed (%d): %s", containerID, exitCode, strings.TrimSpace(string(stderr)))
	}
	return nil
}

// EnsureWorkspace 幂等地预置 workspace 目录布局并修复 ownership(方案 §6)。
// 只创建目录结构与 setgid/共享组权限, 不种 memory 模板(模板初始化仍由
// CreateAndStart 首次创建 Runner 时完成, 避免并发初始化竞态)。
func (d *DockerCLI) EnsureWorkspace(ctx context.Context, workspaceHash string) error {
	_ = ctx
	if ok, err := d.cfg.Profile.IsValidWorkspaceHash(workspaceHash); !ok {
		return fmt.Errorf("invalid workspace hash: %w", err)
	}
	p := d.cfg.Profile
	if _, err := prepareWorkspaceDirs(d.cfg.WorkspacesRoot, workspaceHash, "", p.UID, p.GID, p.ShareGID); err != nil {
		return fmt.Errorf("ensure workspace dirs: %w", err)
	}
	return nil
}

// RunnerName 返回 workspace hash + generation 的确定性容器名。Platform 与
// Manager 各自独立推导同一名字, 用于证书 SAN 与拨号地址(方案 §7)。
func (d *DockerCLI) RunnerName(workspaceHash string, generation uint64) string {
	return fmt.Sprintf("%s-%s-g%d", d.cfg.ContainerNamePrefix, workspaceHash[:12], generation)
}

// Destroy removes the Runner container (workspace data is never removed).
// 容器已不存在时幂等成功(审查 F6): 重复销毁/并发清理不得报错, 调用方的
// 缓存清理依赖此幂等性。
func (d *DockerCLI) Destroy(ctx context.Context, name string) error {
	_, stderr, exitCode, err := d.runner.Run(ctx, d.cfg.Binary, "rm", "-f", name)
	if err != nil {
		return fmt.Errorf("docker rm runner: %w", err)
	}
	if exitCode != 0 {
		if strings.Contains(string(stderr), "No such container") {
			return nil // 幂等: 容器已不存在
		}
		return fmt.Errorf("docker rm runner %s failed (%d): %s", name, exitCode, strings.TrimSpace(string(stderr)))
	}
	return nil
}

// IsManagerRunner 校验容器带 com.genericagent.runner=true 且 manager label
// 匹配本 Manager 实例(审查 F7)。ManagerID 为空(未配置)时仅校验 runner
// label。容器不存在或无法读取时返回 (false, nil)。
func (d *DockerCLI) IsManagerRunner(ctx context.Context, idOrName string) (bool, error) {
	format := `{{index .Config.Labels "com.genericagent.runner"}}|{{index .Config.Labels "com.genericagent.runner.manager"}}`
	stdout, _, exitCode, err := d.runner.Run(ctx, d.cfg.Binary, "inspect", "--format", format, idOrName)
	if err != nil || exitCode != 0 {
		return false, nil // 不存在或无法读取: 一律视为未归属
	}
	parts := strings.SplitN(strings.TrimSpace(string(stdout)), "|", 2)
	if len(parts) != 2 || parts[0] != "true" {
		return false, nil
	}
	if d.cfg.ManagerID != "" && parts[1] != d.cfg.ManagerID {
		return false, nil
	}
	return true, nil
}

// RunnerWorkspaceHash 返回容器 label 中的 workspace hash(审查 R5-C6:
// 销毁路径定位 config/ 清理目标)。容器不存在或 label 缺失返回 ok=false。
func (d *DockerCLI) RunnerWorkspaceHash(ctx context.Context, idOrName string) (string, bool, error) {
	format := `{{index .Config.Labels "com.genericagent.runner.hash"}}`
	stdout, _, exitCode, err := d.runner.Run(ctx, d.cfg.Binary, "inspect", "--format", format, idOrName)
	if err != nil || exitCode != 0 {
		return "", false, nil // 不存在或无法读取: 视为无法定位
	}
	hash := strings.TrimSpace(string(stdout))
	if !workspaceHashPattern.MatchString(hash) {
		return "", false, nil
	}
	return hash, true, nil
}

// RunnerGenerationLabel 返回容器 label 中的 runner generation(审查
// Round8: 按容器 ID 销毁时从 label 恢复 generation, 否则 config/g<gen>
// 清理因无法定位 generation 而跳过, 短期 mTLS 材料残留)。容器不存在或
// label 缺失返回 ok=false。
func (d *DockerCLI) RunnerGenerationLabel(ctx context.Context, idOrName string) (uint64, bool, error) {
	format := `{{index .Config.Labels "com.genericagent.runner.generation"}}`
	stdout, _, exitCode, err := d.runner.Run(ctx, d.cfg.Binary, "inspect", "--format", format, idOrName)
	if err != nil || exitCode != 0 {
		return 0, false, nil // 不存在或无法读取: 视为无法定位
	}
	gen, err := strconv.ParseUint(strings.TrimSpace(string(stdout)), 10, 64)
	if err != nil || gen == 0 {
		return 0, false, nil
	}
	return gen, true, nil
}

// ContainerExists 返回容器(按 ID 或名称)是否存在。
func (d *DockerCLI) ContainerExists(ctx context.Context, idOrName string) (bool, error) {
	_, _, exitCode, err := d.runner.Run(ctx, d.cfg.Binary, "inspect", idOrName)
	if err != nil {
		return false, err
	}
	return exitCode == 0, nil
}

// IsRunnerContainer 校验容器(按 ID 或名称)带 com.genericagent.runner=true
// label。用于销毁前的归属校验: 只有本 Manager 创建的容器可被 rm。
func (d *DockerCLI) IsRunnerContainer(ctx context.Context, idOrName string) (bool, error) {
	stdout, _, exitCode, err := d.runner.Run(ctx, d.cfg.Binary,
		"inspect", "--format", `{{index .Config.Labels "com.genericagent.runner"}}`, idOrName)
	if err != nil || exitCode != 0 {
		return false, nil // 不存在或无法读取: 一律拒绝
	}
	return strings.TrimSpace(string(stdout)) == "true", nil
}

// ListRunnerContainers 返回本 Manager 创建的 Runner 容器(label 过滤,
// 避免误删其他组件容器)及其状态。用于孤儿回收(spec §7)。ManagerID
// 非空时额外按实例 label 过滤(审查 F7: 共享 daemon 的多部署互不可见)。
func (d *DockerCLI) ListRunnerContainers(ctx context.Context, namePrefix string) ([]RunnerInfo, error) {
	filters := []string{
		"label=com.genericagent.runner=true",
		"name=" + namePrefix,
	}
	if d.cfg.ManagerID != "" {
		filters = append(filters, "label=com.genericagent.runner.manager="+d.cfg.ManagerID)
	}
	args := []string{"ps", "-a"}
	for _, f := range filters {
		args = append(args, "--filter", f)
	}
	args = append(args,
		"--format", "{{.Names}}\t{{.State}}\t{{.Label \"com.genericagent.runner.created\"}}\t{{.Label \"com.genericagent.runner.hash\"}}\t{{.Label \"com.genericagent.runner.generation\"}}")
	stdout, stderr, exitCode, err := d.runner.Run(ctx, d.cfg.Binary, args...)
	if err != nil {
		return nil, fmt.Errorf("docker ps runners: %w", err)
	}
	if exitCode != 0 {
		return nil, fmt.Errorf("docker ps runners failed (%d): %s", exitCode, strings.TrimSpace(string(stderr)))
	}
	var infos []RunnerInfo
	for _, line := range strings.Split(strings.TrimSpace(string(stdout)), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		parts := strings.Split(line, "\t")
		info := RunnerInfo{Name: strings.TrimSpace(parts[0]), Running: len(parts) > 1 && parts[1] == "running"}
		if len(parts) > 2 {
			if secs, parseErr := strconv.ParseInt(strings.TrimSpace(parts[2]), 10, 64); parseErr == nil && secs > 0 {
				info.CreatedAt = time.Unix(secs, 0)
			}
		}
		// round11 审查(I6): 附带 workspace hash 与 generation label, 供
		// 孤儿 config 目录对账(容器已销毁的 config/g<gen> 短期凭据清理)。
		if len(parts) > 3 {
			info.WorkspaceHash = strings.TrimSpace(parts[3])
		}
		if len(parts) > 4 {
			if g, parseErr := strconv.ParseUint(strings.TrimSpace(parts[4]), 10, 64); parseErr == nil {
				info.Generation = g
			}
		}
		infos = append(infos, info)
	}
	return infos, nil
}

// RunnerInfo 是孤儿回收需要的容器状态摘要。
type RunnerInfo struct {
	Name      string
	Running   bool
	CreatedAt time.Time
	// WorkspaceHash/Generation 是容器 label 中的工作区与 lease generation
	// (round11 审查 I6): 供孤儿 config 目录对账判定容器归属。
	WorkspaceHash string
	Generation    uint64
}
