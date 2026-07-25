package application

import (
	"context"
	"fmt"
	"strings"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
)

// UserStore is the persistence port for user lifecycle operations.
// *postgres.Store implements it implicitly.
type UserStore interface {
	CreateUser(ctx context.Context, username, passwordHash string) (domain.User, error)
	GetUserByUsername(ctx context.Context, username string) (domain.User, error)
	ApproveUser(ctx context.Context, userID int64) (domain.User, error)
	BlockUser(ctx context.Context, userID int64) (domain.User, []domain.Task, error)
	ListPendingUsers(ctx context.Context) ([]domain.User, error)
	GetUserStatus(ctx context.Context, userID int64) (domain.UserStatus, error)
	GetUserByID(ctx context.Context, userID int64) (int64, string, domain.UserStatus, error)
}

// UserServiceConfig wires the user store and optional worker-cancel callback.
type UserServiceConfig struct {
	Store        UserStore
	CancelWorker func(ctx context.Context, task domain.Task) error
}

// UserService manages the platform user lifecycle (create/approve/block).
type UserService interface {
	CreateUser(ctx context.Context, username, password string) (domain.User, error)
	ApproveUser(ctx context.Context, userID int64) (domain.User, error)
	BlockUser(ctx context.Context, userID int64) (domain.User, error)
	ListPendingUsers(ctx context.Context) ([]domain.User, error)
}

type userService struct {
	store        UserStore
	cancelWorker func(ctx context.Context, task domain.Task) error
}

// NewUserService constructs the service.
func NewUserService(cfg UserServiceConfig) (UserService, error) {
	if cfg.Store == nil {
		return nil, fmt.Errorf("user store is required")
	}
	return &userService{
		store:        cfg.Store,
		cancelWorker: cfg.CancelWorker,
	}, nil
}

func (s *userService) CreateUser(ctx context.Context, username, password string) (domain.User, error) {
	trimmed := strings.TrimSpace(username)
	if trimmed == "" {
		return domain.User{}, fmt.Errorf("username is required")
	}
	if len(trimmed) > MaxUsernameLen {
		return domain.User{}, fmt.Errorf("username must be <= %d bytes", MaxUsernameLen)
	}
	passwordHash, err := HashPassword(password)
	if err != nil {
		return domain.User{}, fmt.Errorf("hash password: %w", err)
	}
	return s.store.CreateUser(ctx, trimmed, passwordHash)
}

func (s *userService) ApproveUser(ctx context.Context, userID int64) (domain.User, error) {
	if userID <= 0 {
		return domain.User{}, fmt.Errorf("user id must be positive")
	}
	return s.store.ApproveUser(ctx, userID)
}

func (s *userService) BlockUser(ctx context.Context, userID int64) (domain.User, error) {
	if userID <= 0 {
		return domain.User{}, fmt.Errorf("user id must be positive")
	}
	user, affected, err := s.store.BlockUser(ctx, userID)
	if err != nil {
		return domain.User{}, err
	}
	// Async worker cancellation for running tasks (spec §5.3).
	for _, task := range affected {
		if s.cancelWorker != nil {
			if e := s.cancelWorker(ctx, task); e != nil {
				// Durable cancel_requested_at is already set; surface but keep state.
				return user, fmt.Errorf("worker cancel for task %s: %w", task.ID, e)
			}
		}
	}
	return user, nil
}

func (s *userService) ListPendingUsers(ctx context.Context) ([]domain.User, error) {
	return s.store.ListPendingUsers(ctx)
}

// MaxUsernameLen is the application-enforced username byte limit.
const MaxUsernameLen = 64
