package application

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
)

// InviteCodeLen is the length of generated invite codes in characters.
const InviteCodeLen = 12

// InviteCodeTTL is the default validity window for a new invite code.
const InviteCodeTTL = 7 * 24 * time.Hour

// SessionTTL is the default validity window for a user bearer session.
const SessionTTL = 30 * 24 * time.Hour

// ErrInviteCodesRequired reports an empty permanent-delete request.
var ErrInviteCodesRequired = errors.New("at least one invite code is required")

// InviteStore is the persistence port for invite codes and user sessions.
type InviteStore interface {
	CreateInviteCode(ctx context.Context, code string, createdByUserID int64, expiresAt time.Time) (domain.InviteCode, error)
	CheckInviteCode(ctx context.Context, code string, now time.Time) error
	RevokeInviteCode(ctx context.Context, code string) error
	DeleteInviteCodes(ctx context.Context, codes []string) (int64, error)
	ListInviteCodes(ctx context.Context) ([]domain.InviteCode, error)
	CreateUserWithInvite(ctx context.Context, username, passwordHash, code, tokenHash string, now, sessionExpiresAt time.Time) (domain.User, error)
	CreateUserSession(ctx context.Context, tokenHash string, userID int64, expiresAt time.Time) (domain.UserSession, error)
	GetUserSession(ctx context.Context, tokenHash string) (domain.UserSession, error)
}

// InviteService manages invite codes and issues user sessions.
type InviteService interface {
	GenerateInviteCode(ctx context.Context, createdByUserID int64) (string, domain.InviteCode, error)
	RevokeInviteCode(ctx context.Context, code string) error
	DeleteInviteCodes(ctx context.Context, codes []string) (int64, error)
	ListInviteCodes(ctx context.Context) ([]domain.InviteCode, error)
	RegisterWithInvite(ctx context.Context, username, password, code string) (domain.User, string, error)
	Login(ctx context.Context, username, password string) (domain.User, string, error)
	ValidateSession(ctx context.Context, token string) (int64, error)
}

type inviteService struct {
	store      InviteStore
	users      UserStore
	codeTTL    time.Duration
	sessionTTL time.Duration
}

// InviteServiceConfig wires the invite service.
type InviteServiceConfig struct {
	Store      InviteStore
	Users      UserStore
	CodeTTL    time.Duration
	SessionTTL time.Duration
}

// NewInviteService constructs the service.
func NewInviteService(cfg InviteServiceConfig) (InviteService, error) {
	if cfg.Store == nil {
		return nil, fmt.Errorf("invite store is required")
	}
	if cfg.Users == nil {
		return nil, fmt.Errorf("user store is required")
	}
	if cfg.CodeTTL <= 0 {
		cfg.CodeTTL = InviteCodeTTL
	}
	if cfg.SessionTTL <= 0 {
		cfg.SessionTTL = SessionTTL
	}
	return &inviteService{
		store:      cfg.Store,
		users:      cfg.Users,
		codeTTL:    cfg.CodeTTL,
		sessionTTL: cfg.SessionTTL,
	}, nil
}

func (s *inviteService) GenerateInviteCode(ctx context.Context, createdByUserID int64) (string, domain.InviteCode, error) {
	if createdByUserID <= 0 {
		return "", domain.InviteCode{}, fmt.Errorf("created by user id must be positive")
	}
	code, err := generateInviteCode(InviteCodeLen)
	if err != nil {
		return "", domain.InviteCode{}, fmt.Errorf("generate code: %w", err)
	}
	expiresAt := time.Now().Add(s.codeTTL).UTC()
	ic, err := s.store.CreateInviteCode(ctx, code, createdByUserID, expiresAt)
	if err != nil {
		return "", domain.InviteCode{}, err
	}
	return code, ic, nil
}

func (s *inviteService) RevokeInviteCode(ctx context.Context, code string) error {
	return s.store.RevokeInviteCode(ctx, code)
}

func (s *inviteService) DeleteInviteCodes(ctx context.Context, codes []string) (int64, error) {
	unique := make([]string, 0, len(codes))
	seen := make(map[string]struct{}, len(codes))
	for _, code := range codes {
		trimmed := strings.TrimSpace(code)
		if trimmed == "" {
			continue
		}
		if _, exists := seen[trimmed]; exists {
			continue
		}
		seen[trimmed] = struct{}{}
		unique = append(unique, trimmed)
	}
	if len(unique) == 0 {
		return 0, ErrInviteCodesRequired
	}
	return s.store.DeleteInviteCodes(ctx, unique)
}

func (s *inviteService) ListInviteCodes(ctx context.Context) ([]domain.InviteCode, error) {
	return s.store.ListInviteCodes(ctx)
}

func (s *inviteService) RegisterWithInvite(ctx context.Context, username, password, code string) (domain.User, string, error) {
	trimmed := strings.TrimSpace(username)
	if trimmed == "" {
		return domain.User{}, "", fmt.Errorf("username is required")
	}
	if len(trimmed) > MaxUsernameLen {
		return domain.User{}, "", fmt.Errorf("username must be <= %d bytes", MaxUsernameLen)
	}
	if len(password) < MinPasswordLen {
		return domain.User{}, "", fmt.Errorf("password must be >= %d characters", MinPasswordLen)
	}
	if code == "" {
		return domain.User{}, "", fmt.Errorf("invite code is required")
	}
	now := time.Now().UTC()
	if err := s.store.CheckInviteCode(ctx, code, now); err != nil {
		return domain.User{}, "", fmt.Errorf("invalid invite code")
	}
	passwordHash, err := HashPassword(password)
	if err != nil {
		return domain.User{}, "", fmt.Errorf("hash password: %w", err)
	}
	token, err := createSessionToken()
	if err != nil {
		return domain.User{}, "", err
	}
	now = time.Now().UTC()
	user, err := s.store.CreateUserWithInvite(
		ctx,
		trimmed,
		passwordHash,
		code,
		hashToken(token),
		now,
		now.Add(s.sessionTTL),
	)
	if err != nil {
		return domain.User{}, "", err
	}
	return user, token, nil
}

func (s *inviteService) Login(ctx context.Context, username, password string) (domain.User, string, error) {
	trimmed := strings.TrimSpace(username)
	if trimmed == "" {
		return domain.User{}, "", fmt.Errorf("username is required")
	}
	if password == "" {
		return domain.User{}, "", fmt.Errorf("password is required")
	}
	user, err := s.users.GetUserByUsername(ctx, trimmed)
	if err != nil {
		return domain.User{}, "", fmt.Errorf("invalid username or password")
	}
	if err := CheckPassword(password, user.PasswordHash); err != nil {
		return domain.User{}, "", fmt.Errorf("invalid username or password")
	}
	now := time.Now().UTC()
	token, err := createSessionToken()
	if err != nil {
		return domain.User{}, "", err
	}
	if _, err := s.store.CreateUserSession(ctx, hashToken(token), user.ID, now.Add(s.sessionTTL)); err != nil {
		return domain.User{}, "", err
	}
	return user, token, nil
}

func (s *inviteService) ValidateSession(ctx context.Context, token string) (int64, error) {
	if token == "" {
		return 0, fmt.Errorf("token is required")
	}
	sess, err := s.store.GetUserSession(ctx, hashToken(token))
	if err != nil {
		return 0, fmt.Errorf("invalid or expired session")
	}
	return sess.UserID, nil
}

func generateInviteCode(length int) (string, error) {
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

func createSessionToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}
