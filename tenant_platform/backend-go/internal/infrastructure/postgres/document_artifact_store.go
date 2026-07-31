package postgres

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
)

const documentArtifactColumns = `
id::text,job_id::text,command_id,file_name,media_type,content,size_bytes,sha256,created_at`

func (s *Store) CompleteDocumentCommandWithArtifact(
	ctx context.Context,
	cmd domain.CompleteDocumentArtifactCommand,
) (domain.DocumentCommand, domain.DocumentArtifact, error) {
	if err := validateDocumentArtifactCommand(cmd); err != nil {
		return domain.DocumentCommand{}, domain.DocumentArtifact{}, err
	}
	digest := sha256.Sum256(cmd.Content)
	digestHex := hex.EncodeToString(digest[:])
	var completed domain.DocumentCommand
	var artifact domain.DocumentArtifact
	err := s.withTx(ctx, func(tx pgx.Tx) error {
		jobStatus, err := lockActiveDocumentJobFence(ctx, tx, cmd.JobID, cmd.Owner, cmd.Generation)
		if err != nil {
			return err
		}
		if jobStatus != domain.DocumentJobRunning {
			return domain.ErrDocumentFenceLost
		}

		existing, err := scanDocumentArtifact(tx.QueryRow(ctx, `
SELECT `+documentArtifactColumns+` FROM document_artifacts
WHERE job_id=$1 AND command_id=$2 FOR UPDATE
`, cmd.JobID, cmd.CommandID))
		if err == nil {
			if existing.FileName != cmd.FileName || existing.MediaType != cmd.MediaType || existing.SHA256 != digestHex || !bytes.Equal(existing.Content, cmd.Content) {
				return domain.ErrDocumentIdempotencyConflict
			}
			stored, err := scanDocumentCommand(tx.QueryRow(ctx, `
SELECT `+documentCommandColumns+` FROM document_commands
WHERE job_id=$1 AND command_id=$2 AND status='succeeded'
`, cmd.JobID, cmd.CommandID))
			if err != nil {
				return err
			}
			completed, artifact = stored, existing
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}

		var executing bool
		if err := tx.QueryRow(ctx, `
SELECT EXISTS(
 SELECT 1 FROM document_commands
 WHERE job_id=$1 AND command_id=$2 AND status='executing' AND generation=$3
)
`, cmd.JobID, cmd.CommandID, cmd.Generation).Scan(&executing); err != nil {
			return err
		}
		if !executing {
			return domain.ErrDocumentFenceLost
		}
		storedArtifact, err := scanDocumentArtifact(tx.QueryRow(ctx, `
INSERT INTO document_artifacts(
 id,job_id,command_id,file_name,media_type,content,size_bytes,sha256
) VALUES($1,$2,$3,$4,$5,$6,$7,$8)
RETURNING `+documentArtifactColumns,
			uuid.NewString(), cmd.JobID, cmd.CommandID, cmd.FileName, cmd.MediaType,
			cmd.Content, len(cmd.Content), digestHex))
		if err != nil {
			return err
		}
		storedCommand, err := scanDocumentCommand(tx.QueryRow(ctx, `
UPDATE document_commands
SET status='succeeded',completed_at=timezone('utc',now()),updated_at=timezone('utc',now())
WHERE job_id=$1 AND command_id=$2 AND status='executing' AND generation=$3
RETURNING `+documentCommandColumns, cmd.JobID, cmd.CommandID, cmd.Generation))
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrDocumentFenceLost
		}
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
UPDATE document_jobs SET last_activity_at=timezone('utc',now()),updated_at=timezone('utc',now())
WHERE id=$1
`, cmd.JobID); err != nil {
			return err
		}
		completed, artifact = storedCommand, storedArtifact
		return nil
	})
	return completed, artifact, err
}

func (s *Store) GetDocumentArtifact(ctx context.Context, jobID, commandID string) (domain.DocumentArtifact, error) {
	if _, err := uuid.Parse(strings.TrimSpace(jobID)); err != nil {
		return domain.DocumentArtifact{}, fmt.Errorf("artifact job id must be a UUID: %w", err)
	}
	commandID = strings.TrimSpace(commandID)
	if commandID == "" || len(commandID) > 256 {
		return domain.DocumentArtifact{}, fmt.Errorf("artifact command id is invalid")
	}
	artifact, err := scanDocumentArtifact(s.pool.QueryRow(ctx, `
SELECT `+documentArtifactColumns+` FROM document_artifacts
WHERE job_id=$1 AND command_id=$2
`, jobID, commandID))
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.DocumentArtifact{}, domain.ErrDocumentArtifactNotFound
	}
	return artifact, err
}

func validateDocumentArtifactCommand(cmd domain.CompleteDocumentArtifactCommand) error {
	if _, err := uuid.Parse(strings.TrimSpace(cmd.JobID)); err != nil {
		return fmt.Errorf("artifact job id must be a UUID: %w", err)
	}
	cmd.CommandID = strings.TrimSpace(cmd.CommandID)
	cmd.Owner = strings.TrimSpace(cmd.Owner)
	cmd.FileName = strings.TrimSpace(cmd.FileName)
	cmd.MediaType = strings.TrimSpace(cmd.MediaType)
	if cmd.CommandID == "" || len(cmd.CommandID) > 256 || cmd.Owner == "" || cmd.Generation <= 0 {
		return fmt.Errorf("artifact command, owner, and generation are required")
	}
	if err := domain.ValidateDocumentArtifactMetadata(cmd.FileName, cmd.MediaType); err != nil {
		return err
	}
	if len(cmd.Content) == 0 || len(cmd.Content) > domain.MaxDocumentArtifactBytes {
		return fmt.Errorf("artifact content must be between 1 and %d bytes", domain.MaxDocumentArtifactBytes)
	}
	return nil
}

func scanDocumentArtifact(row documentScannable) (domain.DocumentArtifact, error) {
	var artifact domain.DocumentArtifact
	var content []byte
	err := row.Scan(
		&artifact.ID, &artifact.JobID, &artifact.CommandID,
		&artifact.FileName, &artifact.MediaType, &content,
		&artifact.SizeBytes, &artifact.SHA256, &artifact.CreatedAt,
	)
	artifact.Content = append([]byte(nil), content...)
	return artifact, err
}
