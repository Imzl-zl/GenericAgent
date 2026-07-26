package application

import (
	"context"
	"errors"
	"testing"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/transport"
)

// fakeRelayStore is an in-memory RelayStore for relay service tests.
type fakeRelayStore struct {
	recipient domain.RelayRecipient
	getErr    error
	users     map[int64]fakeUserRec // sender lookup by ID
	setErr    error
	setCalls  []setOptOutCall
}

type fakeUserRec struct {
	username string
	status   domain.UserStatus
}

type setOptOutCall struct {
	userID  int64
	optOut  bool
}

func (f *fakeRelayStore) GetRelayRecipient(_ context.Context, _ string) (domain.RelayRecipient, error) {
	return f.recipient, f.getErr
}

func (f *fakeRelayStore) GetUserByID(_ context.Context, userID int64) (int64, string, domain.UserStatus, error) {
	u, ok := f.users[userID]
	if !ok {
		return 0, "", "", errors.New("user not found")
	}
	return userID, u.username, u.status, nil
}

func (f *fakeRelayStore) SetRelayOptOut(_ context.Context, userID int64, optOut bool) error {
	if f.setErr != nil {
		return f.setErr
	}
	f.setCalls = append(f.setCalls, setOptOutCall{userID: userID, optOut: optOut})
	return nil
}

// fakeAuditRecorder is an in-memory AuditRecorder for relay service tests.
type fakeAuditRecorder struct {
	events []domain.AuditEvent
	err    error
}

func (f *fakeAuditRecorder) AppendAuditEvent(_ context.Context, event domain.AuditEvent) error {
	if f.err != nil {
		return f.err
	}
	f.events = append(f.events, event)
	return nil
}

func newRelayServiceForTest(store *fakeRelayStore, tr *transport.LoopbackTransport) RelayService {
	svc, _ := NewRelayService(RelayServiceConfig{
		Store:     store,
		Transport: tr,
		Audit:     &fakeAuditRecorder{},
	})
	return svc
}

func TestRelaySuccess(t *testing.T) {
	store := &fakeRelayStore{
		recipient: domain.RelayRecipient{
			UserID:      100,
			Username:    "bob",
			Status:      domain.UserApproved,
			OptOut:      false,
			BotID:       7,
			BotUUID:     "bot-bob",
			IlinkUserID: "ilink-bob",
		},
		users: map[int64]fakeUserRec{
			42: {username: "alice", status: domain.UserApproved},
		},
	}
	tr := transport.NewLoopbackTransport()
	svc := newRelayServiceForTest(store, tr)

	err := svc.Relay(context.Background(), 42, "bob", "hello")
	if err != nil {
		t.Fatalf("expected success, got %v", err)
	}
	sent, ok := tr.LastSentMessage()
	if !ok {
		t.Fatal("expected one sent message")
	}
	if sent.BotUUID != "bot-bob" || sent.IlinkUserID != "ilink-bob" {
		t.Fatalf("unexpected send target: %+v", sent)
	}
	want := "[来自 alice 的消息] hello"
	if sent.Text != want {
		t.Fatalf("text mismatch:\n got: %q\nwant: %q", sent.Text, want)
	}
}

func TestRelayUserNotFound(t *testing.T) {
	store := &fakeRelayStore{
		getErr: domain.ErrRelayUserNotFound,
	}
	svc := newRelayServiceForTest(store, transport.NewLoopbackTransport())
	err := svc.Relay(context.Background(), 42, "nobody", "hi")
	if !errors.Is(err, domain.ErrRelayUserNotFound) {
		t.Fatalf("expected ErrRelayUserNotFound, got %v", err)
	}
}

func TestRelaySelfTarget(t *testing.T) {
	store := &fakeRelayStore{
		recipient: domain.RelayRecipient{
			UserID:   42,
			Username: "alice",
			Status:   domain.UserApproved,
		},
		users: map[int64]fakeUserRec{
			42: {username: "alice", status: domain.UserApproved},
		},
	}
	svc := newRelayServiceForTest(store, transport.NewLoopbackTransport())
	err := svc.Relay(context.Background(), 42, "alice", "hi")
	if !errors.Is(err, domain.ErrRelaySelfTarget) {
		t.Fatalf("expected ErrRelaySelfTarget, got %v", err)
	}
}

func TestRelayRecipientNotApproved(t *testing.T) {
	store := &fakeRelayStore{
		recipient: domain.RelayRecipient{
			UserID:   100,
			Username: "bob",
			Status:   domain.UserPending,
		},
	}
	svc := newRelayServiceForTest(store, transport.NewLoopbackTransport())
	err := svc.Relay(context.Background(), 42, "bob", "hi")
	if !errors.Is(err, domain.ErrRelayUserNotApproved) {
		t.Fatalf("expected ErrRelayUserNotApproved, got %v", err)
	}
}

func TestRelayRecipientOptedOut(t *testing.T) {
	store := &fakeRelayStore{
		recipient: domain.RelayRecipient{
			UserID:   100,
			Username: "bob",
			Status:   domain.UserApproved,
			OptOut:   true,
		},
	}
	svc := newRelayServiceForTest(store, transport.NewLoopbackTransport())
	err := svc.Relay(context.Background(), 42, "bob", "hi")
	if !errors.Is(err, domain.ErrRelayOptedOut) {
		t.Fatalf("expected ErrRelayOptedOut, got %v", err)
	}
}

func TestRelayRecipientNotBound(t *testing.T) {
	store := &fakeRelayStore{
		recipient: domain.RelayRecipient{
			UserID:   100,
			Username: "bob",
			Status:   domain.UserApproved,
			OptOut:   false,
			// BotUUID and IlinkUserID empty
		},
	}
	svc := newRelayServiceForTest(store, transport.NewLoopbackTransport())
	err := svc.Relay(context.Background(), 42, "bob", "hi")
	if !errors.Is(err, domain.ErrRelayUserNotBound) {
		t.Fatalf("expected ErrRelayUserNotBound, got %v", err)
	}
}

func TestRelayEmptyMessage(t *testing.T) {
	store := &fakeRelayStore{}
	svc := newRelayServiceForTest(store, transport.NewLoopbackTransport())
	err := svc.Relay(context.Background(), 42, "bob", "   ")
	if !errors.Is(err, domain.ErrRelayEmptyMessage) {
		t.Fatalf("expected ErrRelayEmptyMessage, got %v", err)
	}
}

func TestRelayEmptyUsername(t *testing.T) {
	store := &fakeRelayStore{}
	svc := newRelayServiceForTest(store, transport.NewLoopbackTransport())
	err := svc.Relay(context.Background(), 42, "  ", "hi")
	if err == nil {
		t.Fatal("expected error for empty username")
	}
}

func TestRelayInvalidSenderID(t *testing.T) {
	store := &fakeRelayStore{}
	svc := newRelayServiceForTest(store, transport.NewLoopbackTransport())
	err := svc.Relay(context.Background(), 0, "bob", "hi")
	if err == nil {
		t.Fatal("expected error for invalid sender id")
	}
}

func TestRelaySenderUnknown(t *testing.T) {
	store := &fakeRelayStore{
		recipient: domain.RelayRecipient{
			UserID:      100,
			Username:    "bob",
			Status:      domain.UserApproved,
			BotUUID:     "bot-bob",
			IlinkUserID: "ilink-bob",
		},
		users: map[int64]fakeUserRec{
			// sender 42 not present → GetUserByID returns error
		},
	}
	svc := newRelayServiceForTest(store, transport.NewLoopbackTransport())
	err := svc.Relay(context.Background(), 42, "bob", "hi")
	if err == nil {
		t.Fatal("expected error for unknown sender")
	}
}

func TestRelayTransportFailure(t *testing.T) {
	store := &fakeRelayStore{
		recipient: domain.RelayRecipient{
			UserID:      100,
			Username:    "bob",
			Status:      domain.UserApproved,
			BotUUID:     "bot-bob",
			IlinkUserID: "ilink-bob",
		},
		users: map[int64]fakeUserRec{
			42: {username: "alice", status: domain.UserApproved},
		},
	}
	tr := transport.NewLoopbackTransport()
	tr.SetSendError(errors.New("ilink down"))
	svc := newRelayServiceForTest(store, tr)

	err := svc.Relay(context.Background(), 42, "bob", "hi")
	if err == nil {
		t.Fatal("expected transport error")
	}
}

func TestRelaySetOptOut(t *testing.T) {
	store := &fakeRelayStore{}
	svc := newRelayServiceForTest(store, transport.NewLoopbackTransport())

	if err := svc.SetOptOut(context.Background(), 42, true); err != nil {
		t.Fatalf("set opt out true: %v", err)
	}
	if err := svc.SetOptOut(context.Background(), 42, false); err != nil {
		t.Fatalf("set opt out false: %v", err)
	}
	if len(store.setCalls) != 2 {
		t.Fatalf("expected 2 set calls, got %d", len(store.setCalls))
	}
	if !store.setCalls[0].optOut || store.setCalls[0].userID != 42 {
		t.Fatalf("call 0 mismatch: %+v", store.setCalls[0])
	}
	if store.setCalls[1].optOut || store.setCalls[1].userID != 42 {
		t.Fatalf("call 1 mismatch: %+v", store.setCalls[1])
	}
}

func TestRelaySetOptOutInvalidID(t *testing.T) {
	svc := newRelayServiceForTest(&fakeRelayStore{}, transport.NewLoopbackTransport())
	if err := svc.SetOptOut(context.Background(), 0, true); err == nil {
		t.Fatal("expected error for invalid user id")
	}
}

func TestNewRelayServiceValidation(t *testing.T) {
	if _, err := NewRelayService(RelayServiceConfig{}); err == nil {
		t.Fatal("expected error for nil store")
	}
	if _, err := NewRelayService(RelayServiceConfig{
		Store: &fakeRelayStore{},
	}); err == nil {
		t.Fatal("expected error for nil transport")
	}
	if _, err := NewRelayService(RelayServiceConfig{
		Store:     &fakeRelayStore{},
		Transport: transport.NewLoopbackTransport(),
	}); err == nil {
		t.Fatal("expected error for nil audit")
	}
}
