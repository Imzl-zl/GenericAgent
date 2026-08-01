// Command sandbox-manager 是唯一持有 Docker runtime socket 的组件(方案 §7):
// 按固定 profile 创建/检查/销毁用户 Runner, 处理 idle 回收与孤儿清理。
// 不进行业务调度, 不接受任何业务输入作为容器参数。
package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/infrastructure/sandbox"
)

func main() {
	var (
		dockerBinary = flag.String("docker-binary", envOr("GA_DOCKER_BINARY", "docker"), "docker binary path")
		image        = flag.String("runner-image", envOr("GA_RUNNER_IMAGE", "ga-runner:local"), "ga-runner image reference")
		workspaces   = flag.String("workspaces-root", envOr("GA_WORKSPACES_ROOT", "/var/lib/ga/workspaces"), "host root containing workspaces/<hash>/")
		memoryTmpl   = flag.String("memory-template", envOr("GA_MEMORY_TEMPLATE", "/ga/memory-template"), "read-only memory template path inside the manager image")
		idleTTL      = flag.Duration("idle-ttl", 30*time.Minute, "Runner idle reclamation TTL")
		prefix       = flag.String("container-prefix", "ga-runner", "Runner container name prefix")
		profileName  = flag.String("security-profile", envOr("GA_RUNNER_SECURITY_PROFILE", ""), "container runtime (runsc for untrusted production; empty = docker)")
		reapInterval = flag.Duration("reap-interval", 5*time.Minute, "idle/orphan reclamation sweep interval")
	)
	flag.Parse()

	if strings.TrimSpace(*workspaces) == "" {
		slog.Error("workspaces-root is required")
		os.Exit(2)
	}

	profile := sandbox.ValidProfile()
	profile.Image = *image
	profile.Runtime = *profileName
	if err := profile.Validate(); err != nil {
		slog.Error("invalid runner profile", "error", err)
		os.Exit(2)
	}

	cli, err := sandbox.NewDockerCLI(sandbox.DockerConfig{
		Binary:             *dockerBinary,
		Profile:            profile,
		WorkspacesRoot:     *workspaces,
		ContainerNamePrefix: *prefix,
	})
	if err != nil {
		slog.Error("docker cli", "error", err)
		os.Exit(2)
	}
	manager := sandbox.NewManager(sandbox.ManagerConfig{
		CLI:                 cli,
		WorkspaceRoot:       *workspaces,
		MemoryTemplate:      *memoryTmpl,
		ContainerNamePrefix: *prefix,
		Image:               *image,
	})
	_ = manager

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	slog.Info("sandbox-manager started",
		"image", *image,
		"workspaces_root", *workspaces,
		"idle_ttl", idleTTL.String(),
		"runtime", profile.Runtime,
	)

	// 周期清理: 查找本 Manager 拥有的过期 Runner 容器并销毁(孤儿清理)。
	ticker := time.NewTicker(*reapInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			slog.Info("sandbox-manager shutting down")
			return
		case <-ticker.C:
			sweepIdleRunners(ctx, cli, *prefix, *idleTTL)
		}
	}
}

// sweepIdleRunners 列出本 Manager 创建的 Runner 容器, 销毁 idle TTL 内无
// 活跃任务且容器创建时间早于 TTL 的孤儿(容器 label 带 owner 标识)。
func sweepIdleRunners(ctx context.Context, cli *sandbox.DockerCLI, prefix string, idleTTL time.Duration) {
	// V1: 容器销毁由 Platform 侧 Runner lease 到期驱动(任务 4/5 接线后),
	// 此处先按容器创建时间做兜底孤儿回收。label 过滤防止误删其他组件容器。
	names, err := cli.ListRunnerContainers(ctx, prefix)
	if err != nil {
		slog.WarnContext(ctx, "sandbox-manager: list runner containers failed", "error", err)
		return
	}
	for _, name := range names {
		if err := cli.Destroy(ctx, name); err != nil {
			slog.WarnContext(ctx, "sandbox-manager: destroy orphan runner failed", "name", name, "error", err)
		} else {
			slog.InfoContext(ctx, "sandbox-manager: destroyed orphan runner", "name", name)
		}
	}
}

func envOr(name, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(name)); v != "" {
		return v
	}
	return fallback
}
