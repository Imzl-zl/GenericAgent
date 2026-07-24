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
	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/postgres"
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
func (c *LocalCoordinator) Prepare(ctx context.Context, request CheckpointPrepareRequest) (CheckpointLease, error) {
	if strings.TrimSpace(request.TaskID) == "" {
		return CheckpointLease{}, fmt.Errorf("task_id required")
	}
	maxB := request.MaxBundleBytes
	if maxB == 0 {
		maxB = c.defaultMaxBundle
	}
	// Provisional staging path uses task id; final token is returned from store.
	provisional := filepath.Join(c.runtimeRoot, "staging", request.TaskID+".pending.bundle.json")
	snapshotID, token, _, err := c.store.PrepareCheckpoint(ctx, request.TaskID, c.platformInstanceID, provisional, maxB)
	if err != nil {
		return CheckpointLease{}, err
	}
	staging := filepath.Join(c.runtimeRoot, "staging", token+".bundle.json")
	// Update staging_ref to token-scoped path (rewrite writing row via prepare already stored provisional;
	// Worker receives the token-scoped path below; Commit verifies against DB staging_ref.
	// Re-prepare is avoided: store provisional then we rewrite staging_ref in a small update.
	if err := c.rewriteStagingRef(ctx, snapshotID, staging); err != nil {
		return CheckpointLease{}, err
	}
	return CheckpointLease{
		SnapshotID:     snapshotID,
		Token:          token,
		StagingRef:     staging,
		MaxBundleBytes: maxB,
	}, nil
}

func (c *LocalCoordinator) rewriteStagingRef(ctx context.Context, snapshotID, staging string) error {
	_, err := c.store.Pool().Exec(ctx, `
UPDATE workspace_snapshots SET staging_ref = $2
WHERE id = $1::uuid AND state = 'writing'
`, snapshotID, staging)
	return err
}

// Commit verifies staging bundle, renames immutably, extracts result, returns opaque refs.
func (c *LocalCoordinator) Commit(ctx context.Context, ready ReadyCheckpoint) (CommittedCheckpoint, error) {
	if ready.TaskID == "" || ready.SnapshotID == "" || ready.CheckpointToken == "" {
		return CommittedCheckpoint{}, fmt.Errorf("ready checkpoint missing identifiers")
	}
	wsID, taskID, stagingRef, leaseOwner, leaseUntil, _, maxBundleBytes, state, err := c.store.LoadSnapshotToken(ctx, ready.SnapshotID, ready.CheckpointToken)
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
	resultObj, _ := bundle["result"].(map[string]any)
	if resultObj == nil {
		return CommittedCheckpoint{}, fmt.Errorf("bundle missing result")
	}
	body, _ := resultObj["body"].(string)
	resultDigest, _ := bundle["result_digest"].(string)
	if resultDigest == "" {
		resultDigest = "sha256:" + hex.EncodeToString(hashBytes([]byte(body)))
	}
	if ready.ResultDigest != "" && ready.ResultDigest != resultDigest {
		return CommittedCheckpoint{}, fmt.Errorf("result digest mismatch")
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
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
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
