package sandbox

import (
	"context"
	"errors"
	"os"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// TestMain 预置测试固定的 workspace root(validConfig 的 WorkspacesRoot):
// unix 实现要求 root 已存在(openat O_DIRECTORY), Windows 实现不检查——
// 修复前这些测试只在 Linux 上失败(round13 审查 CI)。
func TestMain(m *testing.M) {
	_ = os.MkdirAll("/tmp/ws-root", 0o755)
	os.Exit(m.Run())
}

// fakeRunner 记录 docker 调用并返回预设输出。scripted 非空时按调用顺序
// 依次弹出(create 成功、start 失败等时序场景)。
type fakeRunner struct {
	calls    []string
	stdout   string
	stderr   string
	err      error
	scripted []fakeRunResult
}

type fakeRunResult struct {
	stdout string
	stderr string
	code   int
	err    error
}

func (f *fakeRunner) Run(_ context.Context, _ string, args ...string) ([]byte, []byte, int, error) {
	f.calls = append(f.calls, strings.Join(args, " "))
	if len(f.scripted) > 0 {
		r := f.scripted[0]
		f.scripted = f.scripted[1:]
		if r.err != nil {
			return nil, nil, r.code, r.err
		}
		return []byte(r.stdout), []byte(r.stderr), r.code, nil
	}
	if f.err != nil {
		return nil, nil, 1, f.err
	}
	return []byte(f.stdout), []byte(f.stderr), 0, nil
}

// hostScript 返回创建 Runner 前 manager 的 network inspect + 逐容器 inspect
// 脚本输出(runnerNetworkHosts 调用), 后续调用由 extra 追加。
func hostScript(extra ...fakeRunResult) []fakeRunResult {
	base := []fakeRunResult{
		{stdout: `[{"Containers":{"abc123":{"Name":"genericagent-platform-1","IPv4Address":"172.26.0.3/16"},"def456":{"Name":"genericagent-llm-proxy-1","IPv4Address":"172.26.0.2/16"}}}]`},
		// 按容器名排序: 先 inspect genericagent-llm-proxy-1, 再 platform-1。
		{stdout: `[{"NetworkSettings":{"Networks":{"runner-control":{"Aliases":["genericagent-llm-proxy-1","llm-proxy"]}}}}]`},
		{stdout: `[{"NetworkSettings":{"Networks":{"runner-control":{"Aliases":["genericagent-platform-1","platform"]}}}}]`},
	}
	return append(base, extra...)
}

func validConfig() DockerConfig {
	profile := ValidProfile()
	profile.AllowRunc = true // 测试模拟默认 docker 运行时, 显式 trusted 开关
	return DockerConfig{
		Binary:              "docker",
		Profile:             profile,
		WorkspacesRoot:      "/tmp/ws-root",
		ContainerNamePrefix: "ga-runner",
	}
}

func validSpec() RunnerSpec {
	return RunnerSpec{
		WorkspaceKey:  "personal:1",
		WorkspaceHash: strings.Repeat("ab", 32),
		Generation:    3,
		Image:         "ga-runner@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Env:           []string{"GA_LLM_PROXY_ADDR=http://llm-proxy:8081"},
		ConfigFiles: map[string][]byte{
			"server.crt":  []byte("cert"),
			"server.key":  []byte("key"),
			"ca.crt":      []byte("ca"),
			"policy.json": []byte(`{"version":"foundation.no-host-tools.v1"}`),
		},
	}
}

func TestCreateRunnerUsesFixedProfileFlags(t *testing.T) {
	runner := &fakeRunner{scripted: hostScript(fakeRunResult{stdout: "0123456789abcdef\n"})}
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
		// runsc 下 Docker 内嵌 DNS 不可用: 内部服务名必须进 /etc/hosts。
		"--add-host", "genericagent-llm-proxy-1:172.26.0.2",
		"--add-host", "genericagent-platform-1:172.26.0.3",
		"--add-host", "llm-proxy:172.26.0.2",
		"--add-host", "platform:172.26.0.3",
		"--memory", "1073741824",
		"--pids-limit", "128",
		"--user", "10002:10002",
		"--group-add", "10003",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("create args missing %q:\n%s", want, joined)
		}
	}
	// 工作区 subpath 必须全部挂载(memory/temp/state 读写, config 只读),
	// state/committed 与 state/results 必须以只读子挂载遮蔽顶层 rw
	// (审查 C3: Runner 不得删除/替换已提交快照与结果文件), 不得有 docker.sock。
	for _, want := range []string{
		"destination=/ga/legacy/memory",
		"destination=/ga/legacy/temp",
		"destination=/ga/runner-state",
		"destination=/ga/runner-state/committed,readonly",
		"destination=/ga/runner-state/results,readonly",
		"destination=/ga/runner-config,readonly",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("mount missing %q:\n%s", want, joined)
		}
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
	// 显式命令参数覆盖镜像 CMD: 固定 mTLS TCP 监听(镜像默认 unix socket
	// 仅供本地冒烟; 覆盖后 Platform 才能按 runner-name:9443 拨号)。
	if !strings.Contains(joined, "--listen tcp:0.0.0.0:9443") {
		t.Fatalf("create must pass --listen tcp:0.0.0.0:9443 as command:\n%s", joined)
	}
}

func TestCreateRunnerUsesVolumeSubpathWhenConfigured(t *testing.T) {
	runner := &fakeRunner{scripted: hostScript(fakeRunResult{stdout: "0123456789abcdef\n"})}
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

// fixedInspectEnv 返回与 docker_cli.CreateAndStart 完全一致的固定环境变量
// (inspect 校验要求精确匹配, 审查 I9)。
func fixedInspectEnv(workspaceHash string) []string {
	return []string{
		"GA_CONFIG_ROOT=" + RunnerConfigMount,
		"GA_LEGACY_ROOT=" + LegacyRoot,
		"GA_RUNTIME_DIR=" + RunnerStateMount,
		"GA_POLICY_FILE=" + RunnerConfigMount + "/policy.json",
		"GA_WORKER_LISTEN=tcp:0.0.0.0:" + strconv.Itoa(RunnerControlPort),
		"GA_RUNNER_TLS_CERT=" + RunnerConfigMount + "/server.crt",
		"GA_RUNNER_TLS_KEY=" + RunnerConfigMount + "/server.key",
		"GA_RUNNER_TLS_CA=" + RunnerConfigMount + "/ca.crt",
		"GA_WORKSPACE_MEMORY=" + LegacyMemoryMount,
		"GA_WORKSPACE_TEMP=" + LegacyTempMount,
		"GA_WORKSPACE_KEY=personal:1",
		"GA_RUNNER_GENERATION=1",
		"GA_OVERLAY_ROOT=" + RunnerOverlayMount,
	}
}

// deepCopyInspect 深拷贝 inspectOutput(审查: 原测试 `bad := good` 浅拷贝
// 共享 Mounts/TmpfsOpts/GroupAdd 底层数组与 map, 前序 case 的 mutation 会
// 污染后续 case, 使缺失 tmpfs/extra group 等 drift 测试错误通过)。
func deepCopyInspect(in inspectOutput) inspectOutput {
	out := in
	out.CapDrop = append([]string(nil), in.CapDrop...)
	out.CapAdd = append([]string(nil), in.CapAdd...)
	out.Networks = append([]string(nil), in.Networks...)
	out.Devices = append([]string(nil), in.Devices...)
	out.GroupAdd = append([]string(nil), in.GroupAdd...)
	out.Env = append([]string(nil), in.Env...)
	out.Cmd = append([]string(nil), in.Cmd...)
	out.Tmpfs = append([]string(nil), in.Tmpfs...)
	out.TmpfsOpts = make(map[string]string, len(in.TmpfsOpts))
	for k, v := range in.TmpfsOpts {
		out.TmpfsOpts[k] = v
	}
	out.Labels = make(map[string]string, len(in.Labels))
	for k, v := range in.Labels {
		out.Labels[k] = v
	}
	out.Mounts = make([]inspectMount, len(in.Mounts))
	copy(out.Mounts, in.Mounts)
	out.HostMounts = make([]hostMount, len(in.HostMounts))
	copy(out.HostMounts, in.HostMounts)
	return out
}

// hostMountsForWorkspace 构造真实 Docker HostConfig.Mounts 形状：volume-subpath
// 的请求参数（卷名/目标/子路径）在 HostConfig.Mounts，顶层 .Mounts 只有实际
// 挂载结果（审查 C1：Docker 29.6.2 实测顶层无 VolumeOptions.Subpath）。
func hostMountsForWorkspace(workspaceHash, volume string) []hostMount {
	return []hostMount{
		{Type: "volume", Source: volume, Target: LegacyMemoryMount, VolumeSubpath: workspaceHash + "/memory"},
		{Type: "volume", Source: volume, Target: LegacyTempMount, VolumeSubpath: workspaceHash + "/temp"},
		// 审查 C1/I6: config 按 generation 隔离为 config/g<gen>(测试固定 g1,
		// 与 good inspect 的 Labels generation=1 一致)。
		{Type: "volume", Source: volume, Target: RunnerConfigMount, ReadOnly: true, VolumeSubpath: workspaceHash + "/config/g1"},
		{Type: "volume", Source: volume, Target: RunnerStateMount, VolumeSubpath: workspaceHash + "/state"},
		{Type: "volume", Source: volume, Target: RunnerStateMount + "/committed", ReadOnly: true, VolumeSubpath: workspaceHash + "/state/committed"},
		{Type: "volume", Source: volume, Target: RunnerStateMount + "/results", ReadOnly: true, VolumeSubpath: workspaceHash + "/state/results"},
	}
}

func TestInspectRunnerRejectsDrift(t *testing.T) {
	profile := ValidProfile()
	profile.AllowRunc = true // 测试模拟默认 docker 运行时, 显式 trusted 开关
	profile.Image = "ga-runner@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	workspaceHash := strings.Repeat("ab", 32)
	good := inspectOutput{
		ReadOnlyRootFS: true, Running: true, Privileged: false, CapDrop: []string{"ALL"},
		NoNewPrivileges: true, Networks: []string{"runner-control"},
		User: "10002:10002", Image: profile.Image, Runtime: "runc",
		MemoryBytes: profile.MemoryBytes, CPUQuota: profile.CPUQuota,
		CPUPeriod: profile.CPUPeriod, PIDsLimit: profile.PIDsLimit,
		GroupAdd:  []string{strconv.Itoa(profile.ShareGID)},
		Env:       fixedInspectEnv(workspaceHash),
		Cmd:       []string{"--listen", "tcp:0.0.0.0:" + strconv.Itoa(RunnerControlPort)},
		Tmpfs:     []string{RunnerOverlayMount, "/tmp"},
		TmpfsOpts: map[string]string{
			"/tmp": "rw,noexec,nosuid,nodev,size=64m",
			RunnerOverlayMount: "rw,noexec,nosuid,nodev,size=128m",
		},
		Labels: map[string]string{
			"com.genericagent.runner.hash":       workspaceHash,
			"com.genericagent.runner.generation": "1",
		},
		Mounts: []inspectMount{
			// 实测 Docker 29.6.2: 顶层 .Mounts 的 volume 挂载不含
			// VolumeOptions.Subpath（真实 subpath 在 HostConfig.Mounts）。
			// 按 Destination 字典序: memory < temp < config < state < state/committed < state/results
			{Type: "volume", Source: "/var/lib/docker/volumes/runner_workspaces/_data", Destination: LegacyMemoryMount, RW: true},
			{Type: "volume", Source: "/var/lib/docker/volumes/runner_workspaces/_data", Destination: LegacyTempMount, RW: true},
			{Type: "volume", Source: "/var/lib/docker/volumes/runner_workspaces/_data", Destination: RunnerConfigMount, RW: false},
			{Type: "volume", Source: "/var/lib/docker/volumes/runner_workspaces/_data", Destination: RunnerStateMount, RW: true},
			{Type: "volume", Source: "/var/lib/docker/volumes/runner_workspaces/_data", Destination: RunnerStateMount + "/committed", RW: false},
			{Type: "volume", Source: "/var/lib/docker/volumes/runner_workspaces/_data", Destination: RunnerStateMount + "/results", RW: false},
		},
		HostMounts: hostMountsForWorkspace(workspaceHash, "runner_workspaces"),
	}
	mountSubs := map[string]string{
		LegacyMemoryMount: "memory", LegacyTempMount: "temp", RunnerStateMount: "state",
		RunnerConfigMount: "config", RunnerStateMount + "/committed": "state/committed",
		RunnerStateMount + "/results": "state/results",
	}
	if err := validateInspect(good, profile, "/tmp/ws-root", "runner_workspaces", mountSubs); err != nil {
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
		{"stopped runner", func(o *inspectOutput) { o.Running = false }},
		{"privileged", func(o *inspectOutput) { o.Privileged = true }},
		{"no cap drop", func(o *inspectOutput) { o.CapDrop = nil }},
		{"writable root", func(o *inspectOutput) { o.ReadOnlyRootFS = false }},
		{"root user", func(o *inspectOutput) { o.User = "0:0" }},
		{"image drift", func(o *inspectOutput) { o.Image = "ga-runner:local" }},
		{"runtime drift", func(o *inspectOutput) { o.Runtime = "runsc" }},
		{"seccomp unconfined", func(o *inspectOutput) { o.Seccomp = "unconfined" }},
		{"config mount rw", func(o *inspectOutput) {
			o.Mounts[2].RW = true
		}},
		{"committed mount rw", func(o *inspectOutput) {
			// 审查 C3: committed 只读是不可侵犯边界,
			// 变回 rw 必须拒绝(否则 Runner 可删除快照)。
			o.Mounts[4].RW = true
		}},
		{"results mount rw", func(o *inspectOutput) {
			// 审查 C3: results 不得变回可写。
			o.Mounts[5].RW = true
		}},
		{"mount missing source", func(o *inspectOutput) {
			o.Mounts[3].Source = ""
		}},
		{"wrong volume source", func(o *inspectOutput) {
			// 任意其他卷: 卷归属校验必须拒绝(审查 I10)。
			o.Mounts[0].Source = "/var/lib/docker/volumes/other_volume/_data"
		}},
		{"missing share group", func(o *inspectOutput) { o.GroupAdd = nil }},
		{"extra share group", func(o *inspectOutput) { o.GroupAdd = []string{"10003", "10004"} }},
		{"wrong user uid", func(o *inspectOutput) { o.User = "10001:10002" }},
		{"tmpfs missing noexec", func(o *inspectOutput) { o.TmpfsOpts = map[string]string{"/tmp": "rw,nosuid,nodev,size=64m", RunnerOverlayMount: "rw,noexec,nosuid,nodev,size=128m"} }},
		{"overlay tmpfs missing", func(o *inspectOutput) { delete(o.TmpfsOpts, RunnerOverlayMount) }},
		{"missing tmpfs", func(o *inspectOutput) { o.TmpfsOpts = map[string]string{} }},
		{"volume subpath drift", func(o *inspectOutput) {
			// 挂到其他 workspace 的 subpath 必须拒绝(审查)。
			other := strings.Repeat("cd", 32)
			o.HostMounts[0].VolumeSubpath = other + "/memory"
		}},
		{"host mount missing subpath", func(o *inspectOutput) {
			// HostConfig.Mounts 缺少该挂载的 volume-subpath 参数(审查 C1:
			// subpath 只从 HostConfig 读取, 缺失时不得静默接受)。
			o.HostMounts = o.HostMounts[1:]
		}},
		{"memory drift", func(o *inspectOutput) { o.MemoryBytes = profile.MemoryBytes - 1 }},
		{"device present", func(o *inspectOutput) { o.Devices = []string{"/dev/kvm"} }},
		{"cap_add present", func(o *inspectOutput) { o.CapAdd = []string{"SYS_ADMIN"} }},
		{"apparmor unconfined", func(o *inspectOutput) { o.AppArmorProfile = "unconfined" }},
		{"env override", func(o *inspectOutput) {
			for i, e := range o.Env {
				if strings.HasPrefix(e, "GA_RUNNER_TLS_CERT=") {
					o.Env[i] = "GA_RUNNER_TLS_CERT="
				}
			}
		}},
		{"env missing fixed", func(o *inspectOutput) {
			o.Env = []string{"GA_WORKSPACE_KEY=personal:1", "GA_RUNNER_GENERATION=1"}
		}},
		{"generation env mismatch", func(o *inspectOutput) {
			for i, e := range o.Env {
				if strings.HasPrefix(e, "GA_RUNNER_GENERATION=") {
					o.Env[i] = "GA_RUNNER_GENERATION=2"
				}
			}
		}},
		{"cmd drift", func(o *inspectOutput) { o.Cmd = []string{"--listen", "tcp:0.0.0.0:9999"} }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bad := deepCopyInspect(good)
			tc.mutate(&bad)
			if err := validateInspect(bad, profile, "/tmp/ws-root", "runner_workspaces", mountSubs); err == nil {
				t.Fatal("drifted inspect must fail")
			}
		})
	}
}

// TestValidateInspectSortedMounts 验证真实 docker inspect 解析路径
// (parse -> sort by destination -> validate) 在挂载顺序与创建顺序不同时
// 也能通过: 防止"单测直调未排序输入, 生产解析排序后误报"的回归。
func TestValidateInspectSortedMounts(t *testing.T) {
	profile := ValidProfile()
	profile.AllowRunc = true // 测试模拟默认 docker 运行时
	profile.Image = "ga-runner@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	// 模拟 docker inspect 返回创建顺序(与排序后不同的乱序)。
	workspaceHash := strings.Repeat("ef", 32)
	raw := inspectOutput{
		ReadOnlyRootFS: true, Running: true, CapDrop: []string{"ALL"}, NoNewPrivileges: true,
		Networks: []string{"runner-control"}, User: "10002:10002",
		Image: profile.Image, Runtime: "runc",
		MemoryBytes: profile.MemoryBytes, CPUQuota: profile.CPUQuota,
		CPUPeriod: profile.CPUPeriod, PIDsLimit: profile.PIDsLimit,
		GroupAdd:  []string{strconv.Itoa(profile.ShareGID)},
		Env:       fixedInspectEnv(workspaceHash),
		Cmd:       []string{"--listen", "tcp:0.0.0.0:" + strconv.Itoa(RunnerControlPort)},
		Tmpfs:     []string{RunnerOverlayMount, "/tmp"},
		TmpfsOpts: map[string]string{
			"/tmp": "rw,noexec,nosuid,nodev,size=64m",
			RunnerOverlayMount: "rw,noexec,nosuid,nodev,size=128m",
		},
		Labels: map[string]string{
			"com.genericagent.runner.hash":       workspaceHash,
			"com.genericagent.runner.generation": "1",
		},
		Mounts: []inspectMount{
			// 真实顶层形状(无 VolumeOptions.Subpath, 审查 C1)。
			{Type: "volume", Source: "/var/lib/docker/volumes/runner_workspaces/_data", Destination: LegacyTempMount, RW: true},
			{Type: "volume", Source: "/var/lib/docker/volumes/runner_workspaces/_data", Destination: RunnerStateMount, RW: true},
			{Type: "volume", Source: "/var/lib/docker/volumes/runner_workspaces/_data", Destination: LegacyMemoryMount, RW: true},
			{Type: "volume", Source: "/var/lib/docker/volumes/runner_workspaces/_data", Destination: RunnerConfigMount, RW: false},
			{Type: "volume", Source: "/var/lib/docker/volumes/runner_workspaces/_data", Destination: RunnerStateMount + "/committed", RW: false},
			{Type: "volume", Source: "/var/lib/docker/volumes/runner_workspaces/_data", Destination: RunnerStateMount + "/results", RW: false},
		},
		HostMounts: hostMountsForWorkspace(workspaceHash, "runner_workspaces"),
	}

	// 复刻 Inspect 中的排序(生产解析路径)。
	sort.Slice(raw.Mounts, func(i, j int) bool { return raw.Mounts[i].Destination < raw.Mounts[j].Destination })
	if err := validateInspect(raw, profile, "/tmp/ws-root", "runner_workspaces",
		map[string]string{
			LegacyMemoryMount: "memory", LegacyTempMount: "temp", RunnerStateMount: "state",
			RunnerConfigMount: "config", RunnerStateMount + "/committed": "state/committed",
			RunnerStateMount + "/results": "state/results",
		}); err != nil {
		t.Fatalf("sorted real-world mounts rejected: %v", err)
	}
}

// TestValidateInspectBindExactSource 验证 bind 挂载必须精确匹配
// <workspacesRoot>/<hash>/<sub> 全路径(审查 I10: 拒绝其他 workspace 子目录)。
func TestValidateInspectBindExactSource(t *testing.T) {
	profile := ValidProfile()
	profile.AllowRunc = true // 测试模拟默认 docker 运行时
	workspaceHash := strings.Repeat("12", 32)
	mountSubs := map[string]string{
		LegacyMemoryMount: "memory", LegacyTempMount: "temp", RunnerStateMount: "state",
		RunnerConfigMount: "config", RunnerStateMount + "/committed": "state/committed",
		RunnerStateMount + "/results": "state/results",
	}
	good := inspectOutput{
		ReadOnlyRootFS: true, Running: true, CapDrop: []string{"ALL"}, NoNewPrivileges: true,
		Networks: []string{"runner-control"}, User: "10002:10002",
		Image: profile.Image, Runtime: "runc",
		MemoryBytes: profile.MemoryBytes, CPUQuota: profile.CPUQuota,
		CPUPeriod: profile.CPUPeriod, PIDsLimit: profile.PIDsLimit,
		GroupAdd:  []string{strconv.Itoa(profile.ShareGID)},
		Env:       fixedInspectEnv(workspaceHash),
		Cmd:       []string{"--listen", "tcp:0.0.0.0:" + strconv.Itoa(RunnerControlPort)},
		Tmpfs:     []string{RunnerOverlayMount, "/tmp"},
		TmpfsOpts: map[string]string{
			"/tmp": "rw,noexec,nosuid,nodev,size=64m",
			RunnerOverlayMount: "rw,noexec,nosuid,nodev,size=128m",
		},
		Labels: map[string]string{
			"com.genericagent.runner.hash":       workspaceHash,
			"com.genericagent.runner.generation": "1",
		},
		Mounts: []inspectMount{
			{Type: "bind", Source: "/ws/" + workspaceHash + "/memory", Destination: LegacyMemoryMount, RW: true},
			{Type: "bind", Source: "/ws/" + workspaceHash + "/temp", Destination: LegacyTempMount, RW: true},
			// 审查 C1/I6: config 按 generation 隔离为 config/g<gen>(测试 g1)。
			{Type: "bind", Source: "/ws/" + workspaceHash + "/config/g1", Destination: RunnerConfigMount, RW: false},
			{Type: "bind", Source: "/ws/" + workspaceHash + "/state", Destination: RunnerStateMount, RW: true},
			{Type: "bind", Source: "/ws/" + workspaceHash + "/state/committed", Destination: RunnerStateMount + "/committed", RW: false},
			{Type: "bind", Source: "/ws/" + workspaceHash + "/state/results", Destination: RunnerStateMount + "/results", RW: false},
		},
	}
	if err := validateInspect(good, profile, "/ws", "", mountSubs); err != nil {
		t.Fatalf("bind exact source rejected: %v", err)
	}
	// 其他 workspace 的同名子目录必须拒绝。
	other := strings.Repeat("cd", 32)
	bad := deepCopyInspect(good)
	bad.Mounts[0].Source = "/ws/" + other + "/memory"
	if err := validateInspect(bad, profile, "/ws", "", mountSubs); err == nil {
		t.Fatal("cross-workspace bind source must be rejected")
	}
}

// TestCreateRunnerRejectsProtectedEnvOverride 验证控制面透传 env 不得覆盖
// Manager 固定的安全变量(空 GA_RUNNER_TLS_* 会让 Worker 以 insecure 监听,
// 破坏 mTLS 控制面; 审查 I8 fail-closed)。
func TestCreateRunnerRejectsProtectedEnvOverride(t *testing.T) {
	runner := &fakeRunner{}
	cli := &DockerCLI{cfg: validConfig(), runner: runner}
	for _, key := range []string{
		"GA_RUNNER_TLS_CERT", "GA_RUNNER_TLS_KEY", "GA_RUNNER_TLS_CA",
		"GA_WORKER_LISTEN", "GA_CONFIG_ROOT", "GA_WORKSPACE_KEY",
		"GA_RUNNER_GENERATION", "GA_POLICY_FILE", "GA_RUNTIME_DIR",
	} {
		spec := validSpec()
		spec.Env = []string{key + "=evil"}
		if _, err := cli.CreateAndStart(context.Background(), spec); err == nil {
			t.Fatalf("env override of %s must be rejected", key)
		}
	}
	// 非保护键仍允许透传。
	runner.scripted = hostScript(fakeRunResult{stdout: "cid123"})
	spec := validSpec()
	spec.Env = []string{"GA_LLM_PROXY_ADDR=http://llm-proxy:8081"}
	if _, err := cli.CreateAndStart(context.Background(), spec); err != nil {
		t.Fatalf("unprotected env must be allowed: %v", err)
	}
}

// 审查 R5-I7: docker create 成功但 docker start 失败时, 必须立即 rm -f
// 已创建容器——否则遗留 Created/stopped 容器, 且 config/ 清理依赖 Manager
// 的 cleanupWorkspaceConfig(仅 CreateAndStart 返回错误路径)。
func TestCreateAndStartRemovesContainerWhenStartFails(t *testing.T) {
	runner := &fakeRunner{scripted: hostScript(
		fakeRunResult{stdout: "cid123\n"},
		fakeRunResult{stderr: "start failed", code: 1},
	)}
	cli := &DockerCLI{cfg: validConfig(), runner: runner}
	ctx := context.Background()

	if _, err := cli.CreateAndStart(ctx, validSpec()); err == nil {
		t.Fatal("start failure must return error")
	}
	// 必须发起 rm -f(按容器 ID)。
	found := false
	for _, call := range runner.calls {
		if strings.Contains(call, "rm") && strings.Contains(call, "-f") && strings.Contains(call, "cid123") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected rm -f of created container, calls=%v", runner.calls)
	}
}

// Round8(review): DockerCLI 必须能从容器 label 恢复 generation——按容器 ID
// 销毁(Manager 重启后)时, 没有它 config/g<gen> 清理会因 generation=0 跳过,
// 短期 mTLS 材料残留。
func TestDockerCLIRunnerGenerationLabel(t *testing.T) {
	fr := &fakeRunner{stdout: "7\n"}
	d := &DockerCLI{runner: fr, cfg: validConfig()}

	gen, ok, err := d.RunnerGenerationLabel(context.Background(), "some-container-id")
	if err != nil || !ok || gen != 7 {
		t.Fatalf("expected gen=7 ok=true, got gen=%d ok=%v err=%v", gen, ok, err)
	}
	// 非法 label → ok=false。
	fr2 := &fakeRunner{stdout: "not-a-number"}
	d2 := &DockerCLI{runner: fr2, cfg: validConfig()}
	if _, ok, _ := d2.RunnerGenerationLabel(context.Background(), "x"); ok {
		t.Fatal("invalid generation label must be ok=false")
	}
	// 容器不存在 → ok=false。
	fr3 := &fakeRunner{err: errors.New("no such container")}
	d3 := &DockerCLI{runner: fr3, cfg: validConfig()}
	if _, ok, _ := d3.RunnerGenerationLabel(context.Background(), "x"); ok {
		t.Fatal("missing container must be ok=false")
	}
}
