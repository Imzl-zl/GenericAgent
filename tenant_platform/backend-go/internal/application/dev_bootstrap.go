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

// DevBootstrapConfig is required for --dev-loopback startup.
type DevBootstrapConfig struct {
	Enabled      bool
	UserID       int64
	Username     string
	DevToken     string
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

// LoadDevBootstrapFromEnv reads PLATFORM_DEV_* and related env for loopback mode.
func LoadDevBootstrapFromEnv() (DevBootstrapConfig, error) {
	cfg := DevBootstrapConfig{
		DatabaseURL:  strings.TrimSpace(os.Getenv("DATABASE_URL")),
		DevToken:     strings.TrimSpace(os.Getenv("PLATFORM_DEV_TOKEN")),
		Username:     strings.TrimSpace(os.Getenv("PLATFORM_DEV_USERNAME")),
		RuntimeRoot:  strings.TrimSpace(os.Getenv("GA_RUNTIME_DIR")),
		ConfigRoot:   strings.TrimSpace(os.Getenv("GA_CONFIG_ROOT")),
		LegacyRoot:   strings.TrimSpace(os.Getenv("GA_LEGACY_ROOT")),
		WorkerPython: strings.TrimSpace(os.Getenv("GA_WORKER_PYTHON")),
		WorkerSrc:    strings.TrimSpace(os.Getenv("GA_WORKER_SRC")),
		ListenAddr:   "127.0.0.1:8080",
	}
	if v := strings.TrimSpace(os.Getenv("PLATFORM_DEV_USER_ID")); v != "" {
		id, err := strconv.ParseInt(v, 10, 64)
		if err != nil || id <= 0 {
			return DevBootstrapConfig{}, fmt.Errorf("PLATFORM_DEV_USER_ID must be a positive int64")
		}
		cfg.UserID = id
	}
	return cfg, nil
}

// EnsureDevelopmentContext is the only foundation bootstrap mutation and is gated by Enabled.
func EnsureDevelopmentContext(ctx context.Context, store *postgres.Store, cfg DevBootstrapConfig) (postgres.DevelopmentContext, error) {
	if !cfg.Enabled {
		return postgres.DevelopmentContext{}, fmt.Errorf("development bootstrap requires --dev-loopback")
	}
	if store == nil {
		return postgres.DevelopmentContext{}, fmt.Errorf("store is nil")
	}
	if cfg.UserID <= 0 {
		return postgres.DevelopmentContext{}, fmt.Errorf("PLATFORM_DEV_USER_ID is required for --dev-loopback")
	}
	if strings.TrimSpace(cfg.DevToken) == "" {
		return postgres.DevelopmentContext{}, fmt.Errorf("PLATFORM_DEV_TOKEN is required for --dev-loopback")
	}
	return store.EnsureDevelopmentContext(ctx, cfg.UserID, cfg.Username)
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
