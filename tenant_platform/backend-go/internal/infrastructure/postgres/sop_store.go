package postgres

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
)

func (s *Store) UpsertSOPCandidate(ctx context.Context, cmd domain.ImportSOPCandidateCommand) (domain.SOPCandidate, error) {
	if err := domain.ValidateImportSOPCandidate(cmd); err != nil {
		return domain.SOPCandidate{}, err
	}
	digest, err := domain.SOPContentDigest(cmd.Content)
	if err != nil {
		return domain.SOPCandidate{}, err
	}
	var candidate domain.SOPCandidate
	err = scanSOPCandidate(s.pool.QueryRow(ctx, `
INSERT INTO sop_candidates(id,remote_sop_id,title,description,file_type,content,source_digest)
VALUES($1::uuid,$2,$3,$4,$5,$6,$7)
ON CONFLICT(remote_sop_id,source_digest)
DO UPDATE SET remote_sop_id=EXCLUDED.remote_sop_id
RETURNING id,remote_sop_id,title,description,file_type,content,source_digest,status,
          COALESCE(reviewed_by,0),review_note,created_at,updated_at,reviewed_at
`, uuid.NewString(), cmd.RemoteSOPID, cmd.Title, cmd.Description, cmd.FileType, cmd.Content, digest), &candidate)
	return candidate, err
}

func (s *Store) ApproveSOPCandidate(ctx context.Context, candidateID string, adminUserID int64) (domain.SOPVersion, error) {
	if _, err := uuid.Parse(candidateID); err != nil {
		return domain.SOPVersion{}, fmt.Errorf("candidate id is invalid")
	}
	if adminUserID <= 0 {
		return domain.SOPVersion{}, fmt.Errorf("admin user id must be positive")
	}
	var version domain.SOPVersion
	err := s.withTx(ctx, func(tx pgx.Tx) error {
		var candidate domain.SOPCandidate
		if err := scanSOPCandidate(tx.QueryRow(ctx, `
SELECT id,remote_sop_id,title,description,file_type,content,source_digest,status,
       COALESCE(reviewed_by,0),review_note,created_at,updated_at,reviewed_at
FROM sop_candidates WHERE id=$1::uuid FOR UPDATE
`, candidateID), &candidate); err != nil {
			return err
		}
		if candidate.Status == domain.SOPCandidateApproved {
			return scanSOPVersion(tx.QueryRow(ctx, sopVersionSelect+` WHERE candidate_id=$1::uuid`, candidateID), &version)
		}
		if candidate.Status != domain.SOPCandidatePending {
			return domain.ErrSOPCandidateState
		}

		entryID := uuid.NewString()
		if _, err := tx.Exec(ctx, `
INSERT INTO sop_entries(id,remote_sop_id) VALUES($1::uuid,$2)
ON CONFLICT(remote_sop_id) DO NOTHING
`, entryID, candidate.RemoteSOPID); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT id FROM sop_entries WHERE remote_sop_id=$1 FOR UPDATE`, candidate.RemoteSOPID).Scan(&entryID); err != nil {
			return err
		}
		var nextVersion int
		if err := tx.QueryRow(ctx, `SELECT COALESCE(MAX(version),0)+1 FROM sop_versions WHERE entry_id=$1::uuid`, entryID).Scan(&nextVersion); err != nil {
			return err
		}
		if err := scanSOPVersion(tx.QueryRow(ctx, `
INSERT INTO sop_versions(id,entry_id,candidate_id,version,title,description,content,content_digest,approved_by)
VALUES($1::uuid,$2::uuid,$3::uuid,$4,$5,$6,$7,$8,$9)
RETURNING id,entry_id,candidate_id,version,title,description,content,content_digest,approved_by,approved_at
`, uuid.NewString(), entryID, candidate.ID, nextVersion, candidate.Title, candidate.Description, candidate.Content, candidate.SourceDigest, adminUserID), &version); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `
UPDATE sop_candidates
SET status='approved',reviewed_by=$2,review_note='',reviewed_at=timezone('utc',now()),updated_at=timezone('utc',now())
WHERE id=$1::uuid
`, candidateID, adminUserID)
		return err
	})
	return version, err
}

func (s *Store) RejectSOPCandidate(ctx context.Context, candidateID string, adminUserID int64, note string) error {
	if _, err := uuid.Parse(candidateID); err != nil {
		return fmt.Errorf("candidate id is invalid")
	}
	if adminUserID <= 0 {
		return fmt.Errorf("admin user id must be positive")
	}
	if err := domain.ValidateSOPReviewNote(note); err != nil {
		return err
	}
	tag, err := s.pool.Exec(ctx, `
UPDATE sop_candidates
SET status='rejected',reviewed_by=$2,review_note=$3,reviewed_at=timezone('utc',now()),updated_at=timezone('utc',now())
WHERE id=$1::uuid AND status='pending'
`, candidateID, adminUserID, note)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 1 {
		return nil
	}
	var status domain.SOPCandidateStatus
	if err := s.pool.QueryRow(ctx, `SELECT status FROM sop_candidates WHERE id=$1::uuid`, candidateID).Scan(&status); err != nil {
		return err
	}
	if status == domain.SOPCandidateRejected {
		return nil
	}
	return domain.ErrSOPCandidateState
}

func (s *Store) LoadSOPVersion(ctx context.Context, versionID string, adminUserID int64) (domain.SOPEntry, error) {
	if _, err := uuid.Parse(versionID); err != nil {
		return domain.SOPEntry{}, fmt.Errorf("SOP version id is invalid")
	}
	if adminUserID <= 0 {
		return domain.SOPEntry{}, fmt.Errorf("admin user id must be positive")
	}
	var entry domain.SOPEntry
	err := s.withTx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(87350035)`); err != nil {
			return err
		}
		var entryID string
		var contentBytes int
		if err := tx.QueryRow(ctx, `
SELECT entry_id,octet_length(content)
FROM sop_versions
WHERE id=$1::uuid
`, versionID).Scan(&entryID, &contentBytes); err != nil {
			return err
		}
		var loadedCount, loadedBytes int
		if err := tx.QueryRow(ctx, `
SELECT count(*),COALESCE(sum(octet_length(version.content)),0)
FROM sop_entries AS entry
JOIN sop_versions AS version ON version.id=entry.loaded_version_id
WHERE entry.id<>$1::uuid
`, entryID).Scan(&loadedCount, &loadedBytes); err != nil {
			return err
		}
		if loadedCount+1 > domain.MaxLoadedSOPs || loadedBytes+contentBytes > domain.MaxLoadedSOPBytes {
			return domain.ErrSOPLoadLimit
		}
		return scanSOPEntry(tx.QueryRow(ctx, `
UPDATE sop_entries AS entry
SET loaded_version_id=version.id,loaded_by=$2,loaded_at=timezone('utc',now()),updated_at=timezone('utc',now())
FROM sop_versions AS version
WHERE version.id=$1::uuid AND version.entry_id=entry.id
RETURNING entry.id,entry.remote_sop_id,entry.loaded_version_id,COALESCE(entry.loaded_by,0),entry.created_at,entry.updated_at,entry.loaded_at
`, versionID, adminUserID), &entry)
	})
	return entry, err
}

func (s *Store) UnloadSOP(ctx context.Context, entryID string, adminUserID int64) (domain.SOPEntry, error) {
	if _, err := uuid.Parse(entryID); err != nil {
		return domain.SOPEntry{}, fmt.Errorf("SOP entry id is invalid")
	}
	if adminUserID <= 0 {
		return domain.SOPEntry{}, fmt.Errorf("admin user id must be positive")
	}
	var entry domain.SOPEntry
	err := s.withTx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(87350035)`); err != nil {
			return err
		}
		return scanSOPEntry(tx.QueryRow(ctx, `
UPDATE sop_entries
SET loaded_version_id=NULL,loaded_by=$2,loaded_at=NULL,updated_at=timezone('utc',now())
WHERE id=$1::uuid
RETURNING id,remote_sop_id,loaded_version_id,COALESCE(loaded_by,0),created_at,updated_at,loaded_at
`, entryID, adminUserID), &entry)
	})
	return entry, err
}

func (s *Store) ListSOPCandidates(ctx context.Context, status domain.SOPCandidateStatus) ([]domain.SOPCandidate, error) {
	if status != "" && !status.IsValid() {
		return nil, fmt.Errorf("invalid SOP candidate status")
	}
	query := `
SELECT id,remote_sop_id,title,description,file_type,content,source_digest,status,
       COALESCE(reviewed_by,0),review_note,created_at,updated_at,reviewed_at
FROM sop_candidates`
	args := []any{}
	if status != "" {
		query += ` WHERE status=$1`
		args = append(args, status)
	}
	query += ` ORDER BY created_at,id`
	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	candidates := make([]domain.SOPCandidate, 0)
	for rows.Next() {
		var candidate domain.SOPCandidate
		if err := scanSOPCandidate(rows, &candidate); err != nil {
			return nil, err
		}
		candidates = append(candidates, candidate)
	}
	return candidates, rows.Err()
}

func (s *Store) ListSOPRegistry(ctx context.Context) ([]domain.SOPRegistryItem, error) {
	rows, err := s.pool.Query(ctx, `
SELECT sop_versions.id,sop_versions.entry_id,sop_versions.candidate_id,sop_versions.version,
       sop_versions.title,sop_versions.description,sop_versions.content,sop_versions.content_digest,
       sop_versions.approved_by,sop_versions.approved_at,
       COALESCE(entry.loaded_version_id=sop_versions.id,FALSE)
FROM sop_versions
JOIN sop_entries AS entry ON entry.id=sop_versions.entry_id
ORDER BY sop_versions.title,sop_versions.version DESC
`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]domain.SOPRegistryItem, 0)
	for rows.Next() {
		var item domain.SOPRegistryItem
		if err := rows.Scan(
			&item.Version.ID, &item.Version.EntryID, &item.Version.CandidateID, &item.Version.Version,
			&item.Version.Title, &item.Version.Description, &item.Version.Content, &item.Version.ContentDigest,
			&item.Version.ApprovedBy, &item.Version.ApprovedAt, &item.Loaded,
		); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (s *Store) ListLoadedSOPVersions(ctx context.Context) ([]domain.SOPVersion, error) {
	rows, err := s.pool.Query(ctx, sopVersionSelect+`
JOIN sop_entries AS entry ON entry.loaded_version_id=sop_versions.id
ORDER BY sop_versions.title,sop_versions.content_digest
`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	versions := make([]domain.SOPVersion, 0)
	for rows.Next() {
		var version domain.SOPVersion
		if err := scanSOPVersion(rows, &version); err != nil {
			return nil, err
		}
		versions = append(versions, version)
	}
	return versions, rows.Err()
}

const sopVersionSelect = `
SELECT sop_versions.id,sop_versions.entry_id,sop_versions.candidate_id,sop_versions.version,
       sop_versions.title,sop_versions.description,sop_versions.content,sop_versions.content_digest,
       sop_versions.approved_by,sop_versions.approved_at
FROM sop_versions`

type rowScanner interface {
	Scan(dest ...any) error
}

func scanSOPCandidate(row rowScanner, candidate *domain.SOPCandidate) error {
	return row.Scan(
		&candidate.ID, &candidate.RemoteSOPID, &candidate.Title, &candidate.Description,
		&candidate.FileType, &candidate.Content, &candidate.SourceDigest, &candidate.Status,
		&candidate.ReviewedBy, &candidate.ReviewNote, &candidate.CreatedAt, &candidate.UpdatedAt,
		&candidate.ReviewedAt,
	)
}

func scanSOPVersion(row rowScanner, version *domain.SOPVersion) error {
	return row.Scan(
		&version.ID, &version.EntryID, &version.CandidateID, &version.Version,
		&version.Title, &version.Description, &version.Content, &version.ContentDigest,
		&version.ApprovedBy, &version.ApprovedAt,
	)
}

func scanSOPEntry(row rowScanner, entry *domain.SOPEntry) error {
	var loadedVersionID *string
	if err := row.Scan(
		&entry.ID, &entry.RemoteSOPID, &loadedVersionID, &entry.LoadedBy,
		&entry.CreatedAt, &entry.UpdatedAt, &entry.LoadedAt,
	); err != nil {
		return err
	}
	if loadedVersionID != nil {
		entry.LoadedVersionID = *loadedVersionID
	}
	return nil
}
