package application

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
)

type inviteDeleteStore struct {
	deletedCodes []string
	deletedCount int64
}

func (s *inviteDeleteStore) CreateInviteCode(context.Context, string, int64, time.Time) (domain.InviteCode, error) {
	return domain.InviteCode{}, nil
}

func (s *inviteDeleteStore) RevokeInviteCode(context.Context, string) error { return nil }

func (s *inviteDeleteStore) CheckInviteCode(context.Context, string, time.Time) error { return nil }

func (s *inviteDeleteStore) ListInviteCodes(context.Context) ([]domain.InviteCode, error) {
	return nil, nil
}

func (s *inviteDeleteStore) CreateUserWithInvite(
	context.Context,
	string,
	string,
	string,
	string,
	time.Time,
	time.Time,
) (domain.User, error) {
	return domain.User{}, nil
}

func (s *inviteDeleteStore) DeleteInviteCodes(_ context.Context, codes []string) (int64, error) {
	s.deletedCodes = append([]string(nil), codes...)
	return s.deletedCount, nil
}

func (s *inviteDeleteStore) CreateUserSession(context.Context, string, int64, time.Time) (domain.UserSession, error) {
	return domain.UserSession{}, nil
}

func (s *inviteDeleteStore) GetUserSession(context.Context, string) (domain.UserSession, error) {
	return domain.UserSession{}, nil
}

func TestInviteServiceDeleteInviteCodesTrimsAndDeduplicates(t *testing.T) {
	store := &inviteDeleteStore{deletedCount: 2}
	svc := &inviteService{store: store}
	deleter, ok := any(svc).(interface {
		DeleteInviteCodes(context.Context, []string) (int64, error)
	})
	if !ok {
		t.Fatal("invite service does not support permanent deletion")
	}

	deleted, err := deleter.DeleteInviteCodes(context.Background(), []string{" CODE-A ", "CODE-B", "CODE-A"})
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 2 {
		t.Fatalf("deleted=%d, want 2", deleted)
	}
	if len(store.deletedCodes) != 2 || store.deletedCodes[0] != "CODE-A" || store.deletedCodes[1] != "CODE-B" {
		t.Fatalf("deleted codes=%v", store.deletedCodes)
	}
}

func TestInviteServiceDeleteInviteCodesRejectsEmptySelection(t *testing.T) {
	store := &inviteDeleteStore{}
	svc := &inviteService{store: store}
	deleter, ok := any(svc).(interface {
		DeleteInviteCodes(context.Context, []string) (int64, error)
	})
	if !ok {
		t.Fatal("invite service does not support permanent deletion")
	}

	if _, err := deleter.DeleteInviteCodes(context.Background(), []string{"", "  "}); err == nil {
		t.Fatal("empty invite-code selection must be rejected")
	}
	if len(store.deletedCodes) != 0 {
		t.Fatalf("store must not be called, got %v", store.deletedCodes)
	}
}

type inviteRegistrationStore struct {
	inviteDeleteStore
	precheckCalls int
	precheckErr   error
	registerCalls int
	registered    domain.User
}

func (s *inviteRegistrationStore) CheckInviteCode(context.Context, string, time.Time) error {
	s.precheckCalls++
	return s.precheckErr
}

func (s *inviteRegistrationStore) CreateUserWithInvite(
	context.Context,
	string,
	string,
	string,
	string,
	time.Time,
	time.Time,
) (domain.User, error) {
	s.registerCalls++
	return s.registered, nil
}

func TestRegisterWithInviteUsesAtomicStoreBoundary(t *testing.T) {
	store := &inviteRegistrationStore{
		registered: domain.User{ID: 42, Username: "alice", Status: domain.UserPending},
	}
	svc := &inviteService{store: store, sessionTTL: time.Hour}

	user, token, err := svc.RegisterWithInvite(context.Background(), "alice", "password123", "CODE-A")
	if err != nil {
		t.Fatal(err)
	}
	if user.ID != 42 || token == "" {
		t.Fatalf("user=%+v token=%q", user, token)
	}
	if store.precheckCalls != 1 {
		t.Fatalf("invite precheck calls=%d, want 1", store.precheckCalls)
	}
	if store.registerCalls != 1 {
		t.Fatalf("atomic registration calls=%d, want 1", store.registerCalls)
	}
}

func TestRegisterWithInviteRejectsInvalidCodeBeforeAtomicRegistration(t *testing.T) {
	store := &inviteRegistrationStore{precheckErr: errors.New("invalid invite code")}
	svc := &inviteService{store: store, sessionTTL: time.Hour}

	if _, _, err := svc.RegisterWithInvite(context.Background(), "alice", "password123", "BAD-CODE"); err == nil {
		t.Fatal("invalid invite code must be rejected")
	}
	if store.precheckCalls != 1 {
		t.Fatalf("invite precheck calls=%d, want 1", store.precheckCalls)
	}
	if store.registerCalls != 0 {
		t.Fatalf("atomic registration calls=%d, want 0", store.registerCalls)
	}
}
