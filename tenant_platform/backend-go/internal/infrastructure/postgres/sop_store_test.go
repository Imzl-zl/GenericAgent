package postgres

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
)

func TestSophubBindingStorePersistsCiphertextOnly(t *testing.T) {
	pool := requireDB(t)
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	admin := seedDev(t, store)

	binding, err := store.UpsertSophubBinding(ctx, domain.SophubBinding{
		APIKeyCiphertext: []byte("encrypted-sentinel"),
		APIKeyVersion:    3,
		Identity: domain.SophubIdentity{
			AuthorType: "agent", AgentUID: "agent-1", DisplayName: "platform",
		},
		UpdatedBy: admin.UserID,
	})
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := store.GetSophubBinding(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if string(loaded.APIKeyCiphertext) != "encrypted-sentinel" || loaded.APIKeyVersion != 3 || loaded.Identity.AgentUID != "agent-1" || binding.VerifiedAt == nil {
		t.Fatalf("binding=%+v loaded=%+v", binding, loaded)
	}
	status := loaded.Status()
	if !status.Configured || status.AgentUID != "agent-1" {
		t.Fatalf("status=%+v", status)
	}
}

func TestSOPStoreInstallAndLoadLifecycle(t *testing.T) {
	pool := requireDB(t)
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	admin := seedDev(t, store)

	registered := false
	for _, name := range migrationFiles() {
		registered = registered || name == "0035_sophub_sop_registry.sql"
	}
	if !registered {
		t.Fatal("0035 migration is not registered")
	}
	for _, table := range []string{"sophub_bindings", "sop_candidates", "sop_entries", "sop_versions", "task_sop_snapshots"} {
		var exists bool
		if err := pool.QueryRow(ctx, `SELECT to_regclass('public.' || $1) IS NOT NULL`, table).Scan(&exists); err != nil || !exists {
			t.Fatalf("table %s exists=%v err=%v", table, exists, err)
		}
	}

	input := domain.ImportSOPCandidateCommand{
		RemoteSOPID: "remote-sop-1",
		Title:       "Document report",
		Description: "Generate a reviewed Word report.",
		FileType:    domain.SOPFileTypeMarkdown,
		Content:     "# Document report\n\nUse the available GA document tools.\n",
	}
	candidate, err := store.UpsertSOPCandidate(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	duplicate, err := store.UpsertSOPCandidate(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	if duplicate.ID != candidate.ID || duplicate.SourceDigest != candidate.SourceDigest || candidate.Status != domain.SOPCandidatePending {
		t.Fatalf("candidate=%+v duplicate=%+v", candidate, duplicate)
	}

	version, err := store.ApproveSOPCandidate(ctx, candidate.ID, admin.UserID)
	if err != nil {
		t.Fatal(err)
	}
	repeated, err := store.ApproveSOPCandidate(ctx, candidate.ID, admin.UserID)
	if err != nil {
		t.Fatal(err)
	}
	if repeated.ID != version.ID || version.Content != input.Content || version.ContentDigest != candidate.SourceDigest || version.Version != 1 {
		t.Fatalf("version=%+v repeated=%+v", version, repeated)
	}

	if _, err := pool.Exec(ctx, `UPDATE sop_versions SET content='tampered' WHERE id=$1::uuid`, version.ID); err == nil || !strings.Contains(err.Error(), "append-only") {
		t.Fatalf("expected append-only UPDATE rejection, got %v", err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM sop_versions WHERE id=$1::uuid`, version.ID); err == nil || !strings.Contains(err.Error(), "append-only") {
		t.Fatalf("expected append-only DELETE rejection, got %v", err)
	}

	registry, err := store.ListSOPRegistry(ctx)
	if err != nil || len(registry) != 1 || registry[0].Version.ID != version.ID || registry[0].Loaded {
		t.Fatalf("registry before load=%+v err=%v", registry, err)
	}
	approvedCandidates, err := store.ListSOPCandidates(ctx, domain.SOPCandidateApproved)
	if err != nil || len(approvedCandidates) != 1 || approvedCandidates[0].ID != candidate.ID {
		t.Fatalf("approved candidates=%+v err=%v", approvedCandidates, err)
	}

	entry, err := store.LoadSOPVersion(ctx, version.ID, admin.UserID)
	if err != nil {
		t.Fatal(err)
	}
	if entry.LoadedVersionID != version.ID || entry.ID != version.EntryID {
		t.Fatalf("loaded entry=%+v version=%+v", entry, version)
	}
	registry, err = store.ListSOPRegistry(ctx)
	if err != nil || len(registry) != 1 || !registry[0].Loaded {
		t.Fatalf("registry after load=%+v err=%v", registry, err)
	}
	loaded, err := store.ListLoadedSOPVersions(ctx)
	if err != nil || len(loaded) != 1 || loaded[0].ID != version.ID {
		t.Fatalf("loaded=%+v err=%v", loaded, err)
	}
	entry, err = store.UnloadSOP(ctx, entry.ID, admin.UserID)
	if err != nil {
		t.Fatal(err)
	}
	if entry.LoadedVersionID != "" {
		t.Fatalf("unloaded entry=%+v", entry)
	}
	loaded, err = store.ListLoadedSOPVersions(ctx)
	if err != nil || len(loaded) != 0 {
		t.Fatalf("loaded after unload=%+v err=%v", loaded, err)
	}
}

func TestTaskSOPSnapshotIsAtomicAndIdempotent(t *testing.T) {
	pool := requireDB(t)
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	dev := seedDev(t, store)

	installAndLoad := func(remoteID, title, content string) (domain.SOPVersion, domain.SOPEntry) {
		t.Helper()
		candidate, err := store.UpsertSOPCandidate(ctx, domain.ImportSOPCandidateCommand{
			RemoteSOPID: remoteID, Title: title, FileType: domain.SOPFileTypeMarkdown, Content: content,
		})
		if err != nil {
			t.Fatal(err)
		}
		version, err := store.ApproveSOPCandidate(ctx, candidate.ID, dev.UserID)
		if err != nil {
			t.Fatal(err)
		}
		entry, err := store.LoadSOPVersion(ctx, version.ID, dev.UserID)
		if err != nil {
			t.Fatal(err)
		}
		return version, entry
	}

	versionA, entryA := installAndLoad("snapshot-a", "SOP A", "# SOP A\n")
	first, err := store.SubmitTask(ctx, domain.SubmitTaskCommand{
		SessionKey: dev.SessionKey, RequesterUserID: dev.UserID, Source: domain.SourceWeb,
		SourceInstanceID: "sop-snapshot", MessageID: "message-1", Prompt: "first",
		PersonaSnapshot: []string{}, ToolPolicyVersion: "foundation.no-host-tools.v1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(first.SOPSnapshots) != 1 || first.SOPSnapshots[0].SOPVersionID != versionA.ID || first.SOPSnapshots[0].Content != "# SOP A\n" {
		t.Fatalf("first snapshots=%+v", first.SOPSnapshots)
	}

	if _, err := store.UnloadSOP(ctx, entryA.ID, dev.UserID); err != nil {
		t.Fatal(err)
	}
	versionB, _ := installAndLoad("snapshot-b", "SOP B", "# SOP B\n")
	if _, err := pool.Exec(ctx, `
UPDATE task_sop_snapshots
SET sop_version_id=$2::uuid, content_digest=$3
WHERE task_id=$1
`, first.ID, versionB.ID, versionB.ContentDigest); err == nil || !strings.Contains(err.Error(), "sealed") {
		t.Fatalf("expected sealed task snapshot UPDATE rejection, got %v", err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM task_sop_snapshots WHERE task_id=$1`, first.ID); err == nil || !strings.Contains(err.Error(), "sealed") {
		t.Fatalf("expected sealed task snapshot DELETE rejection, got %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE tasks SET created_at=timezone('utc', now()) WHERE id=$1`, first.ID); err == nil || !strings.Contains(err.Error(), "creation timestamp is immutable") {
		t.Fatalf("expected task snapshot creation timestamp mutation rejection, got %v", err)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO task_sop_snapshots(task_id,ordinal,sop_version_id,content_digest)
VALUES($1,1,$2::uuid,$3)
`, first.ID, versionB.ID, versionB.ContentDigest); err == nil || !strings.Contains(err.Error(), "creation transaction") {
		t.Fatalf("expected late task snapshot INSERT rejection, got %v", err)
	}
	duplicate, err := store.SubmitTask(ctx, domain.SubmitTaskCommand{
		SessionKey: dev.SessionKey, RequesterUserID: dev.UserID, Source: domain.SourceWeb,
		SourceInstanceID: "sop-snapshot", MessageID: "message-1", Prompt: "changed",
		PersonaSnapshot: []string{}, ToolPolicyVersion: "foundation.no-host-tools.v1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if duplicate.ID != first.ID || len(duplicate.SOPSnapshots) != 1 || duplicate.SOPSnapshots[0].SOPVersionID != versionA.ID {
		t.Fatalf("duplicate snapshots=%+v", duplicate.SOPSnapshots)
	}

	second, err := store.SubmitTask(ctx, domain.SubmitTaskCommand{
		SessionKey: dev.SessionKey, RequesterUserID: dev.UserID, Source: domain.SourceWeb,
		SourceInstanceID: "sop-snapshot", MessageID: "message-2", Prompt: "second",
		PersonaSnapshot: []string{}, ToolPolicyVersion: "foundation.no-host-tools.v1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(second.SOPSnapshots) != 1 || second.SOPSnapshots[0].SOPVersionID != versionB.ID || second.SOPSnapshots[0].ContentDigest != versionB.ContentDigest {
		t.Fatalf("second snapshots=%+v", second.SOPSnapshots)
	}
	loaded, err := store.GetTask(ctx, first.ID)
	if err != nil || len(loaded.SOPSnapshots) != 1 || loaded.SOPSnapshots[0].SOPVersionID != versionA.ID {
		t.Fatalf("loaded first=%+v err=%v", loaded.SOPSnapshots, err)
	}
}

func TestSOPStoreLoadLimit(t *testing.T) {
	pool := requireDB(t)
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	admin := seedDev(t, store)

	for index := 0; index < domain.MaxLoadedSOPs+1; index++ {
		candidate, err := store.UpsertSOPCandidate(ctx, domain.ImportSOPCandidateCommand{
			RemoteSOPID: fmt.Sprintf("load-limit-%d", index),
			Title:       fmt.Sprintf("Load limit %d", index),
			FileType:    domain.SOPFileTypeMarkdown,
			Content:     fmt.Sprintf("# Load limit %d\n", index),
		})
		if err != nil {
			t.Fatal(err)
		}
		version, err := store.ApproveSOPCandidate(ctx, candidate.ID, admin.UserID)
		if err != nil {
			t.Fatal(err)
		}
		_, err = store.LoadSOPVersion(ctx, version.ID, admin.UserID)
		if index < domain.MaxLoadedSOPs && err != nil {
			t.Fatalf("load %d: %v", index, err)
		}
		if index == domain.MaxLoadedSOPs && !errors.Is(err, domain.ErrSOPLoadLimit) {
			t.Fatalf("load over limit err=%v", err)
		}
	}
}

func TestSOPStoreRejectsUnsupportedAndInvalidTransitions(t *testing.T) {
	pool := requireDB(t)
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	admin := seedDev(t, store)

	_, err = store.UpsertSOPCandidate(ctx, domain.ImportSOPCandidateCommand{
		RemoteSOPID: "python-1", Title: "script", FileType: "python", Content: "print('no')",
	})
	if err == nil {
		t.Fatal("expected executable candidate rejection")
	}

	candidate, err := store.UpsertSOPCandidate(ctx, domain.ImportSOPCandidateCommand{
		RemoteSOPID: "reject-1", Title: "Rejected SOP", FileType: domain.SOPFileTypeMarkdown, Content: "# Rejected\n",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RejectSOPCandidate(ctx, candidate.ID, admin.UserID, "unsafe instructions"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ApproveSOPCandidate(ctx, candidate.ID, admin.UserID); !errors.Is(err, domain.ErrSOPCandidateState) {
		t.Fatalf("approve rejected candidate err=%v", err)
	}
	if _, err := store.LoadSOPVersion(ctx, "00000000-0000-4000-8000-000000000099", admin.UserID); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("load missing version err=%v", err)
	}
}
