package application

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
)

// fakeInviteStore is an in-memory InviteStore for login/session lifecycle tests.
type fakeInviteStore struct {
	sessions map[string]domain.UserSession // tokenHash → session
	nextHash int
}

func hashForTest(pw string) string {
	h, err := HashPassword(pw)
	if err != nil {
		panic(err)
	}
	return h
}

func newFakeInviteStore() *fakeInviteStore {
	return &fakeInviteStore{sessions: make(map[string]domain.UserSession)}
}

func (f *fakeInviteStore) CreateInviteCode(_ context.Context, code string, _ int64, _ time.Time) (domain.InviteCode, error) {
	return domain.InviteCode{Code: code}, nil
}
func (f *fakeInviteStore) CheckInviteCode(_ context.Context, _ string, _ time.Time) error { return nil }
func (f *fakeInviteStore) RevokeInviteCode(_ context.Context, _ string) error             { return nil }
func (f *fakeInviteStore) DeleteInviteCodes(_ context.Context, _ []string) (int64, error) { return 0, nil }
func (f *fakeInviteStore) ListInviteCodes(_ context.Context) ([]domain.InviteCode, error) {
	return nil, nil
}
func (f *fakeInviteStore) CreateUserWithInvite(_ context.Context, _, _, _, _ string, _, _ time.Time) (domain.User, error) {
	return domain.User{}, nil
}
func (f *fakeInviteStore) CreateUserSession(_ context.Context, tokenHash string, userID int64, expiresAt time.Time) (domain.UserSession, error) {
	s := domain.UserSession{TokenHash: tokenHash, UserID: userID, ExpiresAt: expiresAt, CreatedAt: time.Now().UTC()}
	f.sessions[tokenHash] = s
	return s, nil
}
func (f *fakeInviteStore) GetUserSession(_ context.Context, tokenHash string) (domain.UserSession, error) {
	s, ok := f.sessions[tokenHash]
	if !ok {
		return domain.UserSession{}, fmt.Errorf("session not found")
	}
	if !s.ExpiresAt.After(time.Now().UTC()) {
		return domain.UserSession{}, fmt.Errorf("session expired")
	}
	return s, nil
}

// TestLoginRejectsBlockedUser 审查 D3: blocked 用户不得登录。
func TestLoginRejectsBlockedUser(t *testing.T) {
	users := newFakeUserStore()
	users.users[1] = domain.User{ID: 1, Username: "bob", PasswordHash: hashForTest("pw"), Status: domain.UserBlocked}
	svc, _ := NewInviteService(InviteServiceConfig{Store: newFakeInviteStore(), Users: users})
	if _, _, err := svc.Login(context.Background(), "bob", "pw"); err == nil {
		t.Fatal("expected login rejected for blocked user")
	}
}

// TestLoginApprovedUserSucceeds 审查 D3: 正常用户登录不受影响。
func TestLoginApprovedUserSucceeds(t *testing.T) {
	users := newFakeUserStore()
	users.users[1] = domain.User{ID: 1, Username: "alice", PasswordHash: hashForTest("pw"), Status: domain.UserApproved}
	svc, _ := NewInviteService(InviteServiceConfig{Store: newFakeInviteStore(), Users: users})
	u, token, err := svc.Login(context.Background(), "alice", "pw")
	if err != nil {
		t.Fatalf("login failed: %v", err)
	}
	if u.ID != 1 || token == "" {
		t.Fatalf("unexpected login result: user=%+v token=%q", u, token)
	}
}

// TestValidateSessionRejectsBlockedUser 审查 D3: 即使会话未过期, blocked
// 用户的 token 也必须被拒绝(封禁后会话被撤销, 此测试覆盖"会话仍存在但
// 用户状态已变更"的极端情况)。
func TestValidateSessionRejectsBlockedUser(t *testing.T) {
	users := newFakeUserStore()
	users.users[1] = domain.User{ID: 1, Username: "carol", Status: domain.UserBlocked}
	invites := newFakeInviteStore()
	// 模拟封禁前签发的会话未被清理(直接注入)。
	invites.sessions[hashToken("token")] = domain.UserSession{
		TokenHash: hashToken("token"), UserID: 1, ExpiresAt: time.Now().UTC().Add(time.Hour),
	}
	svc, _ := NewInviteService(InviteServiceConfig{Store: invites, Users: users})
	if _, err := svc.ValidateSession(context.Background(), "token"); err == nil {
		t.Fatal("expected session validation rejected for blocked user")
	}
}

// TestValidateSessionApprovedUserSucceeds 审查 D3: 正常会话校验不受影响。
func TestValidateSessionApprovedUserSucceeds(t *testing.T) {
	users := newFakeUserStore()
	users.users[1] = domain.User{ID: 1, Username: "dave", Status: domain.UserApproved}
	invites := newFakeInviteStore()
	invites.sessions[hashToken("token")] = domain.UserSession{
		TokenHash: hashToken("token"), UserID: 1, ExpiresAt: time.Now().UTC().Add(time.Hour),
	}
	svc, _ := NewInviteService(InviteServiceConfig{Store: invites, Users: users})
	uid, err := svc.ValidateSession(context.Background(), "token")
	if err != nil {
		t.Fatalf("validate session failed: %v", err)
	}
	if uid != 1 {
		t.Fatalf("unexpected user id %d", uid)
	}
}
