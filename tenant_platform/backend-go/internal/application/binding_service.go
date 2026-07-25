package application

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
)

// BindingCodeLen is the length of the generated one-time binding code (bytes).
const BindingCodeLen = 8

// BindingStore is the persistence port for binding operations.
type BindingStore interface {
	CreateBindingAttempt(ctx context.Context, userID int64, codeHash string, expiresAt time.Time) (domain.BindingAttempt, error)
	ConsumeBindingAndBindBot(ctx context.Context, codeHash, botUUID, ilinkUserID string, now time.Time) (domain.BindingAttempt, error)
}

// BindingServiceConfig wires the binding store and code TTL.
type BindingServiceConfig struct {
	Store    BindingStore
	CodeTTL  time.Duration
}

// BindingService manages the WeChat binding flow (spec §5.1).
type BindingService interface {
	GenerateBindingCode(ctx context.Context, userID int64) (string, domain.BindingAttempt, error)
	Activate(ctx context.Context, code, botUUID, ilinkUserID string) (domain.BindingAttempt, error)
}

type bindingService struct {
	store   BindingStore
	codeTTL time.Duration
}

// NewBindingService constructs the service.
func NewBindingService(cfg BindingServiceConfig) (BindingService, error) {
	if cfg.Store == nil {
		return nil, fmt.Errorf("binding store is required")
	}
	if cfg.CodeTTL <= 0 {
		cfg.CodeTTL = 10 * time.Minute
	}
	return &bindingService{store: cfg.Store, codeTTL: cfg.CodeTTL}, nil
}

// GenerateBindingCode creates a one-time code, stores its SHA-256 hash, and
// returns the plaintext code. The plaintext is never persisted.
func (s *bindingService) GenerateBindingCode(ctx context.Context, userID int64) (string, domain.BindingAttempt, error) {
	if userID <= 0 {
		return "", domain.BindingAttempt{}, fmt.Errorf("user id must be positive")
	}
	code, err := generateCode(BindingCodeLen)
	if err != nil {
		return "", domain.BindingAttempt{}, fmt.Errorf("generate code: %w", err)
	}
	codeHash := hashCode(code)
	expiresAt := time.Now().Add(s.codeTTL).UTC()
	attempt, err := s.store.CreateBindingAttempt(ctx, userID, codeHash, expiresAt)
	if err != nil {
		return "", domain.BindingAttempt{}, err
	}
	return code, attempt, nil
}

// Activate consumes a binding code and pairs the bot with the ilink_user_id.
// The code is hashed and matched against the stored hash; plaintext is never
// looked up. One-time use is enforced by the store's state transition.
func (s *bindingService) Activate(ctx context.Context, code, botUUID, ilinkUserID string) (domain.BindingAttempt, error) {
	if code == "" {
		return domain.BindingAttempt{}, fmt.Errorf("code is required")
	}
	if botUUID == "" {
		return domain.BindingAttempt{}, fmt.Errorf("bot uuid is required")
	}
	if ilinkUserID == "" {
		return domain.BindingAttempt{}, fmt.Errorf("ilink user id is required")
	}
	codeHash := hashCode(code)
	return s.store.ConsumeBindingAndBindBot(ctx, codeHash, botUUID, ilinkUserID, time.Now().UTC())
}

// generateCode returns a cryptographically random alphanumeric code.
func generateCode(length int) (string, error) {
	const alphabet = "0123456789ABCDEFGHJKLMNPQRSTUVWXYZ"
	buf := make([]byte, length)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	for i := range buf {
		buf[i] = alphabet[int(buf[i])%len(alphabet)]
	}
	return string(buf), nil
}

// hashCode returns the lowercase hex SHA-256 digest of the code.
func hashCode(code string) string {
	sum := sha256.Sum256([]byte(code))
	return hex.EncodeToString(sum[:])
}
