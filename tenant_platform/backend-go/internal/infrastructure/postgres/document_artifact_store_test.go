package postgres

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
)

func TestDocumentArtifactStore(t *testing.T) {
	pool := requireDB(t)
	store := newDocumentTestStore(t, pool, 2)
	ctx := context.Background()

	registered := false
	for _, name := range migrationFiles() {
		registered = registered || name == "0034_document_artifacts.sql"
	}
	if !registered {
		t.Fatal("0034 migration is not registered")
	}
	var tableExists bool
	if err := pool.QueryRow(ctx, `SELECT to_regclass('public.document_artifacts') IS NOT NULL`).Scan(&tableExists); err != nil || !tableExists {
		t.Fatalf("document_artifacts exists=%v err=%v", tableExists, err)
	}

	t.Run("artifact and success are atomic", func(t *testing.T) {
		workspaceID := seedDocumentWorkspace(t, pool, 91)
		setDocumentPoolSettings(t, pool, true, 2, 10, 10, 1)
		job, claim := startRunningDocumentJob(t, store, workspaceID, 91, "artifact-success", "artifact-instance", "artifact-manager")
		if _, err := store.SubmitDocumentCommand(ctx, documentCommand(job.ID, 91, "export", `{}`)); err != nil {
			t.Fatal(err)
		}
		claimed, ok, err := store.ClaimNextDocumentCommand(ctx, job.ID, "artifact-manager", claim.Job.Generation)
		if err != nil || !ok {
			t.Fatalf("claim=%+v ok=%v err=%v", claimed, ok, err)
		}
		content := []byte("bounded-docx-content")
		completed, artifact, err := store.CompleteDocumentCommandWithArtifact(ctx, domain.CompleteDocumentArtifactCommand{
			JobID: job.ID, CommandID: claimed.CommandID, Owner: "artifact-manager", Generation: claim.Job.Generation,
			FileName: "report.docx", MediaType: "application/vnd.openxmlformats-officedocument.wordprocessingml.document", Content: content,
		})
		if err != nil {
			t.Fatal(err)
		}
		if completed.Status != domain.DocumentCommandSucceeded || artifact.JobID != job.ID || artifact.CommandID != claimed.CommandID || !bytes.Equal(artifact.Content, content) || artifact.SizeBytes != int64(len(content)) {
			t.Fatalf("completed=%+v artifact=%+v", completed, artifact)
		}
		loaded, err := store.GetDocumentArtifact(ctx, job.ID, claimed.CommandID)
		if err != nil || !bytes.Equal(loaded.Content, content) || loaded.SHA256 != artifact.SHA256 {
			t.Fatalf("loaded=%+v err=%v", loaded, err)
		}
	})

	t.Run("fence loss and oversize do not partially persist", func(t *testing.T) {
		workspaceID := seedDocumentWorkspace(t, pool, 92)
		setDocumentPoolSettings(t, pool, true, 2, 10, 10, 1)
		job, claim := startRunningDocumentJob(t, store, workspaceID, 92, "artifact-fence", "artifact-fence-instance", "artifact-fence-manager")
		if _, err := store.SubmitDocumentCommand(ctx, documentCommand(job.ID, 92, "export", `{}`)); err != nil {
			t.Fatal(err)
		}
		claimed, ok, err := store.ClaimNextDocumentCommand(ctx, job.ID, "artifact-fence-manager", claim.Job.Generation)
		if err != nil || !ok {
			t.Fatalf("claim=%+v ok=%v err=%v", claimed, ok, err)
		}
		base := domain.CompleteDocumentArtifactCommand{
			JobID: job.ID, CommandID: claimed.CommandID, Owner: "wrong-owner", Generation: claim.Job.Generation,
			FileName: "report.docx", MediaType: "application/vnd.openxmlformats-officedocument.wordprocessingml.document", Content: []byte("docx"),
		}
		if _, _, err := store.CompleteDocumentCommandWithArtifact(ctx, base); !errors.Is(err, domain.ErrDocumentFenceLost) {
			t.Fatalf("wrong owner err=%v", err)
		}
		base.Owner = "artifact-fence-manager"
		base.Content = bytes.Repeat([]byte("x"), domain.MaxDocumentArtifactBytes+1)
		if _, _, err := store.CompleteDocumentCommandWithArtifact(ctx, base); err == nil || !strings.Contains(err.Error(), "artifact") {
			t.Fatalf("oversize err=%v", err)
		}
		var artifacts int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM document_artifacts WHERE job_id=$1`, job.ID).Scan(&artifacts); err != nil || artifacts != 0 {
			t.Fatalf("artifacts=%d err=%v", artifacts, err)
		}
		var status domain.DocumentCommandStatus
		if err := pool.QueryRow(ctx, `SELECT status FROM document_commands WHERE job_id=$1 AND command_id=$2`, job.ID, claimed.CommandID).Scan(&status); err != nil || status != domain.DocumentCommandExecuting {
			t.Fatalf("status=%s err=%v", status, err)
		}
	})

	t.Run("database rejects inconsistent artifact metadata", func(t *testing.T) {
		workspaceID := seedDocumentWorkspace(t, pool, 93)
		job := submitDocumentJob(t, store, workspaceID, 93, "artifact-constraint")
		if _, err := store.SubmitDocumentCommand(ctx, documentCommand(job.ID, 93, "constraint", `{}`)); err != nil {
			t.Fatal(err)
		}
		_, err := pool.Exec(ctx, `
INSERT INTO document_artifacts(id,job_id,command_id,file_name,media_type,content,size_bytes,sha256)
VALUES($1,$2,'constraint','bad.docx','application/octet-stream',$3,999,$4)
`, uuid.NewString(), job.ID, []byte("x"), strings.Repeat("a", 64))
		if err == nil {
			t.Fatal("expected artifact size constraint violation")
		}
	})

	t.Run("store rejects unsafe artifact metadata", func(t *testing.T) {
		for name, edit := range map[string]func(*domain.CompleteDocumentArtifactCommand){
			"unsafe filename": func(cmd *domain.CompleteDocumentArtifactCommand) { cmd.FileName = "../report.docx" },
			"wrong extension": func(cmd *domain.CompleteDocumentArtifactCommand) { cmd.FileName = "report.pdf" },
			"wrong media":     func(cmd *domain.CompleteDocumentArtifactCommand) { cmd.MediaType = "application/octet-stream" },
		} {
			t.Run(name, func(t *testing.T) {
				cmd := domain.CompleteDocumentArtifactCommand{
					JobID: uuid.NewString(), CommandID: "metadata", Owner: "artifact-metadata-manager", Generation: 1,
					FileName: "report.docx", MediaType: "application/vnd.openxmlformats-officedocument.wordprocessingml.document", Content: []byte("docx"),
				}
				edit(&cmd)
				if _, _, err := store.CompleteDocumentCommandWithArtifact(ctx, cmd); err == nil {
					t.Fatal("expected unsafe metadata rejection")
				}
			})
		}
	})

	_ = time.Second
}
