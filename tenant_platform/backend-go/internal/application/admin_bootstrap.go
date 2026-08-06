package application

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/infrastructure/postgres"
)

// AdminBootstrapConfig is required for --dev-loopback startup.
type AdminBootstrapConfig struct {
	Enabled      bool
	UserID       int64
	Username     string
	AdminToken     string
	DatabaseURL  string
	PolicyFile   string
	ClaimLease   string // parsed by main
	RuntimeRoot  string
	ConfigRoot   string
	LegacyRoot   string
	WorkerPython string
	WorkerSrc    string
	ListenAddr   string
	// Sandbox Runner(方案 §7): 工作区根、镜像内只读 memory 模板、Runner 镜像。
	WorkspacesRoot string
	MemoryTemplate string
	RunnerImage    string
}

// DevTeamConfig bootstraps a minimal team workspace for multi-session testing.
// OwnerID is the primary dev user; MemberIDs are additional dev users that
// must already exist (bootstrap them via --dev-extra-users first).
type DevTeamConfig struct {
	TeamID    uuid.UUID
	TeamName  string
	OwnerID   int64
	MemberIDs []int64
}

// LoadAdminBootstrapFromEnv reads PLATFORM_ADMIN_* and related env for loopback mode.
func LoadAdminBootstrapFromEnv() (AdminBootstrapConfig, error) {
	cfg := AdminBootstrapConfig{
		DatabaseURL:  strings.TrimSpace(os.Getenv("DATABASE_URL")),
		AdminToken:     strings.TrimSpace(os.Getenv("PLATFORM_ADMIN_TOKEN")),
		Username:     strings.TrimSpace(os.Getenv("PLATFORM_ADMIN_USERNAME")),
		RuntimeRoot:  strings.TrimSpace(os.Getenv("GA_RUNTIME_DIR")),
		ConfigRoot:   strings.TrimSpace(os.Getenv("GA_CONFIG_ROOT")),
		LegacyRoot:   strings.TrimSpace(os.Getenv("GA_LEGACY_ROOT")),
		WorkerPython: strings.TrimSpace(os.Getenv("GA_WORKER_PYTHON")),
		WorkerSrc:    strings.TrimSpace(os.Getenv("GA_WORKER_SRC")),
		ListenAddr:   "127.0.0.1:8080",
	}
	if v := strings.TrimSpace(os.Getenv("PLATFORM_ADMIN_USER_ID")); v != "" {
		id, err := strconv.ParseInt(v, 10, 64)
		if err != nil || id <= 0 {
			return AdminBootstrapConfig{}, fmt.Errorf("PLATFORM_ADMIN_USER_ID must be a positive int64")
		}
		cfg.UserID = id
	}
	return cfg, nil
}

// EnsureAdminContext is the only foundation bootstrap mutation and is gated by Enabled.
func EnsureAdminContext(ctx context.Context, store *postgres.Store, cfg AdminBootstrapConfig) (postgres.AdminContext, error) {
	if !cfg.Enabled {
		return postgres.AdminContext{}, fmt.Errorf("development bootstrap requires --dev-loopback")
	}
	if store == nil {
		return postgres.AdminContext{}, fmt.Errorf("store is nil")
	}
	if cfg.UserID <= 0 {
		return postgres.AdminContext{}, fmt.Errorf("PLATFORM_ADMIN_USER_ID is required for --dev-loopback")
	}
	if strings.TrimSpace(cfg.AdminToken) == "" {
		return postgres.AdminContext{}, fmt.Errorf("PLATFORM_ADMIN_TOKEN is required for --dev-loopback")
	}
	return store.EnsureAdminContext(ctx, cfg.UserID, cfg.Username)
}

// EnsureDevTeam bootstraps a minimal team workspace for testing. The owner and
// all members must already exist as dev-loopback users. Returns the team
// session_key (team:<uuid>) for HTTP task submission.
func EnsureDevTeam(ctx context.Context, store *postgres.Store, cfg DevTeamConfig) (postgres.TeamContext, error) {
	if store == nil {
		return postgres.TeamContext{}, fmt.Errorf("store is nil")
	}
	if cfg.TeamID == uuid.Nil {
		return postgres.TeamContext{}, fmt.Errorf("team id is required")
	}
	if strings.TrimSpace(cfg.TeamName) == "" {
		return postgres.TeamContext{}, fmt.Errorf("team name is required")
	}
	if cfg.OwnerID <= 0 {
		return postgres.TeamContext{}, fmt.Errorf("owner id is required")
	}
	return store.EnsureTeamContext(ctx, cfg.TeamID, cfg.TeamName, cfg.OwnerID, cfg.MemberIDs)
}
