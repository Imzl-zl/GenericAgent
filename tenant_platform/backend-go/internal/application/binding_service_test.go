package application

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
)

// fakeBindingStore is an in-memory BindingStore for non-DB unit tests.
type fakeBindingStore struct {
	attempts      map[string]domain.BindingAttempt // codeHash → attempt
	consumeErr    error
	lastCodeHash  string
	lastBotUUID   string
	lastIlinkUser string
	bound         bool
}

func newFakeBindingStore() *fakeBindingStore {
	return &fakeBindingStore{attempts: make(map[string]domain.BindingAttempt)}
}

func (f *fakeBindingStore) CreateBindingAttempt(_ context.Context, userID int64, codeHash string, expiresAt time.Time) (domain.BindingAttempt, error) {
	b := domain.BindingAttempt{
		ID:        int64(len(f.attempts) + 1),
		UserID:    userID,
		CodeHash:  codeHash,
		State:     domain.BindingRequested,
		ExpiresAt: expiresAt,
	}
	f.attempts[codeHash] = b
	return b, nil
}

func (f *fakeBindingStore) ConsumeBindingAndBindBot(_ context.Context, codeHash, botUUID, ilinkUserID string, _ time.Time) (domain.BindingAttempt, error) {
	f.lastCodeHash = codeHash
	f.lastBotUUID = botUUID
	f.lastIlinkUser = ilinkUserID
	if f.consumeErr != nil {
		return domain.BindingAttempt{}, f.consumeErr
	}
	b, ok := f.attempts[codeHash]
	if !ok {
		return domain.BindingAttempt{}, fmt.Errorf("no consumable binding for this code")
	}
	if !b.State.IsConsumable() {
		return domain.BindingAttempt{}, fmt.Errorf("binding not consumable: %s", b.State)
	}
	b.State = domain.BindingActive
	b.BotUUID = botUUID
	f.attempts[codeHash] = b
	f.bound = true
	return b, nil
}

func TestBindingServiceGenerateCodeReturnsNonEmptyCode(t *testing.T) {
	store := newFakeBindingStore()
	svc, err := NewBindingService(BindingServiceConfig{Store: store})
	if err != nil {
		t.Fatal(err)
	}
	code, attempt, err := svc.GenerateBindingCode(context.Background(), 42)
	if err != nil {
		t.Fatal(err)
	}
	if code == "" {
		t.Fatal("expected non-empty code")
	}
	if len(code) != BindingCodeLen {
		t.Fatalf("expected code length %d, got %d", BindingCodeLen, len(code))
	}
	if attempt.UserID != 42 {
		t.Fatalf("expected user id 42, got %d", attempt.UserID)
	}
	if attempt.State != domain.BindingRequested {
		t.Fatalf("expected state %s, got %s", domain.BindingRequested, attempt.State)
	}
}

func TestBindingServiceGenerateCodeStoresHashNotPlaintext(t *testing.T) {
	store := newFakeBindingStore()
	svc, _ := NewBindingService(BindingServiceConfig{Store: store})
	code, attempt, _ := svc.GenerateBindingCode(context.Background(), 1)
	// The stored hash must not be the plaintext code.
	if attempt.CodeHash == code {
		t.Fatal("store must not persist plaintext code")
	}
	// The hash must be a 64-char hex SHA-256 digest.
	if len(attempt.CodeHash) != 64 {
		t.Fatalf("expected 64-char hash, got %d", len(attempt.CodeHash))
	}
}

func TestBindingServiceGenerateCodeRejectsInvalidUserID(t *testing.T) {
	svc, _ := NewBindingService(BindingServiceConfig{Store: newFakeBindingStore()})
	if _, _, err := svc.GenerateBindingCode(context.Background(), 0); err == nil {
		t.Fatal("expected error for zero user id")
	}
	if _, _, err := svc.GenerateBindingCode(context.Background(), -1); err == nil {
		t.Fatal("expected error for negative user id")
	}
}

func TestBindingServiceActivateHashesCodeAndBinds(t *testing.T) {
	store := newFakeBindingStore()
	svc, _ := NewBindingService(BindingServiceConfig{Store: store})
	code, _, _ := svc.GenerateBindingCode(context.Background(), 1)
	attempt, err := svc.Activate(context.Background(), code, "bot-uuid-123", "ilink-user-999")
	if err != nil {
		t.Fatal(err)
	}
	if attempt.State != domain.BindingActive {
		t.Fatalf("expected state active, got %s", attempt.State)
	}
	if !store.bound {
		t.Fatal("expected bot to be bound")
	}
	// The code passed to the store must be hashed, not plaintext.
	if store.lastCodeHash == code {
		t.Fatal("store must receive hash, not plaintext code")
	}
	if store.lastBotUUID != "bot-uuid-123" {
		t.Fatalf("expected bot uuid bot-uuid-123, got %s", store.lastBotUUID)
	}
	if store.lastIlinkUser != "ilink-user-999" {
		t.Fatalf("expected ilink user ilink-user-999, got %s", store.lastIlinkUser)
	}
}

func TestBindingServiceActivateOneTimeUseFailsSecondTime(t *testing.T) {
	store := newFakeBindingStore()
	svc, _ := NewBindingService(BindingServiceConfig{Store: store})
	code, _, _ := svc.GenerateBindingCode(context.Background(), 1)
	if _, err := svc.Activate(context.Background(), code, "bot-uuid", "ilink-user"); err != nil {
		t.Fatalf("first activate should succeed, got %v", err)
	}
	// Second activate with same code should fail (binding is now 'active').
	_, err := svc.Activate(context.Background(), code, "bot-uuid", "ilink-user")
	if err == nil {
		t.Fatal("expected error on second activate (one-time use)")
	}
}

func TestBindingServiceActivateRejectsEmptyInputs(t *testing.T) {
	svc, _ := NewBindingService(BindingServiceConfig{Store: newFakeBindingStore()})
	cases := []struct{ code, botUUID, ilinkUserID string }{
		{"", "bot", "user"},
		{"code", "", "user"},
		{"code", "bot", ""},
	}
	for i, c := range cases {
		if _, err := svc.Activate(context.Background(), c.code, c.botUUID, c.ilinkUserID); err == nil {
			t.Fatalf("case %d: expected error for empty input", i)
		}
	}
}

func TestBindingServiceActivateUnknownCodeFails(t *testing.T) {
	svc, _ := NewBindingService(BindingServiceConfig{Store: newFakeBindingStore()})
	_, err := svc.Activate(context.Background(), "unknown-code", "bot-uuid", "ilink-user")
	if err == nil || !strings.Contains(err.Error(), "no consumable binding") {
		t.Fatalf("expected 'no consumable binding' error, got %v", err)
	}
}

func TestNewBindingServiceRejectsNilStore(t *testing.T) {
	if _, err := NewBindingService(BindingServiceConfig{}); err == nil {
		t.Fatal("expected error for nil store")
	}
}

func TestNewBindingServiceDefaultsCodeTTL(t *testing.T) {
	svc, err := NewBindingService(BindingServiceConfig{Store: newFakeBindingStore()})
	if err != nil {
		t.Fatal(err)
	}
	bs := svc.(*bindingService)
	if bs.codeTTL != 10*time.Minute {
		t.Fatalf("expected default 10min TTL, got %v", bs.codeTTL)
	}
}
