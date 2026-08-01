package sandbox

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
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
	Binary             string
	Profile            Profile
	WorkspacesRoot     string // host root: workspaces/<hash>/
	ContainerNamePrefix string
}

// RunnerSpec is the validated, server-side derived creation request.
// None of these fields come from business input; only from the authenticated
// workspace_key + Runner lease generation (spec §7).
type RunnerSpec struct {
	WorkspaceHash string // 64-hex, derived from workspace_key
	Generation    uint64
	Image         string // fixed digest
	MemoryTemplate string // 镜像内只读模板路径,空则跳过初始化
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
	return &DockerCLI{cfg: cfg, runner: osCommandRunner{}}, nil
}

// CreateAndStart creates and starts a Runner container with the fixed profile.
// The only mounts are the three workspace subpaths; the container joins only
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

	name := fmt.Sprintf("%s-%s-g%d", d.cfg.ContainerNamePrefix, spec.WorkspaceHash[:12], spec.Generation)
	p := d.cfg.Profile

	// 预置工作区目录(创建容器前),memory 为空时从模板初始化。
	dirs, err := prepareWorkspaceDirs(d.cfg.WorkspacesRoot, spec.WorkspaceHash, spec.MemoryTemplate)
	if err != nil {
		return Runner{}, fmt.Errorf("prepare workspace dirs: %w", err)
	}

	args := []string{
		"create",
		"--name", name,
		"--label", "com.genericagent.runner=true",
		"--label", "com.genericagent.runner.hash=" + spec.WorkspaceHash,
		"--label", "com.genericagent.runner.generation=" + strconv.FormatUint(spec.Generation, 10),
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
		"--mount", "type=bind,source=" + dirs.Memory + ",destination=" + LegacyMemoryMount,
		"--mount", "type=bind,source=" + dirs.Temp + ",destination=" + LegacyTempMount,
		"--mount", "type=bind,source=" + dirs.State + ",destination=" + RunnerStateMount,
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

// ListRunnerContainers 返回本 Manager 创建的 Runner 容器名(按 label 过滤,
// 避免误删其他组件容器)。用于孤儿/空闲回收(spec §7)。
func (d *DockerCLI) ListRunnerContainers(ctx context.Context, namePrefix string) ([]string, error) {
	stdout, stderr, exitCode, err := d.runner.Run(ctx, d.cfg.Binary,
		"ps", "--filter", "label=com.genericagent.runner=true",
		"--filter", "name="+namePrefix, "--format", "{{.Names}}")
	if err != nil {
		return nil, fmt.Errorf("docker ps runners: %w", err)
	}
	if exitCode != 0 {
		return nil, fmt.Errorf("docker ps runners failed (%d): %s", exitCode, strings.TrimSpace(string(stderr)))
	}
	var names []string
	for _, line := range strings.Split(strings.TrimSpace(string(stdout)), "\n") {
		if name := strings.TrimSpace(line); name != "" {
			names = append(names, name)
		}
	}
	return names, nil
}
