package application

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	goruntime "runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
	documentinfra "github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/infrastructure/document"
)

func TestDocumentManagerConstructor(t *testing.T) {
	root := canonicalTempDir(t)
	store := newDocumentManagerFakeStore(0)
	runtime := &documentManagerFakeRuntime{}
	compiler := documentManagerCompiler{"write": {"doc-tool", "write"}}
	valid := ManagerConfig{
		Owner: "manager-1", Store: store, Runtime: runtime, Compiler: compiler,
		WorkRoot: root, ClaimLease: 100 * time.Millisecond, HeartbeatInterval: 20 * time.Millisecond,
		PollInterval: time.Millisecond, CommandPollInterval: time.Millisecond, ShutdownTimeout: time.Second,
	}
	if _, err := NewDocumentManager(valid); err != nil {
		t.Fatalf("valid config: %v", err)
	}

	tests := []struct {
		name string
		edit func(*ManagerConfig)
	}{
		{"owner", func(c *ManagerConfig) { c.Owner = " " }},
		{"store", func(c *ManagerConfig) { c.Store = nil }},
		{"runtime", func(c *ManagerConfig) { c.Runtime = nil }},
		{"compiler", func(c *ManagerConfig) { c.Compiler = nil }},
		{"lease", func(c *ManagerConfig) { c.ClaimLease = 0 }},
		{"heartbeat", func(c *ManagerConfig) { c.HeartbeatInterval = 0 }},
		{"lease not greater than heartbeat", func(c *ManagerConfig) { c.ClaimLease = c.HeartbeatInterval }},
		{"poll", func(c *ManagerConfig) { c.PollInterval = 0 }},
		{"command poll", func(c *ManagerConfig) { c.CommandPollInterval = 0 }},
		{"shutdown", func(c *ManagerConfig) { c.ShutdownTimeout = 0 }},
		{"relative root", func(c *ManagerConfig) { c.WorkRoot = "relative" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := valid
			tt.edit(&cfg)
			if _, err := NewDocumentManager(cfg); err == nil {
				t.Fatal("expected constructor error")
			}
		})
	}

	file := filepath.Join(root, "file")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := valid
	cfg.WorkRoot = file
	if _, err := NewDocumentManager(cfg); err == nil {
		t.Fatal("expected non-directory root error")
	}

	link := filepath.Join(filepath.Dir(root), filepath.Base(root)+"-link")
	if err := os.Symlink(root, link); err == nil {
		t.Cleanup(func() { _ = os.Remove(link) })
		cfg = valid
		cfg.WorkRoot = link
		if _, err := NewDocumentManager(cfg); err == nil {
			t.Fatal("expected symlink root error")
		}
	}
}

func TestDocumentManagerPrewarmsAndRecoversCreating(t *testing.T) {
	root := canonicalTempDir(t)
	store := newDocumentManagerFakeStore(2)
	store.instances = append(store.instances, domain.DocumentInstance{
		ID: "uncertain", InstanceName: "ga-document-uncertain", SlotPath: filepath.Join(root, "ga-document-uncertain"), Status: domain.DocumentInstanceCreating,
	})
	if err := os.Mkdir(store.instances[0].SlotPath, 0o700); err != nil {
		t.Fatal(err)
	}
	runtime := &documentManagerFakeRuntime{}
	manager := newTestDocumentManager(t, root, store, runtime, documentManagerCompiler{})
	cancel, done := runDocumentManager(t, manager)
	waitForDocumentManager(t, time.Second, func() bool {
		store.mu.Lock()
		defer store.mu.Unlock()
		ready := 0
		for _, instance := range store.instances {
			if instance.Status == domain.DocumentInstanceReady {
				ready++
			}
		}
		return ready == 2 && store.destroyed["ga-document-uncertain"] > 0
	})
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("run: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "ga-document-uncertain")); !os.IsNotExist(err) {
		t.Fatalf("recovered slot still exists: %v", err)
	}
	if goruntime.GOOS != "windows" {
		for _, mode := range runtime.createdModes() {
			if mode.Perm() != 0o700 {
				t.Fatalf("slot mode=%o, want 700", mode.Perm())
			}
		}
	}
}

func TestDocumentManagerPrewarmFailureCreatesDurableCleanupIntent(t *testing.T) {
	for name, configure := range map[string]func(*documentManagerFakeStore, *documentManagerFakeRuntime){
		"runtime create": func(_ *documentManagerFakeStore, runtime *documentManagerFakeRuntime) {
			runtime.createErr = errors.New("create failed after side effect")
		},
		"mark ready": func(store *documentManagerFakeStore, _ *documentManagerFakeRuntime) {
			store.markReadyErr = errors.New("mark ready response lost")
		},
	} {
		t.Run(name, func(t *testing.T) {
			root := canonicalTempDir(t)
			store := newDocumentManagerFakeStore(1)
			runtime := &documentManagerFakeRuntime{}
			configure(store, runtime)
			manager := newTestDocumentManager(t, root, store, runtime, documentManagerCompiler{})

			if err := manager.reconcilePrewarm(context.Background()); err == nil {
				t.Fatal("expected prewarm failure")
			}
			store.mu.Lock()
			if len(store.instances) != 1 || store.instances[0].Status != domain.DocumentInstanceDestroyed {
				t.Fatalf("instances=%+v", store.instances)
			}
			instance := store.instances[0]
			store.mu.Unlock()
			if runtime.destroyCount() != 1 {
				t.Fatalf("destroy calls=%d", runtime.destroyCount())
			}
			if _, err := os.Lstat(instance.SlotPath); !os.IsNotExist(err) {
				t.Fatalf("slot still exists: %v", err)
			}
		})
	}
}

func TestDocumentManagerRetriesFailedPrewarmCleanupIntentOnNextTick(t *testing.T) {
	for name, configure := range map[string]func(*documentManagerFakeStore){
		"write rejected":             func(store *documentManagerFakeStore) { store.markDestroyFailures = 1 },
		"response lost after commit": func(store *documentManagerFakeStore) { store.markDestroyAppliedFailures = 1 },
	} {
		t.Run(name, func(t *testing.T) {
			root := canonicalTempDir(t)
			store := newDocumentManagerFakeStore(1)
			configure(store)
			runtime := &documentManagerFakeRuntime{createErr: errors.New("create failed after side effect")}
			manager := newTestDocumentManager(t, root, store, runtime, documentManagerCompiler{})

			if err := manager.reconcilePrewarm(context.Background()); err == nil {
				t.Fatal("expected prewarm failure")
			}
			store.mu.Lock()
			if len(store.instances) != 1 {
				t.Fatalf("instances after failed intent=%+v", store.instances)
			}
			failedInstance := store.instances[0]
			store.mu.Unlock()

			runtime.mu.Lock()
			runtime.createErr = nil
			runtime.mu.Unlock()
			manager.runTick(context.Background())

			store.mu.Lock()
			defer store.mu.Unlock()
			if store.instances[0].Status != domain.DocumentInstanceDestroyed || store.destroyed[failedInstance.InstanceName] != 1 {
				t.Fatalf("instances after retry=%+v destroyed=%v", store.instances, store.destroyed)
			}
			if _, err := os.Lstat(failedInstance.SlotPath); !os.IsNotExist(err) {
				t.Fatalf("failed slot still exists: %v", err)
			}
		})
	}
}

func TestDocumentManagerRunsCommandsFIFOAndWaitsForClose(t *testing.T) {
	root := canonicalTempDir(t)
	store := newDocumentManagerFakeStore(1)
	job := store.enqueueJob("job-1", "tenant-1", false,
		fakeDocumentCommand("job-1", "first", "write", `{"value":1}`),
		fakeDocumentCommand("job-1", "second", "write", `{"value":2}`),
	)
	runtime := &documentManagerFakeRuntime{}
	manager := newTestDocumentManager(t, root, store, runtime, documentManagerCompiler{"write": {"doc-tool", "write"}})
	cancel, done := runDocumentManager(t, manager)
	waitForDocumentManager(t, time.Second, func() bool { return runtime.execCount() == 2 })
	store.mu.Lock()
	if store.jobs[job.ID].Status == domain.DocumentJobSucceeded {
		store.mu.Unlock()
		t.Fatal("job succeeded before commands were closed")
	}
	store.jobs[job.ID] = withClosedCommands(store.jobs[job.ID])
	store.mu.Unlock()
	waitForDocumentManager(t, time.Second, func() bool {
		store.mu.Lock()
		defer store.mu.Unlock()
		return store.jobs[job.ID].Status == domain.DocumentJobSucceeded && store.destroyedCount() == 1
	})
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("run: %v", err)
	}
	execs := runtime.execCallsSnapshot()
	if len(execs) != 2 || execs[0].name != execs[1].name {
		t.Fatalf("commands did not share one instance: %#v", execs)
	}
	if got := []string{execs[0].argv[0], execs[1].argv[0]}; !reflect.DeepEqual(got, []string{documentToolExecutable, documentToolExecutable}) {
		t.Fatalf("FIFO execs=%v", got)
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if got := store.completed; !reflect.DeepEqual(got, []string{"first:succeeded", "second:succeeded"}) {
		t.Fatalf("completion order=%v", got)
	}
}

func TestDocumentManagerPersistsExportArtifactBeforeSuccess(t *testing.T) {
	root := canonicalTempDir(t)
	store := newDocumentManagerFakeStore(1)
	job := store.enqueueJob("job-export", "tenant", true,
		fakeDocumentCommand("job-export", "export-request", "export_docx", `{"output_name":"Quarterly Report.docx","title":"Q2","content":"hello"}`),
	)
	docx := testDOCXArtifact(t, "hello")
	runtime := &documentManagerFakeRuntime{execInputOutput: docx}
	manager := newTestDocumentManager(t, root, store, runtime, FixedDocumentOperationCompiler{})
	cancel, done := runDocumentManager(t, manager)
	waitForDocumentManager(t, time.Second, func() bool {
		store.mu.Lock()
		defer store.mu.Unlock()
		return store.jobs[job.ID].Status == domain.DocumentJobSucceeded && len(store.artifacts) == 1
	})
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("run: %v", err)
	}

	calls := runtime.execCallsSnapshot()
	if len(calls) != 1 || calls[0].stdoutLimit != domain.MaxDocumentArtifactBytes {
		t.Fatalf("exec calls=%+v", calls)
	}
	if strings.Contains(strings.Join(calls[0].argv, "\x00"), "hello") || len(calls[0].stdin) == 0 {
		t.Fatalf("unsafe export invocation=%+v", calls[0])
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	artifact := store.artifacts[0]
	if artifact.FileName != "Quarterly Report.docx" || artifact.MediaType != DocumentDOCXMediaType || !bytes.Equal(artifact.Content, docx) {
		t.Fatalf("artifact=%+v", artifact)
	}
	if got := store.completed; !reflect.DeepEqual(got, []string{"export-request:succeeded"}) {
		t.Fatalf("completions=%v", got)
	}
}

func TestDocumentManagerRejectsInvalidExportArtifact(t *testing.T) {
	for name, output := range map[string][]byte{
		"empty":    nil,
		"not docx": []byte("not-a-docx"),
	} {
		t.Run(name, func(t *testing.T) {
			root := canonicalTempDir(t)
			store := newDocumentManagerFakeStore(1)
			job := store.enqueueJob("job-invalid-export", "tenant", true,
				fakeDocumentCommand("job-invalid-export", "export-request", "export_docx", `{"content":"hello"}`),
			)
			runtime := &documentManagerFakeRuntime{execInputOutput: output}
			manager := newTestDocumentManager(t, root, store, runtime, FixedDocumentOperationCompiler{})
			cancel, done := runDocumentManager(t, manager)
			waitForDocumentManager(t, time.Second, func() bool {
				store.mu.Lock()
				defer store.mu.Unlock()
				return store.jobs[job.ID].Status == domain.DocumentJobFailed
			})
			cancel()
			if err := <-done; err != nil {
				t.Fatalf("run: %v", err)
			}
			store.mu.Lock()
			defer store.mu.Unlock()
			if len(store.artifacts) != 0 || !reflect.DeepEqual(store.completed, []string{"export-request:failed"}) {
				t.Fatalf("artifacts=%+v completions=%v", store.artifacts, store.completed)
			}
		})
	}
}

func testDOCXArtifact(t *testing.T, content string) []byte {
	t.Helper()
	var output bytes.Buffer
	writer := zip.NewWriter(&output)
	for name, body := range map[string]string{
		"[Content_Types].xml": `<Types/>`,
		"_rels/.rels":         `<Relationships/>`,
		"word/document.xml":   `<document>` + content + `</document>`,
	} {
		file, err := writer.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := file.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}

func TestDocumentManagerContinuesWhenCloseRacesWithEmptyClaim(t *testing.T) {
	root := canonicalTempDir(t)
	store := newDocumentManagerFakeStore(1)
	job := store.enqueueJob("job-close-race", "tenant", false)
	store.injectOnEmptyClaim = func(jobID string) {
		if jobID != job.ID {
			return
		}
		store.commands[jobID] = append(store.commands[jobID], fakeDocumentCommand(jobID, "late", "write", `{}`))
		updated := store.jobs[jobID]
		updated = withClosedCommands(updated)
		store.jobs[jobID] = updated
	}
	runtime := &documentManagerFakeRuntime{}
	manager := newTestDocumentManager(t, root, store, runtime, documentManagerCompiler{"write": {"doc-tool"}})
	cancel, done := runDocumentManager(t, manager)
	waitForDocumentManager(t, time.Second, func() bool {
		store.mu.Lock()
		defer store.mu.Unlock()
		return store.jobs[job.ID].Status == domain.DocumentJobSucceeded && store.destroyedCount() == 1
	})
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := runtime.execCount(); got != 1 {
		t.Fatalf("Exec called %d times", got)
	}
}

func TestDocumentManagerDoesNotRecoverActiveCreatingEveryTick(t *testing.T) {
	root := canonicalTempDir(t)
	store := newDocumentManagerFakeStore(1)
	runtime := &documentManagerFakeRuntime{createBlock: make(chan struct{})}
	manager := newTestDocumentManager(t, root, store, runtime, documentManagerCompiler{})
	cancel, done := runDocumentManager(t, manager)
	waitForDocumentManager(t, time.Second, func() bool {
		store.mu.Lock()
		defer store.mu.Unlock()
		return len(store.instances) == 1 && store.instances[0].Status == domain.DocumentInstanceCreating
	})
	time.Sleep(20 * time.Millisecond)
	store.mu.Lock()
	status := store.instances[0].Status
	store.mu.Unlock()
	close(runtime.createBlock)
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("run: %v", err)
	}
	if status != domain.DocumentInstanceCreating {
		t.Fatalf("active creating was recovered during tick: %s", status)
	}
}

func TestDocumentManagerDifferentJobsUseDifferentInstances(t *testing.T) {
	root := canonicalTempDir(t)
	store := newDocumentManagerFakeStore(1)
	store.enqueueJob("job-1", "tenant-1", true, fakeDocumentCommand("job-1", "one", "write", `{}`))
	store.enqueueJob("job-2", "tenant-1", true, fakeDocumentCommand("job-2", "two", "write", `{}`))
	runtime := &documentManagerFakeRuntime{}
	manager := newTestDocumentManager(t, root, store, runtime, documentManagerCompiler{"write": {"doc-tool"}})
	cancel, done := runDocumentManager(t, manager)
	waitForDocumentManager(t, time.Second, func() bool { return runtime.execCount() == 2 })
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("run: %v", err)
	}
	execs := runtime.execCallsSnapshot()
	if execs[0].name == execs[1].name {
		t.Fatalf("different jobs reused %q", execs[0].name)
	}
}

func TestDocumentManagerUnknownOperationFailsWithoutExec(t *testing.T) {
	root := canonicalTempDir(t)
	store := newDocumentManagerFakeStore(1)
	job := store.enqueueJob("job-unknown", "tenant", true, fakeDocumentCommand("job-unknown", "bad", "unknown", `{}`))
	runtime := &documentManagerFakeRuntime{}
	manager := newTestDocumentManager(t, root, store, runtime, EmptyOperationCompiler{})
	cancel, done := runDocumentManager(t, manager)
	waitForDocumentManager(t, time.Second, func() bool {
		store.mu.Lock()
		defer store.mu.Unlock()
		return store.jobs[job.ID].Status == domain.DocumentJobFailed && store.destroyedCount() == 1
	})
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := runtime.execCount(); got != 0 {
		t.Fatalf("Exec called %d times", got)
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if got := store.jobs[job.ID].TerminalErrorCode; got != "DOCUMENT_COMMAND_FAILED" {
		t.Fatalf("error code=%q", got)
	}
}

func TestDocumentManagerRejectsCompilerPlanOutsideFixedToolInvocation(t *testing.T) {
	root := canonicalTempDir(t)
	store := newDocumentManagerFakeStore(1)
	job := store.enqueueJob("job-unsafe-plan", "tenant", true,
		fakeDocumentCommand("job-unsafe-plan", "unsafe", "write", `{}`),
	)
	runtime := &documentManagerFakeRuntime{}
	manager := newTestDocumentManager(t, root, store, runtime, documentManagerCompiler{
		"write": {"/bin/sh", "-c", "id"},
	})
	cancel, done := runDocumentManager(t, manager)
	waitForDocumentManager(t, time.Second, func() bool {
		store.mu.Lock()
		defer store.mu.Unlock()
		return store.jobs[job.ID].Status == domain.DocumentJobFailed
	})
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := runtime.execCount(); got != 0 {
		t.Fatalf("Exec called %d times", got)
	}
}

func TestDocumentManagerRejectsMalformedOperationPayloads(t *testing.T) {
	tests := []struct {
		name    string
		payload string
	}{
		{"unknown field", `{"schema_version":1,"operation":"write","parameters":{},"extra":true}`},
		{"multiple values", `{"schema_version":1,"operation":"write","parameters":{}} {}`},
		{"wrong schema", `{"schema_version":2,"operation":"write","parameters":{}}`},
		{"missing operation", `{"schema_version":1,"operation":"","parameters":{}}`},
		{"array parameters", `{"schema_version":1,"operation":"write","parameters":[]}`},
		{"null parameters", `{"schema_version":1,"operation":"write","parameters":null}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := decodeDocumentOperation(json.RawMessage(tt.payload)); err == nil {
				t.Fatal("expected strict decode error")
			}
		})
	}
	if _, err := decodeDocumentOperation(json.RawMessage(`{"schema_version":1,"operation":"write","parameters":{}}`)); err != nil {
		t.Fatalf("valid payload: %v", err)
	}
}

func TestDocumentManagerRejectsEmptyCompiledArgvWithoutExec(t *testing.T) {
	root := canonicalTempDir(t)
	store := newDocumentManagerFakeStore(1)
	job := store.enqueueJob("job-empty-argv", "tenant", true, fakeDocumentCommand("job-empty-argv", "bad", "write", `{}`))
	runtime := &documentManagerFakeRuntime{}
	manager := newTestDocumentManager(t, root, store, runtime, documentManagerCompiler{"write": {" "}})
	cancel, done := runDocumentManager(t, manager)
	waitForDocumentManager(t, time.Second, func() bool {
		store.mu.Lock()
		defer store.mu.Unlock()
		return store.jobs[job.ID].Status == domain.DocumentJobFailed
	})
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := runtime.execCount(); got != 0 {
		t.Fatalf("Exec called %d times", got)
	}
}

func TestDocumentManagerExecFailureStopsLaterCommands(t *testing.T) {
	root := canonicalTempDir(t)
	store := newDocumentManagerFakeStore(1)
	job := store.enqueueJob("job-fail", "tenant", true,
		fakeDocumentCommand("job-fail", "first", "write", `{}`),
		fakeDocumentCommand("job-fail", "second", "write", `{}`),
	)
	runtime := &documentManagerFakeRuntime{execErrAt: 1}
	manager := newTestDocumentManager(t, root, store, runtime, documentManagerCompiler{"write": {"doc-tool"}})
	cancel, done := runDocumentManager(t, manager)
	waitForDocumentManager(t, time.Second, func() bool {
		store.mu.Lock()
		defer store.mu.Unlock()
		return store.jobs[job.ID].Status == domain.DocumentJobFailed
	})
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := runtime.execCount(); got != 1 {
		t.Fatalf("Exec called %d times", got)
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if got := store.completed; !reflect.DeepEqual(got, []string{"first:failed"}) {
		t.Fatalf("completions=%v", got)
	}
}

func TestDocumentManagerNonZeroExitStopsLaterCommands(t *testing.T) {
	root := canonicalTempDir(t)
	store := newDocumentManagerFakeStore(1)
	job := store.enqueueJob("job-exit", "tenant", true,
		fakeDocumentCommand("job-exit", "first", "write", `{}`),
		fakeDocumentCommand("job-exit", "second", "write", `{}`),
	)
	runtime := &documentManagerFakeRuntime{execExitCodeAt: 1}
	manager := newTestDocumentManager(t, root, store, runtime, documentManagerCompiler{"write": {"doc-tool"}})
	cancel, done := runDocumentManager(t, manager)
	waitForDocumentManager(t, time.Second, func() bool {
		store.mu.Lock()
		defer store.mu.Unlock()
		return store.jobs[job.ID].Status == domain.DocumentJobFailed
	})
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := runtime.execCount(); got != 1 {
		t.Fatalf("Exec called %d times", got)
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if got := store.completed; !reflect.DeepEqual(got, []string{"first:failed"}) {
		t.Fatalf("completions=%v", got)
	}
}

func TestDocumentManagerHeartbeatFenceLossStopsWithoutSuccess(t *testing.T) {
	root := canonicalTempDir(t)
	store := newDocumentManagerFakeStore(1)
	job := store.enqueueJob("job-heartbeat", "tenant", true, fakeDocumentCommand("job-heartbeat", "slow", "write", `{}`))
	store.heartbeatErr = domain.ErrDocumentFenceLost
	runtime := &documentManagerFakeRuntime{execBlock: make(chan struct{})}
	manager := newTestDocumentManager(t, root, store, runtime, documentManagerCompiler{"write": {"doc-tool"}})
	cancel, done := runDocumentManager(t, manager)
	waitForDocumentManager(t, time.Second, func() bool {
		store.mu.Lock()
		defer store.mu.Unlock()
		return store.heartbeatCalls > 0
	})
	waitForDocumentManager(t, time.Second, func() bool { return runtime.cancelledExecs() == 1 })
	store.mu.Lock()
	if store.jobs[job.ID].Status == domain.DocumentJobSucceeded || store.finalizeSuccess > 0 {
		store.mu.Unlock()
		t.Fatal("heartbeat fence loss committed success")
	}
	store.mu.Unlock()
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("run: %v", err)
	}
}

func TestDocumentManagerDestroyFailureRetriesNextTick(t *testing.T) {
	root := canonicalTempDir(t)
	store := newDocumentManagerFakeStore(0)
	slot := filepath.Join(root, "ga-document-destroy")
	if err := os.Mkdir(slot, 0o700); err != nil {
		t.Fatal(err)
	}
	store.instances = append(store.instances, domain.DocumentInstance{ID: "destroy", InstanceName: "ga-document-destroy", SlotPath: slot, Status: domain.DocumentInstanceDestroying})
	runtime := &documentManagerFakeRuntime{destroyFailures: 1}
	manager := newTestDocumentManager(t, root, store, runtime, documentManagerCompiler{})
	cancel, done := runDocumentManager(t, manager)
	waitForDocumentManager(t, time.Second, func() bool { return runtime.destroyCount() >= 2 })
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("run: %v", err)
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.instances[0].Status != domain.DocumentInstanceDestroyed {
		t.Fatalf("instance status=%s", store.instances[0].Status)
	}
}

func TestDocumentManagerRunCancellationWaitsForJobs(t *testing.T) {
	root := canonicalTempDir(t)
	store := newDocumentManagerFakeStore(1)
	store.enqueueJob("job-cancel", "tenant", false, fakeDocumentCommand("job-cancel", "slow", "write", `{}`))
	runtime := &documentManagerFakeRuntime{execBlock: make(chan struct{})}
	manager := newTestDocumentManager(t, root, store, runtime, documentManagerCompiler{"write": {"doc-tool"}})
	cancel, done := runDocumentManager(t, manager)
	waitForDocumentManager(t, time.Second, func() bool { return runtime.execCount() == 1 })
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Run leaked an active job goroutine")
	}
}

func newTestDocumentManager(t *testing.T, root string, store *documentManagerFakeStore, runtime *documentManagerFakeRuntime, compiler OperationCompiler) *DocumentManager {
	t.Helper()
	manager, err := NewDocumentManager(ManagerConfig{
		Owner: "test-manager", Store: store, Runtime: runtime, Compiler: compiler, WorkRoot: root,
		ClaimLease: 100 * time.Millisecond, HeartbeatInterval: 10 * time.Millisecond,
		PollInterval: 2 * time.Millisecond, CommandPollInterval: 2 * time.Millisecond, ShutdownTimeout: 200 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	return manager
}

func runDocumentManager(t *testing.T, manager *DocumentManager) (context.CancelFunc, <-chan error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- manager.Run(ctx) }()
	return cancel, done
}

func waitForDocumentManager(t *testing.T, timeout time.Duration, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("condition not met before timeout")
}

func canonicalTempDir(t *testing.T) string {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Clean(root)
}

func fakeDocumentCommand(jobID, commandID, operation, parameters string) domain.DocumentCommand {
	payload, _ := json.Marshal(domain.DocumentOperationRequest{SchemaVersion: 1, Operation: operation, Parameters: json.RawMessage(parameters)})
	return domain.DocumentCommand{ID: commandID, JobID: jobID, CommandID: commandID, Payload: payload, Status: domain.DocumentCommandPending}
}

func withClosedCommands(job domain.DocumentJob) domain.DocumentJob {
	now := time.Now()
	job.CommandsClosedAt = &now
	return job
}

type documentManagerCompiler map[string][]string

func (c documentManagerCompiler) Compile(request domain.DocumentOperationRequest, _ string) (DocumentOperationPlan, error) {
	argv, ok := c[request.Operation]
	if !ok {
		return DocumentOperationPlan{}, errors.New("unknown operation")
	}
	if len(argv) > 0 && argv[0] == "doc-tool" {
		argv = documentExportDOCXArgv()
	}
	return DocumentOperationPlan{Argv: append([]string(nil), argv...)}, nil
}

type documentManagerExecCall struct {
	name        string
	argv        []string
	stdin       []byte
	stdoutLimit int
}

type documentManagerFakeRuntime struct {
	mu              sync.Mutex
	verifyErr       error
	created         []documentinfra.ContainerSpec
	createdMode     []os.FileMode
	createBlock     chan struct{}
	createErr       error
	execs           []documentManagerExecCall
	execErrAt       int
	execExitCodeAt  int
	execBlock       chan struct{}
	execCancelled   int
	execInputOutput []byte
	destroys        []string
	destroyFailures int
}

func (r *documentManagerFakeRuntime) VerifyHost(context.Context) error { return r.verifyErr }

func (r *documentManagerFakeRuntime) CreateAndStart(ctx context.Context, spec documentinfra.ContainerSpec) (documentinfra.Container, error) {
	info, err := os.Stat(spec.SlotPath)
	if err != nil {
		return documentinfra.Container{}, err
	}
	if r.createBlock != nil {
		select {
		case <-r.createBlock:
		case <-ctx.Done():
			return documentinfra.Container{}, ctx.Err()
		}
	}
	r.mu.Lock()
	r.created = append(r.created, spec)
	r.createdMode = append(r.createdMode, info.Mode())
	err = r.createErr
	r.mu.Unlock()
	if err != nil {
		return documentinfra.Container{}, err
	}
	return documentinfra.Container{ID: spec.Name + "-id", Name: spec.Name, SlotPath: spec.SlotPath}, nil
}

func (r *documentManagerFakeRuntime) Exec(ctx context.Context, name string, argv []string) (documentinfra.CommandResult, error) {
	r.mu.Lock()
	r.execs = append(r.execs, documentManagerExecCall{name: name, argv: append([]string(nil), argv...)})
	call := len(r.execs)
	block := r.execBlock
	errAt := r.execErrAt
	exitCodeAt := r.execExitCodeAt
	r.mu.Unlock()
	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			r.mu.Lock()
			r.execCancelled++
			r.mu.Unlock()
			return documentinfra.CommandResult{}, ctx.Err()
		}
	}
	if errAt == call {
		return documentinfra.CommandResult{ExitCode: 1}, errors.New("exec failed")
	}
	if exitCodeAt == call {
		return documentinfra.CommandResult{ExitCode: 7}, nil
	}
	return documentinfra.CommandResult{}, nil
}

func (r *documentManagerFakeRuntime) ExecInput(ctx context.Context, name string, argv []string, stdin []byte, stdoutLimit int) (documentinfra.CommandResult, error) {
	r.mu.Lock()
	r.execs = append(r.execs, documentManagerExecCall{
		name: name, argv: append([]string(nil), argv...), stdin: append([]byte(nil), stdin...), stdoutLimit: stdoutLimit,
	})
	output := append([]byte(nil), r.execInputOutput...)
	r.mu.Unlock()
	if len(output) > stdoutLimit {
		return documentinfra.CommandResult{}, errors.New("document command stdout exceeded limit")
	}
	return documentinfra.CommandResult{Stdout: output}, nil
}

func (r *documentManagerFakeRuntime) Destroy(_ context.Context, name string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.destroys = append(r.destroys, name)
	if r.destroyFailures > 0 {
		r.destroyFailures--
		return errors.New("destroy failed")
	}
	return nil
}

func (r *documentManagerFakeRuntime) execCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.execs)
}
func (r *documentManagerFakeRuntime) destroyCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.destroys)
}
func (r *documentManagerFakeRuntime) cancelledExecs() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.execCancelled
}
func (r *documentManagerFakeRuntime) execCallsSnapshot() []documentManagerExecCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]documentManagerExecCall(nil), r.execs...)
}
func (r *documentManagerFakeRuntime) createdModes() []os.FileMode {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]os.FileMode(nil), r.createdMode...)
}

type documentManagerFakeStore struct {
	mu                         sync.Mutex
	minReady                   int
	instances                  []domain.DocumentInstance
	jobs                       map[string]domain.DocumentJob
	queue                      []string
	commands                   map[string][]domain.DocumentCommand
	completed                  []string
	artifacts                  []domain.DocumentArtifact
	destroyed                  map[string]int
	markReadyErr               error
	markDestroyFailures        int
	markDestroyAppliedFailures int
	heartbeatErr               error
	heartbeatCalls             int
	injectOnEmptyClaim         func(jobID string)
	finalizeSuccess            int
	nextInstance               int
}

func newDocumentManagerFakeStore(minReady int) *documentManagerFakeStore {
	return &documentManagerFakeStore{minReady: minReady, jobs: map[string]domain.DocumentJob{}, commands: map[string][]domain.DocumentCommand{}, destroyed: map[string]int{}}
}

func (s *documentManagerFakeStore) enqueueJob(id, workspace string, closed bool, commands ...domain.DocumentCommand) domain.DocumentJob {
	s.mu.Lock()
	defer s.mu.Unlock()
	job := domain.DocumentJob{ID: id, WorkspaceID: workspace, Status: domain.DocumentJobQueued, Generation: 0}
	if closed {
		job = withClosedCommands(job)
	}
	s.jobs[id] = job
	s.queue = append(s.queue, id)
	s.commands[id] = append([]domain.DocumentCommand(nil), commands...)
	return job
}

func (s *documentManagerFakeStore) RecoverCreatingDocumentInstances(context.Context) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	count := 0
	for i := range s.instances {
		if s.instances[i].Status == domain.DocumentInstanceCreating {
			s.instances[i].Status = domain.DocumentInstanceDestroying
			count++
		}
	}
	return count, nil
}
func (s *documentManagerFakeStore) ReserveExcessReadyForDestroy(context.Context) (int, error) {
	return 0, nil
}
func (s *documentManagerFakeStore) ListDestroyingDocumentInstances(context.Context) ([]domain.DocumentInstance, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var result []domain.DocumentInstance
	for _, instance := range s.instances {
		if instance.Status == domain.DocumentInstanceDestroying {
			result = append(result, instance)
		}
	}
	return result, nil
}
func (s *documentManagerFakeStore) MarkDocumentInstanceDestroyed(_ context.Context, id string) (domain.DocumentInstance, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.instances {
		if s.instances[i].ID == id && s.instances[i].Status == domain.DocumentInstanceDestroying {
			s.instances[i].Status = domain.DocumentInstanceDestroyed
			s.destroyed[s.instances[i].InstanceName]++
			return s.instances[i], nil
		}
	}
	return domain.DocumentInstance{}, domain.ErrDocumentJobState
}
func (s *documentManagerFakeStore) ReserveDocumentInstance(_ context.Context, name, slot string) (domain.DocumentInstance, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	warm := 0
	for _, instance := range s.instances {
		if instance.Status == domain.DocumentInstanceCreating || instance.Status == domain.DocumentInstanceReady {
			warm++
		}
	}
	if warm >= s.minReady {
		return domain.DocumentInstance{}, false, nil
	}
	s.nextInstance++
	instance := domain.DocumentInstance{ID: "instance-" + name, InstanceName: name, SlotPath: slot, Status: domain.DocumentInstanceCreating}
	s.instances = append(s.instances, instance)
	return instance, true, nil
}
func (s *documentManagerFakeStore) MarkDocumentInstanceReady(_ context.Context, id string) (domain.DocumentInstance, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.markReadyErr != nil {
		return domain.DocumentInstance{}, s.markReadyErr
	}
	for i := range s.instances {
		if s.instances[i].ID == id && s.instances[i].Status == domain.DocumentInstanceCreating {
			s.instances[i].Status = domain.DocumentInstanceReady
			return s.instances[i], nil
		}
	}
	return domain.DocumentInstance{}, domain.ErrDocumentJobState
}
func (s *documentManagerFakeStore) MarkDocumentInstanceDestroying(_ context.Context, id string) (domain.DocumentInstance, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.markDestroyFailures > 0 {
		s.markDestroyFailures--
		return domain.DocumentInstance{}, errors.New("mark destroying failed")
	}
	for i := range s.instances {
		if s.instances[i].ID != id {
			continue
		}
		if (s.instances[i].Status == domain.DocumentInstanceCreating || s.instances[i].Status == domain.DocumentInstanceReady) && s.instances[i].AllocatedJobID == "" {
			s.instances[i].Status = domain.DocumentInstanceDestroying
			if s.markDestroyAppliedFailures > 0 {
				s.markDestroyAppliedFailures--
				return domain.DocumentInstance{}, errors.New("mark destroying response lost")
			}
			return s.instances[i], nil
		}
		if s.instances[i].Status == domain.DocumentInstanceDestroying {
			return s.instances[i], nil
		}
	}
	return domain.DocumentInstance{}, domain.ErrDocumentJobState
}
func (s *documentManagerFakeStore) SweepExpiredDocumentWork(context.Context) (domain.DocumentSweepResult, error) {
	return domain.DocumentSweepResult{}, nil
}
func (s *documentManagerFakeStore) ClaimNextDocumentJob(_ context.Context, owner string, lease time.Duration) (domain.DocumentClaim, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.queue) == 0 {
		return domain.DocumentClaim{}, false, nil
	}
	instanceIndex := -1
	for i := range s.instances {
		if s.instances[i].Status == domain.DocumentInstanceReady {
			instanceIndex = i
			break
		}
	}
	if instanceIndex < 0 {
		return domain.DocumentClaim{}, false, nil
	}
	id := s.queue[0]
	s.queue = s.queue[1:]
	job := s.jobs[id]
	job.Status, job.ClaimOwner, job.Generation = domain.DocumentJobStarting, owner, job.Generation+1
	job.InstanceID = s.instances[instanceIndex].ID
	s.jobs[id] = job
	s.instances[instanceIndex].Status = domain.DocumentInstanceAllocated
	s.instances[instanceIndex].AllocatedJobID = id
	return domain.DocumentClaim{Job: job, Instance: s.instances[instanceIndex]}, true, nil
}
func (s *documentManagerFakeStore) MarkDocumentJobAndInstanceRunning(_ context.Context, id, owner string, generation int64) (domain.DocumentJob, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	job := s.jobs[id]
	if job.ClaimOwner != owner || job.Generation != generation || job.Status != domain.DocumentJobStarting {
		return domain.DocumentJob{}, domain.ErrDocumentFenceLost
	}
	job.Status = domain.DocumentJobRunning
	s.jobs[id] = job
	for i := range s.instances {
		if s.instances[i].ID == job.InstanceID {
			s.instances[i].Status = domain.DocumentInstanceRunning
		}
	}
	return job, nil
}
func (s *documentManagerFakeStore) HeartbeatDocumentJob(context.Context, string, string, int64, time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.heartbeatCalls++
	return s.heartbeatErr
}
func (s *documentManagerFakeStore) ClaimNextDocumentCommand(_ context.Context, jobID, owner string, generation int64) (domain.DocumentCommand, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	job := s.jobs[jobID]
	if job.ClaimOwner != owner || job.Generation != generation || job.Status != domain.DocumentJobRunning {
		return domain.DocumentCommand{}, false, domain.ErrDocumentFenceLost
	}
	for i := range s.commands[jobID] {
		if s.commands[jobID][i].Status == domain.DocumentCommandPending {
			s.commands[jobID][i].Status = domain.DocumentCommandExecuting
			s.commands[jobID][i].Generation = generation
			return s.commands[jobID][i], true, nil
		}
	}
	if s.injectOnEmptyClaim != nil {
		s.injectOnEmptyClaim(jobID)
		s.injectOnEmptyClaim = nil
	}
	return domain.DocumentCommand{}, false, nil
}
func (s *documentManagerFakeStore) CompleteDocumentCommand(_ context.Context, jobID, commandID, owner string, generation int64, status domain.DocumentCommandStatus) (domain.DocumentCommand, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	job := s.jobs[jobID]
	if job.ClaimOwner != owner || job.Generation != generation || job.Status != domain.DocumentJobRunning {
		return domain.DocumentCommand{}, domain.ErrDocumentFenceLost
	}
	for i := range s.commands[jobID] {
		if s.commands[jobID][i].CommandID == commandID && s.commands[jobID][i].Status == domain.DocumentCommandExecuting && s.commands[jobID][i].Generation == generation {
			s.commands[jobID][i].Status = status
			s.completed = append(s.completed, commandID+":"+string(status))
			return s.commands[jobID][i], nil
		}
	}
	return domain.DocumentCommand{}, domain.ErrDocumentFenceLost
}
func (s *documentManagerFakeStore) CompleteDocumentCommandWithArtifact(_ context.Context, cmd domain.CompleteDocumentArtifactCommand) (domain.DocumentCommand, domain.DocumentArtifact, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	job := s.jobs[cmd.JobID]
	if job.ClaimOwner != cmd.Owner || job.Generation != cmd.Generation || job.Status != domain.DocumentJobRunning {
		return domain.DocumentCommand{}, domain.DocumentArtifact{}, domain.ErrDocumentFenceLost
	}
	for i := range s.commands[cmd.JobID] {
		if s.commands[cmd.JobID][i].CommandID == cmd.CommandID && s.commands[cmd.JobID][i].Status == domain.DocumentCommandExecuting && s.commands[cmd.JobID][i].Generation == cmd.Generation {
			s.commands[cmd.JobID][i].Status = domain.DocumentCommandSucceeded
			artifact := domain.DocumentArtifact{JobID: cmd.JobID, CommandID: cmd.CommandID, FileName: cmd.FileName, MediaType: cmd.MediaType, Content: append([]byte(nil), cmd.Content...), SizeBytes: int64(len(cmd.Content))}
			s.artifacts = append(s.artifacts, artifact)
			s.completed = append(s.completed, cmd.CommandID+":"+string(domain.DocumentCommandSucceeded))
			return s.commands[cmd.JobID][i], artifact, nil
		}
	}
	return domain.DocumentCommand{}, domain.DocumentArtifact{}, domain.ErrDocumentFenceLost
}

func (s *documentManagerFakeStore) FinalizeDocumentJob(ctx context.Context, id, owner string, generation int64, status domain.DocumentJobStatus, code, message string) (domain.DocumentJob, error) {
	if err := ctx.Err(); err != nil {
		return domain.DocumentJob{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	job := s.jobs[id]
	if job.ClaimOwner != owner || job.Generation != generation || (job.Status != domain.DocumentJobRunning && job.Status != domain.DocumentJobStarting) {
		return domain.DocumentJob{}, domain.ErrDocumentFenceLost
	}
	if status == domain.DocumentJobSucceeded {
		if job.CommandsClosedAt == nil {
			return domain.DocumentJob{}, domain.ErrDocumentCommandsNotClosed
		}
		for _, command := range s.commands[id] {
			if command.Status == domain.DocumentCommandPending || command.Status == domain.DocumentCommandExecuting {
				return domain.DocumentJob{}, domain.ErrDocumentCommandsPending
			}
			if command.Status == domain.DocumentCommandFailed || command.Status == domain.DocumentCommandExpired {
				return domain.DocumentJob{}, domain.ErrDocumentCommandsFailed
			}
		}
		s.finalizeSuccess++
	}
	job.Status, job.TerminalErrorCode, job.TerminalErrorMessage = status, code, message
	job.ClaimOwner = ""
	s.jobs[id] = job
	for i := range s.instances {
		if s.instances[i].ID == job.InstanceID {
			s.instances[i].Status = domain.DocumentInstanceDestroying
		}
	}
	return job, nil
}
func (s *documentManagerFakeStore) GetDocumentJob(_ context.Context, id string) (domain.DocumentJob, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	job, ok := s.jobs[id]
	if !ok {
		return domain.DocumentJob{}, errors.New("not found")
	}
	return job, nil
}
func (s *documentManagerFakeStore) destroyedCount() int {
	count := 0
	for _, n := range s.destroyed {
		count += n
	}
	return count
}

var _ DocumentManagerStore = (*documentManagerFakeStore)(nil)
var _ DocumentManagerRuntime = (*documentManagerFakeRuntime)(nil)
