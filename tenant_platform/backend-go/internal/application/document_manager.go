package application

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
	documentinfra "github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/infrastructure/document"
)

const (
	documentCommandFailedCode   = "DOCUMENT_COMMAND_FAILED"
	maxDocumentArtifactEntries  = 256
	maxDocumentArtifactInflated = 32 * 1024 * 1024
)

type DocumentManagerStore interface {
	RecoverCreatingDocumentInstances(context.Context) (int, error)
	ReserveExcessReadyForDestroy(context.Context) (int, error)
	ListDestroyingDocumentInstances(context.Context) ([]domain.DocumentInstance, error)
	MarkDocumentInstanceDestroyed(context.Context, string) (domain.DocumentInstance, error)
	ReserveDocumentInstance(context.Context, string, string) (domain.DocumentInstance, bool, error)
	MarkDocumentInstanceReady(context.Context, string) (domain.DocumentInstance, error)
	MarkDocumentInstanceDestroying(context.Context, string) (domain.DocumentInstance, error)
	SweepExpiredDocumentWork(context.Context) (domain.DocumentSweepResult, error)
	ClaimNextDocumentJob(context.Context, string, time.Duration) (domain.DocumentClaim, bool, error)
	MarkDocumentJobAndInstanceRunning(context.Context, string, string, int64) (domain.DocumentJob, error)
	HeartbeatDocumentJob(context.Context, string, string, int64, time.Duration) error
	ClaimNextDocumentCommand(context.Context, string, string, int64) (domain.DocumentCommand, bool, error)
	CompleteDocumentCommand(context.Context, string, string, string, int64, domain.DocumentCommandStatus) (domain.DocumentCommand, error)
	CompleteDocumentCommandWithArtifact(context.Context, domain.CompleteDocumentArtifactCommand) (domain.DocumentCommand, domain.DocumentArtifact, error)
	FinalizeDocumentJob(context.Context, string, string, int64, domain.DocumentJobStatus, string, string) (domain.DocumentJob, error)
	GetDocumentJob(context.Context, string) (domain.DocumentJob, error)
}

type DocumentManagerRuntime interface {
	VerifyHost(context.Context) error
	CreateAndStart(context.Context, documentinfra.ContainerSpec) (documentinfra.Container, error)
	Exec(context.Context, string, []string) (documentinfra.CommandResult, error)
	ExecInput(context.Context, string, []string, []byte, int) (documentinfra.CommandResult, error)
	Destroy(context.Context, string) error
}

type OperationCompiler interface {
	Compile(domain.DocumentOperationRequest, string) (DocumentOperationPlan, error)
}

// EmptyOperationCompiler keeps the manager fail-closed until an explicit
// operation allowlist is wired by the process entry point.
type EmptyOperationCompiler struct{}

func (EmptyOperationCompiler) Compile(request domain.DocumentOperationRequest, _ string) (DocumentOperationPlan, error) {
	return DocumentOperationPlan{}, fmt.Errorf("unknown document operation %q", request.Operation)
}

// EmptyCompiler is retained as the concise name for callers that do not yet
// have an operation allowlist.
type EmptyCompiler = EmptyOperationCompiler

type ManagerConfig struct {
	Owner               string
	Store               DocumentManagerStore
	Runtime             DocumentManagerRuntime
	Compiler            OperationCompiler
	WorkRoot            string
	ClaimLease          time.Duration
	PollInterval        time.Duration
	HeartbeatInterval   time.Duration
	CommandPollInterval time.Duration
	ShutdownTimeout     time.Duration
}

type DocumentManager struct {
	cfg                   ManagerConfig
	workRoot              string
	cleanupMu             sync.Mutex
	pendingPrewarmCleanup map[string]domain.DocumentInstance
	activeMu              sync.Mutex
	active                map[string]context.CancelFunc
	activeWG              sync.WaitGroup
}

func NewDocumentManager(cfg ManagerConfig) (*DocumentManager, error) {
	cfg.Owner = strings.TrimSpace(cfg.Owner)
	if cfg.Owner == "" {
		return nil, fmt.Errorf("ManagerConfig.Owner is required")
	}
	if isNilPort(cfg.Store) {
		return nil, fmt.Errorf("ManagerConfig.Store is required")
	}
	if isNilPort(cfg.Runtime) {
		return nil, fmt.Errorf("ManagerConfig.Runtime is required")
	}
	if isNilPort(cfg.Compiler) {
		return nil, fmt.Errorf("ManagerConfig.Compiler is required")
	}
	if cfg.ClaimLease <= 0 {
		return nil, fmt.Errorf("ManagerConfig.ClaimLease must be positive")
	}
	if cfg.HeartbeatInterval <= 0 || cfg.ClaimLease <= cfg.HeartbeatInterval {
		return nil, fmt.Errorf("ManagerConfig.ClaimLease must be greater than HeartbeatInterval, and both must be positive")
	}
	if cfg.PollInterval <= 0 {
		return nil, fmt.Errorf("ManagerConfig.PollInterval must be positive")
	}
	if cfg.CommandPollInterval <= 0 {
		return nil, fmt.Errorf("ManagerConfig.CommandPollInterval must be positive")
	}
	if cfg.ShutdownTimeout <= 0 {
		return nil, fmt.Errorf("ManagerConfig.ShutdownTimeout must be positive")
	}
	workRoot, err := validateDocumentManagerWorkRoot(cfg.WorkRoot)
	if err != nil {
		return nil, err
	}
	return &DocumentManager{
		cfg: cfg, workRoot: workRoot,
		pendingPrewarmCleanup: make(map[string]domain.DocumentInstance),
		active:                make(map[string]context.CancelFunc),
	}, nil
}

func isNilPort(port any) bool {
	if port == nil {
		return true
	}
	value := reflect.ValueOf(port)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}

func validateDocumentManagerWorkRoot(root string) (string, error) {
	if !filepath.IsAbs(root) || filepath.Clean(root) != root {
		return "", fmt.Errorf("ManagerConfig.WorkRoot must be an absolute clean path")
	}
	info, err := os.Lstat(root)
	if err != nil {
		return "", fmt.Errorf("stat document manager work root: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return "", fmt.Errorf("ManagerConfig.WorkRoot must be a real directory, not a symlink")
	}
	canonical, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", fmt.Errorf("canonicalize document manager work root: %w", err)
	}
	canonical = filepath.Clean(canonical)
	if canonical != root {
		return "", fmt.Errorf("ManagerConfig.WorkRoot must already be canonical")
	}
	return canonical, nil
}

func (m *DocumentManager) Run(ctx context.Context) error {
	if err := m.cfg.Runtime.VerifyHost(ctx); err != nil {
		return fmt.Errorf("verify document runtime host: %w", err)
	}
	if _, err := m.cfg.Store.RecoverCreatingDocumentInstances(ctx); err != nil {
		return fmt.Errorf("recover creating document instances: %w", err)
	}

	m.runTick(ctx)
	ticker := time.NewTicker(m.cfg.PollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			m.shutdown()
			return nil
		case <-ticker.C:
			m.runTick(ctx)
		}
	}
}

func (m *DocumentManager) runTick(ctx context.Context) {
	if err := m.retryPrewarmCleanup(ctx); err != nil {
		slog.ErrorContext(ctx, "document manager: retry prewarm cleanup intent", "error", err)
		return
	}
	if _, err := m.cfg.Store.SweepExpiredDocumentWork(ctx); err != nil {
		slog.ErrorContext(ctx, "document manager: sweep expired work", "error", err)
		return
	}
	if _, err := m.cfg.Store.ReserveExcessReadyForDestroy(ctx); err != nil {
		slog.ErrorContext(ctx, "document manager: reserve excess ready instances", "error", err)
		return
	}
	if err := m.cleanupDestroying(ctx); err != nil {
		slog.ErrorContext(ctx, "document manager: list destroying instances", "error", err)
		return
	}
	if err := m.reconcilePrewarm(ctx); err != nil {
		slog.ErrorContext(ctx, "document manager: reconcile prewarm", "error", err)
		return
	}
	m.claimJobs(ctx)
}

func (m *DocumentManager) reconcilePrewarm(ctx context.Context) error {
	for {
		name := "ga-document-" + uuid.NewString()
		slotPath := filepath.Join(m.workRoot, name)
		instance, reserved, err := m.cfg.Store.ReserveDocumentInstance(ctx, name, slotPath)
		if err != nil {
			return fmt.Errorf("reserve document instance: %w", err)
		}
		if !reserved {
			return nil
		}
		if err := os.Mkdir(slotPath, 0o700); err != nil {
			return m.failPrewarm(ctx, instance, fmt.Errorf("create document slot %s: %w", name, err))
		}
		if _, err := m.cfg.Runtime.CreateAndStart(ctx, documentinfra.ContainerSpec{Name: name, SlotPath: slotPath}); err != nil {
			return m.failPrewarm(ctx, instance, fmt.Errorf("create document instance %s: %w", name, err))
		}
		if _, err := m.cfg.Store.MarkDocumentInstanceReady(ctx, instance.ID); err != nil {
			return m.failPrewarm(ctx, instance, fmt.Errorf("mark document instance %s ready: %w", name, err))
		}
	}
}

func (m *DocumentManager) failPrewarm(ctx context.Context, instance domain.DocumentInstance, cause error) error {
	if _, err := m.cfg.Store.MarkDocumentInstanceDestroying(ctx, instance.ID); err != nil {
		m.pendingPrewarmCleanup[instance.ID] = instance
		return errors.Join(cause, fmt.Errorf("reserve failed document instance %s for cleanup: %w", instance.InstanceName, err))
	}
	delete(m.pendingPrewarmCleanup, instance.ID)
	if err := m.cleanupDestroying(ctx); err != nil {
		return errors.Join(cause, fmt.Errorf("cleanup failed document instance %s: %w", instance.InstanceName, err))
	}
	return cause
}

func (m *DocumentManager) retryPrewarmCleanup(ctx context.Context) error {
	var retryErr error
	for id, instance := range m.pendingPrewarmCleanup {
		if _, err := m.cfg.Store.MarkDocumentInstanceDestroying(ctx, id); err != nil {
			retryErr = errors.Join(retryErr, fmt.Errorf("reserve failed document instance %s for cleanup: %w", instance.InstanceName, err))
			continue
		}
		delete(m.pendingPrewarmCleanup, id)
	}
	return retryErr
}

func (m *DocumentManager) cleanupDestroying(ctx context.Context) error {
	m.cleanupMu.Lock()
	defer m.cleanupMu.Unlock()

	instances, err := m.cfg.Store.ListDestroyingDocumentInstances(ctx)
	if err != nil {
		return err
	}
	for _, instance := range instances {
		if !m.ownsSlot(instance) {
			slog.ErrorContext(ctx, "document manager: refusing invalid destroying slot", "instance", instance.InstanceName, "slot", instance.SlotPath)
			continue
		}
		if err := m.cfg.Runtime.Destroy(ctx, instance.InstanceName); err != nil {
			slog.WarnContext(ctx, "document manager: destroy instance will retry", "instance", instance.InstanceName, "error", err)
			continue
		}
		if err := os.RemoveAll(instance.SlotPath); err != nil {
			slog.WarnContext(ctx, "document manager: remove slot will retry", "instance", instance.InstanceName, "error", err)
			continue
		}
		if _, err := m.cfg.Store.MarkDocumentInstanceDestroyed(ctx, instance.ID); err != nil {
			slog.WarnContext(ctx, "document manager: mark instance destroyed will retry", "instance", instance.InstanceName, "error", err)
		}
	}
	return nil
}

func (m *DocumentManager) ownsSlot(instance domain.DocumentInstance) bool {
	if strings.TrimSpace(instance.InstanceName) == "" {
		return false
	}
	want := filepath.Join(m.workRoot, instance.InstanceName)
	return filepath.Clean(instance.SlotPath) == instance.SlotPath && instance.SlotPath == want && filepath.Dir(instance.SlotPath) == m.workRoot
}

func (m *DocumentManager) claimJobs(ctx context.Context) {
	for {
		claim, claimed, err := m.cfg.Store.ClaimNextDocumentJob(ctx, m.cfg.Owner, m.cfg.ClaimLease)
		if err != nil {
			slog.ErrorContext(ctx, "document manager: claim job", "error", err)
			return
		}
		if !claimed {
			return
		}
		jobCtx, cancel := context.WithCancel(ctx)
		if !m.addActive(claim.Job.ID, cancel) {
			cancel()
			continue
		}
		m.activeWG.Add(1)
		go m.runJob(jobCtx, claim)
	}
}

func (m *DocumentManager) addActive(jobID string, cancel context.CancelFunc) bool {
	m.activeMu.Lock()
	defer m.activeMu.Unlock()
	if _, exists := m.active[jobID]; exists {
		return false
	}
	m.active[jobID] = cancel
	return true
}

func (m *DocumentManager) runJob(ctx context.Context, claim domain.DocumentClaim) {
	defer func() {
		m.activeMu.Lock()
		delete(m.active, claim.Job.ID)
		m.activeMu.Unlock()
		m.activeWG.Done()
	}()

	job, err := m.cfg.Store.MarkDocumentJobAndInstanceRunning(ctx, claim.Job.ID, m.cfg.Owner, claim.Job.Generation)
	if err != nil {
		return
	}
	heartbeat := m.startHeartbeat(ctx, job)
	defer heartbeat.cancel()
	ctx = heartbeat.ctx
	for {
		command, claimed, err := m.cfg.Store.ClaimNextDocumentCommand(ctx, job.ID, m.cfg.Owner, job.Generation)
		if err != nil {
			heartbeat.stop()
			return
		}
		if claimed {
			if err := m.executeCommand(ctx, claim.Instance.InstanceName, job, command); err != nil {
				if heartbeat.stop() != nil || errors.Is(err, domain.ErrDocumentFenceLost) || errors.Is(err, context.Canceled) {
					return
				}
				_, _ = m.cfg.Store.FinalizeDocumentJob(ctx, job.ID, m.cfg.Owner, job.Generation, domain.DocumentJobFailed, documentCommandFailedCode, err.Error())
				_ = m.cleanupDestroying(ctx)
				return
			}
			continue
		}

		current, err := m.cfg.Store.GetDocumentJob(ctx, job.ID)
		if err != nil {
			heartbeat.stop()
			return
		}
		if current.CommandsClosedAt == nil {
			if !waitDocumentManagerInterval(ctx, m.cfg.CommandPollInterval) {
				heartbeat.stop()
				return
			}
			continue
		}

		_, err = m.cfg.Store.FinalizeDocumentJob(ctx, job.ID, m.cfg.Owner, job.Generation, domain.DocumentJobSucceeded, "", "")
		if err != nil {
			if errors.Is(err, domain.ErrDocumentCommandsFailed) {
				_, _ = m.cfg.Store.FinalizeDocumentJob(ctx, job.ID, m.cfg.Owner, job.Generation, domain.DocumentJobFailed, documentCommandFailedCode, "document job contains a failed or expired command")
				heartbeat.stop()
				_ = m.cleanupDestroying(ctx)
				return
			}
			if errors.Is(err, domain.ErrDocumentCommandsNotClosed) || errors.Is(err, domain.ErrDocumentCommandsPending) {
				if !waitDocumentManagerInterval(ctx, m.cfg.CommandPollInterval) {
					heartbeat.stop()
					return
				}
				continue
			}
			heartbeat.stop()
			return
		}
		heartbeat.stop()
		_ = m.cleanupDestroying(ctx)
		return
	}
}

func (m *DocumentManager) executeCommand(ctx context.Context, instanceName string, job domain.DocumentJob, command domain.DocumentCommand) error {
	request, err := decodeDocumentOperation(command.Payload)
	if err == nil {
		var plan DocumentOperationPlan
		plan, err = m.cfg.Compiler.Compile(request, command.CommandID)
		if err == nil {
			err = validateDocumentOperationPlan(plan)
		}
		if err == nil {
			if plan.Artifact == nil {
				var result documentinfra.CommandResult
				result, err = m.cfg.Runtime.Exec(ctx, instanceName, plan.Argv)
				if err == nil && result.ExitCode != 0 {
					err = fmt.Errorf("document operation exited %d", result.ExitCode)
				}
			} else {
				var result documentinfra.CommandResult
				result, err = m.cfg.Runtime.ExecInput(ctx, instanceName, plan.Argv, plan.Stdin, domain.MaxDocumentArtifactBytes)
				if err == nil && result.ExitCode != 0 {
					err = fmt.Errorf("document operation exited %d", result.ExitCode)
				}
				if err == nil && len(result.Stdout) == 0 {
					err = fmt.Errorf("document operation returned an empty artifact")
				}
				if err == nil {
					err = validateDOCXArtifact(result.Stdout)
				}
				if err == nil {
					_, _, err = m.cfg.Store.CompleteDocumentCommandWithArtifact(ctx, domain.CompleteDocumentArtifactCommand{
						JobID: job.ID, CommandID: command.CommandID, Owner: m.cfg.Owner, Generation: job.Generation,
						FileName: plan.Artifact.FileName, MediaType: plan.Artifact.MediaType, Content: result.Stdout,
					})
					return err
				}
			}
		}
	}
	if err != nil {
		_, completionErr := m.cfg.Store.CompleteDocumentCommand(ctx, job.ID, command.CommandID, m.cfg.Owner, job.Generation, domain.DocumentCommandFailed)
		if completionErr != nil {
			return errors.Join(err, completionErr)
		}
		return err
	}
	_, err = m.cfg.Store.CompleteDocumentCommand(ctx, job.ID, command.CommandID, m.cfg.Owner, job.Generation, domain.DocumentCommandSucceeded)
	return err
}

func validateDOCXArtifact(content []byte) error {
	reader, err := zip.NewReader(bytes.NewReader(content), int64(len(content)))
	if err != nil {
		return fmt.Errorf("document artifact is not a valid DOCX archive: %w", err)
	}
	if len(reader.File) == 0 || len(reader.File) > maxDocumentArtifactEntries {
		return fmt.Errorf("document artifact has an invalid entry count")
	}
	required := map[string]bool{
		"[Content_Types].xml": false,
		"_rels/.rels":         false,
		"word/document.xml":   false,
	}
	seen := make(map[string]struct{}, len(reader.File))
	var inflated uint64
	for _, file := range reader.File {
		name := file.Name
		if name == "" || strings.HasPrefix(name, "/") || path.Clean(name) != name || name == ".." || strings.HasPrefix(name, "../") || strings.Contains(name, "\\") {
			return fmt.Errorf("document artifact contains an unsafe entry name")
		}
		if _, exists := seen[name]; exists {
			return fmt.Errorf("document artifact contains duplicate entries")
		}
		seen[name] = struct{}{}
		if file.UncompressedSize64 > uint64(maxDocumentArtifactInflated)-inflated {
			return fmt.Errorf("document artifact uncompressed content exceeds limit")
		}
		inflated += file.UncompressedSize64
		handle, err := file.Open()
		if err != nil {
			return fmt.Errorf("open document artifact entry %s: %w", name, err)
		}
		_, readErr := io.Copy(io.Discard, io.LimitReader(handle, int64(file.UncompressedSize64)+1))
		closeErr := handle.Close()
		if readErr != nil {
			return fmt.Errorf("read document artifact entry %s: %w", name, readErr)
		}
		if closeErr != nil {
			return fmt.Errorf("close document artifact entry %s: %w", name, closeErr)
		}
		if _, ok := required[name]; ok {
			required[name] = true
		}
	}
	for name, present := range required {
		if !present {
			return fmt.Errorf("document artifact is missing %s", name)
		}
	}
	return nil
}

func validateDocumentOperationPlan(plan DocumentOperationPlan) error {
	if !isDocumentExportDOCXArgv(plan.Argv) {
		return fmt.Errorf("document operation compiler returned a non-allowlisted invocation")
	}
	if plan.Artifact == nil {
		if len(plan.Stdin) != 0 {
			return fmt.Errorf("document operation without artifact must not provide stdin")
		}
		return nil
	}
	if len(plan.Stdin) == 0 {
		return fmt.Errorf("document artifact operation requires stdin")
	}
	if err := domain.ValidateDocumentArtifactMetadata(plan.Artifact.FileName, plan.Artifact.MediaType); err != nil {
		return err
	}
	return nil
}

func decodeDocumentOperation(payload json.RawMessage) (domain.DocumentOperationRequest, error) {
	var request domain.DocumentOperationRequest
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		return domain.DocumentOperationRequest{}, fmt.Errorf("decode document operation: %w", err)
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return domain.DocumentOperationRequest{}, err
	}
	if request.SchemaVersion != 1 {
		return domain.DocumentOperationRequest{}, fmt.Errorf("document operation schema_version must be 1")
	}
	if strings.TrimSpace(request.Operation) == "" {
		return domain.DocumentOperationRequest{}, fmt.Errorf("document operation is required")
	}
	var parameters map[string]json.RawMessage
	if len(request.Parameters) == 0 || json.Unmarshal(request.Parameters, &parameters) != nil || parameters == nil {
		return domain.DocumentOperationRequest{}, fmt.Errorf("document operation parameters must be a JSON object")
	}
	return request, nil
}

func ensureJSONEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return fmt.Errorf("document operation payload must contain one JSON value")
		}
		return fmt.Errorf("decode trailing document operation payload: %w", err)
	}
	return nil
}

type documentJobHeartbeat struct {
	ctx      context.Context
	cancel   context.CancelFunc
	stopOnce sync.Once
	stopCh   chan struct{}
	done     chan struct{}
	mu       sync.Mutex
	err      error
}

func (m *DocumentManager) startHeartbeat(parent context.Context, job domain.DocumentJob) *documentJobHeartbeat {
	ctx, cancel := context.WithCancel(parent)
	heartbeat := &documentJobHeartbeat{ctx: ctx, cancel: cancel, stopCh: make(chan struct{}), done: make(chan struct{})}
	go func() {
		defer close(heartbeat.done)
		ticker := time.NewTicker(m.cfg.HeartbeatInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-heartbeat.stopCh:
				return
			case <-ticker.C:
				if err := m.cfg.Store.HeartbeatDocumentJob(ctx, job.ID, m.cfg.Owner, job.Generation, m.cfg.ClaimLease); err != nil {
					slog.WarnContext(ctx, "document manager: heartbeat failed", "job_id", job.ID, "generation", job.Generation, "error", err)
					heartbeat.mu.Lock()
					heartbeat.err = err
					heartbeat.mu.Unlock()
					cancel()
					return
				}
			}
		}
	}()
	return heartbeat
}

func (h *documentJobHeartbeat) stop() error {
	h.stopOnce.Do(func() { close(h.stopCh) })
	<-h.done
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.err
}

func waitDocumentManagerInterval(ctx context.Context, interval time.Duration) bool {
	timer := time.NewTimer(interval)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func (m *DocumentManager) shutdown() {
	m.activeMu.Lock()
	for _, cancel := range m.active {
		cancel()
	}
	m.activeMu.Unlock()

	done := make(chan struct{})
	go func() {
		m.activeWG.Wait()
		close(done)
	}()
	timer := time.NewTimer(m.cfg.ShutdownTimeout)
	select {
	case <-done:
		if !timer.Stop() {
			<-timer.C
		}
	case <-timer.C:
	}

	cleanupCtx, cancel := context.WithTimeout(context.Background(), m.cfg.ShutdownTimeout)
	defer cancel()
	_ = m.cleanupDestroying(cleanupCtx)
}
