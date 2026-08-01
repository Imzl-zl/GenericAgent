package document

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

const (
	documentToolExecutable       = "/usr/local/bin/ga-document-tool"
	documentToolIdleCommand      = "idle"
	containerWorkdir             = "/workspace"
	dockerCLIOutputLimit         = 64 * 1024
	maxDocumentCommandInputBytes = 2 * 1024 * 1024
	maxDocumentArtifactBytes     = 8 * 1024 * 1024
	cleanupTimeout               = 10 * time.Second
	managerLabel                 = "com.genericagent.document-manager"
	instanceLabel                = "com.genericagent.document.instance"
	dockerHostInfoFormat         = `{"SecurityOptions":{{json .SecurityOptions}},"CgroupVersion":{{json .CgroupVersion}},"MemoryLimit":{{json .MemoryLimit}},"CpuCfsPeriod":{{json .CPUCfsPeriod}},"CpuCfsQuota":{{json .CPUCfsQuota}},"PidsLimit":{{json .PidsLimit}}}`
	podmanHostInfoFormat         = `{"cgroupsVersion":{{json .Host.CgroupsVersion}},"cgroupControllers":{{json .Host.CgroupControllers}},"security":{{json .Host.Security}}}`
	ownershipInspectFormat       = `{"ID":{{json .Id}},"Labels":{{json .Config.Labels}}}`
)

var (
	fixedImagePattern                = regexp.MustCompile(`^[a-z0-9][a-z0-9._/:\-]*@sha256:[a-f0-9]{64}$`)
	mutableLocalImagePattern         = regexp.MustCompile(`^genericagent-document-tool:[a-z0-9][a-z0-9._-]{0,127}$`)
	containerNamePattern             = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$`)
	containerIDPattern               = regexp.MustCompile(`^[a-f0-9]{12,64}$`)
	errDocumentCommandOutputTooLarge = errors.New("document command stdout exceeded limit")
)

type DockerConfig struct {
	Binary              string
	Image               string
	WorkRoot            string
	SeccompProfile      string
	UID                 int
	GID                 int
	MemoryBytes         int64
	CPUPeriod           int64
	CPUQuota            int64
	PIDsLimit           int64
	TmpfsBytes          int64
	Command             []string
	AllowRootfulRuntime bool
	AllowMutableImage   bool
}

type commandResult struct {
	stdout   []byte
	stderr   []byte
	exitCode int
}

type commandRunner interface {
	Run(context.Context, string, ...string) (commandResult, error)
	RunInput(context.Context, string, []byte, int, ...string) (commandResult, error)
}

type DockerCLI struct {
	cfg      DockerConfig
	workRoot string
	runner   commandRunner
	podman   bool
}

func NewDockerCLI(cfg DockerConfig) (*DockerCLI, error) {
	return newDockerCLI(cfg, osCommandRunner{})
}

func newDockerCLI(cfg DockerConfig, runner commandRunner) (*DockerCLI, error) {
	if runner == nil {
		return nil, fmt.Errorf("docker command runner is nil")
	}
	cfg.Binary = strings.TrimSpace(cfg.Binary)
	binaryName := strings.ToLower(filepath.Base(cfg.Binary))
	if binaryName != "docker" && binaryName != "docker.exe" && binaryName != "podman" && binaryName != "podman.exe" {
		return nil, fmt.Errorf("runtime binary must be docker or podman")
	}
	if (!fixedImagePattern.MatchString(cfg.Image) || imageReferenceHasTag(cfg.Image)) &&
		(!cfg.AllowMutableImage || !mutableLocalImagePattern.MatchString(cfg.Image)) {
		return nil, fmt.Errorf("image must be pinned as untagged repository@sha256:<64 lowercase hex>; the only mutable opt-in is genericagent-document-tool:<tag>")
	}
	if err := validateSeccompProfile(cfg.SeccompProfile); err != nil {
		return nil, err
	}
	if cfg.UID <= 0 || cfg.GID <= 0 {
		return nil, fmt.Errorf("container UID and GID must be non-root")
	}
	if cfg.MemoryBytes <= 0 || cfg.CPUPeriod <= 0 || cfg.CPUQuota <= 0 || cfg.PIDsLimit <= 0 || cfg.TmpfsBytes <= 0 {
		return nil, fmt.Errorf("memory, CPU, PID, and tmpfs limits must be positive")
	}
	if !slices.Equal(cfg.Command, []string{documentToolExecutable, documentToolIdleCommand}) {
		return nil, fmt.Errorf("container command must be the fixed document tool idle process")
	}
	if !filepath.IsAbs(cfg.WorkRoot) || filepath.Clean(cfg.WorkRoot) != cfg.WorkRoot {
		return nil, fmt.Errorf("work root must be an absolute clean path")
	}
	if strings.Contains(cfg.WorkRoot, ",") {
		return nil, fmt.Errorf("work root must not contain a comma")
	}
	info, err := os.Lstat(cfg.WorkRoot)
	if err != nil {
		return nil, fmt.Errorf("stat work root: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return nil, fmt.Errorf("work root must be a real directory, not a symlink")
	}
	canonicalRoot, err := filepath.EvalSymlinks(cfg.WorkRoot)
	if err != nil {
		return nil, fmt.Errorf("canonicalize work root: %w", err)
	}
	if filepath.Clean(canonicalRoot) != cfg.WorkRoot {
		return nil, fmt.Errorf("work root must already be canonical")
	}
	cfg.Command = append([]string(nil), cfg.Command...)
	isPodman := binaryName == "podman" || binaryName == "podman.exe"
	return &DockerCLI{cfg: cfg, workRoot: canonicalRoot, runner: runner, podman: isPodman}, nil
}

func (d *DockerCLI) VerifyHost(ctx context.Context) error {
	if d.podman {
		return d.verifyPodmanHost(ctx)
	}
	return d.verifyDockerHost(ctx)
}

func (d *DockerCLI) verifyDockerHost(ctx context.Context) error {
	result, err := d.runner.Run(ctx, d.cfg.Binary, "info", "--format", dockerHostInfoFormat)
	if err != nil {
		return fmt.Errorf("inspect Docker host security: %w", err)
	}
	if result.exitCode != 0 {
		return commandError("inspect Docker host security", result)
	}
	var info struct {
		SecurityOptions []string `json:"SecurityOptions"`
		CgroupVersion   string   `json:"CgroupVersion"`
		MemoryLimit     bool     `json:"MemoryLimit"`
		CPUPeriod       bool     `json:"CpuCfsPeriod"`
		CPUQuota        bool     `json:"CpuCfsQuota"`
		PIDsLimit       bool     `json:"PidsLimit"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(result.stdout), &info); err != nil {
		return fmt.Errorf("decode Docker host info: %w", err)
	}
	var rootless, confinedSeccomp bool
	for _, option := range info.SecurityOptions {
		rootless = rootless || option == "rootless" || strings.HasPrefix(option, "name=rootless")
		if strings.HasPrefix(option, "name=seccomp") && !strings.Contains(strings.ToLower(option), "profile=unconfined") {
			confinedSeccomp = true
		}
	}
	if !rootless && !d.cfg.AllowRootfulRuntime {
		return fmt.Errorf("container runtime is not rootless")
	}
	if !confinedSeccomp {
		return fmt.Errorf("container runtime does not report a confined seccomp profile")
	}
	if info.CgroupVersion != "2" {
		return fmt.Errorf("container runtime requires cgroup v2")
	}
	if !info.MemoryLimit || !info.CPUPeriod || !info.CPUQuota || !info.PIDsLimit {
		return fmt.Errorf("container runtime does not enforce required memory, CPU, and PID controllers")
	}
	return nil
}

func (d *DockerCLI) verifyPodmanHost(ctx context.Context) error {
	result, err := d.runner.Run(ctx, d.cfg.Binary, "info", "--format", podmanHostInfoFormat)
	if err != nil {
		return fmt.Errorf("inspect Podman host security: %w", err)
	}
	if result.exitCode != 0 {
		return commandError("inspect Podman host security", result)
	}
	var info struct {
		CgroupsVersion    string   `json:"cgroupsVersion"`
		CgroupControllers []string `json:"cgroupControllers"`
		Security          struct {
			Rootless       bool `json:"rootless"`
			SeccompEnabled bool `json:"seccompEnabled"`
		} `json:"security"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(result.stdout), &info); err != nil {
		return fmt.Errorf("decode Podman host info: %w", err)
	}
	if !info.Security.Rootless {
		return fmt.Errorf("container runtime is not rootless")
	}
	if !info.Security.SeccompEnabled {
		return fmt.Errorf("container runtime does not report seccomp enforcement")
	}
	if info.CgroupsVersion != "v2" && info.CgroupsVersion != "2" {
		return fmt.Errorf("container runtime requires cgroup v2")
	}
	for _, controller := range []string{"cpu", "memory", "pids"} {
		if !slices.Contains(info.CgroupControllers, controller) {
			return fmt.Errorf("container runtime does not delegate %s cgroup controller", controller)
		}
	}
	return nil
}

func (d *DockerCLI) CreateAndStart(ctx context.Context, spec ContainerSpec) (Container, error) {
	slotPath, err := d.validateFreshSlot(spec)
	if err != nil {
		return Container{}, err
	}
	if err := d.VerifyHost(ctx); err != nil {
		return Container{}, err
	}
	if err := d.verifyImage(ctx); err != nil {
		return Container{}, err
	}
	args := []string{
		"create",
		"--name", spec.Name,
		"--label", managerLabel + "=true",
		"--label", instanceLabel + "=" + spec.Name,
		"--read-only",
		"--network", "none",
		"--cap-drop", "ALL",
		"--security-opt", "no-new-privileges:true",
		"--security-opt", "seccomp=" + d.cfg.SeccompProfile,
		"--user", strconv.Itoa(d.cfg.UID) + ":" + strconv.Itoa(d.cfg.GID),
		"--memory", strconv.FormatInt(d.cfg.MemoryBytes, 10),
		"--cpu-period", strconv.FormatInt(d.cfg.CPUPeriod, 10),
		"--cpu-quota", strconv.FormatInt(d.cfg.CPUQuota, 10),
		"--pids-limit", strconv.FormatInt(d.cfg.PIDsLimit, 10),
		"--tmpfs", "/tmp:rw,noexec,nosuid,nodev,size=" + strconv.FormatInt(d.cfg.TmpfsBytes, 10),
		"--workdir", containerWorkdir,
		"--entrypoint", documentToolExecutable,
		"--log-driver", "none",
		d.cfg.Image,
		documentToolIdleCommand,
	}
	created, runErr := d.runner.Run(ctx, d.cfg.Binary, args...)
	if runErr != nil {
		return Container{}, d.failWithOwnedCleanup(spec.Name, fmt.Errorf("create document container: %w", runErr))
	}
	if created.exitCode != 0 {
		return Container{}, d.failWithOwnedCleanup(spec.Name, commandError("create document container", created))
	}
	if len(bytes.TrimSpace(created.stderr)) != 0 {
		return Container{}, d.failWithOwnedCleanup(spec.Name, fmt.Errorf("create document container emitted a runtime warning: %s", strings.TrimSpace(string(created.stderr))))
	}
	containerID := strings.TrimSpace(string(created.stdout))
	if containerID == "" {
		return Container{}, d.failWithOwnedCleanup(spec.Name, fmt.Errorf("create document container returned an empty ID"))
	}
	if err := d.verifyCreatedPolicy(ctx, spec.Name, slotPath); err != nil {
		return Container{}, d.failWithOwnedCleanup(spec.Name, err)
	}
	started, runErr := d.runner.Run(ctx, d.cfg.Binary, "start", spec.Name)
	if runErr != nil {
		return Container{}, d.failWithOwnedCleanup(spec.Name, fmt.Errorf("start document container: %w", runErr))
	}
	if started.exitCode != 0 {
		return Container{}, d.failWithOwnedCleanup(spec.Name, commandError("start document container", started))
	}
	if len(bytes.TrimSpace(started.stderr)) != 0 {
		return Container{}, d.failWithOwnedCleanup(spec.Name, fmt.Errorf("start document container emitted a runtime warning: %s", strings.TrimSpace(string(started.stderr))))
	}
	if err := d.verifyAppliedCgroupLimits(ctx, spec.Name); err != nil {
		return Container{}, d.failWithOwnedCleanup(spec.Name, err)
	}
	return Container{ID: containerID, Name: spec.Name, SlotPath: slotPath}, nil
}

func (d *DockerCLI) Exec(ctx context.Context, containerName string, argv []string) (CommandResult, error) {
	if err := validateDocumentExec(containerName, argv); err != nil {
		return CommandResult{}, err
	}
	args := []string{"exec", "--workdir", containerWorkdir, "--user", d.user(), containerName}
	args = append(args, argv...)
	result, err := d.runner.Run(ctx, d.cfg.Binary, args...)
	return convertCommandResult(result, err)
}

func (d *DockerCLI) ExecInput(ctx context.Context, containerName string, argv []string, stdin []byte, stdoutLimit int) (CommandResult, error) {
	if err := validateDocumentExec(containerName, argv); err != nil {
		return CommandResult{}, err
	}
	if len(stdin) == 0 || len(stdin) > maxDocumentCommandInputBytes {
		return CommandResult{}, fmt.Errorf("document command stdin must be between 1 and %d bytes", maxDocumentCommandInputBytes)
	}
	if stdoutLimit <= 0 || stdoutLimit > maxDocumentArtifactBytes {
		return CommandResult{}, fmt.Errorf("document command stdout limit must be between 1 and %d bytes", maxDocumentArtifactBytes)
	}
	args := []string{"exec", "-i", "--workdir", containerWorkdir, "--user", d.user(), containerName}
	args = append(args, argv...)
	result, err := d.runner.RunInput(ctx, d.cfg.Binary, stdin, stdoutLimit, args...)
	return convertCommandResult(result, err)
}

func validateDocumentExec(containerName string, argv []string) error {
	if err := validateContainerName(containerName); err != nil {
		return err
	}
	if len(argv) == 0 || strings.TrimSpace(argv[0]) == "" {
		return fmt.Errorf("container command argv is required")
	}
	for _, arg := range argv {
		if strings.ContainsRune(arg, '\x00') {
			return fmt.Errorf("container command contains a NUL byte")
		}
	}
	return nil
}

func convertCommandResult(result commandResult, err error) (CommandResult, error) {
	converted := CommandResult{Stdout: append([]byte(nil), result.stdout...), Stderr: append([]byte(nil), result.stderr...), ExitCode: result.exitCode}
	if err != nil {
		return converted, fmt.Errorf("execute document command: %w", err)
	}
	if result.exitCode != 0 {
		return converted, commandError("execute document command", result)
	}
	return converted, nil
}

func (d *DockerCLI) Destroy(ctx context.Context, containerName string) error {
	if err := validateContainerName(containerName); err != nil {
		return err
	}
	return d.destroyOwnedContainer(ctx, containerName)
}

func (d *DockerCLI) verifyImage(ctx context.Context) error {
	result, err := d.runner.Run(ctx, d.cfg.Binary, "image", "inspect", "--format", "{{json .Config.Volumes}}", d.cfg.Image)
	if err != nil {
		return fmt.Errorf("inspect document image: %w", err)
	}
	if result.exitCode != 0 {
		return commandError("inspect document image", result)
	}
	var volumes map[string]json.RawMessage
	if err := json.Unmarshal(bytes.TrimSpace(result.stdout), &volumes); err != nil {
		return fmt.Errorf("decode document image volumes: %w", err)
	}
	if len(volumes) != 0 {
		return fmt.Errorf("document image must not declare writable volumes")
	}
	return nil
}

type containerInspect struct {
	Config struct {
		Image      string            `json:"Image"`
		User       string            `json:"User"`
		Entrypoint []string          `json:"Entrypoint"`
		Cmd        []string          `json:"Cmd"`
		Labels     map[string]string `json:"Labels"`
	} `json:"Config"`
	HostConfig struct {
		ReadonlyRootfs bool              `json:"ReadonlyRootfs"`
		NetworkMode    string            `json:"NetworkMode"`
		CapDrop        []string          `json:"CapDrop"`
		SecurityOpt    []string          `json:"SecurityOpt"`
		Memory         int64             `json:"Memory"`
		CPUPeriod      int64             `json:"CpuPeriod"`
		CPUQuota       int64             `json:"CpuQuota"`
		PIDsLimit      int64             `json:"PidsLimit"`
		Tmpfs          map[string]string `json:"Tmpfs"`
		LogConfig      struct {
			Type string `json:"Type"`
		} `json:"LogConfig"`
	} `json:"HostConfig"`
	Mounts []struct {
		Source      string `json:"Source"`
		Destination string `json:"Destination"`
		Propagation string `json:"Propagation"`
		RW          bool   `json:"RW"`
	} `json:"Mounts"`
}

func (d *DockerCLI) verifyCreatedPolicy(ctx context.Context, name, slotPath string) error {
	result, err := d.runner.Run(ctx, d.cfg.Binary, "inspect", "--format", "{{json .}}", name)
	if err != nil {
		return fmt.Errorf("inspect created document container: %w", err)
	}
	if result.exitCode != 0 {
		return commandError("inspect created document container", result)
	}
	var inspected containerInspect
	if err := json.Unmarshal(bytes.TrimSpace(result.stdout), &inspected); err != nil {
		return fmt.Errorf("decode created document container: %w", err)
	}
	if !ownedByDocumentManager(inspected.Config.Labels, name) {
		return fmt.Errorf("created container ownership labels do not match")
	}
	if inspected.Config.Image != d.cfg.Image || inspected.Config.User != d.user() ||
		!slices.Equal(inspected.Config.Entrypoint, []string{documentToolExecutable}) ||
		!slices.Equal(inspected.Config.Cmd, []string{documentToolIdleCommand}) {
		return fmt.Errorf("created container image, user, or process does not match immutable policy")
	}
	if !inspected.HostConfig.ReadonlyRootfs || inspected.HostConfig.NetworkMode != "none" || !slices.Contains(inspected.HostConfig.CapDrop, "ALL") {
		return fmt.Errorf("created container rootfs, network, or capability policy does not match")
	}
	if !slices.Contains(inspected.HostConfig.SecurityOpt, "no-new-privileges:true") || !slices.Contains(inspected.HostConfig.SecurityOpt, "seccomp="+d.cfg.SeccompProfile) {
		return fmt.Errorf("created container security options do not match")
	}
	if inspected.HostConfig.Memory != d.cfg.MemoryBytes || inspected.HostConfig.CPUPeriod != d.cfg.CPUPeriod || inspected.HostConfig.CPUQuota != d.cfg.CPUQuota || inspected.HostConfig.PIDsLimit != d.cfg.PIDsLimit {
		return fmt.Errorf("created container resource policy does not match")
	}
	wantTmpfs := "rw,noexec,nosuid,nodev,size=" + strconv.FormatInt(d.cfg.TmpfsBytes, 10)
	if inspected.HostConfig.Tmpfs["/tmp"] != wantTmpfs || inspected.HostConfig.LogConfig.Type != "none" {
		return fmt.Errorf("created container tmpfs or log policy does not match")
	}
	if len(inspected.Mounts) != 0 {
		return fmt.Errorf("created document container must not expose host mounts")
	}
	return nil
}

func (d *DockerCLI) verifyAppliedCgroupLimits(ctx context.Context, name string) error {
	checks := []struct {
		name string
		want string
	}{
		{"memory.max", strconv.FormatInt(d.cfg.MemoryBytes, 10)},
		{"cpu.max", strconv.FormatInt(d.cfg.CPUQuota, 10) + " " + strconv.FormatInt(d.cfg.CPUPeriod, 10)},
		{"pids.max", strconv.FormatInt(d.cfg.PIDsLimit, 10)},
	}
	for _, check := range checks {
		result, err := d.runner.Run(ctx, d.cfg.Binary, "exec", "--user", d.user(), name, "/usr/local/bin/ga-document-tool", "read-cgroup", check.name)
		if err != nil {
			return fmt.Errorf("read applied cgroup limit %s: %w", check.name, err)
		}
		if result.exitCode != 0 {
			return commandError("read applied cgroup limit "+check.name, result)
		}
		if got := strings.TrimSpace(string(result.stdout)); got != check.want {
			return fmt.Errorf("applied cgroup limit %s is %q, want %q", check.name, got, check.want)
		}
	}
	return nil
}

func (d *DockerCLI) failWithOwnedCleanup(name string, original error) error {
	ctx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
	defer cancel()
	if cleanupErr := d.destroyOwnedContainer(ctx, name); cleanupErr != nil {
		return errors.Join(original, fmt.Errorf("cleanup uncertain document container: %w", cleanupErr))
	}
	return original
}

func (d *DockerCLI) destroyOwnedContainer(ctx context.Context, name string) error {
	inspected, err := d.runner.Run(ctx, d.cfg.Binary, "inspect", "--format", ownershipInspectFormat, name)
	if err != nil {
		return fmt.Errorf("inspect document container ownership: %w", err)
	}
	if inspected.exitCode != 0 {
		if isMissingContainer(inspected.stderr) {
			return nil
		}
		return commandError("inspect document container ownership", inspected)
	}
	var ownership struct {
		ID     string            `json:"ID"`
		Labels map[string]string `json:"Labels"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(inspected.stdout), &ownership); err != nil {
		return fmt.Errorf("decode document container ownership: %w", err)
	}
	if !containerIDPattern.MatchString(ownership.ID) {
		return fmt.Errorf("refusing to destroy container with invalid immutable ID")
	}
	if !ownedByDocumentManager(ownership.Labels, name) {
		return fmt.Errorf("refusing to destroy container without matching manager ownership labels")
	}
	removed, err := d.runner.Run(ctx, d.cfg.Binary, "rm", "-f", ownership.ID)
	if err != nil {
		return fmt.Errorf("destroy document container: %w", err)
	}
	if removed.exitCode == 0 || isMissingContainer(removed.stderr) {
		return nil
	}
	return commandError("destroy document container", removed)
}

func ownedByDocumentManager(labels map[string]string, name string) bool {
	return labels[managerLabel] == "true" && labels[instanceLabel] == name
}

func (d *DockerCLI) validateFreshSlot(spec ContainerSpec) (string, error) {
	if err := validateContainerName(spec.Name); err != nil {
		return "", err
	}
	if !filepath.IsAbs(spec.SlotPath) || filepath.Clean(spec.SlotPath) != spec.SlotPath {
		return "", fmt.Errorf("slot path must be an absolute clean path under the work root")
	}
	if strings.Contains(spec.SlotPath, ",") {
		return "", fmt.Errorf("slot path must not contain a comma")
	}
	rel, err := filepath.Rel(d.workRoot, spec.SlotPath)
	if err != nil || rel == "." || filepath.IsAbs(rel) || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("slot path escapes the manager work root")
	}
	if filepath.Base(rel) != rel {
		return "", fmt.Errorf("slot path must be a direct child of the manager work root")
	}
	if filepath.Base(spec.SlotPath) != spec.Name {
		return "", fmt.Errorf("slot directory name must match the container name")
	}
	info, err := os.Lstat(spec.SlotPath)
	if err != nil {
		return "", fmt.Errorf("stat slot path: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("slot path must not be a symlink")
	}
	if !info.IsDir() {
		return "", fmt.Errorf("slot path must be a directory")
	}
	canonicalSlot, err := filepath.EvalSymlinks(spec.SlotPath)
	if err != nil {
		return "", fmt.Errorf("canonicalize slot path: %w", err)
	}
	if canonicalSlot != spec.SlotPath {
		return "", fmt.Errorf("slot path must already be canonical and contain no symlink")
	}
	entries, err := os.ReadDir(canonicalSlot)
	if err != nil {
		return "", fmt.Errorf("read slot path: %w", err)
	}
	if len(entries) != 0 {
		return "", fmt.Errorf("slot path must be empty before first use")
	}
	return canonicalSlot, nil
}

func validateSeccompProfile(profile string) error {
	profile = strings.TrimSpace(profile)
	if profile == "" || strings.EqualFold(profile, "unconfined") || strings.ContainsRune(profile, '\x00') {
		return fmt.Errorf("a confined seccomp profile is required")
	}
	if profile == "builtin" {
		return nil
	}
	if !filepath.IsAbs(profile) || filepath.Clean(profile) != profile {
		return fmt.Errorf("seccomp profile must be builtin or an absolute clean path")
	}
	info, err := os.Lstat(profile)
	if err != nil {
		return fmt.Errorf("stat seccomp profile: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return fmt.Errorf("seccomp profile must be a regular file, not a symlink")
	}
	return nil
}

func imageReferenceHasTag(image string) bool {
	repository, _, found := strings.Cut(image, "@")
	if !found {
		return true
	}
	lastSlash := strings.LastIndex(repository, "/")
	return strings.Contains(repository[lastSlash+1:], ":")
}

func validateContainerName(name string) error {
	if !containerNamePattern.MatchString(name) {
		return fmt.Errorf("container name is invalid")
	}
	return nil
}

func (d *DockerCLI) user() string {
	return strconv.Itoa(d.cfg.UID) + ":" + strconv.Itoa(d.cfg.GID)
}

func isMissingContainer(stderr []byte) bool {
	message := strings.ToLower(string(stderr))
	return strings.Contains(message, "no such container") || strings.Contains(message, "container not found") || strings.Contains(message, "no such object")
}

func commandError(operation string, result commandResult) error {
	message := strings.TrimSpace(string(result.stderr))
	if message == "" {
		message = strings.TrimSpace(string(result.stdout))
	}
	if message == "" {
		message = "no runtime error output"
	}
	return fmt.Errorf("%s exited %d: %s", operation, result.exitCode, message)
}

type osCommandRunner struct{}

func (osCommandRunner) Run(ctx context.Context, binary string, args ...string) (commandResult, error) {
	return runOSCommand(ctx, binary, nil, dockerCLIOutputLimit, args...)
}

func (osCommandRunner) RunInput(ctx context.Context, binary string, stdin []byte, stdoutLimit int, args ...string) (commandResult, error) {
	return runOSCommand(ctx, binary, stdin, stdoutLimit, args...)
}

func runOSCommand(ctx context.Context, binary string, stdin []byte, stdoutLimit int, args ...string) (commandResult, error) {
	cmd := exec.CommandContext(ctx, binary, args...)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	stdout := strictLimitedBuffer{limit: stdoutLimit}
	var stderr limitedBuffer
	stderr.limit = dockerCLIOutputLimit
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	result := commandResult{stdout: stdout.Bytes(), stderr: stderr.Bytes()}
	if errors.Is(stdout.err, errDocumentCommandOutputTooLarge) {
		return result, errDocumentCommandOutputTooLarge
	}
	if err == nil {
		return result, nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return result, ctxErr
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		result.exitCode = exitErr.ExitCode()
		return result, nil
	}
	return result, err
}

type strictLimitedBuffer struct {
	buffer bytes.Buffer
	limit  int
	err    error
}

func (b *strictLimitedBuffer) Write(p []byte) (int, error) {
	if b.err != nil {
		return 0, b.err
	}
	remaining := b.limit - b.buffer.Len()
	if len(p) > remaining {
		if remaining > 0 {
			_, _ = b.buffer.Write(p[:remaining])
		}
		b.err = errDocumentCommandOutputTooLarge
		return remaining, b.err
	}
	return b.buffer.Write(p)
}

func (b *strictLimitedBuffer) Bytes() []byte {
	return append([]byte(nil), b.buffer.Bytes()...)
}

type limitedBuffer struct {
	buffer bytes.Buffer
	limit  int
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	originalLength := len(p)
	remaining := b.limit - b.buffer.Len()
	if remaining > 0 {
		if len(p) > remaining {
			p = p[:remaining]
		}
		_, _ = b.buffer.Write(p)
	}
	return originalLength, nil
}

func (b *limitedBuffer) Bytes() []byte {
	return append([]byte(nil), b.buffer.Bytes()...)
}
