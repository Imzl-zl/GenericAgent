package sandbox

import (
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
	WorkspaceHash  string            // 64-hex, derived from workspace_key
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
	dirs, err := prepareWorkspaceDirs(d.cfg.WorkspacesRoot, spec.WorkspaceHash, spec.MemoryTemplate, p.UID, p.GID)
	if err != nil {
		return Runner{}, fmt.Errorf("prepare workspace dirs: %w", err)
	}
	// 控制面材料(短期 mTLS 证书/策略清单)原子写入 config/ 并只读挂载。
	if err := writeConfigFiles(d.cfg.WorkspacesRoot, spec.WorkspaceHash, spec.ConfigFiles, p.UID, p.GID); err != nil {
		return Runner{}, fmt.Errorf("write runner config files: %w", err)
	}

	args := []string{
		"create",
		"--name", name,
		"--label", "com.genericagent.runner=true",
		"--label", "com.genericagent.runner.hash=" + spec.WorkspaceHash,
		"--label", "com.genericagent.runner.generation=" + strconv.FormatUint(spec.Generation, 10),
		"--label", "com.genericagent.runner.created=" + strconv.FormatInt(time.Now().Unix(), 10),
		"--read-only",
		"--network", RunnerNetwork,
		"--cap-drop", "ALL",
		"--security-opt", "no-new-privileges:true",
		"--user", strconv.Itoa(p.UID) + ":" + strconv.Itoa(p.GID),
		"--memory", strconv.FormatInt(p.MemoryBytes, 10),
		"--cpu-period", strconv.FormatInt(p.CPUPeriod, 10),
		"--cpu-quota", strconv.FormatInt(p.CPUQuota, 10),
		"--pids-limit", strconv.FormatInt(p.PIDsLimit, 10),
		"--tmpfs", "/tmp:rw,noexec,nosuid,nodev,size=64m",
		"--workdir", LegacyTempMount,
	}
	// 工作区挂载: Compose 部署用 named volume + volume-subpath(daemon 可解析),
	// 裸机用 bind source(Manager 与 daemon 同主机时等价)。
	subpaths := []struct {
		sub, dst string
		ro       bool
	}{
		{"memory", LegacyMemoryMount, false},
		{"temp", LegacyTempMount, false},
		{"state", RunnerStateMount, false},
		{"config", RunnerConfigMount, true},
		{"attachments", RunnerAttachmentsMount, true},
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
	)
	for _, e := range spec.Env {
		if strings.TrimSpace(e) == "" || strings.Contains(e, "\x00") {
			return Runner{}, fmt.Errorf("invalid runner env entry")
		}
		args = append(args, "--env", e)
	}
	if p.Runtime != "" {
		args = append(args, "--runtime", p.Runtime)
	}
	if p.SeccompProfile != "" {
		args = append(args, "--security-opt", "seccomp="+p.SeccompProfile)
	}
	args = append(args, image)

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
		return Runner{}, fmt.Errorf("docker start runner %s failed (%d): %s", name, exitCode, strings.TrimSpace(string(stderr)))
	}
	return Runner{ContainerID: containerID, Name: name}, nil
}

// RunnerName 返回 workspace hash + generation 的确定性容器名。Platform 与
// Manager 各自独立推导同一名字, 用于证书 SAN 与拨号地址(方案 §7)。
func (d *DockerCLI) RunnerName(workspaceHash string, generation uint64) string {
	return fmt.Sprintf("%s-%s-g%d", d.cfg.ContainerNamePrefix, workspaceHash[:12], generation)
}

// Destroy removes the Runner container (workspace data is never removed).
func (d *DockerCLI) Destroy(ctx context.Context, name string) error {
	_, stderr, exitCode, err := d.runner.Run(ctx, d.cfg.Binary, "rm", "-f", name)
	if err != nil {
		return fmt.Errorf("docker rm runner: %w", err)
	}
	if exitCode != 0 {
		return fmt.Errorf("docker rm runner %s failed (%d): %s", name, exitCode, strings.TrimSpace(string(stderr)))
	}
	return nil
}

// ListRunnerContainers 返回本 Manager 创建的 Runner 容器(label 过滤,
// 避免误删其他组件容器)及其状态。用于孤儿回收(spec §7)。
func (d *DockerCLI) ListRunnerContainers(ctx context.Context, namePrefix string) ([]RunnerInfo, error) {
	stdout, stderr, exitCode, err := d.runner.Run(ctx, d.cfg.Binary,
		"ps", "-a",
		"--filter", "label=com.genericagent.runner=true",
		"--filter", "name="+namePrefix,
		"--format", "{{.Names}}\t{{.State}}\t{{.Label \"com.genericagent.runner.created\"}}")
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
		infos = append(infos, info)
	}
	return infos, nil
}

// RunnerInfo 是孤儿回收需要的容器状态摘要。
type RunnerInfo struct {
	Name      string
	Running   bool
	CreatedAt time.Time
}
