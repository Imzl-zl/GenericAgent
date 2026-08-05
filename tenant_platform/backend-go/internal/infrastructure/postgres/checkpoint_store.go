package postgres

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
)

// ErrTaskNotOwned 表示任务当前不被调用方拥有(owner 不匹配/lease 已过期/
// 已终态)。调用方不得把任务终态化——任务由 RecoverAfterRestart 或新 owner
// 接管处理(审查 R5-Critical-2)。
var ErrTaskNotOwned = errors.New("task claim not owned by this platform instance or lease expired")

// PrepareCheckpoint inserts workspace_snapshots(state=writing) with generation token.
// StagingRefFunc computes the token-scoped staging reference inside the same
// transaction that inserts the workspace_snapshots row, so the DB-stored ref
// and the ref returned to the caller can never diverge (plan Task 5: token and
// staging_ref must be created atomically).
type StagingRefFunc func(snapshotID, token string, generation int64) string

func (s *Store) PrepareCheckpoint(ctx context.Context, taskID, platformInstanceID string, runnerGeneration uint64, stagingRefFor StagingRefFunc, maxBundleBytes uint64) (snapshotID, token string, generation int64, err error) {
	if maxBundleBytes == 0 || maxBundleBytes > uint64(math.MaxInt64) {
		return "", "", 0, fmt.Errorf("max bundle bytes must be between 1 and %d", int64(math.MaxInt64))
	}
	if stagingRefFor == nil {
		return "", "", 0, fmt.Errorf("stagingRefFor callback is required")
	}
	if runnerGeneration == 0 {
		return "", "", 0, fmt.Errorf("runner generation must be positive")
	}
	err = s.withTx(ctx, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `SELECT `+taskSelectColumns+` FROM tasks WHERE id = $1 FOR UPDATE`, taskID)
		t, err := scanTask(row)
		if err != nil {
			return err
		}
		if t.ClaimOwner != platformInstanceID {
			return fmt.Errorf("prepare checkpoint: claim owner mismatch")
		}
		// round9 审查: 时钟统一用 DB 时钟(与 claim_lease_until 同源), 避免
		// Platform 本地时钟偏差使 DB 视角已过期的 claim 仍能提交恢复点。
		var dbNow time.Time
		if err := tx.QueryRow(ctx, `SELECT timezone('utc', now())`).Scan(&dbNow); err != nil {
			return fmt.Errorf("prepare checkpoint: read db clock: %w", err)
		}
		// 审查: task claim 本身也必须未过期(heartbeat 丢失后不得提交恢复点)。
		if t.ClaimLeaseUntil.IsZero() || !t.ClaimLeaseUntil.After(dbNow) {
			return fmt.Errorf("prepare checkpoint: task claim lease expired")
		}
		if t.Status != domain.TaskStarting && t.Status != domain.TaskRunning {
			return fmt.Errorf("prepare checkpoint: task status %s", t.Status)
		}
		// 校验当前 Runner lease generation 仍等于签发时值(fencing, 审查 I7)。
		// 生产 Runner 模式必有 lease 行; loopback 开发模式无 lease 行时跳过。
		// 锁行 + 校验 owner/expiry: 旧 owner 或已过期 lease 的 Runner 不得
		// 推进恢复指针(审查: lease 失效后提交)。
		var leaseGen int64
		var leaseOwner string
		var leaseExpires time.Time
		if err = tx.QueryRow(ctx, `
SELECT generation, owner, expires_at FROM runner_leases WHERE runner_key = $1 FOR UPDATE
`, t.SessionKey).Scan(&leaseGen, &leaseOwner, &leaseExpires); err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if err == nil {
			if leaseGen != int64(runnerGeneration) {
				return fmt.Errorf("prepare checkpoint: runner generation %d != lease %d", runnerGeneration, leaseGen)
			}
			if leaseOwner != platformInstanceID {
				return fmt.Errorf("prepare checkpoint: runner lease owned by %q", leaseOwner)
			}
			if !leaseExpires.After(dbNow) {
				return fmt.Errorf("prepare checkpoint: runner lease expired")
			}
		}
		var gen int64
		if err := tx.QueryRow(ctx, `
SELECT COALESCE(MAX(generation), 0) + 1 FROM workspace_snapshots WHERE workspace_id = $1::uuid
`, t.WorkspaceID).Scan(&gen); err != nil {
			return err
		}
		sid := uuid.New()
		tok := fmt.Sprintf("ckpt-%s-g%d-%s", taskID, gen, uuid.NewString())
		// Resolve the token-scoped staging ref before INSERT so the row stores
		// the final ref in the same transaction (no out-of-band rewrite).
		stagingRef := stagingRefFor(sid.String(), tok, gen)
		leaseUntil := time.Now().UTC().Add(2 * time.Minute)
		if _, err := tx.Exec(ctx, `
INSERT INTO workspace_snapshots (
  id, workspace_id, task_id, schema_version, state, generation,
  runner_generation, lease_owner, lease_until, token, staging_ref, max_bundle_bytes
) VALUES (
  $1, $2::uuid, $3, 'genericagent.snapshot.v1', 'writing', $4,
  $5, $6, $7, $8, $9, $10
)
`, sid, t.WorkspaceID, t.ID, gen, runnerGeneration, platformInstanceID, leaseUntil, tok, stagingRef, int64(maxBundleBytes)); err != nil {
			return err
		}
		snapshotID = sid.String()
		token = tok
		generation = gen
		return nil
	})
	return snapshotID, token, generation, err
}

// LoadSnapshotToken returns writing snapshot metadata for token validation.
func (s *Store) LoadSnapshotToken(ctx context.Context, snapshotID, token string) (workspaceID, taskID, stagingRef, leaseOwner string, leaseUntil time.Time, generation, runnerGeneration int64, maxBundleBytes uint64, state string, err error) {
	var maxBundle int64
	err = s.pool.QueryRow(ctx, `
SELECT workspace_id::text, task_id, COALESCE(staging_ref,''), COALESCE(lease_owner,''),
       COALESCE(lease_until, timezone('utc', now())), generation, runner_generation, max_bundle_bytes, state
FROM workspace_snapshots WHERE id = $1::uuid AND token = $2
`, snapshotID, token).Scan(&workspaceID, &taskID, &stagingRef, &leaseOwner, &leaseUntil, &generation, &runnerGeneration, &maxBundle, &state)
	if err == nil {
		maxBundleBytes = uint64(maxBundle)
	}
	return
}

// CurrentWorkspaceSnapshot returns the latest committed snapshot selected by a workspace.
func (s *Store) CurrentWorkspaceSnapshot(
	ctx context.Context,
	workspaceID string,
) (snapshotID, fileRef, checksum string, ok bool, err error) {
	err = s.pool.QueryRow(ctx, `
SELECT snapshot.id::text, snapshot.file_ref, snapshot.checksum
FROM workspaces AS workspace
JOIN workspace_snapshots AS snapshot ON snapshot.id = workspace.current_snapshot_id
WHERE workspace.id = $1::uuid AND snapshot.state = 'committed'
`, workspaceID).Scan(&snapshotID, &fileRef, &checksum)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", "", false, nil
	}
	if err != nil {
		return "", "", "", false, err
	}
	return snapshotID, fileRef, checksum, true, nil
}

// SnapshotMaxBundleBytes 返回 snapshot 的 max_bundle_bytes(供 ReadResult
// 对 Runner 可写的 results 文件限长读取, 审查: 防止恶意超大结果耗尽内存)。
func (s *Store) SnapshotMaxBundleBytes(ctx context.Context, snapshotID string) (int64, error) {
	var maxBytes int64
	err := s.pool.QueryRow(ctx, `
SELECT max_bundle_bytes FROM workspace_snapshots WHERE id = $1::uuid
`, snapshotID).Scan(&maxBytes)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, fmt.Errorf("snapshot %s not found", snapshotID)
	}
	return maxBytes, err
}

// ErrSnapshotNotFound 表示 workspace_snapshots 中不存在对应行(供对账流程
// 区分"DB 无行 = 孤儿文件"与"DB 错误")。
var ErrSnapshotNotFound = errors.New("workspace snapshot not found")

// SnapshotState 返回 snapshot 的当前 state; 行不存在时返回 ErrSnapshotNotFound。
func (s *Store) SnapshotState(ctx context.Context, snapshotID string) (string, error) {
	var state string
	err := s.pool.QueryRow(ctx, `SELECT state FROM workspace_snapshots WHERE id = $1::uuid`, snapshotID).Scan(&state)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrSnapshotNotFound
	}
	if err != nil {
		return "", fmt.Errorf("load snapshot state %s: %w", snapshotID, err)
	}
	return state, nil
}

// QuarantinedWriting 是 SweepExpiredCheckpoints 返回的过期 writing snapshot
// (staging_ref 保留供调用方删除宿主 staging 文件)。
type QuarantinedWriting struct {
	SnapshotID string
	TaskID     string
	StagingRef string
}

// QuarantineExpiredWritingSnapshots 把所有 checkpoint lease 已过期的
// writing snapshot 置为 quarantined, 并返回其 staging_ref(审查 R4-I12:
// Prepare 后未 Commit 的 snapshot——崩溃/任务失败/DB 错误——必须定期清理,
// 否则永久占用 DB 行与宿主磁盘)。staging_ref 在清理前返回, 调用方负责
// 删除文件。
func (s *Store) QuarantineExpiredWritingSnapshots(ctx context.Context, before time.Time) ([]QuarantinedWriting, error) {
	var out []QuarantinedWriting
	err := s.withTx(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
SELECT id::text, task_id, COALESCE(staging_ref, '')
FROM workspace_snapshots
WHERE state = 'writing' AND lease_until IS NOT NULL AND lease_until <= $1
FOR UPDATE
`, before.UTC())
		if err != nil {
			return err
		}
		for rows.Next() {
			var q QuarantinedWriting
			if err := rows.Scan(&q.SnapshotID, &q.TaskID, &q.StagingRef); err != nil {
				rows.Close()
				return err
			}
			out = append(out, q)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, q := range out {
			if _, err := tx.Exec(ctx, `
UPDATE workspace_snapshots SET
  state = 'quarantined',
  lease_owner = NULL,
  lease_until = NULL
WHERE id = $1::uuid AND state = 'writing'
`, q.SnapshotID); err != nil {
				return err
			}
		}
		return nil
	})
	return out, err
}

// CompleteSucceeded commits snapshot + task + delivery in one transaction.
// deliveryFiles 是任务完成时捕获的输出文件快照(审查 R5-I3): 与成功状态
// 同事务绑定到 task_complete outbox, 异步 delivery 直接发送快照内容,
// 不再重新解析 workspace 路径(下一条串行任务可能覆盖/删除同名输出)。
func (s *Store) CompleteSucceeded(ctx context.Context, taskID, platformInstanceID, snapshotID, fileRef, checksum, resultRef, resultDigest string, resultBytes int, deliveryFiles []domain.DeliveryFile) (domain.Task, error) {
	var task domain.Task
	err := s.withTx(ctx, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `SELECT `+taskSelectColumns+` FROM tasks WHERE id = $1 FOR UPDATE`, taskID)
		t, err := scanTask(row)
		if err != nil {
			return err
		}
		if t.ClaimOwner != platformInstanceID {
			return fmt.Errorf("complete: claim owner mismatch")
		}
		// round9 审查: 时钟统一用 DB 时钟(与 claim_lease_until 同源)。
		var dbNow time.Time
		if err := tx.QueryRow(ctx, `SELECT timezone('utc', now())`).Scan(&dbNow); err != nil {
			return fmt.Errorf("complete: read db clock: %w", err)
		}
		// 审查: task claim 必须未过期(heartbeat 丢失后不得提交成功状态)。
		if t.ClaimLeaseUntil.IsZero() || !t.ClaimLeaseUntil.After(dbNow) {
			return fmt.Errorf("complete: task claim lease expired")
		}
		if t.Status.IsTerminal() {
			return fmt.Errorf("complete: already terminal %s", t.Status)
		}
		if t.CancelRequestedAt != nil {
			if _, err := tx.Exec(ctx, `
UPDATE workspace_snapshots SET
  state = 'quarantined',
  lease_owner = NULL,
  lease_until = NULL,
  staging_ref = NULL
WHERE id = $1::uuid AND task_id = $2 AND state = 'writing'
`, snapshotID, taskID); err != nil {
				return err
			}
			tt, err := finalizeTerminal(ctx, tx, t, domain.TaskInterrupted, domain.DeliveryTaskInterrupted,
				"TASK_INTERRUPTED", "task interrupted after accepted cancellation", "", "", "")
			if err != nil {
				return err
			}
			task = tt
			return nil
		}
		// Runner generation fencing(审查 I7): 提交前校验 snapshot 签发时的
		// runner_generation 仍是当前 lease generation, 旧 generation Runner
		// 的收尾无法推进恢复点。锁行并校验 owner/expiry(审查: lease 失效
		// 或已接管后不得提交)。
		var snapRunnerGen, leaseGen int64
		var leaseOwner string
		var leaseExpires time.Time
		if err := tx.QueryRow(ctx, `
SELECT runner_generation FROM workspace_snapshots WHERE id = $1::uuid
`, snapshotID).Scan(&snapRunnerGen); err != nil {
			return fmt.Errorf("complete: load snapshot runner generation: %w", err)
		}
		// 生产 Runner 模式必有 lease 行, 校验 snapshot 签发时的 generation 仍
		// 是当前 lease generation(审查 I7); loopback 开发模式无 lease 行时跳过。
		if err = tx.QueryRow(ctx, `
SELECT generation, owner, expires_at FROM runner_leases WHERE runner_key = $1 FOR UPDATE
`, t.SessionKey).Scan(&leaseGen, &leaseOwner, &leaseExpires); err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if err == nil {
			if snapRunnerGen != leaseGen {
				return fmt.Errorf("complete: snapshot runner generation %d != lease %d", snapRunnerGen, leaseGen)
			}
			if leaseOwner != platformInstanceID {
				return fmt.Errorf("complete: runner lease owned by %q", leaseOwner)
			}
			if !leaseExpires.After(dbNow) {
				return fmt.Errorf("complete: runner lease expired")
			}
		}
		tag, err := tx.Exec(ctx, `
UPDATE workspace_snapshots SET
  state = 'committed',
  file_ref = $2,
  checksum = $3,
  result_ref = $4,
  result_digest = $5,
  result_bytes = $6,
  committed_at = timezone('utc', now()),
  lease_owner = NULL,
  lease_until = NULL,
  staging_ref = NULL
WHERE id = $1::uuid AND task_id = $7 AND state = 'writing' AND token IS NOT NULL
`, snapshotID, fileRef, checksum, resultRef, resultDigest, resultBytes, taskID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return fmt.Errorf("complete: snapshot %s not writable", snapshotID)
		}
		if _, err := tx.Exec(ctx, `
UPDATE workspaces SET current_snapshot_id = $2::uuid WHERE id = $1::uuid
`, t.WorkspaceID, snapshotID); err != nil {
			return err
		}
		row = tx.QueryRow(ctx, `
UPDATE tasks SET
  status = 'succeeded',
  claim_owner = NULL,
  claim_lease_until = NULL,
  claimed_at = NULL,
  snapshot_id = $2::uuid,
  snapshot_checksum = $3,
  result_ref = $4,
  result_digest = $5,
  succeeded_at = timezone('utc', now()),
  terminal_at = timezone('utc', now()),
  updated_at = timezone('utc', now())
WHERE id = $1
RETURNING `+taskSelectColumns, taskID, snapshotID, checksum, resultRef, resultDigest)
		tt, err := scanTask(row)
		if err != nil {
			return err
		}
		if err := insertNextEvent(ctx, tx, tt.ID, "status_transition", nil, strPtr(resultDigest), string(t.Status), "succeeded", tt.WorkerInstanceID, ""); err != nil {
			return err
		}
		if err := insertDelivery(ctx, tx, tt.ID, domain.DeliveryTaskComplete, resultRef, resultDigest, "", "", ""); err != nil {
			return err
		}
		// 审查 R5-I3: 输出文件快照与成功事务原子绑定。
		// delivery_id 与 insertDelivery 的 StableDeliveryID 一致。
		for _, f := range deliveryFiles {
			if _, err := tx.Exec(ctx, `
INSERT INTO task_delivery_files (delivery_id, marker, file_name, rel_path, content, digest, size_bytes)
VALUES ($1, $2, $3, $4, $5, $6, $7)
ON CONFLICT (delivery_id, marker) DO NOTHING
`, domain.StableDeliveryID(tt.ID, domain.DeliveryTaskComplete), f.Marker, f.FileName, f.RelPath, f.Content, f.Digest, f.SizeBytes); err != nil {
				return fmt.Errorf("insert delivery file %q: %w", f.Marker, err)
			}
		}
		// 审查 R4-C3: 成功终态事务内同步撤销 capability JTI, 与任务状态
		// 变更原子——成功任务的 token 同样不得在终态后继续被复用。
		if err := revokeTaskCapabilityJTIs(ctx, tx, tt.ID); err != nil {
			return err
		}
		// 审查 R4-I8: fresh 任务成功终态才清除 reset_at 标记——失败/取消的
		// fresh 任务保留重置标记, 下一任务仍从干净状态开始, 不会静默恢复
		// /new 前的旧 snapshot。
		if t.FreshSession {
			if _, err := tx.Exec(ctx, `UPDATE workspaces SET reset_at = NULL WHERE session_key = $1`, t.SessionKey); err != nil {
				return err
			}
		}
		task = tt
		return nil
	})
	return task, err
}

// CompleteFailedTerminal commits failed/cancelled/interrupted without success checkpoint.
// 审查 R5-Critical-2: 失败终态由当前 claim owner 在 lease 有效期内执行——
// 旧实例在 lease 被接管/过期后不得把新 owner 的任务终态化。owner 为空或
// 不匹配/lease 过期时返回 ErrTaskNotOwned, 任务保持原状(由 RecoverAfterRestart
// 或新 owner 处理)。管理型终态路径(CancelTask/RemoveMember)使用独立 SQL, 不走此处。
func (s *Store) CompleteFailedTerminal(ctx context.Context, taskID, owner string, status domain.TaskStatus, deliveryType domain.DeliveryType, code, message, traceID string) (domain.Task, error) {
	if strings.TrimSpace(owner) == "" {
		return domain.Task{}, ErrTaskNotOwned
	}
	var task domain.Task
	err := s.withTx(ctx, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `SELECT `+taskSelectColumns+` FROM tasks
WHERE id = $1 AND claim_owner = $2
  AND claim_lease_until > timezone('utc', now())
  AND status IN ('starting','running')
FOR UPDATE`, taskID, owner)
		t, err := scanTask(row)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrTaskNotOwned
		}
		if err != nil {
			return err
		}
		if t.CancelRequestedAt != nil && t.WorkerDispatchStartedAt != nil {
			status = domain.TaskInterrupted
			deliveryType = domain.DeliveryTaskInterrupted
			code = "TASK_INTERRUPTED"
			message = "task interrupted after accepted cancellation"
		}
		tt, err := finalizeTerminal(ctx, tx, t, status, deliveryType, code, message, "", "", traceID)
		if err != nil {
			return err
		}
		task = tt
		return nil
	})
	return task, err
}
