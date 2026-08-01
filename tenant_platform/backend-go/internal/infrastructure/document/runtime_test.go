package document

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const testImage = "alpine@sha256:d9e853e87e55526f6b2917df91a2115c36dd7c696a35be12163d44e6e2a4b6bc"

type runnerCall struct {
	binary      string
	args        []string
	stdin       []byte
	stdoutLimit int
}

type runnerReply struct {
	result commandResult
	err    error
}

type scriptedRunner struct {
	calls   []runnerCall
	replies []runnerReply
}

func (r *scriptedRunner) Run(_ context.Context, binary string, args ...string) (commandResult, error) {
	r.calls = append(r.calls, runnerCall{binary: binary, args: append([]string(nil), args...)})
	return r.reply()
}

func (r *scriptedRunner) RunInput(_ context.Context, binary string, stdin []byte, stdoutLimit int, args ...string) (commandResult, error) {
	r.calls = append(r.calls, runnerCall{
		binary: binary, args: append([]string(nil), args...), stdin: append([]byte(nil), stdin...), stdoutLimit: stdoutLimit,
	})
	return r.reply()
}

func (r *scriptedRunner) reply() (commandResult, error) {
	if len(r.replies) == 0 {
		return commandResult{}, errors.New("unexpected command")
	}
	reply := r.replies[0]
	r.replies = r.replies[1:]
	return reply.result, reply.err
}

func TestNewDockerCLIRejectsUnsafePolicy(t *testing.T) {
	root := t.TempDir()
	valid := validDockerConfig(root)
	tests := []struct {
		name   string
		mutate func(*DockerConfig)
	}{
		{"tagged image", func(cfg *DockerConfig) { cfg.Image = "alpine:3.20" }},
		{"tagged digest image", func(cfg *DockerConfig) {
			cfg.Image = "alpine:3.20@sha256:d9e853e87e55526f6b2917df91a2115c36dd7c696a35be12163d44e6e2a4b6bc"
		}},
		{"missing seccomp", func(cfg *DockerConfig) { cfg.SeccompProfile = "" }},
		{"unconfined seccomp", func(cfg *DockerConfig) { cfg.SeccompProfile = "unconfined" }},
		{"root uid", func(cfg *DockerConfig) { cfg.UID = 0 }},
		{"root gid", func(cfg *DockerConfig) { cfg.GID = 0 }},
		{"no memory limit", func(cfg *DockerConfig) { cfg.MemoryBytes = 0 }},
		{"no cpu quota", func(cfg *DockerConfig) { cfg.CPUQuota = 0 }},
		{"no pids limit", func(cfg *DockerConfig) { cfg.PIDsLimit = 0 }},
		{"no tmpfs limit", func(cfg *DockerConfig) { cfg.TmpfsBytes = 0 }},
		{"custom process", func(cfg *DockerConfig) { cfg.Command = []string{"/bin/sh"} }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := valid
			tt.mutate(&cfg)
			if _, err := newDockerCLI(cfg, &scriptedRunner{}); err == nil {
				t.Fatal("expected unsafe policy rejection")
			}
		})
	}
}

func TestDockerCLICreateAndStartUsesFixedSecurityPolicy(t *testing.T) {
	root := t.TempDir()
	name := "ga-document-01"
	slot := makeEmptySlot(t, root, name)
	cfg := validDockerConfig(root)
	runner := &scriptedRunner{replies: happyCreateReplies(cfg, name, slot)}
	runtime, err := newDockerCLI(cfg, runner)
	if err != nil {
		t.Fatal(err)
	}
	container, err := runtime.CreateAndStart(context.Background(), ContainerSpec{Name: name, SlotPath: slot})
	if err != nil {
		t.Fatal(err)
	}
	if container.ID != "container-id" || container.Name != name || container.SlotPath != slot {
		t.Fatalf("container=%+v", container)
	}
	if len(runner.calls) != 8 {
		t.Fatalf("calls=%d want 8", len(runner.calls))
	}
	if !reflect.DeepEqual(runner.calls[0].args, []string{"info", "--format", dockerHostInfoFormat}) {
		t.Fatalf("info args=%q", runner.calls[0].args)
	}
	if !reflect.DeepEqual(runner.calls[1].args, []string{"image", "inspect", "--format", "{{json .Config.Volumes}}", testImage}) {
		t.Fatalf("image inspect args=%q", runner.calls[1].args)
	}
	create := runner.calls[2].args
	for flag, value := range map[string]string{
		"--name":         name,
		"--network":      "none",
		"--cap-drop":     "ALL",
		"--security-opt": "no-new-privileges:true",
		"--user":         "1000:1000",
		"--memory":       "134217728",
		"--cpu-period":   "100000",
		"--cpu-quota":    "50000",
		"--pids-limit":   "64",
		"--tmpfs":        "/tmp:rw,noexec,nosuid,nodev,size=67108864",
		"--workdir":      "/workspace",
		"--entrypoint":   "/usr/local/bin/ga-document-tool",
		"--log-driver":   "none",
	} {
		assertFlagValue(t, create, flag, value)
	}
	assertFlagValue(t, create, "--security-opt", "seccomp=builtin")
	assertFlagValue(t, create, "--label", managerLabel+"=true")
	assertFlagValue(t, create, "--label", instanceLabel+"="+name)
	assertFlag(t, create, "--read-only")
	assertAbsent(t, create, "--privileged")
	assertAbsent(t, create, "--volume")
	assertAbsent(t, create, "--mount")
	wantTail := []string{testImage, "idle"}
	if !reflect.DeepEqual(create[len(create)-len(wantTail):], wantTail) {
		t.Fatalf("create tail=%q want=%q", create, wantTail)
	}
	if !reflect.DeepEqual(runner.calls[3].args, []string{"inspect", "--format", "{{json .}}", name}) {
		t.Fatalf("policy inspect args=%q", runner.calls[3].args)
	}
	if !reflect.DeepEqual(runner.calls[4].args, []string{"start", name}) {
		t.Fatalf("start args=%q", runner.calls[4].args)
	}
	if !reflect.DeepEqual(runner.calls[5].args, []string{"exec", "--user", "1000:1000", name, "/usr/local/bin/ga-document-tool", "read-cgroup", "memory.max"}) ||
		!reflect.DeepEqual(runner.calls[6].args, []string{"exec", "--user", "1000:1000", name, "/usr/local/bin/ga-document-tool", "read-cgroup", "cpu.max"}) ||
		!reflect.DeepEqual(runner.calls[7].args, []string{"exec", "--user", "1000:1000", name, "/usr/local/bin/ga-document-tool", "read-cgroup", "pids.max"}) {
		t.Fatalf("cgroup calls=%+v", runner.calls[5:])
	}
}

func TestDockerCLIVerifyHostFailsClosed(t *testing.T) {
	tests := []struct {
		name      string
		host      []byte
		wantError string
	}{
		{"not rootless", dockerHostInfo(false, "builtin", "2", true), "rootless"},
		{"unconfined seccomp", dockerHostInfo(true, "unconfined", "2", true), "seccomp"},
		{"cgroup v1", dockerHostInfo(true, "builtin", "1", true), "cgroup"},
		{"controllers unavailable", dockerHostInfo(true, "builtin", "2", false), "controller"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runtime, err := newDockerCLI(validDockerConfig(t.TempDir()), &scriptedRunner{replies: []runnerReply{{result: commandResult{stdout: tt.host}}}})
			if err != nil {
				t.Fatal(err)
			}
			if err := runtime.VerifyHost(context.Background()); err == nil || !strings.Contains(strings.ToLower(err.Error()), tt.wantError) {
				t.Fatalf("err=%v want substring %q", err, tt.wantError)
			}
		})
	}
}

func TestDockerCLIRequiresExplicitOptInForRootfulAndMutableComposeImage(t *testing.T) {
	cfg := validDockerConfig(t.TempDir())
	cfg.Image = "genericagent-document-tool:local"
	if _, err := newDockerCLI(cfg, &scriptedRunner{}); err == nil {
		t.Fatal("mutable local image must require explicit opt-in")
	}

	cfg.AllowMutableImage = true
	runtime, err := newDockerCLI(cfg, &scriptedRunner{replies: []runnerReply{{result: commandResult{stdout: dockerHostInfo(false, "builtin", "2", true)}}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.VerifyHost(context.Background()); err == nil || !strings.Contains(strings.ToLower(err.Error()), "rootless") {
		t.Fatalf("err=%v want rootless rejection", err)
	}

	cfg.AllowRootfulRuntime = true
	runtime, err = newDockerCLI(cfg, &scriptedRunner{replies: []runnerReply{{result: commandResult{stdout: dockerHostInfo(false, "builtin", "2", true)}}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.VerifyHost(context.Background()); err != nil {
		t.Fatalf("explicit rootful runtime should be accepted: %v", err)
	}
}

func TestPodmanVerifyHostUsesHostSecurity(t *testing.T) {
	cfg := validDockerConfig(t.TempDir())
	cfg.Binary = "podman"
	runner := &scriptedRunner{replies: []runnerReply{{result: commandResult{stdout: podmanHostInfo()}}}}
	runtime, err := newDockerCLI(cfg, runner)
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.VerifyHost(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := []string{"info", "--format", podmanHostInfoFormat}
	if len(runner.calls) != 1 || !reflect.DeepEqual(runner.calls[0].args, want) {
		t.Fatalf("calls=%+v want args=%q", runner.calls, want)
	}
}

func TestDockerCLIRejectsImageDeclaredVolumes(t *testing.T) {
	root := t.TempDir()
	name := "ga-document-volume"
	slot := makeEmptySlot(t, root, name)
	runner := &scriptedRunner{replies: []runnerReply{
		{result: commandResult{stdout: dockerHostInfo(true, "builtin", "2", true)}},
		{result: commandResult{stdout: []byte(`{"/data":{}}`)}},
	}}
	runtime, err := newDockerCLI(validDockerConfig(root), runner)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.CreateAndStart(context.Background(), ContainerSpec{Name: name, SlotPath: slot}); err == nil || !strings.Contains(err.Error(), "volume") {
		t.Fatalf("err=%v", err)
	}
	if len(runner.calls) != 2 {
		t.Fatalf("image with declared volume reached create: calls=%d", len(runner.calls))
	}
}

func TestDockerCLIRejectsUntrustedSlots(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	tests := []struct {
		name      string
		slot      string
		prepare   func(string) error
		wantError string
	}{
		{"outside", filepath.Join(outside, "outside"), func(path string) error { return os.Mkdir(path, 0o700) }, "work root"},
		{"nested", filepath.Join(root, "parent", "nested"), func(path string) error { return os.MkdirAll(path, 0o700) }, "direct child"},
		{"name mismatch", filepath.Join(root, "different"), func(path string) error { return os.Mkdir(path, 0o700) }, "match"},
		{"non-empty", filepath.Join(root, "non-empty"), func(path string) error {
			if err := os.Mkdir(path, 0o700); err != nil {
				return err
			}
			return os.WriteFile(filepath.Join(path, "data"), []byte("x"), 0o600)
		}, "empty"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.prepare(tt.slot); err != nil {
				t.Fatal(err)
			}
			runtime, err := newDockerCLI(validDockerConfig(root), &scriptedRunner{})
			if err != nil {
				t.Fatal(err)
			}
			name := filepath.Base(tt.slot)
			if tt.name == "name mismatch" {
				name = "expected"
			}
			if _, err := runtime.CreateAndStart(context.Background(), ContainerSpec{Name: name, SlotPath: tt.slot}); err == nil || !strings.Contains(err.Error(), tt.wantError) {
				t.Fatalf("err=%v want substring %q", err, tt.wantError)
			}
		})
	}

	t.Run("symlink", func(t *testing.T) {
		target := filepath.Join(root, "target")
		link := filepath.Join(root, "link")
		if err := os.Mkdir(target, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, link); err != nil {
			t.Skipf("symlink unavailable: %v", err)
		}
		runtime, err := newDockerCLI(validDockerConfig(root), &scriptedRunner{})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := runtime.CreateAndStart(context.Background(), ContainerSpec{Name: "link", SlotPath: link}); err == nil || !strings.Contains(err.Error(), "symlink") {
			t.Fatalf("err=%v", err)
		}
	})
}

func TestDockerCLICleansOwnedContainerAfterUncertainCreate(t *testing.T) {
	root := t.TempDir()
	name := "ga-document-uncertain"
	slot := makeEmptySlot(t, root, name)
	runner := &scriptedRunner{replies: []runnerReply{
		{result: commandResult{stdout: dockerHostInfo(true, "builtin", "2", true)}},
		{result: commandResult{stdout: []byte(`null`)}},
		{err: errors.New("connection reset after create")},
		{result: commandResult{stdout: ownedInspectJSON(name)}},
		{result: commandResult{}},
	}}
	runtime, err := newDockerCLI(validDockerConfig(root), runner)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.CreateAndStart(context.Background(), ContainerSpec{Name: name, SlotPath: slot}); err == nil {
		t.Fatal("expected uncertain create failure")
	}
	if len(runner.calls) != 5 || !reflect.DeepEqual(runner.calls[4].args, []string{"rm", "-f", testContainerID}) {
		t.Fatalf("calls=%+v", runner.calls)
	}
}

func TestDockerCLICleansPolicyAndCgroupFailures(t *testing.T) {
	t.Run("pre-start policy mismatch", func(t *testing.T) {
		root := t.TempDir()
		name := "ga-document-policy-mismatch"
		slot := makeEmptySlot(t, root, name)
		replies := []runnerReply{
			{result: commandResult{stdout: dockerHostInfo(true, "builtin", "2", true)}},
			{result: commandResult{stdout: []byte(`null`)}},
			{result: commandResult{stdout: []byte("container-id")}},
			{result: commandResult{stdout: insecureInspectJSON(name)}},
			{result: commandResult{stdout: ownedInspectJSON(name)}},
			{result: commandResult{}},
		}
		runner := &scriptedRunner{replies: replies}
		runtime, _ := newDockerCLI(validDockerConfig(root), runner)
		if _, err := runtime.CreateAndStart(context.Background(), ContainerSpec{Name: name, SlotPath: slot}); err == nil {
			t.Fatal("expected policy mismatch")
		}
		if !reflect.DeepEqual(runner.calls[len(runner.calls)-1].args, []string{"rm", "-f", testContainerID}) {
			t.Fatalf("calls=%+v", runner.calls)
		}
	})

	t.Run("pre-start process mismatch", func(t *testing.T) {
		root := t.TempDir()
		name := "ga-document-process-mismatch"
		slot := makeEmptySlot(t, root, name)
		cfg := validDockerConfig(root)
		var inspected map[string]any
		if err := json.Unmarshal(secureInspectJSON(cfg, name, slot), &inspected); err != nil {
			t.Fatal(err)
		}
		config := inspected["Config"].(map[string]any)
		config["Entrypoint"] = []string{"/bin/sh"}
		encoded, err := json.Marshal(inspected)
		if err != nil {
			t.Fatal(err)
		}
		runner := &scriptedRunner{replies: []runnerReply{
			{result: commandResult{stdout: dockerHostInfo(true, "builtin", "2", true)}},
			{result: commandResult{stdout: []byte(`null`)}},
			{result: commandResult{stdout: []byte("container-id")}},
			{result: commandResult{stdout: encoded}},
			{result: commandResult{stdout: ownedInspectJSON(name)}},
			{result: commandResult{}},
		}}
		runtime, _ := newDockerCLI(cfg, runner)
		if _, err := runtime.CreateAndStart(context.Background(), ContainerSpec{Name: name, SlotPath: slot}); err == nil || !strings.Contains(err.Error(), "process") {
			t.Fatalf("process mismatch err=%v", err)
		}
		for _, call := range runner.calls {
			if len(call.args) > 0 && call.args[0] == "start" {
				t.Fatalf("process mismatch reached start: calls=%+v", runner.calls)
			}
		}
		if !reflect.DeepEqual(runner.calls[len(runner.calls)-1].args, []string{"rm", "-f", testContainerID}) {
			t.Fatalf("calls=%+v", runner.calls)
		}
	})

	t.Run("applied cgroup mismatch", func(t *testing.T) {
		root := t.TempDir()
		name := "ga-document-cgroup-mismatch"
		slot := makeEmptySlot(t, root, name)
		cfg := validDockerConfig(root)
		replies := happyCreateReplies(cfg, name, slot)
		replies[5].result.stdout = []byte("max\n")
		replies = append(replies[:6], runnerReply{result: commandResult{stdout: ownedInspectJSON(name)}}, runnerReply{result: commandResult{}})
		runner := &scriptedRunner{replies: replies}
		runtime, _ := newDockerCLI(cfg, runner)
		if _, err := runtime.CreateAndStart(context.Background(), ContainerSpec{Name: name, SlotPath: slot}); err == nil {
			t.Fatal("expected applied cgroup mismatch")
		}
		if !reflect.DeepEqual(runner.calls[len(runner.calls)-1].args, []string{"rm", "-f", testContainerID}) {
			t.Fatalf("calls=%+v", runner.calls)
		}
	})
}

func TestDockerCLIStartFailureCleanupAndDestroyOwnership(t *testing.T) {
	root := t.TempDir()
	name := "ga-document-cleanup"
	slot := makeEmptySlot(t, root, name)
	cfg := validDockerConfig(root)
	replies := happyCreateReplies(cfg, name, slot)
	replies = append(replies[:4],
		runnerReply{result: commandResult{exitCode: 1, stderr: []byte("start failed")}},
		runnerReply{result: commandResult{stdout: ownedInspectJSON(name)}},
		runnerReply{result: commandResult{}},
		runnerReply{result: commandResult{exitCode: 1, stderr: []byte("No such container")}},
	)
	runner := &scriptedRunner{replies: replies}
	runtime, _ := newDockerCLI(cfg, runner)
	if _, err := runtime.CreateAndStart(context.Background(), ContainerSpec{Name: name, SlotPath: slot}); err == nil {
		t.Fatal("expected start failure")
	}
	if !reflect.DeepEqual(runner.calls[6].args, []string{"rm", "-f", testContainerID}) {
		t.Fatalf("cleanup args=%q", runner.calls[6].args)
	}
	if err := runtime.Destroy(context.Background(), name); err != nil {
		t.Fatalf("missing destroy: %v", err)
	}

	unownedOutput, _ := json.Marshal(map[string]any{
		"ID":     testContainerID,
		"Labels": map[string]string{"other": "true"},
	})
	unowned := &scriptedRunner{replies: []runnerReply{{result: commandResult{stdout: unownedOutput}}}}
	unownedRuntime, _ := newDockerCLI(cfg, unowned)
	if err := unownedRuntime.Destroy(context.Background(), "ga-document-unowned"); err == nil || !strings.Contains(err.Error(), "ownership") {
		t.Fatalf("unowned destroy err=%v", err)
	}
	if len(unowned.calls) != 1 {
		t.Fatalf("unowned container reached rm: calls=%+v", unowned.calls)
	}
}

func TestDockerCLIDestroyUsesInspectedImmutableID(t *testing.T) {
	cfg := validDockerConfig(t.TempDir())
	runner := &scriptedRunner{replies: []runnerReply{
		{result: commandResult{stdout: ownedInspectJSON("ga-document-owned")}},
		{result: commandResult{}},
	}}
	runtime, err := newDockerCLI(cfg, runner)
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.Destroy(context.Background(), "ga-document-owned"); err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != 2 || !reflect.DeepEqual(runner.calls[1].args, []string{"rm", "-f", testContainerID}) {
		t.Fatalf("destroy calls=%+v", runner.calls)
	}
}

func TestDockerCLIExecPassesStructuredArgvWithoutHostShell(t *testing.T) {
	root := t.TempDir()
	runner := &scriptedRunner{replies: []runnerReply{{result: commandResult{stdout: []byte("ok")}}}}
	runtime, err := newDockerCLI(validDockerConfig(root), runner)
	if err != nil {
		t.Fatal(err)
	}
	result, err := runtime.Exec(context.Background(), "ga-document-exec", []string{"python", "-c", "print('ok')"})
	if err != nil {
		t.Fatal(err)
	}
	if string(result.Stdout) != "ok" || result.ExitCode != 0 {
		t.Fatalf("result=%+v", result)
	}
	want := []string{"exec", "--workdir", "/workspace", "--user", "1000:1000", "ga-document-exec", "python", "-c", "print('ok')"}
	if !reflect.DeepEqual(runner.calls[0].args, want) {
		t.Fatalf("exec args=%q want=%q", runner.calls[0].args, want)
	}
}

func TestDockerCLIExecInputPassesBoundedStdinAndArtifactOutput(t *testing.T) {
	root := t.TempDir()
	runner := &scriptedRunner{replies: []runnerReply{{result: commandResult{stdout: []byte("docx-bytes")}}}}
	runtime, err := newDockerCLI(validDockerConfig(root), runner)
	if err != nil {
		t.Fatal(err)
	}
	stdin := []byte(`{"schema_version":1,"content":"hello"}`)
	result, err := runtime.ExecInput(context.Background(), "ga-document-exec", []string{"/usr/local/bin/ga-document-tool", "export-docx", "--input", "-", "--output", "-"}, stdin, 8<<20)
	if err != nil {
		t.Fatal(err)
	}
	if string(result.Stdout) != "docx-bytes" || result.ExitCode != 0 {
		t.Fatalf("result=%+v", result)
	}
	want := []string{"exec", "-i", "--workdir", "/workspace", "--user", "1000:1000", "ga-document-exec", "/usr/local/bin/ga-document-tool", "export-docx", "--input", "-", "--output", "-"}
	if !reflect.DeepEqual(runner.calls[0].args, want) || !reflect.DeepEqual(runner.calls[0].stdin, stdin) || runner.calls[0].stdoutLimit != 8<<20 {
		t.Fatalf("call=%+v", runner.calls[0])
	}
}

func TestDockerCLIExecInputRejectsInvalidBoundsAndPropagatesOverflow(t *testing.T) {
	runtime, err := newDockerCLI(validDockerConfig(t.TempDir()), &scriptedRunner{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.ExecInput(context.Background(), "ga-document-exec", []string{"tool"}, []byte("input"), 0); err == nil {
		t.Fatal("expected zero output limit rejection")
	}
	if _, err := runtime.ExecInput(context.Background(), "ga-document-exec", []string{"tool"}, make([]byte, maxDocumentCommandInputBytes+1), 1); err == nil {
		t.Fatal("expected oversized input rejection")
	}

	overflow := errors.New("command stdout exceeded limit")
	runner := &scriptedRunner{replies: []runnerReply{{err: overflow}}}
	runtime, err = newDockerCLI(validDockerConfig(t.TempDir()), runner)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.ExecInput(context.Background(), "ga-document-exec", []string{"tool"}, []byte("input"), 4); !errors.Is(err, overflow) {
		t.Fatalf("overflow err=%v", err)
	}
}

func validDockerConfig(root string) DockerConfig {
	return DockerConfig{
		Binary:         "docker",
		Image:          testImage,
		WorkRoot:       root,
		SeccompProfile: "builtin",
		UID:            1000,
		GID:            1000,
		MemoryBytes:    128 << 20,
		CPUPeriod:      100000,
		CPUQuota:       50000,
		PIDsLimit:      64,
		TmpfsBytes:     64 << 20,
		Command:        []string{"/usr/local/bin/ga-document-tool", "idle"},
	}
}

func happyCreateReplies(cfg DockerConfig, name, slot string) []runnerReply {
	return []runnerReply{
		{result: commandResult{stdout: dockerHostInfo(true, "builtin", "2", true)}},
		{result: commandResult{stdout: []byte(`null`)}},
		{result: commandResult{stdout: []byte("container-id\n")}},
		{result: commandResult{stdout: secureInspectJSON(cfg, name, slot)}},
		{result: commandResult{}},
		{result: commandResult{stdout: []byte("134217728\n")}},
		{result: commandResult{stdout: []byte("50000 100000\n")}},
		{result: commandResult{stdout: []byte("64\n")}},
	}
}

func dockerHostInfo(rootless bool, seccompProfile, cgroupVersion string, controllers bool) []byte {
	security := []string{"name=seccomp,profile=" + seccompProfile}
	if rootless {
		security = append(security, "name=rootless")
	}
	payload := map[string]any{
		"SecurityOptions": security,
		"CgroupVersion":   cgroupVersion,
		"MemoryLimit":     controllers,
		"CpuCfsPeriod":    controllers,
		"CpuCfsQuota":     controllers,
		"PidsLimit":       controllers,
	}
	encoded, _ := json.Marshal(payload)
	return encoded
}

func podmanHostInfo() []byte {
	payload := map[string]any{
		"cgroupsVersion":    "v2",
		"cgroupControllers": []string{"cpu", "memory", "pids"},
		"security":          map[string]any{"rootless": true, "seccompEnabled": true},
	}
	encoded, _ := json.Marshal(payload)
	return encoded
}

func secureInspectJSON(cfg DockerConfig, name, slot string) []byte {
	payload := map[string]any{
		"Config": map[string]any{
			"Image":      cfg.Image,
			"User":       "1000:1000",
			"Entrypoint": []string{documentToolExecutable},
			"Cmd":        []string{documentToolIdleCommand},
			"Labels":     map[string]string{managerLabel: "true", instanceLabel: name},
		},
		"HostConfig": map[string]any{
			"ReadonlyRootfs": true,
			"NetworkMode":    "none",
			"CapDrop":        []string{"ALL"},
			"SecurityOpt":    []string{"no-new-privileges:true", "seccomp=" + cfg.SeccompProfile},
			"Memory":         cfg.MemoryBytes,
			"CpuPeriod":      cfg.CPUPeriod,
			"CpuQuota":       cfg.CPUQuota,
			"PidsLimit":      cfg.PIDsLimit,
			"Tmpfs":          map[string]string{"/tmp": "rw,noexec,nosuid,nodev,size=67108864"},
			"LogConfig":      map[string]string{"Type": "none"},
		},
		"Mounts": []map[string]any{},
	}
	encoded, _ := json.Marshal(payload)
	return encoded
}

func insecureInspectJSON(name string) []byte {
	payload := map[string]any{
		"Config":     map[string]any{"Image": testImage, "User": "0:0", "Labels": map[string]string{managerLabel: "true", instanceLabel: name}},
		"HostConfig": map[string]any{},
	}
	encoded, _ := json.Marshal(payload)
	return encoded
}

const testContainerID = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func ownedInspectJSON(name string) []byte {
	encoded, _ := json.Marshal(map[string]any{
		"ID":     testContainerID,
		"Labels": map[string]string{managerLabel: "true", instanceLabel: name},
	})
	return encoded
}

func makeEmptySlot(t *testing.T, root, name string) string {
	t.Helper()
	slot := filepath.Join(root, name)
	if err := os.Mkdir(slot, 0o700); err != nil {
		t.Fatal(err)
	}
	return slot
}

func assertFlag(t *testing.T, args []string, flag string) {
	t.Helper()
	for _, arg := range args {
		if arg == flag {
			return
		}
	}
	t.Fatalf("missing flag %s in %q", flag, args)
}

func assertFlagValue(t *testing.T, args []string, flag, value string) {
	t.Helper()
	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag && args[i+1] == value {
			return
		}
	}
	t.Fatalf("missing %s %q in %q", flag, value, args)
}

func assertAbsent(t *testing.T, args []string, forbidden string) {
	t.Helper()
	for _, arg := range args {
		if arg == forbidden {
			t.Fatalf("forbidden argument %s in %q", forbidden, args)
		}
	}
}
