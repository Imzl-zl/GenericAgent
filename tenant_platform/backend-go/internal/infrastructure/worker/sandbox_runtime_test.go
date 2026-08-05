package worker

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	workerv1 "github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/gen/worker/v1"
	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/infrastructure/sandbox"
)

// fakeLeaseStore records lease lifecycle calls.
type fakeLeaseStore struct {
	mu            sync.Mutex
	acquired      []string
	released      []string
	renewed       []string
	attached      []string
	acquireErr    error
	renewErr      error
	attachErr     error
	acquireCalls  int
	created       bool // AcquireRunnerLease 的接管创建标记(接管路径测试)
	lease         domain.RunnerLease
	// round12 审查(I3): 续租阻塞模拟——renewBlock 置位后 RenewRunnerLease
	// 阻塞到 ctx 取消(验证 cleanup 的可取消等待)。
	renewBlock   bool
	renewEntered chan struct{}
}

func (f *fakeLeaseStore) AcquireRunnerLease(ctx context.Context, runnerKey, owner string, leaseTTL time.Duration, maxActive int64) (domain.RunnerLease, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.acquireCalls++
	f.acquired = append(f.acquired, runnerKey)
	if f.acquireErr != nil {
		return domain.RunnerLease{}, false, f.acquireErr
	}
	return f.lease, f.created, nil
}

func (f *fakeLeaseStore) RenewRunnerLease(ctx context.Context, runnerKey, owner string, generation uint64, leaseTTL time.Duration) error {
	f.mu.Lock()
	f.renewed = append(f.renewed, runnerKey)
	block := f.renewBlock
	entered := f.renewEntered
	f.mu.Unlock()
	if block {
		if entered != nil {
			close(entered)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Second):
			return errors.New("renew never unblocked")
		}
	}
	return f.renewErr
}

func (f *fakeLeaseStore) AttachRunnerContainer(ctx context.Context, runnerKey, containerID string, generation uint64, owner string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.attached = append(f.attached, runnerKey)
	return f.attachErr
}

func (f *fakeLeaseStore) ReleaseRunnerLease(ctx context.Context, runnerKey, owner string, generation uint64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.released = append(f.released, runnerKey)
	return nil
}

// fakeManagerCLI simulates the Sandbox Manager control plane.
type fakeManagerCLI struct {
	ensureErr  error
	destroyErr error
	created    int
	destroyed  []string
}

func (f *fakeManagerCLI) EnsureRunner(ctx context.Context, req sandbox.EnsureRunnerRequest) (sandbox.Runner, bool, error) {
	if f.ensureErr != nil {
		return sandbox.Runner{}, false, f.ensureErr
	}
	f.created++
	return sandbox.Runner{Name: "ga-runner-test-g1", ContainerID: "cid"}, true, nil
}

func (f *fakeManagerCLI) Destroy(ctx context.Context, name string) error {
	f.destroyed = append(f.destroyed, name)
	return f.destroyErr
}

func (f *fakeManagerCLI) CreateAndStart(ctx context.Context, spec sandbox.RunnerSpec) (sandbox.Runner, error) {
	return sandbox.Runner{}, nil
}

func (f *fakeManagerCLI) Inspect(ctx context.Context, name string) error { return nil }
func (f *fakeManagerCLI) IsRunnerContainer(ctx context.Context, idOrName string) (bool, error) {
	return true, nil
}
func (f *fakeManagerCLI) EnsureWorkspace(ctx context.Context, workspaceHash string) error { return nil }
func (f *fakeManagerCLI) RunnerWorkspaceHash(ctx context.Context, idOrName string) (string, bool, error) {
	return "", false, nil
}

func newSandboxRuntimeForTest(t *testing.T, leases *fakeLeaseStore, manager *fakeManagerCLI) *SandboxWorkerRuntime {
	t.Helper()
	ca, err := NewPlatformCA()
	if err != nil {
		t.Fatalf("ca: %v", err)
	}
	policyPath := filepath.Join(t.TempDir(), "policy.json")
	if err := os.WriteFile(policyPath, []byte(`{"schema_version":"x"}`), 0o600); err != nil {
		t.Fatalf("policy file: %v", err)
	}
	leases.lease = domain.RunnerLease{Generation: 1}
	r, err := NewSandbox(SandboxConfig{
		Manager:            manager,
		CA:                 ca,
		LeaseStore:         leases,
		PlatformInstanceID: "test-platform",
		WorkspaceRoot:      t.TempDir(),
		PolicyFile:         policyPath,
		ContainerPrefix:    "ga-runner-test",
		RunnerLeaseTTL:     time.Minute,
		ControlDialTimeout: 200 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("NewSandbox: %v", err)
	}
	return r
}

// TestStartReleasesLeaseWhenEnsureRunnerFails: acquire 成功后 Manager 创建
// 失败, lease 必须立即释放(fail-closed, 审查), 不能占容量到 TTL。
func TestStartReleasesLeaseWhenEnsureRunnerFails(t *testing.T) {
	leases := &fakeLeaseStore{}
	manager := &fakeManagerCLI{ensureErr: os.ErrPermission}
	r := newSandboxRuntimeForTest(t, leases, manager)

	if _, err := r.Start(context.Background(), StartRequest{SessionKey: "personal:1"}); err == nil {
		t.Fatal("Start must fail when EnsureRunner fails")
	}
	leases.mu.Lock()
	defer leases.mu.Unlock()
	if len(leases.released) != 1 || leases.released[0] != "personal:1" {
		t.Fatalf("lease must be released on failure, released=%v", leases.released)
	}
}

// TestStartFailsClosedWhenStaleContainerDestroyFails: lease 接管时旧 generation
// 容器销毁失败必须 fail-closed(审查 R5-C3)——旧 Runner 仍挂载同一 workspace
// 且可能写穿 memory/temp/state, 继续创建新容器会让两代并发写破坏 generation
// fencing。不得继续创建, 并必须释放 lease 归还容量。
func TestStartFailsClosedWhenStaleContainerDestroyFails(t *testing.T) {
	leases := &fakeLeaseStore{}
	manager := &fakeManagerCLI{destroyErr: errors.New("docker daemon unreachable")}
	r := newSandboxRuntimeForTest(t, leases, manager)
	// 接管: created=true + stale_container_id 非空。
	leases.created = true
	leases.lease = domain.RunnerLease{Generation: 2, StaleContainerID: "old-cid-123"}

	if _, err := r.Start(context.Background(), StartRequest{SessionKey: "personal:1"}); err == nil {
		t.Fatal("Start must fail when stale container destroy fails")
	}
	if len(manager.destroyed) != 1 || manager.destroyed[0] != "old-cid-123" {
		t.Fatalf("destroyed = %v, want [old-cid-123]", manager.destroyed)
	}
	if manager.created != 0 {
		t.Fatalf("created = %d, want 0 (must not proceed after stale destroy failure)", manager.created)
	}
	leases.mu.Lock()
	defer leases.mu.Unlock()
	if len(leases.released) != 1 {
		t.Fatalf("lease must be released on stale-destroy failure, released=%v", leases.released)
	}
}

// TestStartReleasesLeaseWhenDialFails: 容器创建成功但 mTLS 拨号失败, lease
// 必须释放且容器销毁。
func TestStartReleasesLeaseWhenDialFails(t *testing.T) {
	leases := &fakeLeaseStore{}
	manager := &fakeManagerCLI{}
	r := newSandboxRuntimeForTest(t, leases, manager)

	if _, err := r.Start(context.Background(), StartRequest{SessionKey: "personal:2"}); err == nil {
		t.Fatal("Start must fail when dial fails")
	}
	leases.mu.Lock()
	defer leases.mu.Unlock()
	if len(leases.released) != 1 {
		t.Fatalf("lease must be released on dial failure, released=%v", leases.released)
	}
	if len(manager.destroyed) != 1 {
		t.Fatalf("runner container must be destroyed on dial failure, destroyed=%v", manager.destroyed)
	}
}

// TestStartFailClosedWhenAttachFails: 新容器创建后 AttachRunnerContainer
// 失败(lease 被接管/DB 故障)必须 fail-closed——销毁容器、释放 lease、
// 返回错误, 不得继续拨号执行(审查 R4-I5)。
func TestStartFailClosedWhenAttachFails(t *testing.T) {
	leases := &fakeLeaseStore{attachErr: context.DeadlineExceeded}
	manager := &fakeManagerCLI{}
	r := newSandboxRuntimeForTest(t, leases, manager)

	if _, err := r.Start(context.Background(), StartRequest{SessionKey: "personal:4"}); err == nil {
		t.Fatal("Start must fail when AttachRunnerContainer fails")
	}
	leases.mu.Lock()
	defer leases.mu.Unlock()
	if len(leases.attached) != 1 {
		t.Fatalf("attach must be attempted, attached=%v", leases.attached)
	}
	if len(leases.released) != 1 {
		t.Fatalf("lease must be released on attach failure, released=%v", leases.released)
	}
	if len(manager.destroyed) != 1 {
		t.Fatalf("runner container must be destroyed on attach failure, destroyed=%v", manager.destroyed)
	}
}

// TestStartCleanupReleasesLeaseExactlyOnce: cleanup 幂等(once), 多次调用只
// 释放一次 lease。
func TestStartCleanupReleasesLeaseExactlyOnce(t *testing.T) {
	leases := &fakeLeaseStore{}
	manager := &fakeManagerCLI{}
	r := newSandboxRuntimeForTest(t, leases, manager)

	// dial 失败时 Start 不返回实例; 此处验证失败路径的 release 只发生一次。
	if _, err := r.Start(context.Background(), StartRequest{SessionKey: "personal:3"}); err == nil {
		t.Fatal("Start must fail when dial fails")
	}
	leases.mu.Lock()
	defer leases.mu.Unlock()
	if got := len(leases.released); got != 1 {
		t.Fatalf("expected exactly one release, got %d", got)
	}
}

// round9 审查: scheduler 先 ResolveGeneration(接管, created=true) 再 Start
// (同 owner 续租, created=false) 时, 二次获取会丢失接管标记——Start 必须
// 对任何非空 stale_container_id 无条件销毁(不依赖 created), 否则旧容器
// 继续挂载同一工作区产生双写。
func TestStartDestroysStaleContainerEvenWhenLeaseNotCreated(t *testing.T) {
	leases := &fakeLeaseStore{}
	manager := &fakeManagerCLI{}
	r := newSandboxRuntimeForTest(t, leases, manager)
	// 模拟二次获取: created=false 但 stale_container_id 非空(上次接管写入,
	// 同 owner 续租不清理)。
	leases.created = false
	leases.lease = domain.RunnerLease{Generation: 2, StaleContainerID: "old-cid-456"}

	// dial 会失败, 但 stale 销毁必须发生在 dial 之前。
	if _, err := r.Start(context.Background(), StartRequest{SessionKey: "personal:9"}); err == nil {
		t.Fatal("Start expected to fail on dial (no server)")
	}
	if len(manager.destroyed) == 0 || manager.destroyed[0] != "old-cid-456" {
		t.Fatalf("stale container must be destroyed first regardless of created flag, destroyed=%v", manager.destroyed)
	}
	// 销毁后失败路径还释放 lease。
	leases.mu.Lock()
	defer leases.mu.Unlock()
	if len(leases.released) != 1 {
		t.Fatalf("lease must be released, released=%v", leases.released)
	}
}

// round12 审查(I3): cleanup 必须可取消等待卡住的续租——renewer 卡在
// RenewRunnerLease(DB 半开)时, cleanup 不得无限阻塞任务收尾/关闭流程。
// 需要 Start 成功拿到 cleanup: 经 DialControl 注入 bufconn gRPC 服务端。
func TestCleanupUnblocksWhenRenewerStuck(t *testing.T) {
	leases := &fakeLeaseStore{renewEntered: make(chan struct{})}
	manager := &fakeManagerCLI{}

	// bufconn gRPC 服务端(Shutdown 足够, cleanup 只调用 Shutdown)。
	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer()
	workerv1.RegisterWorkerServiceServer(srv, &stubControlWorker{})
	go func() { _ = srv.Serve(lis) }()
	defer srv.Stop()

	r := newSandboxRuntimeForTest(t, leases, manager)
	r.cfg.RunnerLeaseTTL = 300 * time.Millisecond
	r.cfg.DialControl = func(ctx context.Context, _ string, _ CertMaterial) (*grpc.ClientConn, error) {
		return grpc.DialContext(ctx, "bufnet",
			grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return lis.Dial() }),
			grpc.WithTransportCredentials(insecure.NewCredentials()))
	}
	inst, err := r.Start(context.Background(), StartRequest{SessionKey: "personal:7"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	// 让续租进入阻塞调用。
	leases.mu.Lock()
	leases.renewBlock = true
	leases.mu.Unlock()
	select {
	case <-leases.renewEntered:
	case <-time.After(3 * time.Second):
		t.Fatal("renewer never entered blocked renew")
	}

	// cleanup 必须在其等待窗口内返回, 不能无限阻塞。
	done := make(chan struct{})
	go func() { inst.Cleanup(""); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("cleanup blocked forever while renewer stuck")
	}
	leases.mu.Lock()
	defer leases.mu.Unlock()
	if len(leases.released) != 1 {
		t.Fatalf("lease must be released after cleanup, released=%v", leases.released)
	}
}

// stubControlWorker 是 sandbox_runtime 测试的最小 Worker 服务端。
type stubControlWorker struct {
	workerv1.UnimplementedWorkerServiceServer
}

func (s *stubControlWorker) Shutdown(ctx context.Context, req *workerv1.ShutdownRequest) (*workerv1.ShutdownResponse, error) {
	return &workerv1.ShutdownResponse{}, nil
}
