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

const (
	snapshotSchemaVersion = "genericagent.snapshot.v1"
	opaqueResultPrefix    = "result:"
	opaqueFilePrefix      = "snapshot:"
)

// LocalConfig configures the development-only local coordinator.
type LocalConfig struct {
	RuntimeRoot        string
	PlatformInstanceID string
	Store              *postgres.Store
	DefaultMaxBundle   uint64
}

// LocalCoordinator is wired only under --dev-loopback.
// Production startup must refuse this coordinator.
type LocalCoordinator struct {
	runtimeRoot        string
	platformInstanceID string
	store              *postgres.Store
	defaultMaxBundle   uint64
}

// NewLocalCoordinator constructs a loopback filesystem coordinator.
func NewLocalCoordinator(cfg LocalConfig) (*LocalCoordinator, error) {
	if cfg.Store == nil {
		return nil, fmt.Errorf("store is required")
	}
	if strings.TrimSpace(cfg.PlatformInstanceID) == "" {
		return nil, fmt.Errorf("platform instance id is required")
	}
	if strings.TrimSpace(cfg.RuntimeRoot) == "" {
		return nil, fmt.Errorf("runtime root is required")
	}
	if cfg.DefaultMaxBundle == 0 {
		cfg.DefaultMaxBundle = 2 * 1024 * 1024
	}
	root, err := filepath.Abs(cfg.RuntimeRoot)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(root, "staging"), 0o755); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(root, "committed"), 0o755); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(root, "results"), 0o755); err != nil {
		return nil, err
	}
	return &LocalCoordinator{
		runtimeRoot:        root,
		platformInstanceID: cfg.PlatformInstanceID,
		store:              cfg.Store,
		defaultMaxBundle:   cfg.DefaultMaxBundle,
	}, nil
}

// Prepare creates writing snapshot metadata and a token-scoped staging path under runtime root.
// The staging_ref is resolved inside the store transaction via stagingRefFor so
// the DB row and the returned lease always agree (no out-of-band rewrite).
func (c *LocalCoordinator) Prepare(ctx context.Context, request CheckpointPrepareRequest) (CheckpointLease, error) {
	if strings.TrimSpace(request.TaskID) == "" {
		return CheckpointLease{}, fmt.Errorf("task_id required")
	}
	if request.RunnerGeneration == 0 {
		return CheckpointLease{}, fmt.Errorf("runner generation required")
	}
	maxB := request.MaxBundleBytes
	if maxB == 0 {
		maxB = c.defaultMaxBundle
	}
	stagingRefFor := func(snapshotID, token string, generation int64) string {
		return filepath.Join(c.runtimeRoot, "staging", token+".bundle.json")
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

// Commit verifies staging bundle, renames immutably, extracts result, returns opaque refs.
func (c *LocalCoordinator) Commit(ctx context.Context, ready ReadyCheckpoint) (CommittedCheckpoint, error) {
	if ready.TaskID == "" || ready.SnapshotID == "" || ready.CheckpointToken == "" {
		return CommittedCheckpoint{}, fmt.Errorf("ready checkpoint missing identifiers")
	}
	wsID, taskID, stagingRef, leaseOwner, leaseUntil, _, runnerGeneration, maxBundleBytes, state, err := c.store.LoadSnapshotToken(ctx, ready.SnapshotID, ready.CheckpointToken)
	if err != nil {
		return CommittedCheckpoint{}, fmt.Errorf("token mismatch or unknown snapshot: %w", err)
	}
	_ = wsID
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
	if ready.RunnerGeneration == 0 || ready.RunnerGeneration != uint64(runnerGeneration) {
		return CommittedCheckpoint{}, fmt.Errorf("runner generation mismatch: worker=%d snapshot=%d", ready.RunnerGeneration, runnerGeneration)
	}
	if stagingRef != ready.StagingRef {
		return CommittedCheckpoint{}, fmt.Errorf("staging ref mismatch")
	}
	if !strings.HasPrefix(filepath.Clean(ready.StagingRef), filepath.Clean(c.runtimeRoot)) {
		return CommittedCheckpoint{}, fmt.Errorf("staging ref escapes runtime root")
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
	// Bundle 身份校验(审查 R4-M14): 与 WorkspaceCoordinator 对齐, loopback
	// 模式不得绕过 task_id/session_key/runner_generation 绑定。
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
	// result_digest in bundle is authoritative when present; otherwise derive
	// from body for backward compatibility with older Worker bundles.
	resultDigest, _ := bundle["result_digest"].(string)
	if resultDigest == "" {
		resultDigest = "sha256:" + hex.EncodeToString(hashBytes([]byte(body)))
	}
	// Worker MUST supply ready.ResultDigest so a tampered staging file (body
	// swapped after PrepareCheckpoint) is detected. Empty ResultDigest is
	// rejected outright — it previously bypassed the integrity check.
	if ready.ResultDigest == "" {
		return CommittedCheckpoint{}, fmt.Errorf("ready.ResultDigest is required (integrity check cannot be skipped)")
	}
	if ready.ResultDigest != resultDigest {
		return CommittedCheckpoint{}, fmt.Errorf("result digest mismatch: worker=%s bundle=%s", ready.ResultDigest, resultDigest)
	}

	committedPath := filepath.Join(c.runtimeRoot, "committed", ready.SnapshotID+".bundle.json")
	if err := atomicWrite(committedPath, raw); err != nil {
		return CommittedCheckpoint{}, err
	}
	// Remove staging after durable rename.
	_ = os.Remove(ready.StagingRef)

	resultID := ready.SnapshotID
	resultPath := filepath.Join(c.runtimeRoot, "results", resultID+".result")
	if err := atomicWrite(resultPath, []byte(body)); err != nil {
		return CommittedCheckpoint{}, err
	}

	fileRef := opaqueFilePrefix + ready.SnapshotID
	resultRef := opaqueResultPrefix + resultID

	// Map opaque refs to absolute paths via small sidecar index under runtime root.
	if err := writeIndex(c.runtimeRoot, fileRef, committedPath); err != nil {
		return CommittedCheckpoint{}, err
	}
	if err := writeIndex(c.runtimeRoot, resultRef, resultPath); err != nil {
		return CommittedCheckpoint{}, err
	}

	return CommittedCheckpoint{
		SnapshotID:   ready.SnapshotID,
		FileRef:      fileRef,
		Checksum:     actual,
		ResultRef:    resultRef,
		ResultDigest: resultDigest,
	}, nil
}

// CurrentRestorePoint resolves the workspace's opaque committed snapshot for Worker startup.
func (c *LocalCoordinator) CurrentRestorePoint(
	ctx context.Context,
	workspaceID string,
) (RestorePoint, bool, error) {
	snapshotID, fileRef, checksum, ok, err := c.store.CurrentWorkspaceSnapshot(ctx, workspaceID)
	if err != nil || !ok {
		return RestorePoint{}, ok, err
	}
	if !strings.HasPrefix(fileRef, opaqueFilePrefix) ||
		strings.Contains(fileRef, `\`) || strings.Contains(fileRef, `/`) || strings.Contains(fileRef, "..") {
		return RestorePoint{}, false, fmt.Errorf("invalid committed snapshot ref")
	}
	path, err := readIndex(c.runtimeRoot, fileRef)
	if err != nil {
		return RestorePoint{}, false, err
	}
	if !strings.HasPrefix(filepath.Clean(path), filepath.Clean(c.runtimeRoot)) {
		return RestorePoint{}, false, fmt.Errorf("snapshot path escapes runtime root")
	}
	if _, err := os.Stat(path); err != nil {
		return RestorePoint{}, false, fmt.Errorf("stat committed snapshot: %w", err)
	}
	// 审查 R4-I6: 恢复必须携带快照的 max_bundle_bytes, Worker 按此限长读取。
	maxBytes, err := c.store.SnapshotMaxBundleBytes(ctx, snapshotID)
	if err != nil {
		return RestorePoint{}, false, fmt.Errorf("resolve snapshot bundle limit: %w", err)
	}
	if maxBytes <= 0 {
		return RestorePoint{}, false, fmt.Errorf("snapshot %s has no bundle limit", snapshotID)
	}
	return RestorePoint{
		SnapshotID: snapshotID, SnapshotRef: path, Checksum: checksum,
		MaxBundleBytes: maxBytes,
	}, true, nil
}

// SweepExpiredCheckpoints 与 WorkspaceCoordinator 同语义(审查 R4-I12):
// loopback 模式同样清理过期 writing snapshot 与 staging 文件。
func (c *LocalCoordinator) SweepExpiredCheckpoints(ctx context.Context) (int, error) {
	expired, err := c.store.QuarantineExpiredWritingSnapshots(ctx, time.Now().UTC())
	if err != nil {
		return 0, err
	}
	for _, q := range expired {
		if q.StagingRef == "" {
			continue
		}
		rel, err := filepath.Rel(c.runtimeRoot, q.StagingRef)
		if err != nil || filepath.IsAbs(rel) || strings.HasPrefix(rel, "..") {
			continue
		}
		if err := os.Remove(filepath.Join(c.runtimeRoot, rel)); err != nil && !os.IsNotExist(err) {
			// best-effort: 文件已丢失属预期。
		}
	}
	return len(expired), nil
}

// RunnerStagingRef: loopback 路径下宿主与 Worker 同一文件系统, 原样返回。
func (c *LocalCoordinator) RunnerStagingRef(hostRef string) (string, error) {
	return hostRef, nil
}

// HostStagingRef: loopback 路径下容器内 ref 即宿主 ref, 校验后返回。
func (c *LocalCoordinator) HostStagingRef(runnerRef, expectedHostRef string) (string, error) {
	if runnerRef != expectedHostRef {
		return "", fmt.Errorf("staging ref mismatch: got %q want %q", runnerRef, expectedHostRef)
	}
	return runnerRef, nil
}

// ReadResult resolves only opaque result refs and verifies digest.
func (c *LocalCoordinator) ReadResult(ctx context.Context, ref string, expectedDigest string) (domain.ResultPayload, error) {
	_ = ctx
	if strings.TrimSpace(ref) == "" {
		return domain.ResultPayload{}, fmt.Errorf("result ref required")
	}
	if strings.Contains(ref, `\`) || strings.Contains(ref, `/`) || strings.Contains(ref, "..") {
		return domain.ResultPayload{}, fmt.Errorf("path-like result ref rejected")
	}
	if !strings.HasPrefix(ref, opaqueResultPrefix) {
		return domain.ResultPayload{}, fmt.Errorf("unknown result ref scheme")
	}
	path, err := readIndex(c.runtimeRoot, ref)
	if err != nil {
		return domain.ResultPayload{}, err
	}
	if !strings.HasPrefix(filepath.Clean(path), filepath.Clean(c.runtimeRoot)) {
		return domain.ResultPayload{}, fmt.Errorf("result path escapes runtime root")
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

func hashBytes(b []byte) []byte {
	sum := sha256.Sum256(b)
	return sum[:]
}

func atomicWrite(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	// Use a unique temp name per call to avoid collisions between concurrent
	// commits of the same target path. Two Commit goroutines racing for the
	// same committedPath would otherwise clobber each other's .tmp file.
	// os.CreateTemp replaces ".*" in the pattern with a random suffix.
	f, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	// Best-effort cleanup if any step below fails before Rename.
	defer func() {
		if _, statErr := os.Stat(tmp); statErr == nil {
			_ = os.Remove(tmp)
		}
	}()
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	if err := syncDirectory(filepath.Dir(path)); err != nil {
		return fmt.Errorf("sync directory after rename: %w", err)
	}
	return nil
}

func writeIndex(runtimeRoot, ref, path string) error {
	dir := filepath.Join(runtimeRoot, "index")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	safe := strings.ReplaceAll(ref, ":", "_")
	return atomicWrite(filepath.Join(dir, safe+".path"), []byte(path))
}

func readIndex(runtimeRoot, ref string) (string, error) {
	safe := strings.ReplaceAll(ref, ":", "_")
	b, err := os.ReadFile(filepath.Join(runtimeRoot, "index", safe+".path"))
	if err != nil {
		return "", fmt.Errorf("unknown opaque ref %q", ref)
	}
	return string(b), nil
}

var _ Coordinator = (*LocalCoordinator)(nil)
