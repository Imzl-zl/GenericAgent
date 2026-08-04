package application

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/infrastructure/llmproxy"
	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/infrastructure/worker"
	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/infrastructure/workerclient"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
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
		Generation: 1, ProxyBaseURL: proxyAddr, RoutingSnapshotID: "snapshot",
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
		Generation: 1, ProxyBaseURL: "http://127.0.0.1:8081", RoutingSnapshotID: "snapshot",
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

func TestRoutingSnapshotIgnoresDefaultSwitchAndDetectsBoundProviderChange(t *testing.T) {
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

	source.providers = []domain.LLMProvider{secondary, defaultProvider}
	source.providers[0].IsDefault = true
	source.providers[1].IsDefault = false
	sameStream := true
	source.providers[1].SessionConfig.Stream = &sameStream
	replace, err := s.routingSnapshotRequiresReplacement(context.Background(), snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if replace || snapshot.Providers[0].ID != defaultProvider.ID {
		t.Fatal("default switch must not change an already-bound Worker snapshot")
	}

	newFallback := testProvider(3, 1, domain.ProviderNativeOAI, false)
	source.providers = append(source.providers, newFallback)
	replace, err = s.routingSnapshotRequiresReplacement(context.Background(), snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if !replace {
		t.Fatal("new active provider must replace the Worker")
	}
	source.providers = source.providers[:2]

	source.providers[0].Revision++
	replace, err = s.routingSnapshotRequiresReplacement(context.Background(), snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if !replace {
		t.Fatal("bound provider revision change must replace the Worker")
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
		ModelPolicyVersion: "test.v1", MaxTaskWallClock: 45 * time.Minute,
		TokenRefreshSkew: 5 * time.Minute,
	}}
	snapshot, err := s.resolveRoutingSnapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	set, files, err := s.issueProviderCapabilities(context.Background(), "personal:1", snapshot, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(set.JTIs) != 2 || set.Generation != 1 || set.Checksum != files.Checksum {
		t.Fatalf("credential set=%+v", set)
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
		ModelPolicyVersion: "test.v1", MaxTaskWallClock: taskWallClock, TokenRefreshSkew: refreshSkew,
	}}
	snapshot, err := s.resolveRoutingSnapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.issueProviderCapabilities(context.Background(), "personal:1", snapshot, 1); err != nil {
		t.Fatal(err)
	}
}

func TestRefreshWorkerCredentialsRollsBackDefinitiveReloadRejection(t *testing.T) {
	dir := t.TempDir()
	provider := testProvider(1, 1, domain.ProviderNativeOAI, true)
	source := &fakeLLMProviderSource{providers: []domain.LLMProvider{provider}}
	issuer, err := llmproxy.NewIssuer([]byte("test-signing-key-at-least-32-bytes"), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := (&scheduler{cfg: SchedulerConfig{LLMProvider: source}}).resolveRoutingSnapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	oldFiles, err := BuildRuntimeConfig(RuntimeConfigInput{
		Generation: 1, ProxyBaseURL: "http://127.0.0.1:9999", RoutingSnapshotID: snapshot.ID,
		Providers: []RuntimeProviderBinding{{Provider: provider, Token: "old-token"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteRuntimeConfigAtomic(dir, oldFiles); err != nil {
		t.Fatal(err)
	}
	worker := newControlledWorker()
	worker.reloadErr = status.Error(codes.FailedPrecondition, "reload failed")
	capabilities := &routingCapabilityStore{}
	oldExpiry := time.Now().UTC().Add(time.Minute)
	entry := &workerEntry{client: worker, sessionKey: "personal:1", credentials: workerCredentialSet{
		Generation: 1, Checksum: oldFiles.Checksum, ExpiresAt: oldExpiry,
		JTIs: []string{"old-jti"}, Snapshot: snapshot,
	}}
	s := &scheduler{cfg: SchedulerConfig{
		TokenIssuer: issuer, CapabilityStore: capabilities, LLMProvider: source,
		LLMProxyAddr: "http://127.0.0.1:9999", ConfigRoot: dir,
		ModelPolicyVersion: "test.v1",
		TokenTTL:           time.Hour, TokenRefreshSkew: 5 * time.Minute, MaxTaskWallClock: 45 * time.Minute,
	}}

	err = s.refreshWorkerCredentials(context.Background(), entry)
	if err == nil {
		t.Fatal("expected reload error")
	}
	if entry.credentials.Generation != 1 || entry.credentials.JTIs[0] != "old-jti" {
		t.Fatalf("old credentials changed: %+v", entry.credentials)
	}
	gotJSON, readErr := os.ReadFile(filepath.Join(dir, runtimeConfigFilename))
	if readErr != nil {
		t.Fatal(readErr)
	}
	if !bytes.Equal(gotJSON, oldFiles.JSON) {
		t.Fatal("previous runtime JSON was not restored byte-for-byte")
	}
	if len(capabilities.revoked) == 0 {
		t.Fatal("new JTIs were not revoked after failed reload")
	}
	for _, revoked := range capabilities.revoked {
		if revoked.jti == "old-jti" {
			t.Fatal("old JTI revoked before Worker acknowledgment")
		}
	}
	worker.reloadErr = nil
	if err := s.refreshWorkerCredentials(context.Background(), entry); err != nil {
		t.Fatal(err)
	}
	if len(worker.reloadRequests) != 2 ||
		worker.reloadRequests[0].GetCredentialGeneration() != 2 ||
		worker.reloadRequests[1].GetCredentialGeneration() != 2 {
		t.Fatalf("reload generations=%+v", worker.reloadRequests)
	}
	if entry.credentials.Generation != 2 {
		t.Fatalf("credential generation=%d", entry.credentials.Generation)
	}
}

func TestRefreshWorkerCredentialsRetriesAmbiguousOutcomeIdempotently(t *testing.T) {
	dir := t.TempDir()
	provider := testProvider(1, 1, domain.ProviderNativeOAI, true)
	source := &fakeLLMProviderSource{providers: []domain.LLMProvider{provider}}
	issuer, err := llmproxy.NewIssuer([]byte("test-signing-key-at-least-32-bytes"), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := (&scheduler{cfg: SchedulerConfig{LLMProvider: source}}).resolveRoutingSnapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	oldFiles, err := BuildRuntimeConfig(RuntimeConfigInput{
		Generation: 1, ProxyBaseURL: "http://127.0.0.1:9999", RoutingSnapshotID: snapshot.ID,
		Providers: []RuntimeProviderBinding{{Provider: provider, Token: "old-token"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteRuntimeConfigAtomic(dir, oldFiles); err != nil {
		t.Fatal(err)
	}
	worker := newControlledWorker()
	worker.reloadErr = status.Error(codes.Unavailable, "response lost")
	capabilities := &routingCapabilityStore{}
	entry := &workerEntry{client: worker, sessionKey: "personal:1", credentials: workerCredentialSet{
		Generation: 1, Checksum: oldFiles.Checksum, ExpiresAt: time.Now().UTC().Add(time.Minute),
		JTIs: []string{"old-jti"}, Snapshot: snapshot,
	}}
	s := &scheduler{cfg: SchedulerConfig{
		TokenIssuer: issuer, CapabilityStore: capabilities, LLMProvider: source,
		LLMProxyAddr: "http://127.0.0.1:9999", ConfigRoot: dir, ModelPolicyVersion: "test.v1",
		TokenTTL: time.Hour, TokenRefreshSkew: 5 * time.Minute, MaxTaskWallClock: 45 * time.Minute,
	}}

	if err := s.refreshWorkerCredentials(context.Background(), entry); err == nil {
		t.Fatal("expected ambiguous reload error")
	}
	if entry.credentials.Generation != 1 || entry.pendingRefresh == nil {
		t.Fatalf("entry state=%+v pending=%+v", entry.credentials, entry.pendingRefresh)
	}
	currentJSON, err := os.ReadFile(filepath.Join(dir, runtimeConfigFilename))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(currentJSON, oldFiles.JSON) {
		t.Fatal("ambiguous reload restored old JSON")
	}
	if len(capabilities.revoked) != 0 {
		t.Fatalf("ambiguous reload revoked credentials: %+v", capabilities.revoked)
	}

	worker.reloadErr = nil
	if err := s.refreshWorkerCredentials(context.Background(), entry); err != nil {
		t.Fatal(err)
	}
	if len(worker.reloadRequests) != 2 ||
		worker.reloadRequests[0].GetCredentialGeneration() != 2 ||
		worker.reloadRequests[1].GetCredentialGeneration() != 2 {
		t.Fatalf("reload generations=%+v", worker.reloadRequests)
	}
	if entry.credentials.Generation != 2 || entry.pendingRefresh != nil {
		t.Fatalf("entry state=%+v pending=%+v", entry.credentials, entry.pendingRefresh)
	}
	if len(capabilities.revoked) != 1 || capabilities.revoked[0].jti != "old-jti" {
		t.Fatalf("revoked=%+v", capabilities.revoked)
	}
}

func TestRefreshWorkerCredentialsRevokesOldJTIOnlyAfterAcknowledgment(t *testing.T) {
	dir := t.TempDir()
	provider := testProvider(1, 1, domain.ProviderNativeOAI, true)
	source := &fakeLLMProviderSource{providers: []domain.LLMProvider{provider}}
	issuer, err := llmproxy.NewIssuer([]byte("test-signing-key-at-least-32-bytes"), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := (&scheduler{cfg: SchedulerConfig{LLMProvider: source}}).resolveRoutingSnapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	oldFiles, err := BuildRuntimeConfig(RuntimeConfigInput{
		Generation: 1, ProxyBaseURL: "http://127.0.0.1:9999", RoutingSnapshotID: snapshot.ID,
		Providers: []RuntimeProviderBinding{{Provider: provider, Token: "old-token"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteRuntimeConfigAtomic(dir, oldFiles); err != nil {
		t.Fatal(err)
	}
	worker := newControlledWorker()
	capabilities := &routingCapabilityStore{}
	capabilities.beforeRevoke = func() {
		if len(worker.reloadRequests) == 0 {
			t.Fatal("JTI revoked before ReloadCredentials acknowledgment")
		}
	}
	entry := &workerEntry{client: worker, sessionKey: "personal:1", credentials: workerCredentialSet{
		Generation: 1, Checksum: oldFiles.Checksum, ExpiresAt: time.Now().UTC().Add(time.Minute),
		JTIs: []string{"old-jti"}, Snapshot: snapshot,
	}}
	s := &scheduler{cfg: SchedulerConfig{
		TokenIssuer: issuer, CapabilityStore: capabilities, LLMProvider: source,
		LLMProxyAddr: "http://127.0.0.1:9999", ConfigRoot: dir,
		ModelPolicyVersion: "test.v1",
		TokenTTL:           time.Hour, TokenRefreshSkew: 5 * time.Minute, MaxTaskWallClock: 45 * time.Minute,
	}}

	if err := s.refreshWorkerCredentials(context.Background(), entry); err != nil {
		t.Fatal(err)
	}
	if entry.credentials.Generation != 2 || len(worker.reloadRequests) != 1 {
		t.Fatalf("entry=%+v reloads=%d", entry.credentials, len(worker.reloadRequests))
	}
	if len(capabilities.revoked) != 1 || capabilities.revoked[0].jti != "old-jti" {
		t.Fatalf("revoked=%+v", capabilities.revoked)
	}
}

func TestCredentialRefreshRetainsOldJTIsUntilRevocationSucceeds(t *testing.T) {
	worker := newControlledWorker()
	storeErr := errors.New("revocation store unavailable")
	capabilities := &routingCapabilityStore{err: storeErr}
	oldSet := workerCredentialSet{
		Generation: 1, Checksum: "old", ExpiresAt: time.Now().UTC().Add(time.Hour),
		JTIs: []string{"old-jti"}, Snapshot: routingSnapshot{ID: "snapshot"},
	}
	newSet := workerCredentialSet{
		Generation: 2, Checksum: "new", ExpiresAt: time.Now().UTC().Add(time.Hour),
		JTIs: []string{"new-jti"}, Snapshot: oldSet.Snapshot,
	}
	entry := &workerEntry{
		client: worker, credentials: oldSet,
		pendingRefresh: &pendingCredentialRefresh{Previous: oldSet, Next: newSet},
	}
	s := &scheduler{cfg: SchedulerConfig{CapabilityStore: capabilities}}

	err := s.acknowledgePendingCredentialRefresh(context.Background(), entry)
	if !errors.Is(err, storeErr) {
		t.Fatalf("ack error=%v", err)
	}
	if entry.credentials.Generation != 2 || len(entry.pendingRevocations) != 1 {
		t.Fatalf("entry=%+v pending revocations=%+v", entry.credentials, entry.pendingRevocations)
	}
	capabilities.err = nil
	if err := s.flushPendingCredentialRevocations(context.Background(), entry); err != nil {
		t.Fatal(err)
	}
	if len(entry.pendingRevocations) != 0 || len(capabilities.revoked) != 1 || capabilities.revoked[0].jti != "old-jti" {
		t.Fatalf("pending=%+v revoked=%+v", entry.pendingRevocations, capabilities.revoked)
	}
}

func TestEnsureWorkerAppliesProviderChangesOnlyAtNextTaskBoundary(t *testing.T) {
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
	task := domain.Task{SessionKey: "personal:1"}
	_, first, err := s.ensureWorker(context.Background(), task)
	if err != nil {
		t.Fatal(err)
	}

	// A key-only rotation preserves revision and must not disturb the bound Worker.
	source.providers[0].APIKey = "rotated-key"
	_, same, err := s.ensureWorker(context.Background(), task)
	if err != nil {
		t.Fatal(err)
	}
	if same != first || cleanupCalls != 0 {
		t.Fatal("key-only rotation replaced the current Worker snapshot")
	}

	// The currently bound entry remains immutable until the next ensureWorker call.
	source.providers[0].Revision = 2
	if first.credentials.Snapshot.Providers[0].Revision != 1 {
		t.Fatal("mid-task provider edit mutated the current routing snapshot")
	}
	_, replacement, err := s.ensureWorker(context.Background(), task)
	if err != nil {
		t.Fatal(err)
	}
	if replacement == first || cleanupCalls != 1 || len(started) != 2 {
		t.Fatalf("replacement=%t cleanup=%d starts=%d", replacement != first, cleanupCalls, len(started))
	}
	if replacement.credentials.Snapshot.Providers[0].Revision != 2 {
		t.Fatalf("replacement snapshot=%+v", replacement.credentials.Snapshot)
	}
	if len(capabilities.revoked) != 1 {
		t.Fatalf("old capability revocations=%d", len(capabilities.revoked))
	}
}

func TestRoutingAuditContainsMetadataWithoutCredentialMaterial(t *testing.T) {
	recorder := &routingAuditRecorder{}
	s := &scheduler{cfg: SchedulerConfig{Audit: recorder, ModelPolicyVersion: "policy.v1"}}
	entry := &workerEntry{credentials: workerCredentialSet{
		Generation: 3,
		JTIs:       []string{"sensitive-jti-must-not-appear"},
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
		!strings.Contains(detail, `"routing_snapshot_id":"sha256:snapshot"`) ||
		!strings.Contains(detail, `"credential_generation":3`) {
		t.Fatalf("audit metadata incomplete: %s", detail)
	}
}

// TestRefreshWorkerCredentialsKeepsRunnerGeneration 验证 credential 刷新只递增
// credential 版本(JSON/reload 协议), token 绑定的 RunnerGeneration 保持不变
// (审查 C4: generation 分离, 复用 Runner 的第二个 task 不会被 Worker 拒绝)。
func TestRefreshWorkerCredentialsKeepsRunnerGeneration(t *testing.T) {
	dir := t.TempDir()
	provider := testProvider(1, 1, domain.ProviderNativeOAI, true)
	source := &fakeLLMProviderSource{providers: []domain.LLMProvider{provider}}
	issuer, err := llmproxy.NewIssuer([]byte("test-signing-key-at-least-32-bytes"), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := (&scheduler{cfg: SchedulerConfig{LLMProvider: source}}).resolveRoutingSnapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	oldFiles, err := BuildRuntimeConfig(RuntimeConfigInput{
		Generation: 1, ProxyBaseURL: "http://127.0.0.1:9999", RoutingSnapshotID: snapshot.ID,
		Providers: []RuntimeProviderBinding{{Provider: provider, Token: "old-token"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteRuntimeConfigAtomic(dir, oldFiles); err != nil {
		t.Fatal(err)
	}
	worker := newControlledWorker()
	capabilities := &routingCapabilityStore{}
	entry := &workerEntry{
		client: worker, sessionKey: "personal:1",
		runnerGeneration: 7, // 复用 Runner 的真实 lease generation
		credentials: workerCredentialSet{
			Generation: 1, RunnerGeneration: 7, Checksum: oldFiles.Checksum,
			ExpiresAt: time.Now().UTC().Add(time.Minute),
			JTIs:      []string{"old-jti"}, Snapshot: snapshot,
		},
	}
	s := &scheduler{cfg: SchedulerConfig{
		TokenIssuer: issuer, CapabilityStore: capabilities, LLMProvider: source,
		LLMProxyAddr: "http://127.0.0.1:9999", ConfigRoot: dir,
		ModelPolicyVersion: "test.v1",
		TokenTTL:           time.Hour, TokenRefreshSkew: 5 * time.Minute, MaxTaskWallClock: 45 * time.Minute,
	}}

	if err := s.refreshWorkerCredentials(context.Background(), entry); err != nil {
		t.Fatal(err)
	}
	if entry.credentials.Generation != 2 {
		t.Fatalf("credential generation = %d, want 2", entry.credentials.Generation)
	}
	if entry.credentials.RunnerGeneration != 7 {
		t.Fatalf("runner generation = %d, want 7 (must not change on refresh)", entry.credentials.RunnerGeneration)
	}
	if len(worker.reloadRequests) != 1 ||
		worker.reloadRequests[0].GetCredentialGeneration() != 2 {
		t.Fatalf("reload requests = %+v", worker.reloadRequests)
	}
}

// TestIssueInitialWorkerCredentialsSetsBothGenerations 验证首次签发时
// credential generation 与 runner generation 都等于 Runner lease generation。
func TestIssueInitialWorkerCredentialsSetsBothGenerations(t *testing.T) {
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
	if set.Generation != 3 || set.RunnerGeneration != 3 {
		t.Fatalf("set = %+v, want Generation=3 RunnerGeneration=3", set)
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

// TestEnsureWorkerSwitchesTaskBeforePrepareRefresh 验证审查 R5-I2: 复用
// Worker 时 entry.taskID 必须先切换到新任务再执行 prepareWorkerEntry——
// prepare 内部因凭据到期触发的 credential 刷新以 entry.taskID 签发并持久化
// JTI; 若仍指向已终态旧任务, 新 token 会挂到无法被撤销的行上(旧任务终态
// 事务已提交, 恢复扫描只处理未终态任务), 崩溃窗口内无人撤销。
func TestEnsureWorkerSwitchesTaskBeforePrepareRefresh(t *testing.T) {
	dir := t.TempDir()
	provider := testProvider(1, 1, domain.ProviderNativeOAI, true)
	source := &fakeLLMProviderSource{providers: []domain.LLMProvider{provider}}
	issuer, err := llmproxy.NewIssuer([]byte("test-signing-key-at-least-32-bytes"), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := (&scheduler{cfg: SchedulerConfig{LLMProvider: source}}).resolveRoutingSnapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// 旧 runtime config(刷新会读取作为 rollback 基线)。
	oldFiles, err := BuildRuntimeConfig(RuntimeConfigInput{
		Generation: 1, ProxyBaseURL: "http://127.0.0.1:9999", RoutingSnapshotID: snapshot.ID,
		Providers: []RuntimeProviderBinding{{Provider: provider, Token: "old-token"}},
		JTIs:      []string{"old-jti"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteRuntimeConfigAtomic(dir, oldFiles); err != nil {
		t.Fatal(err)
	}
	worker := newControlledWorker()
	pstore := &jtiPersistingStore{}
	entry := &workerEntry{
		client: worker, sessionKey: "personal:r5", taskID: "old-task",
		credentials: workerCredentialSet{
			Generation: 1, Checksum: oldFiles.Checksum,
			ExpiresAt: time.Now().UTC().Add(time.Second), // 立即触发 needRefresh
			JTIs:      []string{"old-jti"}, Snapshot: snapshot,
		},
		runnerGeneration: 1,
	}
	s := &scheduler{
		cfg: SchedulerConfig{
			PlatformInstanceID: "p1",
			TokenIssuer:        issuer,
			CapabilityStore:    &routingCapabilityStore{},
			LLMProvider:        source,
			LLMProxyAddr:       "http://127.0.0.1:9999",
			ConfigRoot:         dir,
			ModelPolicyVersion: "test.v1",
			TokenTTL:           time.Hour, TokenRefreshSkew: 5 * time.Minute, MaxTaskWallClock: 45 * time.Minute,
			Runtime: &capacityRuntime{}, // ResolveGeneration=1
			Store:   pstore,
		},
		workers: map[string]*workerEntry{"personal:r5": entry},
		mu:      sync.Mutex{},
	}

	newTask := domain.Task{ID: "new-task", SessionKey: "personal:r5", Status: domain.TaskStarting}
	if _, _, err := s.ensureWorker(context.Background(), newTask); err != nil {
		t.Fatalf("ensureWorker: %v", err)
	}
	if len(pstore.calls) == 0 {
		t.Fatal("no SetTaskCapabilityJTIs call from refresh")
	}
	// 任何刷新签发的 JTI 都不得挂到已终态旧任务下(泄漏窗口内无人撤销)。
	for _, call := range pstore.calls {
		if strings.HasPrefix(call, "old-task:") {
			t.Fatalf("refresh must not persist JTI under terminal old task, got %q", call)
		}
	}
	if entry.taskID != "new-task" {
		t.Fatalf("entry.taskID = %q, want new-task", entry.taskID)
	}
}

// 审查 R5-I3: 上一任务 ambiguous reload 留下的 pendingRefresh 绑定旧 task_id,
// 不得在下一任务边界被 ack 提升——否则下一任务使用已终态撤销的旧 token。
// 修复行为: pending.taskID != entry.taskID 时丢弃 pending(撤销 Next)并按
// 新任务重新签发。
func TestRefreshWorkerCredentialsDiscardsPendingBoundToPreviousTask(t *testing.T) {
	dir := t.TempDir()
	provider := testProvider(1, 1, domain.ProviderNativeOAI, true)
	source := &fakeLLMProviderSource{providers: []domain.LLMProvider{provider}}
	issuer, err := llmproxy.NewIssuer([]byte("test-signing-key-at-least-32-bytes"), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := (&scheduler{cfg: SchedulerConfig{LLMProvider: source}}).resolveRoutingSnapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	oldFiles, err := BuildRuntimeConfig(RuntimeConfigInput{
		Generation: 1, ProxyBaseURL: "http://127.0.0.1:9999", RoutingSnapshotID: snapshot.ID,
		Providers: []RuntimeProviderBinding{{Provider: provider, Token: "old-token"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteRuntimeConfigAtomic(dir, oldFiles); err != nil {
		t.Fatal(err)
	}
	worker := newControlledWorker()
	worker.reloadErr = status.Error(codes.Unavailable, "response lost")
	capabilities := &routingCapabilityStore{}
	entry := &workerEntry{client: worker, sessionKey: "personal:1", taskID: "task-A",
		credentials: workerCredentialSet{
			Generation: 1, Checksum: oldFiles.Checksum, ExpiresAt: time.Now().UTC().Add(time.Minute),
			JTIs: []string{"old-jti"}, Snapshot: snapshot,
		}}
	s := &scheduler{cfg: SchedulerConfig{
		TokenIssuer: issuer, CapabilityStore: capabilities, LLMProvider: source,
		LLMProxyAddr: "http://127.0.0.1:9999", ConfigRoot: dir, ModelPolicyVersion: "test.v1",
		TokenTTL: time.Hour, TokenRefreshSkew: 5 * time.Minute, MaxTaskWallClock: 45 * time.Minute,
	}}

	// 第一次: 任务 A 的刷新响应丢失 → pending 绑定 task-A(gen2)。
	if err := s.refreshWorkerCredentials(context.Background(), entry); err == nil {
		t.Fatal("expected ambiguous reload error for task A")
	}
	if entry.pendingRefresh == nil || entry.pendingRefresh.TaskID != "task-A" {
		t.Fatalf("pendingRefresh = %+v, want taskID=task-A", entry.pendingRefresh)
	}
	revokedBefore := len(capabilities.revoked)

	// 任务 A 终态撤销; 下一任务 B 复用 Worker。
	worker.reloadErr = nil
	entry.taskID = "task-B"

	if err := s.refreshWorkerCredentials(context.Background(), entry); err != nil {
		t.Fatalf("refresh for task B: %v", err)
	}
	// 绑定 A 的 pending.Next 必须被撤销(丢弃), 而不是提升为当前凭据。
	if len(capabilities.revoked) <= revokedBefore {
		t.Fatal("task-A pending.Next JTIs must be revoked when discarded")
	}
	// 必须为 B 重新签发: A 的 pending(gen2)被丢弃后, 从当前凭据(gen1)
	// 重新签发 gen2 绑定 B——A 的 gen2 从未被 ack, 不产生 generation 空洞。
	if entry.credentials.Generation != 2 {
		t.Fatalf("credential generation = %d, want 2 (re-issued for task B)", entry.credentials.Generation)
	}
	if entry.credentials.JTIs[0] == "old-jti" {
		t.Fatal("credentials must be re-issued for task B, not stale")
	}
	if entry.pendingRefresh != nil {
		t.Fatalf("pendingRefresh must be cleared after successful ack, got %+v", entry.pendingRefresh)
	}
	// Worker 最终 ack 的是绑定 B 的新 gen2; 序列 [A 的失败 ack, B 的成功 ack]。
	if n := len(worker.reloadRequests); n != 2 || worker.reloadRequests[n-1].GetCredentialGeneration() != 2 {
		t.Fatalf("reload generations = %+v, want [2, 2]", worker.reloadRequests)
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
