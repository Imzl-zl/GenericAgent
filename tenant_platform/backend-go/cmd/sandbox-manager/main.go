// Command sandbox-manager 是唯一持有 Docker runtime socket 的组件(方案 §7):
// 按固定 profile 创建/检查/销毁用户 Runner, 处理孤儿兜底回收。
// 不进行业务调度, 不接受任何业务输入作为容器参数。
//
// Platform 经认证的 HTTP 控制面(/v1/runners/*)调用本组件:
//   - POST   /v1/runners/ensure   创建/复用 Runner(携带 mTLS 材料与控制面环境)
//   - DELETE /v1/runners/{name}   销毁 Runner
//   - GET    /v1/runners/{name}   校验 Runner 固定 profile
//   - GET    /v1/runners          列出 Runner
package main

import (
	"context"
	"flag"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/infrastructure/sandbox"
)

func main() {
	var (
		dockerBinary       = flag.String("docker-binary", envOr("GA_DOCKER_BINARY", "docker"), "docker binary path")
		image              = flag.String("runner-image", envOr("GA_RUNNER_IMAGE", "ga-runner:local"), "ga-runner image reference")
		workspaces         = flag.String("workspaces-root", envOr("GA_WORKSPACES_ROOT", "/var/lib/ga/workspaces"), "daemon-visible root containing workspaces/<hash>/")
		workspacesVolume   = flag.String("workspaces-volume", envOr("GA_WORKSPACES_VOLUME", ""), "named volume for workspaces (Compose); empty = bind mount from workspaces-root")
		memoryTmpl         = flag.String("memory-template", envOr("GA_MEMORY_TEMPLATE", "/ga/memory-template"), "read-only memory template path inside the manager image")
		idleTTL            = flag.Duration("idle-ttl", 30*time.Minute, "Runner idle TTL(日志参考; 实际空闲回收由 Platform lease 驱动, 见 GA_RUNNER_IDLE_TTL)")
		absTTL             = flag.Duration("reap-abs-ttl", 24*time.Hour, "legacy flag kept for CLI compatibility; running containers are never reaped by age (review I5: active runners are managed by Platform lease lifecycle)")
		prefix             = flag.String("container-prefix", "ga-runner", "Runner container name prefix")
		profileName        = flag.String("security-profile", envOr("GA_RUNNER_SECURITY_PROFILE", ""), "container runtime (runsc for untrusted production; empty = docker, requires --allow-runc)")
		allowRunc          = flag.Bool("allow-runc", envOrBool("GA_RUNNER_ALLOW_RUNC", false), "explicitly allow the default docker runtime (trusted local dev only; production must use runsc)")
		shareGID           = flag.Int("share-gid", envOrInt("GA_RUNNER_SHARE_GID", 10003), "shared workspace group id (Platform compose group_add)")
		memoryBytes        = flag.Int64("memory-bytes", envIntOr("GA_RUNNER_MEMORY_BYTES", 1<<30), "Runner memory limit in bytes (or GA_RUNNER_MEMORY_BYTES)")
		cpuQuota           = flag.Int64("cpu-quota", envIntOr("GA_RUNNER_CPU_QUOTA", 100000), "Runner CPU quota per period (or GA_RUNNER_CPU_QUOTA)")
		pidsLimit          = flag.Int64("pids-limit", envIntOr("GA_RUNNER_PIDS_LIMIT", 128), "Runner pids limit (or GA_RUNNER_PIDS_LIMIT)")
		allowMutableTag    = flag.Bool("allow-mutable-tag", envOrBool("GA_RUNNER_ALLOW_TAG", false), "allow non-digest runner image tag (local dev only)")
		reapInterval       = flag.Duration("reap-interval", 5*time.Minute, "orphan reclamation sweep interval")
		controlAddr        = flag.String("control-addr", envOr("GA_MANAGER_CONTROL_ADDR", "0.0.0.0:8091"), "Platform control API listen address")
		controlSecret      = flag.String("control-secret", envOr("GA_MANAGER_SECRET", ""), "HMAC secret for the Platform control API (required)")
		managerID          = flag.String("manager-id", envOr("GA_MANAGER_ID", ""), "manager instance id stamped on Runner labels")
	)
	flag.Parse()

	if strings.TrimSpace(*workspaces) == "" {
		slog.Error("workspaces-root is required")
		os.Exit(2)
	}
	if len(*controlSecret) < 16 {
		slog.Error("control-secret (GA_MANAGER_SECRET) is required and must be at least 16 bytes")
		os.Exit(2)
	}
	if strings.TrimSpace(*managerID) == "" {
		slog.Error("manager-id (GA_MANAGER_ID) is required")
		os.Exit(2)
	}

	profile := sandbox.ValidProfile()
	profile.Image = *image
	profile.Runtime = *profileName
	profile.ShareGID = *shareGID
	profile.AllowMutableTag = *allowMutableTag
	profile.AllowRunc = *allowRunc
	// 审查 M12: compose 暴露的调优变量真正接入固定 profile, 非法值拒绝。
	profile.MemoryBytes = *memoryBytes
	profile.CPUQuota = *cpuQuota
	profile.PIDsLimit = *pidsLimit
	if err := profile.Validate(); err != nil {
		slog.Error("invalid runner profile", "error", err)
		os.Exit(2)
	}

	cli, err := sandbox.NewDockerCLI(sandbox.DockerConfig{
		Binary:              *dockerBinary,
		Profile:             profile,
		WorkspacesRoot:      *workspaces,
		WorkspaceVolume:     *workspacesVolume,
		ContainerNamePrefix: *prefix,
		ManagerID:           *managerID,
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

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Platform 控制面: 认证的 HTTP API(唯一入口)。
	server, err := sandbox.NewManagerServer(manager, *controlSecret)
	if err != nil {
		slog.Error("manager control server", "error", err)
		os.Exit(2)
	}
	httpSrv := &http.Server{
		Addr:              *controlAddr,
		Handler:           server.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	ln, err := net.Listen("tcp", *controlAddr)
	if err != nil {
		slog.Error("manager control listen", "error", err)
		os.Exit(2)
	}
	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shutCtx)
	}()
	go func() {
		slog.Info("sandbox-manager control API listening", "addr", *controlAddr)
		if err := httpSrv.Serve(ln); err != nil && err != http.ErrServerClosed {
			slog.Error("manager control server failed", "error", err)
			stop()
		}
	}()

	slog.Info("sandbox-manager started",
		"image", *image,
		"workspaces_root", *workspaces,
		"workspaces_volume", *workspacesVolume,
		"idle_ttl", idleTTL.String(),
		"abs_orphan_ttl", absTTL.String(),
		"runtime", profile.Runtime,
		"manager_id", *managerID,
	)

	// 周期兜底回收: 仅清理本 Manager 的孤儿(已退出容器 + 超过绝对 TTL 的
	// 运行容器)。正常 idle 回收由 Platform lease 驱动(lease 过期 → 销毁)。
	// 绝不根据容器创建时间杀掉活跃 Runner。
	ticker := time.NewTicker(*reapInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			slog.Info("sandbox-manager shutting down")
			return
		case <-ticker.C:
			sweepOrphans(ctx, cli, *prefix, *absTTL)
		}
	}
}

// sweepOrphans 列出本 Manager 创建的 Runner 容器(label 过滤), 清理:
//   - 已退出/从未启动的容器(僵尸);
//   - 运行中容器一律跳过(审查): 活跃 Runner 由 Platform lease 生命周期
//     管理(续租→idle 回收; lease 过期→Platform reconcile 销毁容器),
//     按容器创建时间杀运行中容器会误杀仍在续租/正在执行任务的 Runner。
func sweepOrphans(ctx context.Context, cli *sandbox.DockerCLI, prefix string, absTTL time.Duration) {
	_ = absTTL
	containers, err := cli.ListRunnerContainers(ctx, prefix)
	if err != nil {
		slog.WarnContext(ctx, "sandbox-manager: list runner containers failed", "error", err)
		return
	}
	for _, c := range containers {
		if c.Running {
			continue // 活跃 Runner 由 Platform lease 生命周期管理
		}
		if err := cli.Destroy(ctx, c.Name); err != nil {
			slog.WarnContext(ctx, "sandbox-manager: destroy orphan runner failed", "name", c.Name, "error", err)
		} else {
			slog.InfoContext(ctx, "sandbox-manager: destroyed orphan runner",
				"name", c.Name, "running", c.Running, "created_at", c.CreatedAt.UTC().Format(time.RFC3339))
		}
	}
}

func envOr(name, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(name)); v != "" {
		return v
	}
	return fallback
}

func envOrInt(name string, fallback int) int {
	if v := strings.TrimSpace(os.Getenv(name)); v != "" {
		if parsed, err := strconv.Atoi(v); err == nil {
			return parsed
		}
	}
	return fallback
}

// envIntOr 严格解析环境变量为 int64; 值存在但非法时返回 -1 并输出错误,
// 由调用方拒绝启动(审查 M12: 部署暴露的调优变量非法值不得静默回退)。
func envIntOr(name string, fallback int64) int64 {
	v := strings.TrimSpace(os.Getenv(name))
	if v == "" {
		return fallback
	}
	parsed, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		slog.Error("invalid integer environment variable", "name", name, "value", v)
		os.Exit(2)
	}
	return parsed
}

func envOrBool(name string, fallback bool) bool {
	if v := strings.TrimSpace(os.Getenv(name)); v != "" {
		if parsed, err := strconv.ParseBool(v); err == nil {
			return parsed
		}
	}
	return fallback
}
