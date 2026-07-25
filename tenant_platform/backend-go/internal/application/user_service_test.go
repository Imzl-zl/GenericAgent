package application

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
)

// fakeUserStore is an in-memory UserStore for non-DB unit tests.
type fakeUserStore struct {
	users     map[int64]domain.User
	nextID    int64
	approveErr error
	blockErr  error
	blockAffected []domain.Task
}

func newFakeUserStore() *fakeUserStore {
	return &fakeUserStore{users: make(map[int64]domain.User), nextID: 1}
}

func (f *fakeUserStore) CreateUser(_ context.Context, username string) (domain.User, error) {
	u := domain.User{ID: f.nextID, Username: username, Status: domain.UserPending}
	f.users[f.nextID] = u
	f.nextID++
	return u, nil
}

func (f *fakeUserStore) ApproveUser(_ context.Context, userID int64) (domain.User, error) {
	if f.approveErr != nil {
		return domain.User{}, f.approveErr
	}
	u, ok := f.users[userID]
	if !ok {
		return domain.User{}, fmt.Errorf("user %d not found", userID)
	}
	if u.Status != domain.UserPending {
		return domain.User{}, fmt.Errorf("user %d is %s, not pending", userID, u.Status)
	}
	u.Status = domain.UserApproved
	f.users[userID] = u
	return u, nil
}

func (f *fakeUserStore) BlockUser(_ context.Context, userID int64) (domain.User, []domain.Task, error) {
	if f.blockErr != nil {
		return domain.User{}, nil, f.blockErr
	}
	u, ok := f.users[userID]
	if !ok {
		return domain.User{}, nil, fmt.Errorf("user %d not found", userID)
	}
	if u.Status == domain.UserBlocked {
		return domain.User{}, nil, fmt.Errorf("already blocked")
	}
	u.Status = domain.UserBlocked
	f.users[userID] = u
	return u, f.blockAffected, nil
}

func (f *fakeUserStore) ListPendingUsers(_ context.Context) ([]domain.User, error) {
	var out []domain.User
	for _, u := range f.users {
		if u.Status == domain.UserPending {
			out = append(out, u)
		}
	}
	return out, nil
}

func (f *fakeUserStore) GetUserStatus(_ context.Context, userID int64) (domain.UserStatus, error) {
	u, ok := f.users[userID]
	if !ok {
		return "", fmt.Errorf("not found")
	}
	return u.Status, nil
}

func (f *fakeUserStore) GetUserByID(_ context.Context, userID int64) (int64, string, domain.UserStatus, error) {
	u, ok := f.users[userID]
	if !ok {
		return 0, "", "", fmt.Errorf("not found")
	}
	return u.ID, u.Username, u.Status, nil
}

func TestUserServiceCreateUserRejectsEmptyUsername(t *testing.T) {
	svc, err := NewUserService(UserServiceConfig{Store: newFakeUserStore()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CreateUser(context.Background(), ""); err == nil {
		t.Fatal("expected error for empty username")
	}
	if _, err := svc.CreateUser(context.Background(), "   "); err == nil {
		t.Fatal("expected error for whitespace-only username")
	}
}

func TestUserServiceCreateUserRejectsTooLongUsername(t *testing.T) {
	svc, err := NewUserService(UserServiceConfig{Store: newFakeUserStore()})
	if err != nil {
		t.Fatal(err)
	}
	long := strings.Repeat("a", MaxUsernameLen+1)
	if _, err := svc.CreateUser(context.Background(), long); err == nil {
		t.Fatal("expected error for too-long username")
	}
}

func TestUserServiceCreateUserTrimsAndSucceeds(t *testing.T) {
	store := newFakeUserStore()
	svc, err := NewUserService(UserServiceConfig{Store: store})
	if err != nil {
		t.Fatal(err)
	}
	u, err := svc.CreateUser(context.Background(), "  alice  ")
	if err != nil {
		t.Fatal(err)
	}
	if u.Username != "alice" {
		t.Fatalf("expected trimmed username, got %q", u.Username)
	}
	if u.Status != domain.UserPending {
		t.Fatalf("expected pending, got %s", u.Status)
	}
}

func TestUserServiceApproveUserRejectsNonPositiveID(t *testing.T) {
	svc, _ := NewUserService(UserServiceConfig{Store: newFakeUserStore()})
	if _, err := svc.ApproveUser(context.Background(), 0); err == nil {
		t.Fatal("expected error for zero user id")
	}
	if _, err := svc.ApproveUser(context.Background(), -1); err == nil {
		t.Fatal("expected error for negative user id")
	}
}

func TestUserServiceBlockUserInvokesCancelWorker(t *testing.T) {
	store := newFakeUserStore()
	store.users[1] = domain.User{ID: 1, Username: "alice", Status: domain.UserApproved}
	store.blockAffected = []domain.Task{{ID: "task-1"}, {ID: "task-2"}}
	cancelCalled := make(map[string]bool)
	cfg := UserServiceConfig{
		Store: store,
		CancelWorker: func(_ context.Context, task domain.Task) error {
			cancelCalled[task.ID] = true
			return nil
		},
	}
	svc, err := NewUserService(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.BlockUser(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	if !cancelCalled["task-1"] || !cancelCalled["task-2"] {
		t.Fatalf("expected cancelWorker called for both tasks, got %v", cancelCalled)
	}
}

func TestUserServiceBlockUserSurfacesCancelWorkerError(t *testing.T) {
	store := newFakeUserStore()
	store.users[1] = domain.User{ID: 1, Username: "alice", Status: domain.UserApproved}
	store.blockAffected = []domain.Task{{ID: "task-1"}}
	cancelErr := errors.New("worker unreachable")
	svc, _ := NewUserService(UserServiceConfig{
		Store: store,
		CancelWorker: func(_ context.Context, _ domain.Task) error { return cancelErr },
	})
	_, err := svc.BlockUser(context.Background(), 1)
	if err == nil || !strings.Contains(err.Error(), "worker unreachable") {
		t.Fatalf("expected worker cancel error surfaced, got %v", err)
	}
}

func TestUserServiceBlockUserNoCancelWorkerStillSucceeds(t *testing.T) {
	store := newFakeUserStore()
	store.users[1] = domain.User{ID: 1, Username: "alice", Status: domain.UserApproved}
	store.blockAffected = []domain.Task{{ID: "task-1"}}
	svc, _ := NewUserService(UserServiceConfig{Store: store})
	if _, err := svc.BlockUser(context.Background(), 1); err != nil {
		t.Fatalf("expected success without cancelWorker, got %v", err)
	}
}

func TestUserServiceListPendingUsers(t *testing.T) {
	store := newFakeUserStore()
	store.users[1] = domain.User{ID: 1, Username: "alice", Status: domain.UserPending}
	store.users[2] = domain.User{ID: 2, Username: "bob", Status: domain.UserApproved}
	svc, _ := NewUserService(UserServiceConfig{Store: store})
	pending, err := svc.ListPendingUsers(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].Username != "alice" {
		t.Fatalf("expected only alice pending, got %v", pending)
	}
}

func TestNewUserServiceRejectsNilStore(t *testing.T) {
	if _, err := NewUserService(UserServiceConfig{}); err == nil {
		t.Fatal("expected error for nil store")
	}
}
