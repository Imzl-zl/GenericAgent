package application

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/infrastructure/llmproxy"
	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/infrastructure/worker"
	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/infrastructure/workerclient"
)

type fakeLLMProviderSource struct {
	providers []domain.LLMProvider
	err       error
}

func (f *fakeLLMProviderSource) ListActiveProviders(context.Context) ([]domain.LLMProvider, error) {
	if f.err != nil {
		return nil, f.err
	}
	return append([]domain.LLMProvider(nil), f.providers...), nil
}

func (f *fakeLLMProviderSource) GetProvider(_ context.Context, id int64) (domain.LLMProvider, error) {
	if f.err != nil {
		return domain.LLMProvider{}, f.err
	}
	for _, provider := range f.providers {
		if provider.ID == id {
			return provider, nil
		}
	}
	return domain.LLMProvider{}, errors.New("provider not found")
}

type revokedCapability struct {
	jti       string
	expiresAt time.Time
}

type routingCapabilityStore struct {
	revoked       []revokedCapability
	err           error
	beforeRevoke  func()
	cleanupBefore []time.Time
}

func (s *routingCapabilityStore) RevokeCapability(_ context.Context, jti string, expiresAt time.Time) error {
	if s.beforeRevoke != nil {
		s.beforeRevoke()
	}
	if s.err != nil {
		return s.err
	}
	s.revoked = append(s.revoked, revokedCapability{jti: jti, expiresAt: expiresAt})
	return nil
}

func (s *routingCapabilityStore) DeleteExpiredCapabilityRevocations(
	_ context.Context,
	before time.Time,
) (int64, error) {
	if s.err != nil {
		return 0, s.err
	}
	s.cleanupBefore = append(s.cleanupBefore, before)
	return 2, nil
}

func TestCleanupExpiredCapabilityRevocationsWhenDue(t *testing.T) {
	store := &routingCapabilityStore{}
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	s := &scheduler{cfg: SchedulerConfig{
		CapabilityStore: store, RevocationCleanupInterval: time.Minute,
	}}

	if err := s.cleanupExpiredCapabilityRevocations(context.Background(), now); err != nil {
		t.Fatal(err)
	}
	if len(store.cleanupBefore) != 1 || !store.cleanupBefore[0].Equal(now) {
		t.Fatalf("cleanup calls = %v", store.cleanupBefore)
	}
	if err := s.cleanupExpiredCapabilityRevocations(context.Background(), now.Add(30*time.Second)); err != nil {
		t.Fatal(err)
	}
	if len(store.cleanupBefore) != 1 {
		t.Fatalf("cleanup ran before interval: %v", store.cleanupBefore)
	}
	if err := s.cleanupExpiredCapabilityRevocations(context.Background(), now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if len(store.cleanupBefore) != 2 {
		t.Fatalf("cleanup calls = %v, want 2", store.cleanupBefore)
	}
}

type routingAuditRecorder struct {
	events []domain.AuditEvent
}

func (r *routingAuditRecorder) AppendAuditEvent(_ context.Context, event domain.AuditEvent) error {
	r.events = append(r.events, event)
	return nil
}

func testProvider(id, revision int64, providerType domain.LLMProviderType, isDefault bool) domain.LLMProvider {
	return domain.LLMProvider{
		ID: id, Revision: revision, ProviderType: providerType,
		Name: "provider", Model: "model", IsDefault: isDefault, State: "active",
	}
}

func TestWriteTokenOnlyRuntimeConfigContainsTokenAndProxyNotRealKey(t *testing.T) {
	dir := t.TempDir()
	const (
		proxyAddr = "http://127.0.0.1:8081"
		realKey   = "sk-real-upstream-key-must-not-leak-1234567890"
		token     = "capability-token-only"
	)
	provider := domain.LLMProvider{
		ID: 1, Revision: 1, ProviderType: domain.ProviderNativeOAI,
		Model: "gpt-test", APIKey: realKey,
	}
	files, err := BuildRuntimeConfig(RuntimeConfigInput{
		ProxyBaseURL: proxyAddr, RoutingSnapshotID: "snapshot",
		Providers: []RuntimeProviderBinding{{Provider: provider, Token: token}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteRuntimeConfigAtomic(dir, files); err != nil {
		t.Fatal(err)
	}
	runtimeJSON, err := os.ReadFile(filepath.Join(dir, runtimeConfigFilename))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(runtimeJSON), token) || !strings.Contains(string(runtimeJSON), proxyAddr) {
		t.Fatalf("runtime JSON missing token or Proxy: %s", runtimeJSON)
	}
	if strings.Contains(string(runtimeJSON), realKey) {
		t.Fatal("real upstream key leaked into runtime JSON")
	}
	loader, err := os.ReadFile(filepath.Join(dir, myKeyLoaderFilename))
	if err != nil {
		t.Fatal(err)
	}
	if string(loader) != MyKeyLoader || strings.Contains(string(loader), token) {
		t.Fatalf("mykey.py is not the fixed token-free loader: %s", loader)
	}
}

func TestWriteRuntimeConfigOverwritesExistingMyKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, myKeyLoaderFilename)
	if err := os.WriteFile(path, []byte("native_oai_config = {'apikey': 'sk-real-user-key'}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	provider := domain.LLMProvider{ID: 1, Revision: 1, ProviderType: domain.ProviderNativeOAI, Model: "gpt-test"}
	files, err := BuildRuntimeConfig(RuntimeConfigInput{
		ProxyBaseURL: "http://127.0.0.1:8081", RoutingSnapshotID: "snapshot",
		Providers: []RuntimeProviderBinding{{Provider: provider, Token: "cap-token-overwrite"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteRuntimeConfigAtomic(dir, files); err != nil {
		t.Fatal(err)
	}
	loader, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(loader) != MyKeyLoader || strings.Contains(string(loader), "sk-real-user-key") {
		t.Fatalf("unsafe mykey.py survived: %s", loader)
	}
}

func TestRoutingSnapshotFollowsDefaultSwitch(t *testing.T) {
	defaultProvider := testProvider(2, 4, domain.ProviderNativeClaude, true)
	stream := true
	defaultProvider.SessionConfig.Stream = &stream
	secondary := testProvider(1, 7, domain.ProviderNativeClaude, false)
	source := &fakeLLMProviderSource{providers: []domain.LLMProvider{defaultProvider, secondary}}
	s := &scheduler{cfg: SchedulerConfig{LLMProvider: source}}

	snapshot, err := s.resolveRoutingSnapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Providers) != 2 || snapshot.Providers[0].ID != defaultProvider.ID {
		t.Fatalf("providers=%+v", snapshot.Providers)
	}
	if snapshot.ID == "" || snapshot.Providers[0].RuntimeName == snapshot.Providers[1].RuntimeName {
		t.Fatalf("invalid snapshot=%+v", snapshot)
	}

	// 决策 D2.1(任务即进程): 每次任务都重新解析快照——默认切换后新任务
	// 跟随新默认(默认 provider 恒第一); provider 集/内容不变时其余顺序稳定。
	source.providers = []domain.LLMProvider{secondary, defaultProvider}
	source.providers[0].IsDefault = true
	source.providers[1].IsDefault = false
	sameStream := true
	source.providers[1].SessionConfig.Stream = &sameStream
	afterSwitch, err := s.resolveRoutingSnapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if afterSwitch.Providers[0].ID != secondary.ID {
		t.Fatal("default switch must move the new default first for fresh workers")
	}
	if afterSwitch.ID == snapshot.ID {
		t.Fatal("default switch must change the snapshot for fresh workers")
	}

	// 新 provider 加入必须改变 snapshot。
	newFallback := testProvider(3, 1, domain.ProviderNativeOAI, false)
	source.providers = append(source.providers, newFallback)
	withNew, err := s.resolveRoutingSnapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if withNew.ID == afterSwitch.ID || len(withNew.Providers) != 3 {
		t.Fatal("new active provider must change the snapshot")
	}
	source.providers = source.providers[:2]

	// 绑定 provider 修订变化必须改变 snapshot。
	source.providers[0].Revision++
	withRev, err := s.resolveRoutingSnapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if withRev.ID == afterSwitch.ID {
		t.Fatal("bound provider revision change must change the snapshot")
	}
}

func TestIssueProviderCapabilitiesBuildsDefaultFirstMixin(t *testing.T) {
	defaultProvider := testProvider(2, 3, domain.ProviderNativeClaude, true)
	defaultProvider.APIKey = "real-key-must-not-appear"
	secondary := testProvider(1, 5, domain.ProviderNativeOAI, false)
	source := &fakeLLMProviderSource{providers: []domain.LLMProvider{defaultProvider, secondary}}
	issuer, err := llmproxy.NewIssuer([]byte("test-signing-key-at-least-32-bytes"), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	s := &scheduler{cfg: SchedulerConfig{
		LLMProvider: source, TokenIssuer: issuer, LLMProxyAddr: "http://127.0.0.1:9999",
		ConfigRoot: t.TempDir(),
		ModelPolicyVersion: "test.v1", MaxTaskWallClock: 45 * time.Minute,
		TokenRefreshSkew: 5 * time.Minute,
	}}
	task := domain.Task{ID: "task-mixin", SessionKey: "personal:1"}
	set, files, err := s.issueInitialWorkerCredentials(context.Background(), task, 1)
	if err != nil {
		t.Fatal(err)
	}
	// round11 I4: 2 个 LLM capability + 1 个独立 control capability。
	if len(set.JTIs) != 3 {
		t.Fatalf("credential set=%+v", set)
	}
	if set.ControlJTI == "" {
		t.Fatal("control capability JTI must be issued")
	}
	if strings.Contains(string(files.JSON), defaultProvider.APIKey) {
		t.Fatal("upstream API key leaked into Worker runtime JSON")
	}
	var document map[string]any
	if err := json.Unmarshal(files.JSON, &document); err != nil {
		t.Fatal(err)
	}
	mixin, ok := document["mixin_config"].(map[string]any)
	if !ok {
		t.Fatalf("mixin_config=%#v", document["mixin_config"])
	}
	names, ok := mixin["llm_nos"].([]any)
	if !ok || len(names) != 2 || names[0] != runtimeProviderName(2) || names[1] != runtimeProviderName(1) {
		t.Fatalf("llm_nos=%#v", mixin["llm_nos"])
	}
}

func TestIssueProviderCapabilitiesAcceptsExactLifetimeCoverage(t *testing.T) {
	provider := testProvider(1, 1, domain.ProviderNativeOAI, true)
	source := &fakeLLMProviderSource{providers: []domain.LLMProvider{provider}}
	const taskWallClock = 45 * time.Minute
	const refreshSkew = 5 * time.Minute
	issuer, err := llmproxy.NewIssuer([]byte("test-signing-key-at-least-32-bytes"), taskWallClock+refreshSkew)
	if err != nil {
		t.Fatal(err)
	}
	if issuer.TTL() != taskWallClock+refreshSkew {
		t.Fatalf("issuer TTL=%s", issuer.TTL())
	}
	s := &scheduler{cfg: SchedulerConfig{
		LLMProvider: source, TokenIssuer: issuer, LLMProxyAddr: "http://127.0.0.1:9999",
		ConfigRoot: t.TempDir(),
		ModelPolicyVersion: "test.v1", MaxTaskWallClock: taskWallClock, TokenRefreshSkew: refreshSkew,
	}}
	task := domain.Task{ID: "task-lifetime", SessionKey: "personal:1"}
	if _, _, err := s.issueInitialWorkerCredentials(context.Background(), task, 1); err != nil {
		t.Fatal(err)
	}
}

func TestCreateTaskWorkerAlwaysStartsFreshWorker(t *testing.T) {
	provider := testProvider(1, 1, domain.ProviderNativeOAI, true)
	source := &fakeLLMProviderSource{providers: []domain.LLMProvider{provider}}
	issuer, err := llmproxy.NewIssuer([]byte("test-signing-key-at-least-32-bytes"), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	var started []*controlledWorker
	cleanupCalls := 0
	runtime := worker.NewStaticRuntime(func(context.Context, string) (workerclient.WorkerClient, func(string), error) {
		client := newControlledWorker()
		started = append(started, client)
		return client, func(_ string) { cleanupCalls++ }, nil
	})
	capabilities := &routingCapabilityStore{}
	s := &scheduler{
		cfg: SchedulerConfig{
			Runtime: runtime, ConfigRoot: t.TempDir(), LLMProxyAddr: "http://127.0.0.1:9999",
			TokenIssuer: issuer, CapabilityStore: capabilities, LLMProvider: source,
			ModelPolicyVersion: "test.v1", TokenTTL: time.Hour,
			TokenRefreshSkew: 5 * time.Minute, MaxTaskWallClock: 45 * time.Minute,
		},
		workers: make(map[string]*workerEntry),
	}
	taskA := domain.Task{ID: "task-A", SessionKey: "personal:1"}
	_, first, err := s.createTaskWorker(context.Background(), taskA)
	if err != nil {
		t.Fatal(err)
	}
	if len(started) != 1 {
		t.Fatalf("starts=%d want 1", len(started))
	}
	// 决策 D2.1: 任务终态销毁 Worker。
	s.destroyTaskWorker(taskA.SessionKey)
	if cleanupCalls != 1 {
		t.Fatalf("cleanup=%d want 1", cleanupCalls)
	}
	// 下一任务冷启动全新进程, 且每次解析最新路由快照。
	source.providers[0].Revision = 2
	taskB := domain.Task{ID: "task-B", SessionKey: "personal:1"}
	_, second, err := s.createTaskWorker(context.Background(), taskB)
	if err != nil {
		t.Fatal(err)
	}
	if second == first || len(started) != 2 {
		t.Fatalf("fresh worker expected: same=%t starts=%d", second == first, len(started))
	}
	if second.credentials.Snapshot.Providers[0].Revision != 2 {
		t.Fatalf("fresh snapshot=%+v", second.credentials.Snapshot)
	}
}

func TestRoutingAuditContainsMetadataWithoutCredentialMaterial(t *testing.T) {
	recorder := &routingAuditRecorder{}
	s := &scheduler{cfg: SchedulerConfig{Audit: recorder, ModelPolicyVersion: "policy.v1"}}
	entry := &workerEntry{credentials: workerCredentialSet{
		JTIs: []string{"sensitive-jti-must-not-appear"},
		Snapshot: routingSnapshot{ID: "sha256:snapshot", Providers: []routingProvider{{
			ID: 42, Revision: 7, ProviderType: domain.ProviderNativeOAI, Model: "model",
		}}},
	}}
	task := domain.Task{ID: "task-1", RequesterID: 9, SessionKey: "personal:9"}
	s.auditRoutingBinding(context.Background(), task, entry, "success", "")
	if len(recorder.events) != 1 {
		t.Fatalf("events=%d", len(recorder.events))
	}
	detail := string(recorder.events[0].Detail)
	for _, secret := range []string{"sensitive-jti-must-not-appear", "capability_token", "apikey"} {
		if strings.Contains(detail, secret) {
			t.Fatalf("audit leaked %q: %s", secret, detail)
		}
	}
	if !strings.Contains(detail, `"provider_ids":[42]`) ||
		!strings.Contains(detail, `"routing_snapshot_id":"sha256:snapshot"`) {
		t.Fatalf("audit metadata incomplete: %s", detail)
	}
}

// TestIssueInitialWorkerCredentialsBindsRunnerGeneration 验证初始签发绑定
// Runner lease generation(方案 §7), 决策 D1 后无独立 credential generation。
func TestIssueInitialWorkerCredentialsBindsRunnerGeneration(t *testing.T) {
	provider := testProvider(1, 1, domain.ProviderNativeOAI, true)
	source := &fakeLLMProviderSource{providers: []domain.LLMProvider{provider}}
	issuer, err := llmproxy.NewIssuer([]byte("test-signing-key-at-least-32-bytes"), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	task := domain.Task{ID: "task-9", SessionKey: "personal:9", ToolPolicyVersion: "foundation.no-host-tools.v1"}
	s := &scheduler{cfg: SchedulerConfig{
		TokenIssuer: issuer, LLMProvider: source,
		LLMProxyAddr: "http://127.0.0.1:9999", ConfigRoot: t.TempDir(),
		ModelPolicyVersion: "test.v1",
		TokenTTL:           time.Hour, TokenRefreshSkew: 5 * time.Minute, MaxTaskWallClock: 45 * time.Minute,
	}}
	set, _, err := s.issueInitialWorkerCredentials(context.Background(), task, 3)
	if err != nil {
		t.Fatal(err)
	}
	// 决策 D1: 凭证 generation 已删除, 只保留 Runner lease generation 绑定。
	if set.RunnerGeneration != 3 {
		t.Fatalf("set = %+v, want RunnerGeneration=3", set)
	}
}

// jtiPersistingStore 是最小 TaskStore fake: 只记录 SetTaskCapabilityJTIs 调用
// (其余方法 panic, 测试只走签发路径)。
type jtiPersistingStore struct {
	calls []string // taskID -> JTIs
}

func (f *jtiPersistingStore) SetTaskCapabilityJTIs(_ context.Context, taskID, _ string, jtis []string) error {
	f.calls = append(f.calls, fmt.Sprintf("%s:%d", taskID, len(jtis)))
	return nil
}

func (f *jtiPersistingStore) RequeueTask(ctx context.Context, taskID, platformInstanceID string) error {
	panic("unexpected")
}

func (f *jtiPersistingStore) SubmitTaskWithInboundMessage(ctx context.Context, cmd domain.SubmitTaskCommand, msg domain.Message) (domain.Task, domain.Message, error) {
	t, err := f.SubmitTask(ctx, cmd)
	return t, domain.Message{}, err
}

func (f *jtiPersistingStore) SubmitTask(ctx context.Context, cmd domain.SubmitTaskCommand) (domain.Task, error) {
	panic("unexpected")
}


func (s *jtiPersistingStore) IsApprovedUser(_ context.Context, _ int64) (bool, error) {
	return true, nil
}
func (f *jtiPersistingStore) IsApprovedTeamMember(_ context.Context, _ string, _ int64) (bool, error) {
	return true, nil
}

func (f *jtiPersistingStore) GetTask(ctx context.Context, taskID string) (domain.Task, error) {
	panic("unexpected")
}
func (f *jtiPersistingStore) CancelTask(ctx context.Context, taskID string, requesterUserID int64) (domain.Task, bool, error) {
	panic("unexpected")
}
func (f *jtiPersistingStore) ClaimNextTask(ctx context.Context, sessionKey, platformInstanceID string, claimLease time.Duration) (domain.Task, bool, error) {
	panic("unexpected")
}
func (f *jtiPersistingStore) RecoverAfterRestart(ctx context.Context, platformInstanceID string) (int, error) {
	panic("unexpected")
}
func (f *jtiPersistingStore) CompleteSucceeded(ctx context.Context, taskID, platformInstanceID, snapshotID, fileRef, checksum, resultRef, resultDigest string, resultBytes int, deliveryFiles []domain.DeliveryFile) (domain.Task, error) {
	panic("unexpected")
}
func (f *jtiPersistingStore) CompleteFailedTerminal(ctx context.Context, taskID, owner string, status domain.TaskStatus, deliveryType domain.DeliveryType, code, message, traceID string) (domain.Task, error) {
	panic("unexpected")
}
func (f *jtiPersistingStore) ListOwnedActiveTasks(ctx context.Context, platformInstanceID string) ([]domain.Task, error) {
	panic("unexpected")
}
func (f *jtiPersistingStore) HeartbeatClaim(ctx context.Context, taskID, platformInstanceID string, claimLease time.Duration) error {
	panic("unexpected")
}
func (f *jtiPersistingStore) ListClaimableSessionKeys(ctx context.Context, limit, perUserRunningLimit int) ([]string, error) {
	panic("unexpected")
}
func (f *jtiPersistingStore) MarkDispatchStarted(ctx context.Context, taskID, platformInstanceID, workerInstanceID string, freshSession bool) (domain.Task, error) {
	panic("unexpected")
}
func (f *jtiPersistingStore) MarkRunning(ctx context.Context, taskID, platformInstanceID string) (domain.Task, error) {
	panic("unexpected")
}
func (f *jtiPersistingStore) RecordChunkEvent(ctx context.Context, taskID string, byteCount int, digest string) error {
	panic("unexpected")
}
func (f *jtiPersistingStore) RecordHeartbeat(ctx context.Context, taskID string) error {
	panic("unexpected")
}
func (f *jtiPersistingStore) CountRunningTasks(ctx context.Context) (int, error) {
	panic("unexpected")
}
func (f *jtiPersistingStore) CountQueuedTasksByRequester(ctx context.Context, requesterUserID int64) (int, error) {
	panic("unexpected")
}
func (f *jtiPersistingStore) ResetWorkspaceForNewSession(ctx context.Context, sessionKey string) (int, error) {
	panic("unexpected")
}
func (f *jtiPersistingStore) WorkspaceIsFresh(ctx context.Context, sessionKey string) (bool, error) {
	panic("unexpected")
}

// TestIssueCredentialsPersistsJTIsBeforeReturn 验证 F1: 签发函数返回前 JTI
// 已持久化到 tasks.capability_jtis——token 暴露给 Runner(写配置/启动容器/
// ReloadCredentials RPC)之前撤销依据必须已落库。
func TestIssueCredentialsPersistsJTIsBeforeReturn(t *testing.T) {
	provider := testProvider(1, 1, domain.ProviderNativeOAI, true)
	source := &fakeLLMProviderSource{providers: []domain.LLMProvider{provider}}
	issuer, err := llmproxy.NewIssuer([]byte("test-signing-key-at-least-32-bytes"), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	pstore := &jtiPersistingStore{}
	task := domain.Task{ID: "task-f1", SessionKey: "personal:f1", ToolPolicyVersion: "foundation.no-host-tools.v1"}
	s := &scheduler{cfg: SchedulerConfig{
		TokenIssuer: issuer, LLMProvider: source, Store: pstore,
		LLMProxyAddr: "http://127.0.0.1:9999", ConfigRoot: t.TempDir(),
		ModelPolicyVersion: "test.v1",
		TokenTTL:           time.Hour, TokenRefreshSkew: 5 * time.Minute, MaxTaskWallClock: 45 * time.Minute,
	}}
	set, _, err := s.issueInitialWorkerCredentials(context.Background(), task, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(set.JTIs) == 0 {
		t.Fatal("expected at least one JTI")
	}
	if len(pstore.calls) != 1 {
		t.Fatalf("SetTaskCapabilityJTIs calls=%d want 1 (persisted before token exposure)", len(pstore.calls))
	}
	if pstore.calls[0] != fmt.Sprintf("task-f1:%d", len(set.JTIs)) {
		t.Fatalf("persisted JTI set mismatch: %v", pstore.calls)
	}
}

// TestCleanupWorkerEntryPassesFirstJTI 验证审查 C1/I7: Worker 清理/关闭时
// 必须携带当前凭据集的首个 JTI(生产会话有活跃 JTI 集, 空 JTI 的 Shutdown
// 会被 Worker 拒绝, 优雅关闭必然失败)。
func TestCleanupWorkerEntryPassesFirstJTI(t *testing.T) {
	s := &scheduler{}
	var got string
	entry := &workerEntry{
		credentials: workerCredentialSet{JTIs: []string{"jti-active", "jti-other"}},
		cleanup:     func(jti string) { got = jti },
	}
	s.cleanupWorkerEntryBestEffort(context.Background(), entry)
	if got != "jti-active" {
		t.Fatalf("cleanup received JTI %q, want firstJTI jti-active", got)
	}
}

// TestIssueInitialCredentialsWritesGenerationScopedConfigDir 验证审查 C1/I6
// 连锁修复: config 按 generation 隔离后, Platform 的 credential 初始写入
// 必须落在容器实际挂载的 config/g<generation> 子目录(而非 config/ 根),
// 否则 Runner 读不到 token。回调必须收到当前 Runner generation。
func TestIssueInitialCredentialsWritesGenerationScopedConfigDir(t *testing.T) {
	dir := t.TempDir()
	provider := testProvider(1, 1, domain.ProviderNativeOAI, true)
	source := &fakeLLMProviderSource{providers: []domain.LLMProvider{provider}}
	issuer, err := llmproxy.NewIssuer([]byte("test-signing-key-at-least-32-bytes"), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	var gotGen uint64
	var gotDir string
	s := &scheduler{cfg: SchedulerConfig{
		PlatformInstanceID: "p1",
		TokenIssuer:        issuer,
		LLMProvider:        source,
		LLMProxyAddr:       "http://127.0.0.1:9999",
		ConfigRoot:         dir,
		ModelPolicyVersion: "test.v1",
		Registry:           testPolicyRegistry(t),
		RuntimeConfigDir: func(sessionKey string, generation uint64) string {
			gotGen = generation
			gotDir = filepath.Join(dir, fmt.Sprintf("g%d", generation))
			return gotDir
		},
	}}
	task := domain.Task{ID: "t-gen-scoped", SessionKey: "personal:1"}
	if _, _, err := s.issueInitialWorkerCredentials(context.Background(), task, 2); err != nil {
		t.Fatalf("issueInitialWorkerCredentials: %v", err)
	}
	if gotGen != 2 {
		t.Fatalf("runtime config dir callback received generation %d, want 2", gotGen)
	}
	if _, err := os.Stat(filepath.Join(gotDir, runtimeConfigFilename)); err != nil {
		t.Fatalf("runtime config must be written under config/g2 (err=%v)", err)
	}
}
