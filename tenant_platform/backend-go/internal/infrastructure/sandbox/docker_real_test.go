package sandbox

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// realDockerAvailable 探测本机 docker daemon 可用性; 不可用时跳过测试。
// 真实集成测试覆盖审查 C1 的生产路径: named volume + volume-subpath 的
// post-create inspect 必须从 HostConfig.Mounts 关联 subpath(Docker 29.6.2
// 实测顶层 .Mounts 不含 VolumeOptions.Subpath)。
func realDockerAvailable(t *testing.T) (string, bool) {
	t.Helper()
	dockerBin, err := exec.LookPath("docker")
	if err != nil {
		return "", false
	}
	cmd := exec.Command(dockerBin, "version", "--format", "{{.Server.Version}}")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Logf("docker daemon unavailable: %v (%s)", err, strings.TrimSpace(string(out)))
		return "", false
	}
	// round13 审查(CI): 真实测试依赖本地构建的 ga-runner:local 镜像——
	// GitHub runner 有 docker daemon 但没有该镜像, 修复前测试直接失败
	// (pull access denied)而非跳过。镜像缺失时视为前置条件不满足。
	img := exec.Command(dockerBin, "image", "inspect", "ga-runner:local")
	if out, err := img.CombinedOutput(); err != nil {
		t.Logf("ga-runner:local image unavailable: %v (%s)", err, strings.TrimSpace(string(out)))
		return "", false
	}
	return dockerBin, true
}

func mustRealRun(t *testing.T, dockerBin string, args ...string) string {
	t.Helper()
	cmd := exec.Command(dockerBin, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("docker %s failed: %v\n%s", strings.Join(args, " "), err, string(out))
	}
	return strings.TrimSpace(string(out))
}

// selfSignedRunnerTLS 生成自签证书三件套, 供真实 Docker 集成测试注入
// config/: Worker 以 mTLS 模式启动后保持常驻(审查 R5-I7: 容器必须处于
// running 状态才能通过 Inspect 复用校验)。
func selfSignedRunnerTLS(t *testing.T) map[string][]byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "ga-runner-it"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	ders, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ders})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	return map[string][]byte{
		"server.crt":  certPEM,
		"server.key":  keyPEM,
		"ca.crt":      certPEM,
		"policy.json": []byte(`{"version":"foundation.no-host-tools.v1"}`),
	}
}

func TestRealDockerCreateStartInspectVolumeSubpath(t *testing.T) {
	dockerBin, ok := realDockerAvailable(t)
	if !ok {
		t.Skip("real docker daemon not available")
	}
	// 确保 runner-control 网络存在(生产 Compose 创建; 本机已有或临时创建)。
	netExists := true
	if out, err := exec.Command(dockerBin, "network", "inspect", RunnerNetwork).CombinedOutput(); err != nil {
		netExists = false
		_ = out
		mustRealRun(t, dockerBin, "network", "create", RunnerNetwork)
		t.Cleanup(func() { mustRealRun(t, dockerBin, "network", "rm", RunnerNetwork) })
	}
	_ = netExists

	volumeName := fmt.Sprintf("ga-review-it-%d", os.Getpid())
	mustRealRun(t, dockerBin, "volume", "create", volumeName)
	t.Cleanup(func() { mustRealRun(t, dockerBin, "volume", "rm", volumeName) })

	// volume-subpath 的卷内子目录必须预先存在(Docker 实测: 不存在时
	// 容器启动失败 "cannot access path ... no such file or directory")。
	// 生产 Compose 中 Manager 挂载同一卷为 workspaces-root, prepareWorkspaceDirs
	// 会创建这些子目录; 这里用 busybox 模拟同等的卷内预建。
	hash := strings.Repeat("ab", 32)
	mustRealRun(t, dockerBin, "run", "--rm",
		"--mount", "type=volume,source="+volumeName+",destination=/v",
		"busybox:latest", "sh", "-c",
		"mkdir -p /v/"+hash+"/memory /v/"+hash+"/temp /v/"+hash+"/state/staging /v/"+hash+"/state/committed /v/"+hash+"/state/results /v/"+hash+"/config/g1")

	profile := ValidProfile()
	profile.AllowRunc = true // 测试用默认 runc 运行时(显式 trusted 开关)
	profile.AllowMutableTag = true
	profile.Image = "ga-runner:local" // 生产镜像(ENTRYPOINT 支持 --listen)
	cli, err := NewDockerCLI(DockerConfig{
		Binary:              dockerBin,
		Profile:             profile,
		WorkspacesRoot:      t.TempDir(),
		WorkspaceVolume:     volumeName,
		ContainerNamePrefix: "ga-runner-it",
		ManagerID:           "it-test",
	})
	if err != nil {
		t.Fatalf("NewDockerCLI: %v", err)
	}
	runner, err := cli.CreateAndStart(context.Background(), RunnerSpec{
		WorkspaceKey:  "personal:1",
		WorkspaceHash: hash,
		Generation:    1,
		Image:         "ga-runner:local",
		ConfigFiles:   selfSignedRunnerTLS(t),
	})
	if err != nil {
		// 创建/启动失败时也清理容器, 避免卷被残留容器占用无法删除。
		_ = cli.Destroy(context.Background(), cli.RunnerName(hash, 1))
		t.Fatalf("CreateAndStart on real docker: %v", err)
	}
	t.Cleanup(func() { _ = cli.Destroy(context.Background(), runner.Name) })

	// 审查 C1 核心: 真实 docker inspect 解析路径必须通过——subpath 从
	// HostConfig.Mounts 关联, 顶层 .Mounts 的 VolumeOptions 为空。
	if err := cli.Inspect(context.Background(), runner.Name); err != nil {
		if out, lerr := exec.Command(dockerBin, "logs", runner.Name).CombinedOutput(); lerr == nil {
		if st, lerr := exec.Command(dockerBin, "inspect", "-f", "{{.State.Status}} exit={{.State.ExitCode}} err={{.State.Error}}", runner.Name).CombinedOutput(); lerr == nil {
			t.Logf("runner state: %s", string(st))
		}
			t.Logf("runner logs:\n%s", string(out))
		}
		t.Fatalf("Inspect on real docker volume-subpath runner: %v", err)
	}
	// 归属标签必须可读(销毁前归属校验依赖)。
	owned, err := cli.IsManagerRunner(context.Background(), runner.Name)
	if err != nil || !owned {
		t.Fatalf("IsManagerRunner = %v, %v; want true", owned, err)
	}
}

// TestRealDockerInspectRejectsVolumeSubpathDrift 用真实 Docker 验证 subpath
// 漂移会被 inspect 拒绝: 把容器改为挂载错误子路径后, Inspect 必须失败
// (fail-closed, 防挂错工作区)。
func TestRealDockerInspectRejectsVolumeSubpathDrift(t *testing.T) {
	dockerBin, ok := realDockerAvailable(t)
	if !ok {
		t.Skip("real docker daemon not available")
	}
	volumeName := fmt.Sprintf("ga-review-drift-%d", os.Getpid())
	mustRealRun(t, dockerBin, "volume", "create", volumeName)
	t.Cleanup(func() { mustRealRun(t, dockerBin, "volume", "rm", volumeName) })
	hash := strings.Repeat("ab", 32)
	other := strings.Repeat("cd", 32)
	mustRealRun(t, dockerBin, "run", "--rm",
		"--mount", "type=volume,source="+volumeName+",destination=/v",
		"busybox:latest", "sh", "-c",
		"mkdir -p /v/"+hash+"/memory /v/"+other+"/memory")

	name := fmt.Sprintf("ga-runner-drift-%d", os.Getpid())
	containerID := mustRealRun(t, dockerBin, "create",
		"--name", name,
		"--label", "com.genericagent.runner=true",
		"--label", "com.genericagent.runner.hash="+hash,
		"--label", "com.genericagent.runner.generation=1",
		"--mount", "type=volume,source="+volumeName+",destination="+LegacyMemoryMount+",volume-subpath="+other+"/memory",
		"busybox:latest", "true")
	t.Cleanup(func() { _ = exec.Command(dockerBin, "rm", "-f", containerID).Run() })

	profile := ValidProfile()
	profile.AllowRunc = true
	profile.AllowMutableTag = true
	profile.Image = "busybox:latest"
	cli, err := NewDockerCLI(DockerConfig{
		Binary:              dockerBin,
		Profile:             profile,
		WorkspacesRoot:      t.TempDir(),
		WorkspaceVolume:     volumeName,
		ContainerNamePrefix: "ga-runner",
	})
	if err != nil {
		t.Fatalf("NewDockerCLI: %v", err)
	}
	if err := cli.Inspect(context.Background(), name); err == nil {
		t.Fatal("inspect must reject volume-subpath pointing at another workspace")
	}
}

// TestRealDockerVolumeSubpathRequiresPreexistingDir 记录 Docker 行为契约:
// volume-subpath 的卷内子目录不存在时容器启动失败。生产 Compose 依赖
// Manager 挂载同一卷为 workspaces-root 并在创建 Runner 前预建子目录
// (prepareWorkspaceDirs/EnsureWorkspace), 此测试防该前提被静默破坏。
func TestRealDockerVolumeSubpathRequiresPreexistingDir(t *testing.T) {
	dockerBin, ok := realDockerAvailable(t)
	if !ok {
		t.Skip("real docker daemon not available")
	}
	volumeName := fmt.Sprintf("ga-review-pre-%d", os.Getpid())
	mustRealRun(t, dockerBin, "volume", "create", volumeName)
	t.Cleanup(func() { mustRealRun(t, dockerBin, "volume", "rm", volumeName) })
	name := fmt.Sprintf("ga-runner-pre-%d", os.Getpid())
	mustRealRun(t, dockerBin, "create", "--name", name,
		"--mount", "type=volume,source="+volumeName+",destination=/m,volume-subpath=missing/sub",
		"busybox:latest", "true")
	t.Cleanup(func() { _ = exec.Command(dockerBin, "rm", "-f", name).Run() })
	cmd := exec.Command(dockerBin, "start", name)
	if out, err := cmd.CombinedOutput(); err == nil {
		t.Fatalf("docker start with missing volume-subpath dir unexpectedly succeeded:\n%s", string(out))
	}
	_ = filepath.Join // 保持 filepath 导入(将来断言 host 路径时使用)
}
