package sandbox

import (
	"context"
	"strings"
	"testing"
)

// fakeRunner 记录 docker 调用并返回预设输出。
type fakeRunner struct {
	calls  []string
	stdout string
	stderr string
	err    error
}

func (f *fakeRunner) Run(_ context.Context, _ string, args ...string) ([]byte, []byte, int, error) {
	f.calls = append(f.calls, strings.Join(args, " "))
	if f.err != nil {
		return nil, nil, 1, f.err
	}
	return []byte(f.stdout), []byte(f.stderr), 0, nil
}

func validConfig() DockerConfig {
	return DockerConfig{
		Binary:              "docker",
		Profile:             ValidProfile(),
		WorkspacesRoot:      "/tmp/ws-root",
		ContainerNamePrefix: "ga-runner",
	}
}

func validSpec() RunnerSpec {
	return RunnerSpec{
		WorkspaceHash: strings.Repeat("ab", 32),
		Generation:    3,
		Image:         "ga-runner@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Env:           []string{"GA_LLM_PROXY_ADDR=http://llm-proxy:8081"},
		ConfigFiles: map[string][]byte{
			"server.crt": []byte("cert"),
			"server.key": []byte("key"),
			"ca.crt":     []byte("ca"),
			"policy.json": []byte(`{"version":"foundation.no-host-tools.v1"}`),
		},
	}
}

func TestCreateRunnerUsesFixedProfileFlags(t *testing.T) {
	runner := &fakeRunner{stdout: "0123456789abcdef\n"}
	cli := &DockerCLI{cfg: validConfig(), runner: runner}
	ctx := context.Background()

	if _, err := cli.CreateAndStart(ctx, validSpec()); err != nil {
		t.Fatalf("CreateAndStart: %v", err)
	}
	joined := strings.Join(runner.calls, "\n")
	for _, want := range []string{
		"--read-only",
		"--cap-drop", "ALL",
		"--security-opt", "no-new-privileges:true",
		"--network", "runner-control",
		"--memory", "1073741824",
		"--pids-limit", "128",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("create args missing %q:\n%s", want, joined)
		}
	}
	// 五个工作区 subpath 必须全部挂载(config/attachments 只读),不得有 docker.sock。
	for _, want := range []string{
		"destination=/ga/legacy/memory",
		"destination=/ga/legacy/temp",
		"destination=/ga/runner-state",
		"destination=/ga/runner-config",
		"destination=/ga/runner-attachments",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("mount missing %q:\n%s", want, joined)
		}
	}
	if !strings.Contains(joined, "destination=/ga/runner-config,readonly") {
		t.Fatalf("config mount must be read-only:\n%s", joined)
	}
	if !strings.Contains(joined, "destination=/ga/runner-attachments,readonly") {
		t.Fatalf("attachments mount must be read-only:\n%s", joined)
	}
	if strings.Contains(joined, "docker.sock") || strings.Contains(joined, "/var/run/docker") {
		t.Fatalf("docker socket mount present:\n%s", joined)
	}
	// 固定运行环境: Worker 必需变量 + mTLS 监听参数。
	for _, want := range []string{
		"GA_CONFIG_ROOT=/ga/runner-config",
		"GA_LEGACY_ROOT=/ga/legacy",
		"GA_RUNTIME_DIR=/ga/runner-state",
		"GA_POLICY_FILE=/ga/runner-config/policy.json",
		"GA_WORKER_LISTEN=tcp:0.0.0.0:9443",
		"GA_RUNNER_TLS_CERT=/ga/runner-config/server.crt",
		"GA_RUNNER_TLS_CA=/ga/runner-config/ca.crt",
		"GA_LLM_PROXY_ADDR=http://llm-proxy:8081",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("create env missing %q:\n%s", want, joined)
		}
	}
	// generation 必须进容器名/label(fencing 可见)。
	if !strings.Contains(joined, "g3") {
		t.Fatalf("generation not in container identity:\n%s", joined)
	}
	// 容器名确定性推导(证书 SAN/拨号地址依赖)。
	expectedName := "ga-runner-" + validSpec().WorkspaceHash[:12] + "-g3"
	if !strings.Contains(joined, "--name "+expectedName) {
		t.Fatalf("container name = want %q:\n%s", expectedName, joined)
	}
}

func TestCreateRunnerUsesVolumeSubpathWhenConfigured(t *testing.T) {
	runner := &fakeRunner{stdout: "0123456789abcdef\n"}
	cfg := validConfig()
	cfg.WorkspaceVolume = "runner_workspaces"
	cli := &DockerCLI{cfg: cfg, runner: runner}
	if _, err := cli.CreateAndStart(context.Background(), validSpec()); err != nil {
		t.Fatalf("CreateAndStart: %v", err)
	}
	joined := strings.Join(runner.calls, "\n")
	want := "type=volume,source=runner_workspaces,destination=/ga/runner-state,volume-subpath=" +
		validSpec().WorkspaceHash + "/state"
	if !strings.Contains(joined, want) {
		t.Fatalf("volume-subpath mount missing %q:\n%s", want, joined)
	}
	// volume 模式下不得出现 bind source(Manager 容器内路径对 daemon 不可解析)。
	if strings.Contains(joined, "type=bind,source=/tmp/ws-root") {
		t.Fatalf("bind source leaked into volume mode:\n%s", joined)
	}
}

func TestCreateRunnerRejectsInvalidWorkspaceHash(t *testing.T) {
	runner := &fakeRunner{}
	cli := &DockerCLI{cfg: validConfig(), runner: runner}
	if _, err := cli.CreateAndStart(context.Background(), RunnerSpec{
		WorkspaceHash: "../../etc",
		Generation:    1,
		Image:         "ga-runner@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
	}); err == nil {
		t.Fatal("traversal workspace hash must fail before docker call")
	}
	if len(runner.calls) != 0 {
		t.Fatal("no docker call expected")
	}
}

func TestCreateRunnerRejectsUnsafeConfigFileName(t *testing.T) {
	runner := &fakeRunner{}
	cli := &DockerCLI{cfg: validConfig(), runner: runner}
	spec := validSpec()
	spec.ConfigFiles = map[string][]byte{"../escape": []byte("x")}
	if _, err := cli.CreateAndStart(context.Background(), spec); err == nil {
		t.Fatal("unsafe config file name must fail")
	}
}

func TestCreateRunnerRejectsUnsafeEnv(t *testing.T) {
	runner := &fakeRunner{}
	cli := &DockerCLI{cfg: validConfig(), runner: runner}
	spec := validSpec()
	spec.Env = []string{"A=1\x00B=2"}
	if _, err := cli.CreateAndStart(context.Background(), spec); err == nil {
		t.Fatal("env with NUL byte must fail")
	}
}

func TestInspectRunnerRejectsDrift(t *testing.T) {
	profile := ValidProfile()
	profile.Image = "ga-runner@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	good := inspectOutput{
		ReadOnlyRootFS: true, Privileged: false, CapDrop: []string{"ALL"},
		NoNewPrivileges: true, Networks: []string{"runner-control"},
		User: "10002:10002", Image: profile.Image, Runtime: "runc",
		Mounts: []inspectMount{
			{Type: "volume", Source: "runner_workspaces/_vol", Destination: LegacyMemoryMount, RW: true},
			{Type: "volume", Source: "runner_workspaces/_vol", Destination: LegacyTempMount, RW: true},
			{Type: "volume", Source: "runner_workspaces/_vol", Destination: RunnerStateMount, RW: true},
			{Type: "volume", Source: "runner_workspaces/_vol", Destination: RunnerConfigMount, RW: false},
			{Type: "volume", Source: "runner_workspaces/_vol", Destination: RunnerAttachmentsMount, RW: false},
		},
	}
	if err := validateInspect(good, profile); err != nil {
		t.Fatalf("good inspect rejected: %v", err)
	}

	cases := []struct {
		name   string
		mutate func(*inspectOutput)
	}{
		{"extra mount", func(o *inspectOutput) {
			o.Mounts = append(o.Mounts, inspectMount{Type: "bind", Source: "/var/run/docker.sock", Destination: "/var/run/docker.sock", RW: true})
		}},
		{"extra network", func(o *inspectOutput) { o.Networks = []string{"runner-control", "database"} }},
		{"privileged", func(o *inspectOutput) { o.Privileged = true }},
		{"no cap drop", func(o *inspectOutput) { o.CapDrop = nil }},
		{"writable root", func(o *inspectOutput) { o.ReadOnlyRootFS = false }},
		{"root user", func(o *inspectOutput) { o.User = "0:0" }},
		{"image drift", func(o *inspectOutput) { o.Image = "ga-runner:local" }},
		{"runtime drift", func(o *inspectOutput) { o.Runtime = "runsc" }},
		{"seccomp unconfined", func(o *inspectOutput) { o.Seccomp = "unconfined" }},
		{"config mount rw", func(o *inspectOutput) {
			o.Mounts[3].RW = true
		}},
		{"attachments mount rw", func(o *inspectOutput) {
			o.Mounts[4].RW = true
		}},
		{"mount missing source", func(o *inspectOutput) {
			o.Mounts[2].Source = ""
		}},
		{"device present", func(o *inspectOutput) { o.Devices = []string{"/dev/kvm"} }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bad := good
			tc.mutate(&bad)
			if err := validateInspect(bad, profile); err == nil {
				t.Fatal("drifted inspect must fail")
			}
		})
	}
}
