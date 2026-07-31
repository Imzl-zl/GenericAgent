package application

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
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

type fakeDocumentGatewayTokenIssuer struct {
	token      string
	sessionKey string
	workspace  string
	calls      int
	err        error
}

func (f *fakeDocumentGatewayTokenIssuer) IssueDocumentGatewayToken(_ context.Context, sessionKey, workspaceID string) (string, error) {
	f.sessionKey = sessionKey
	f.workspace = workspaceID
	f.calls++
	if f.err != nil {
		return "", f.err
	}
	return f.token, nil
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

func TestIssueInitialWorkerCredentialsWritesDocumentGatewayConfig(t *testing.T) {
	dir := t.TempDir()
	provider := testProvider(1, 1, domain.ProviderNativeOAI, true)
	source := &fakeLLMProviderSource{providers: []domain.LLMProvider{provider}}
	issuer, err := llmproxy.NewIssuer([]byte("test-signing-key-at-least-32-bytes"), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	documentIssuer := &fakeDocumentGatewayTokenIssuer{token: "document-token"}
	s := &scheduler{cfg: SchedulerConfig{
		TokenIssuer: issuer, LLMProvider: source, LLMProxyAddr: "http://127.0.0.1:9999",
		ConfigRoot: dir, ModelPolicyVersion: "test.v1",
		MaxTaskWallClock: 45 * time.Minute, TokenRefreshSkew: 5 * time.Minute,
		DocumentGatewayBaseURL:     "http://127.0.0.1:8080/document-gateway",
		DocumentGatewayTokenIssuer: documentIssuer,
	}}
	task := domain.Task{SessionKey: "team:docs", WorkspaceID: "11111111-1111-1111-1111-111111111111", RequesterID: 42}

	set, err := s.issueInitialWorkerCredentials(context.Background(), task)
	if err != nil {
		t.Fatal(err)
	}
	if set.Document.WorkspaceID != task.WorkspaceID || set.Document.SessionKey != task.SessionKey {
		t.Fatalf("credential document config = %+v", set.Document)
	}
	if documentIssuer.sessionKey != task.SessionKey || documentIssuer.workspace != task.WorkspaceID {
		t.Fatalf("document issuer saw session=%q workspace=%q", documentIssuer.sessionKey, documentIssuer.workspace)
	}
	runtimeJSON, err := os.ReadFile(filepath.Join(dir, runtimeConfigFilename))
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]json.RawMessage
	if err := json.Unmarshal(runtimeJSON, &document); err != nil {
		t.Fatal(err)
	}
	var gateway RuntimeDocumentGateway
	if err := json.Unmarshal(document["_platform_document"], &gateway); err != nil {
		t.Fatal(err)
	}
	if gateway.BaseURL != "http://127.0.0.1:8080/document-gateway" || gateway.CapabilityToken != "document-token" || gateway.SessionKey != task.SessionKey || gateway.WorkspaceID != task.WorkspaceID {
		t.Fatalf("document gateway = %+v", gateway)
	}
	for _, forbidden := range []string{"requester_user_id", "DATABASE_URL", "docker", "podman"} {
		if strings.Contains(string(document["_platform_document"]), forbidden) {
			t.Fatalf("document gateway leaked %q: %s", forbidden, document["_platform_document"])
		}
	}
}

func TestRefreshRuntimeDocumentGatewayReissuesBoundCapability(t *testing.T) {
	issuer := &fakeDocumentGatewayTokenIssuer{token: "refreshed-document-token"}
	s := &scheduler{cfg: SchedulerConfig{
		DocumentGatewayBaseURL:     "http://127.0.0.1:8080/document-gateway",
		DocumentGatewayTokenIssuer: issuer,
	}}
	current := RuntimeDocumentGateway{
		BaseURL: "http://127.0.0.1:8080/document-gateway", CapabilityToken: "old-token",
		SessionKey: "team:docs", WorkspaceID: "11111111-1111-1111-1111-111111111111",
	}

	refreshed, err := s.refreshRuntimeDocumentGateway(context.Background(), current)
	if err != nil {
		t.Fatal(err)
	}
	if issuer.calls != 1 || issuer.sessionKey != current.SessionKey || issuer.workspace != current.WorkspaceID {
		t.Fatalf("issuer calls=%d session=%q workspace=%q", issuer.calls, issuer.sessionKey, issuer.workspace)
	}
	if refreshed.CapabilityToken != "refreshed-document-token" || refreshed.SessionKey != current.SessionKey || refreshed.WorkspaceID != current.WorkspaceID {
		t.Fatalf("refreshed gateway = %+v", refreshed)
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
	runtime := worker.NewStaticRuntime(func(context.Context, string) (workerclient.WorkerClient, func(), error) {
		client := newControlledWorker()
		started = append(started, client)
		return client, func() { cleanupCalls++ }, nil
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
