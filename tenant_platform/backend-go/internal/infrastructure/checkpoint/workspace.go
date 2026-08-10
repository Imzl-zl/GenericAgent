package checkpoint

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/infrastructure/postgres"
	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/infrastructure/safefs"
)

// WorkspaceConfig configures the production workspace coordinator.
type WorkspaceConfig struct {
	WorkspacesRoot     string // 共享卷根: workspaces/<hash>/state/...
	PlatformInstanceID string
	Store              *postgres.Store
	DefaultMaxBundle   uint64
	// RunnerStateMount 是 Runner 容器内 state 挂载点(方案 §7: /ga/runner-state)。
	// 恢复路径必须用容器内路径返回(Worker 读取), 提交路径用共享卷宿主路径。
	RunnerStateMount string
}

// WorkspaceCoordinator stages checkpoints inside the shared workspace volume
// so the Sandbox Runner can write them (方案 §5: staging 与 committed 都在
// Runner 可见的 state/ 内), while atomic DB commit stays in checkpoint_store.
//
// 布局(workspaces/<hash>/state/):
//
//	staging/   Runner 写入 checkpoint bundle(Platform 校验后消费)
//	committed/ Platform 写入不可变 bundle(Runner 恢复时读取)
//	results/   Platform 写入最终结果
type WorkspaceCoordinator struct {
	workspacesRoot     string
	platformInstanceID string
	store              *postgres.Store
	defaultMaxBundle   uint64
	runnerStateMount   string
}

// NewWorkspaceCoordinator constructs the production coordinator.
func NewWorkspaceCoordinator(cfg WorkspaceConfig) (*WorkspaceCoordinator, error) {
	if cfg.Store == nil {
		return nil, fmt.Errorf("store is required")
	}
	if strings.TrimSpace(cfg.PlatformInstanceID) == "" {
		return nil, fmt.Errorf("platform instance id is required")
	}
	if strings.TrimSpace(cfg.WorkspacesRoot) == "" {
		return nil, fmt.Errorf("workspaces root is required")
	}
	if cfg.DefaultMaxBundle == 0 {
		cfg.DefaultMaxBundle = 2 * 1024 * 1024
	}
	if strings.TrimSpace(cfg.RunnerStateMount) == "" {
		cfg.RunnerStateMount = "/ga/runner-state"
	}
	root, err := filepath.Abs(cfg.WorkspacesRoot)
	if err != nil {
		return nil, err
	}
	return &WorkspaceCoordinator{
		workspacesRoot:     root,
		platformInstanceID: cfg.PlatformInstanceID,
		store:              cfg.Store,
		defaultMaxBundle:   cfg.DefaultMaxBundle,
		runnerStateMount:   cfg.RunnerStateMount,
	}, nil
}

// sessionKeyFor 从 DB workspaces 表解析 workspace UUID → session key。
func (c *WorkspaceCoordinator) sessionKeyFor(ctx context.Context, workspaceID string) (string, error) {
	key, err := c.store.WorkspaceKeyByID(ctx, workspaceID)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(key) == "" {
		return "", fmt.Errorf("workspace %s has no session key", workspaceID)
	}
	return key, nil
}

// stateRoot 返回 workspaces/<hash(workspace_key)>/state。hash 推导与容器
// 挂载/沙箱共用 domain.WorkspaceDirHash 唯一实现(审查 B1 收敛), 保证
// checkpoint 与容器挂载一致。
func (c *WorkspaceCoordinator) stateRoot(workspaceKey string) (string, string, error) {
	hash, err := domain.WorkspaceDirHash(workspaceKey)
	if err != nil {
		return "", "", fmt.Errorf("invalid session key for workspace dir: %w", err)
	}
	state := filepath.Join(c.workspacesRoot, hash, "state")
	return state, hash, nil
}

// RunnerStagingRef 把 Prepare 返回的宿主 staging 路径映射为 Runner 容器内
// 路径(/ga/runner-state/...): Worker 只允许写 runtime root 内的 staging_ref
// (方案 §7)。校验宿主路径结构, 拒绝越界。
func (c *WorkspaceCoordinator) RunnerStagingRef(hostRef string) (string, error) {
	_, token, err := parseStateStagingRef(hostRef, c.workspacesRoot)
	if err != nil {
		return "", err
	}
	return filepath.ToSlash(filepath.Join(c.runnerStateMount, "staging", token)), nil
}

// HostStagingRef 校验 Worker 返回的容器内 staging ref 与期望宿主 ref
// 指向同一 token, 返回宿主 ref(DB 记录为宿主路径, Commit 按它校验)。
func (c *WorkspaceCoordinator) HostStagingRef(runnerRef, expectedHostRef string) (string, error) {
	cleanRef := filepath.Clean(filepath.FromSlash(runnerRef))
	runnerToken, ok := strings.CutPrefix(cleanRef, filepath.Join(c.runnerStateMount, "staging")+string(filepath.Separator))
	if !ok || runnerToken == "" || strings.ContainsAny(runnerToken, `\/`) {
		return "", fmt.Errorf("runner staging ref %q not under %s/staging", runnerRef, c.runnerStateMount)
	}
	_, expectedToken, err := parseStateStagingRef(expectedHostRef, c.workspacesRoot)
	if err != nil {
		return "", err
	}
	if runnerToken != expectedToken {
		return "", fmt.Errorf("runner staging ref token mismatch: got %q want %q", runnerToken, expectedToken)
	}
	return filepath.Clean(expectedHostRef), nil
}

// parseStateStagingRef 校验宿主 staging ref 位于 workspaces/<hash>/state/staging
// 并返回 hash 与 token。
func parseStateStagingRef(hostRef, workspacesRoot string) (hash, token string, err error) {
	absRoot, err := filepath.Abs(workspacesRoot)
	if err != nil {
		return "", "", err
	}
	absRef, err := filepath.Abs(hostRef)
	if err != nil {
		return "", "", err
	}
	rel, err := filepath.Rel(absRoot, absRef)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", "", fmt.Errorf("staging ref escapes workspaces root")
	}
	parts := strings.Split(filepath.ToSlash(rel), "/")
	if len(parts) != 4 || !isHex64(parts[0]) || parts[1] != "state" || parts[2] != "staging" || parts[3] == "" {
		return "", "", fmt.Errorf("staging ref not under workspaces/<hash>/state/staging")
	}
	return parts[0], parts[3], nil
}

// Prepare creates writing snapshot metadata and a token-scoped staging path
// under workspaces/<hash>/state/staging (Runner-visible).
func (c *WorkspaceCoordinator) Prepare(ctx context.Context, request CheckpointPrepareRequest) (CheckpointLease, error) {
	if strings.TrimSpace(request.TaskID) == "" {
		return CheckpointLease{}, fmt.Errorf("task_id required")
	}
	if request.RunnerGeneration == 0 {
		return CheckpointLease{}, fmt.Errorf("runner generation required")
	}
	// SessionKey == workspace key; fall back to workspace UUID resolution.
	workspaceKey := request.SessionKey
	if workspaceKey == "" {
		var err error
		workspaceKey, err = c.sessionKeyFor(ctx, request.WorkspaceID)
		if err != nil {
			return CheckpointLease{}, fmt.Errorf("resolve workspace key: %w", err)
		}
	}
	stateRoot, hash, err := c.stateRoot(workspaceKey)
	if err != nil {
		return CheckpointLease{}, err
	}
	stagingDir := filepath.Join(stateRoot, "staging")
	if err := safefs.MkdirAllBeneath(c.workspacesRoot, filepath.Join(hash, "state", "staging"), 0o770); err != nil {
		return CheckpointLease{}, fmt.Errorf("create staging dir: %w", err)
	}
	maxB := request.MaxBundleBytes
	if maxB == 0 {
		maxB = c.defaultMaxBundle
	}
	stagingRefFor := func(snapshotID, token string, generation int64) string {
		return filepath.Join(stagingDir, token+".bundle.json")
	}
	snapshotID, token, _, err := c.store.PrepareCheckpoint(ctx, request.TaskID, c.platformInstanceID, request.RunnerGeneration, stagingRefFor, maxB)
	if err != nil {
		return CheckpointLease{}, err
	}
	return CheckpointLease{
		SnapshotID:     snapshotID,
		Token:          token,
		StagingRef:     stagingRefFor(snapshotID, token, 0),
		MaxBundleBytes: maxB,
	}, nil
}

// Commit verifies the staging bundle (path containment, size, checksum,
// result digest), renames it immutably under state/committed, extracts the
// result under state/results, and returns opaque refs.
func (c *WorkspaceCoordinator) Commit(ctx context.Context, ready ReadyCheckpoint) (CommittedCheckpoint, error) {
	if ready.TaskID == "" || ready.SnapshotID == "" || ready.CheckpointToken == "" {
		return CommittedCheckpoint{}, fmt.Errorf("ready checkpoint missing identifiers")
	}
	_, taskID, stagingRef, leaseOwner, leaseUntil, _, runnerGeneration, maxBundleBytes, state, err := c.store.LoadSnapshotToken(ctx, ready.SnapshotID, ready.CheckpointToken)
	if err != nil {
		return CommittedCheckpoint{}, fmt.Errorf("token mismatch or unknown snapshot: %w", err)
	}
	if state != "writing" {
		return CommittedCheckpoint{}, fmt.Errorf("snapshot state is %s, want writing", state)
	}
	if taskID != ready.TaskID {
		return CommittedCheckpoint{}, fmt.Errorf("task id mismatch")
	}
	if leaseOwner != c.platformInstanceID {
		return CommittedCheckpoint{}, fmt.Errorf("lease owner mismatch")
	}
	if time.Now().UTC().After(leaseUntil.UTC()) {
		return CommittedCheckpoint{}, fmt.Errorf("checkpoint lease expired")
	}
	// Runner generation fencing(审查 I7): Worker 回显的 generation 必须与
	// Prepare 时写入 snapshot 行的一致, 防止旧 generation Runner 提交。
	if ready.RunnerGeneration == 0 || ready.RunnerGeneration != uint64(runnerGeneration) {
		return CommittedCheckpoint{}, fmt.Errorf("runner generation mismatch: worker=%d snapshot=%d", ready.RunnerGeneration, runnerGeneration)
	}
	if stagingRef != ready.StagingRef {
		return CommittedCheckpoint{}, fmt.Errorf("staging ref mismatch")
	}
	// workspace hash 从 staging ref 推导(与 Prepare 同一布局)。
	rel, err := filepath.Rel(c.workspacesRoot, ready.StagingRef)
	if err != nil || strings.HasPrefix(rel, "..") || filepath.IsAbs(rel) {
		return CommittedCheckpoint{}, fmt.Errorf("staging ref escapes workspaces root")
	}
	parts := strings.Split(filepath.ToSlash(rel), "/")
	if len(parts) < 4 || !isHex64(parts[0]) || parts[1] != "state" || parts[2] != "staging" {
		return CommittedCheckpoint{}, fmt.Errorf("staging ref not under workspaces/<hash>/state/staging")
	}
	hash := parts[0]
	stateRoot := filepath.Join(c.workspacesRoot, hash, "state")
	stagingDir := filepath.Join(stateRoot, "staging")
	if filepath.Dir(ready.StagingRef) != stagingDir {
		return CommittedCheckpoint{}, fmt.Errorf("staging ref not under staging dir")
	}

	raw, err := safefs.ReadFileBeneathLimited(c.workspacesRoot, rel, int64(maxBundleBytes))
	if err != nil {
		return CommittedCheckpoint{}, fmt.Errorf("read staging: %w", err)
	}
	if uint64(len(raw)) > maxBundleBytes {
		return CommittedCheckpoint{}, fmt.Errorf("checkpoint bundle exceeds prepared max bundle bytes: got %d want <= %d", len(raw), maxBundleBytes)
	}
	if ready.Checksum == "" {
		return CommittedCheckpoint{}, fmt.Errorf("checksum required")
	}
	actual := "sha256:" + hex.EncodeToString(hashBytes(raw))
	if actual != ready.Checksum {
		return CommittedCheckpoint{}, fmt.Errorf("checksum mismatch: got %s want %s", actual, ready.Checksum)
	}

	var bundle map[string]any
	if err := json.Unmarshal(raw, &bundle); err != nil {
		return CommittedCheckpoint{}, fmt.Errorf("bundle json: %w", err)
	}
	if sv, _ := bundle["schema_version"].(string); sv != snapshotSchemaVersion {
		return CommittedCheckpoint{}, fmt.Errorf("unsupported schema_version %q", sv)
	}
	// Bundle 身份校验(审查 I7/R4-M14): bundle 声明的 task_id/session_key 必须
	// 与 DB snapshot 行一致, 防止 Runner 混入其他 task/session 的合法 bundle;
	// session_key 缺失或为空直接拒绝(不允许跳过身份绑定)。
	taskRow, getErr := c.store.GetTask(ctx, ready.TaskID)
	if getErr != nil {
		return CommittedCheckpoint{}, fmt.Errorf("resolve task session: %w", getErr)
	}
	if err := validateBundleIdentity(bundle, ready.TaskID, taskRow.SessionKey, int64(runnerGeneration)); err != nil {
		return CommittedCheckpoint{}, err
	}
	resultObj, _ := bundle["result"].(map[string]any)
	if resultObj == nil {
		return CommittedCheckpoint{}, fmt.Errorf("bundle missing result")
	}
	body, _ := resultObj["body"].(string)
	resultDigest, _ := bundle["result_digest"].(string)
	if resultDigest == "" {
		resultDigest = "sha256:" + hex.EncodeToString(hashBytes([]byte(body)))
	}
	if ready.ResultDigest == "" {
		return CommittedCheckpoint{}, fmt.Errorf("ready.ResultDigest is required (integrity check cannot be skipped)")
	}
	if ready.ResultDigest != resultDigest {
		return CommittedCheckpoint{}, fmt.Errorf("result digest mismatch: worker=%s bundle=%s", ready.ResultDigest, resultDigest)
	}

	committedRel := filepath.Join(hash, "state", "committed", ready.SnapshotID+".bundle.json")
	if err := safefs.MkdirAllBeneath(c.workspacesRoot, filepath.Join(hash, "state", "committed"), 0o770); err != nil {
		return CommittedCheckpoint{}, err
	}
	if err := safefs.AtomicWriteBeneath(c.workspacesRoot, committedRel, raw, 0o640); err != nil {
		return CommittedCheckpoint{}, err
	}
	// 删除 staging 前先持久化 committed(不可变重命名语义)。
	// round12 审查(M2): 删除失败不再吞掉——记录日志; 文件由
	// ReconcileOrphanStagingFiles 按无 writing 引用兜底回收, 不阻断提交
	// (可用性优先: 遗留副本不影响恢复点正确性)。
	if err := safefs.RemoveBeneath(c.workspacesRoot, rel); err != nil && !os.IsNotExist(err) {
		slog.WarnContext(ctx, "checkpoint commit: remove staging file failed; deferred to reconciliation",
			"snapshot_id", ready.SnapshotID, "staging_rel", rel, "error", err)
	}

	resultRel := filepath.Join(hash, "state", "results", ready.SnapshotID+".result")
	if err := safefs.MkdirAllBeneath(c.workspacesRoot, filepath.Join(hash, "state", "results"), 0o770); err != nil {
		// round10 审查(B9a): committed 已写入但 results 写入失败——清理已物化
		// 文件, 避免 DB 未提交时残留伪 committed 文件占用磁盘。
		c.removeCommittedArtifacts(hash, ready.SnapshotID)
		return CommittedCheckpoint{}, err
	}
	if err := safefs.AtomicWriteBeneath(c.workspacesRoot, resultRel, []byte(body), 0o640); err != nil {
		c.removeCommittedArtifacts(hash, ready.SnapshotID)
		return CommittedCheckpoint{}, err
	}

	// opaque refs 编码 workspace hash, 使 ReadResult 无需额外会话上下文。
	fileRef := opaqueFilePrefix + hash + ":" + ready.SnapshotID
	resultRef := opaqueResultPrefix + hash + ":" + ready.SnapshotID

	return CommittedCheckpoint{
		SnapshotID:   ready.SnapshotID,
		FileRef:      fileRef,
		Checksum:     actual,
		ResultRef:    resultRef,
		ResultDigest: resultDigest,
	}, nil
}

// removeCommittedArtifacts 删除已物化的 committed/result 文件(best-effort,
// 不存在视为成功)。供 Commit 内部失败与 CleanupCommittedFiles 使用。
func (c *WorkspaceCoordinator) removeCommittedArtifacts(hash, snapshotID string) {
	_ = safefs.RemoveBeneath(c.workspacesRoot, filepath.Join(hash, "state", "committed", snapshotID+".bundle.json"))
	_ = safefs.RemoveBeneath(c.workspacesRoot, filepath.Join(hash, "state", "results", snapshotID+".result"))
}

// CleanupCommittedFiles 删除 Commit 已物化但 DB 提交失败的 committed/result
// 文件(round10 审查 B9a)。从 opaque ref 解析 workspace hash, 只删除本次
// Commit 产生的 snapshot 文件; 被 workspaces.current_snapshot_id 引用的文件
// 由调用方保证不会走到这里(提交失败且任务未终态时才调用)。
func (c *WorkspaceCoordinator) CleanupCommittedFiles(ctx context.Context, committed CommittedCheckpoint) error {
	hash, id, err := parseOpaqueRef(committed.FileRef, opaqueFilePrefix)
	if err != nil {
		return fmt.Errorf("cleanup committed files: %w", err)
	}
	if id != committed.SnapshotID {
		return fmt.Errorf("cleanup committed files: ref snapshot %s != committed snapshot %s", id, committed.SnapshotID)
	}
	c.removeCommittedArtifacts(hash, id)
	return nil
}

// orphanReconcileAge 是孤儿 committed 文件回收的最小年龄: 覆盖 Commit
// 物化文件→DB 提交的毫秒级窗口, 防止对账器与进行中的提交竞态误删。
const orphanReconcileAge = time.Hour

// ReconcileOrphanCommittedFiles 遍历共享卷全部工作区的 committed/ 目录,
// 删除"DB 中已不存在对应 committed snapshot 且文件超过 orphanReconcileAge"
// 的 bundle/result 文件(round11 审查 C2)。提交结果不确定时 scheduler 保留
// 文件, 本方法按 DB 引用对账兜底回收, 避免不确定窗口误删恢复点。
// 返回删除的文件数(一个 snapshot 计 1)。
func (c *WorkspaceCoordinator) ReconcileOrphanCommittedFiles(ctx context.Context) (int, error) {
	now := time.Now()
	workspaceDirs, err := os.ReadDir(c.workspacesRoot)
	if err != nil {
		return 0, fmt.Errorf("reconcile: list workspaces root: %w", err)
	}
	removed := 0
	for _, ws := range workspaceDirs {
		if !ws.IsDir() {
			continue
		}
		committedDir := filepath.Join(c.workspacesRoot, ws.Name(), "state", "committed")
		committedEntries, err := os.ReadDir(committedDir)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return removed, fmt.Errorf("reconcile: list committed dir %s: %w", ws.Name(), err)
		}
		for _, f := range committedEntries {
			if f.IsDir() || !strings.HasSuffix(f.Name(), ".bundle.json") {
				continue
			}
			snapshotID := strings.TrimSuffix(f.Name(), ".bundle.json")
			if !looksLikeSnapshotID(snapshotID) {
				// 非 UUID 文件名不是本系统产物(防御: 不触碰也不报错)。
				continue
			}
			info, err := f.Info()
			if err != nil {
				return removed, fmt.Errorf("reconcile: stat %s: %w", f.Name(), err)
			}
			if now.Sub(info.ModTime()) < orphanReconcileAge {
				continue
			}
			state, err := c.store.SnapshotState(ctx, snapshotID)
			if err != nil {
				if !errors.Is(err, postgres.ErrSnapshotNotFound) {
					return removed, fmt.Errorf("reconcile: snapshot state %s: %w", snapshotID, err)
				}
			} else if state == "committed" {
				continue // 被当前恢复指针引用
			}
			// DB 无行或非 committed(writing/quarantined): 文件不是恢复点。
			c.removeCommittedArtifacts(ws.Name(), snapshotID)
			removed++
		}
	}
	return removed, nil
}

// looksLikeStagingToken 校验 staging 文件名的 token 形态(PrepareCheckpoint
// 生成: ckpt-<taskID>-g<gen>-<uuid>), 防止对账器触碰目录中非本系统文件。
func looksLikeStagingToken(s string) bool {
	if !strings.HasPrefix(s, "ckpt-") {
		return false
	}
	// 任务 UUID + g + 数字 + uuid: 至少含 3 个 '-' 且非空段。
	parts := strings.Split(s, "-")
	return len(parts) >= 5
}

// ReconcileOrphanStagingFiles 对账回收无 writing 引用且超过孤儿年龄的
// staging 文件(round12 审查 M2): Commit 成功后删除 staging 失败、或提交
// 期间崩溃时, 文件既无 DB 引用也不被 SweepExpiredCheckpoints 覆盖(lease
// 已消费), 本方法按 DB 引用 + 年龄阈值兜底回收。返回删除的文件数。
func (c *WorkspaceCoordinator) ReconcileOrphanStagingFiles(ctx context.Context) (int, error) {
	now := time.Now()
	workspaceDirs, err := os.ReadDir(c.workspacesRoot)
	if err != nil {
		return 0, fmt.Errorf("reconcile staging: list workspaces root: %w", err)
	}
	removed := 0
	for _, ws := range workspaceDirs {
		if !ws.IsDir() {
			continue
		}
		stagingDir := filepath.Join(c.workspacesRoot, ws.Name(), "state", "staging")
		entries, err := os.ReadDir(stagingDir)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return removed, fmt.Errorf("reconcile staging: list staging dir %s: %w", ws.Name(), err)
		}
		for _, f := range entries {
			if f.IsDir() || !strings.HasSuffix(f.Name(), ".bundle.json") {
				continue
			}
			token := strings.TrimSuffix(f.Name(), ".bundle.json")
			if !looksLikeStagingToken(token) {
				// 非本系统产物(防御: 不触碰也不报错)。
				continue
			}
			info, err := f.Info()
			if err != nil {
				return removed, fmt.Errorf("reconcile staging: stat %s: %w", f.Name(), err)
			}
			if now.Sub(info.ModTime()) < orphanReconcileAge {
				continue
			}
			live, err := c.store.StagingTokenIsWriting(ctx, token)
			if err != nil {
				return removed, fmt.Errorf("reconcile staging: token check %s: %w", token, err)
			}
			if live {
				continue
			}
			if err := safefs.RemoveBeneath(c.workspacesRoot, filepath.Join(ws.Name(), "state", "staging", f.Name())); err != nil && !os.IsNotExist(err) {
				slog.WarnContext(ctx, "checkpoint reconcile: remove orphan staging file failed",
					"workspace", ws.Name(), "token", token, "error", err)
				continue
			}
			removed++
		}
	}
	return removed, nil
}

// looksLikeSnapshotID 校验 snapshot 文件名形态(UUID v4 36 字符), 防止
// 对账器触碰目录中不属于本系统的文件。
func looksLikeSnapshotID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for _, r := range s {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F') || r == '-') {
			return false
		}
	}
	return true
}

// CurrentRestorePoint resolves the workspace's committed snapshot. The
// returned SnapshotRef is the Runner-visible container path(state 挂载点内),
// so StartSession can pass it to the Worker unchanged.
// validateBundleIdentity 校验 bundle 声明的身份与 DB snapshot/task 行一致:
// task_id 必须等于 ready 任务; session_key 必须存在且等于任务的 session;
// bundle 内声明的 runner_generation 必须等于 Prepare 时写入 snapshot 行的
// generation(审查 R4-M14: 旧 generation Runner 不得把陈旧 bundle 冒充新
// 快照提交)。纯函数便于无 DB 单测。
func validateBundleIdentity(bundle map[string]any, readyTaskID, taskSessionKey string, snapshotGeneration int64) error {
	if bTask, _ := bundle["task_id"].(string); bTask != readyTaskID {
		return fmt.Errorf("bundle task_id %q != ready %q", bTask, readyTaskID)
	}
	if bSession, _ := bundle["session_key"].(string); bSession == "" {
		return fmt.Errorf("bundle missing session_key")
	} else if bSession != taskSessionKey {
		return fmt.Errorf("bundle session_key %q != task session %q", bSession, taskSessionKey)
	}
	bGen, _ := bundle["runner_generation"].(float64)
	if int64(bGen) != snapshotGeneration {
		return fmt.Errorf(
			"bundle runner_generation %v != snapshot generation %d", bGen, snapshotGeneration,
		)
	}
	return nil
}

func (c *WorkspaceCoordinator) CurrentRestorePoint(
	ctx context.Context,
	workspaceID string,
	conversationKey string,
) (RestorePoint, bool, error) {
	snapshotID, fileRef, checksum, ok, err := c.store.CurrentWorkspaceSnapshot(ctx, workspaceID, conversationKey)
	if err != nil || !ok {
		return RestorePoint{}, ok, err
	}
	hash, snapshotID, err := parseOpaqueFileRef(fileRef)
	if err != nil {
		return RestorePoint{}, false, err
	}
	runnerPath := filepath.Join(c.runnerStateMount, "committed", snapshotID+".bundle.json")
	platformPath := filepath.Join(c.workspacesRoot, hash, "state", "committed", snapshotID+".bundle.json")
	if _, err := os.Stat(platformPath); err != nil {
		return RestorePoint{}, false, fmt.Errorf("stat committed snapshot: %w", err)
	}
	// 审查 R4-I6: 恢复读取必须按 Prepare 时的 max_bundle_bytes 限长, 防止
	// committed/ 被 Runner 替换为超大文件后无界读入 Runner 内存。
	maxBytes, err := c.store.SnapshotMaxBundleBytes(ctx, snapshotID)
	if err != nil {
		return RestorePoint{}, false, fmt.Errorf("resolve snapshot bundle limit: %w", err)
	}
	if maxBytes <= 0 {
		return RestorePoint{}, false, fmt.Errorf("snapshot %s has no bundle limit", snapshotID)
	}
	return RestorePoint{
		SnapshotID: snapshotID, SnapshotRef: runnerPath, Checksum: checksum,
		MaxBundleBytes: maxBytes,
	}, true, nil
}

// SweepExpiredCheckpoints 定期清理 checkpoint lease 已过期的 writing
// snapshot(置为 quarantined)并删除宿主 staging 文件(审查 R4-I12):
// Prepare 后未 Commit 的 snapshot 会在任务失败/崩溃/DB 错误时残留,
// 不清理则永久占用 DB 行与宿主磁盘。
func (c *WorkspaceCoordinator) SweepExpiredCheckpoints(ctx context.Context) (int, error) {
	expired, err := c.store.QuarantineExpiredWritingSnapshots(ctx, time.Now().UTC())
	if err != nil {
		return 0, err
	}
	for _, q := range expired {
		if q.StagingRef == "" {
			continue
		}
		rel, err := filepath.Rel(c.workspacesRoot, q.StagingRef)
		if err != nil || filepath.IsAbs(rel) || strings.HasPrefix(rel, "..") {
			slog.WarnContext(ctx, "checkpoint sweep: skip staging ref outside workspaces root",
				"snapshot_id", q.SnapshotID, "staging_ref", q.StagingRef)
			continue
		}
		if err := safefs.RemoveBeneath(c.workspacesRoot, rel); err != nil && !os.IsNotExist(err) {
			slog.WarnContext(ctx, "checkpoint sweep: remove staging file failed",
				"snapshot_id", q.SnapshotID, "staging_ref", q.StagingRef, "error", err)
		}
	}
	return len(expired), nil
}

// ReadResult resolves only opaque result refs and verifies digest.
func (c *WorkspaceCoordinator) ReadResult(ctx context.Context, ref string, expectedDigest string) (domain.ResultPayload, error) {
	hash, id, err := parseOpaqueRef(ref, opaqueResultPrefix)
	if err != nil {
		return domain.ResultPayload{}, err
	}
	// 审查: results/ 位于 Runner 可写的 state 挂载下, Runner 可替换结果文件
	// 为任意大小——读取必须按 Prepare 时的 max_bundle_bytes 限长, 防止
	// ReadAll 耗尽 Platform 内存(digest 校验在读完之后, 不能先读后查限)。
	maxBytes, err := c.store.SnapshotMaxBundleBytes(ctx, id)
	if err != nil {
		return domain.ResultPayload{}, fmt.Errorf("resolve result limit: %w", err)
	}
	if maxBytes <= 0 {
		return domain.ResultPayload{}, fmt.Errorf("snapshot %s has no bundle limit", id)
	}
	path := filepath.Join(c.workspacesRoot, hash, "state", "results", id+".result")
	if !strings.HasPrefix(filepath.Clean(path), filepath.Clean(c.workspacesRoot)) {
		return domain.ResultPayload{}, fmt.Errorf("result path escapes workspaces root")
	}
	body, err := safefs.ReadFileBeneathLimited(c.workspacesRoot, filepath.Join(hash, "state", "results", id+".result"), maxBytes)
	if err != nil {
		return domain.ResultPayload{}, err
	}
	digest := "sha256:" + hex.EncodeToString(hashBytes(body))
	if expectedDigest != "" && digest != expectedDigest {
		return domain.ResultPayload{}, fmt.Errorf("result digest mismatch")
	}
	return domain.ResultPayload{Ref: ref, Digest: digest, Body: body}, nil
}

// parseOpaqueFileRef 解析 "snapshot:<hash>:<id>"。
func parseOpaqueFileRef(ref string) (hash, id string, err error) {
	return parseOpaqueRef(ref, opaqueFilePrefix)
}

// parseOpaqueRef 解析 "<prefix><hash>:<id>" 并校验 hash/id 字符集。
func parseOpaqueRef(ref, prefix string) (hash, id string, err error) {
	if !strings.HasPrefix(ref, prefix) {
		return "", "", fmt.Errorf("unknown opaque ref scheme %q", ref)
	}
	rest := strings.TrimPrefix(ref, prefix)
	parts := strings.Split(rest, ":")
	if len(parts) != 2 {
		return "", "", fmt.Errorf("malformed opaque ref %q", ref)
	}
	hash, id = parts[0], parts[1]
	if !isHex64(hash) || id == "" || strings.ContainsAny(id, `/\..`) {
		return "", "", fmt.Errorf("invalid opaque ref components in %q", ref)
	}
	return hash, id, nil
}

func isHex64(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, r := range s {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')) {
			return false
		}
	}
	return true
}

var _ Coordinator = (*WorkspaceCoordinator)(nil)
