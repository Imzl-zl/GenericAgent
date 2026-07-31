package postgres

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
)

func insertLoadedSOPSnapshots(ctx context.Context, tx pgx.Tx, taskID string) ([]domain.TaskSOPSnapshot, error) {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(87350035)`); err != nil {
		return nil, err
	}
	rows, err := tx.Query(ctx, `
SELECT version.id,version.title,version.description,version.content,version.content_digest
FROM sop_entries AS entry
JOIN sop_versions AS version ON version.id=entry.loaded_version_id
ORDER BY version.title,version.content_digest
`)
	if err != nil {
		return nil, err
	}
	snapshots := make([]domain.TaskSOPSnapshot, 0)
	totalBytes := 0
	for rows.Next() {
		var snapshot domain.TaskSOPSnapshot
		if err := rows.Scan(
			&snapshot.SOPVersionID, &snapshot.Title, &snapshot.Description,
			&snapshot.Content, &snapshot.ContentDigest,
		); err != nil {
			rows.Close()
			return nil, err
		}
		snapshot.TaskID = taskID
		snapshot.Ordinal = len(snapshots)
		totalBytes += len([]byte(snapshot.Content))
		snapshots = append(snapshots, snapshot)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	if len(snapshots) > domain.MaxLoadedSOPs || totalBytes > domain.MaxLoadedSOPBytes {
		return nil, domain.ErrSOPLoadLimit
	}
	for _, snapshot := range snapshots {
		digest, err := domain.SOPContentDigest(snapshot.Content)
		if err != nil || digest != snapshot.ContentDigest {
			return nil, fmt.Errorf("installed SOP digest mismatch")
		}
		if _, err := tx.Exec(ctx, `
INSERT INTO task_sop_snapshots(task_id,ordinal,sop_version_id,content_digest)
VALUES($1,$2,$3::uuid,$4)
`, snapshot.TaskID, snapshot.Ordinal, snapshot.SOPVersionID, snapshot.ContentDigest); err != nil {
			return nil, err
		}
	}
	return snapshots, nil
}

func loadTaskSOPSnapshots(ctx context.Context, queryer interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}, taskID string) ([]domain.TaskSOPSnapshot, error) {
	rows, err := queryer.Query(ctx, `
SELECT snapshot.task_id,snapshot.ordinal,snapshot.sop_version_id,
       version.title,version.description,version.content,snapshot.content_digest,snapshot.created_at,
       version.content_digest
FROM task_sop_snapshots AS snapshot
JOIN sop_versions AS version ON version.id=snapshot.sop_version_id
WHERE snapshot.task_id=$1
ORDER BY snapshot.ordinal
`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	snapshots := make([]domain.TaskSOPSnapshot, 0)
	totalBytes := 0
	for rows.Next() {
		var snapshot domain.TaskSOPSnapshot
		var versionDigest string
		if err := rows.Scan(
			&snapshot.TaskID, &snapshot.Ordinal, &snapshot.SOPVersionID,
			&snapshot.Title, &snapshot.Description, &snapshot.Content,
			&snapshot.ContentDigest, &snapshot.CreatedAt, &versionDigest,
		); err != nil {
			return nil, err
		}
		if snapshot.Ordinal != len(snapshots) || snapshot.ContentDigest != versionDigest {
			return nil, fmt.Errorf("task SOP snapshot metadata mismatch")
		}
		digest, err := domain.SOPContentDigest(snapshot.Content)
		if err != nil || digest != snapshot.ContentDigest {
			return nil, fmt.Errorf("task SOP snapshot digest mismatch")
		}
		totalBytes += len([]byte(snapshot.Content))
		snapshots = append(snapshots, snapshot)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(snapshots) > domain.MaxLoadedSOPs || totalBytes > domain.MaxLoadedSOPBytes {
		return nil, domain.ErrSOPLoadLimit
	}
	return snapshots, nil
}
