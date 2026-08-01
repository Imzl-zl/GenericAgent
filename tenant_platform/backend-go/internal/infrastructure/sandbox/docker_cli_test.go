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
		Binary: "docker",
		Profile: ValidProfile(),
		WorkspacesRoot: "/tmp/ws-root",
		ContainerNamePrefix: "ga-runner",
	}
}

func TestCreateRunnerUsesFixedProfileFlags(t *testing.T) {
	runner := &fakeRunner{stdout: "0123456789abcdef\n"}
	cli := &DockerCLI{cfg: validConfig(), runner: runner}
	ctx := context.Background()

	_, err := cli.CreateAndStart(ctx, RunnerSpec{
		WorkspaceHash: strings.Repeat("ab", 32),
		Generation:    3,
		Image:         "ga-runner@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
	})
	if err != nil {
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
	// 必须挂三个工作区 subpath,不得有 docker.sock。
	for _, want := range []string{
		"/ga/legacy/memory",
		"/ga/legacy/temp",
		"/ga/runner-state",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("mount missing %q:\n%s", want, joined)
		}
	}
	if strings.Contains(joined, "docker.sock") || strings.Contains(joined, "/var/run/docker") {
		t.Fatalf("docker socket mount present:\n%s", joined)
	}
	// generation 必须进容器名/label(fencing 可见)。
	if !strings.Contains(joined, "g3") {
		t.Fatalf("generation not in container identity:\n%s", joined)
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

func TestInspectRunnerRejectsExtraMounts(t *testing.T) {
	// inspect 输出声明了第四个挂载(非工作区 subpath)→ 必须失败。
	good := inspectOutput{ReadOnlyRootFS: true, Privileged: false, CapDrop: []string{"ALL"},
		Networks: []string{"runner-control"}, Mounts: []string{LegacyMemoryMount, LegacyTempMount, RunnerStateMount},
		User: "10002:10002", NoNewPrivileges: true}
	if err := validateInspect(good); err != nil {
		t.Fatalf("good inspect rejected: %v", err)
	}

	bad := good
	bad.Mounts = append(bad.Mounts, "/var/run/docker.sock")
	if err := validateInspect(bad); err == nil {
		t.Fatal("docker socket mount in inspect must fail")
	}

	bad = good
	bad.Networks = []string{"runner-control", "database"}
	if err := validateInspect(bad); err == nil {
		t.Fatal("extra network in inspect must fail")
	}

	bad = good
	bad.Privileged = true
	if err := validateInspect(bad); err == nil {
		t.Fatal("privileged in inspect must fail")
	}

	bad = good
	bad.CapDrop = nil
	if err := validateInspect(bad); err == nil {
		t.Fatal("missing cap drop in inspect must fail")
	}

	bad = good
	bad.ReadOnlyRootFS = false
	if err := validateInspect(bad); err == nil {
		t.Fatal("writable root in inspect must fail")
	}

	bad = good
	bad.User = "0:0"
	if err := validateInspect(bad); err == nil {
		t.Fatal("root user in inspect must fail")
	}
}
