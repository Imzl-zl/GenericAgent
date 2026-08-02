package checkpoint

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/infrastructure/postgres"
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

// workspaceHashFor 由 session key(== workspace key)推导确定性 hash。
// 与 sandbox.WorkspaceDirHash 同构(SHA-256), 保证 checkpoint 与容器挂载一致。
func workspaceHashFor(sessionKey string) string {
	if sessionKey == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(sessionKey))
	return hex.EncodeToString(sum[:])
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

func (c *WorkspaceCoordinator) stateRoot(workspaceKey string) (string, string, error) {
	hash := workspaceHashFor(workspaceKey)
	if hash == "" {
		return "", "", fmt.Errorf("invalid session key for workspace dir")
	}
	state := filepath.Join(c.workspacesRoot, hash, "state")
	return state, hash, nil
}

// Prepare creates writing snapshot metadata and a token-scoped staging path
// under workspaces/<hash>/state/staging (Runner-visible).
func (c *WorkspaceCoordinator) Prepare(ctx context.Context, request CheckpointPrepareRequest) (CheckpointLease, error) {
	if strings.TrimSpace(request.TaskID) == "" {
		return CheckpointLease{}, fmt.Errorf("task_id required")
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
	stateRoot, _, err := c.stateRoot(workspaceKey)
	if err != nil {
		return CheckpointLease{}, err
	}
	stagingDir := filepath.Join(stateRoot, "staging")
	if err := os.MkdirAll(stagingDir, 0o770); err != nil {
		return CheckpointLease{}, fmt.Errorf("create staging dir: %w", err)
	}
	maxB := request.MaxBundleBytes
	if maxB == 0 {
		maxB = c.defaultMaxBundle
	}
	stagingRefFor := func(snapshotID, token string, generation int64) string {
		return filepath.Join(stagingDir, token+".bundle.json")
	}
	snapshotID, token, _, err := c.store.PrepareCheckpoint(ctx, request.TaskID, c.platformInstanceID, stagingRefFor, maxB)
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
	_, taskID, stagingRef, leaseOwner, leaseUntil, _, maxBundleBytes, state, err := c.store.LoadSnapshotToken(ctx, ready.SnapshotID, ready.CheckpointToken)
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

	raw, err := os.ReadFile(ready.StagingRef)
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

	committedDir := filepath.Join(stateRoot, "committed")
	committedPath := filepath.Join(committedDir, ready.SnapshotID+".bundle.json")
	if err := atomicWrite(committedPath, raw); err != nil {
		return CommittedCheckpoint{}, err
	}
	// 删除 staging 前先持久化 committed(不可变重命名语义)。
	_ = os.Remove(ready.StagingRef)

	resultsDir := filepath.Join(stateRoot, "results")
	resultPath := filepath.Join(resultsDir, ready.SnapshotID+".result")
	if err := atomicWrite(resultPath, []byte(body)); err != nil {
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

// CurrentRestorePoint resolves the workspace's committed snapshot. The
// returned SnapshotRef is the Runner-visible container path(state 挂载点内),
// so StartSession can pass it to the Worker unchanged.
func (c *WorkspaceCoordinator) CurrentRestorePoint(
	ctx context.Context,
	workspaceID string,
) (RestorePoint, bool, error) {
	snapshotID, fileRef, checksum, ok, err := c.store.CurrentWorkspaceSnapshot(ctx, workspaceID)
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
	return RestorePoint{SnapshotID: snapshotID, SnapshotRef: runnerPath, Checksum: checksum}, true, nil
}

// ReadResult resolves only opaque result refs and verifies digest.
func (c *WorkspaceCoordinator) ReadResult(ctx context.Context, ref string, expectedDigest string) (domain.ResultPayload, error) {
	_ = ctx
	hash, id, err := parseOpaqueRef(ref, opaqueResultPrefix)
	if err != nil {
		return domain.ResultPayload{}, err
	}
	path := filepath.Join(c.workspacesRoot, hash, "state", "results", id+".result")
	if !strings.HasPrefix(filepath.Clean(path), filepath.Clean(c.workspacesRoot)) {
		return domain.ResultPayload{}, fmt.Errorf("result path escapes workspaces root")
	}
	body, err := os.ReadFile(path)
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
