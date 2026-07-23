package application

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/postgres"
)

// DevBootstrapConfig is required for --dev-loopback startup.
type DevBootstrapConfig struct {
	Enabled        bool
	UserID         int64
	Username       string
	DevToken       string
	DatabaseURL    string
	PolicyFile     string
	ClaimLease     string // parsed by main
	RuntimeRoot    string
	ConfigRoot     string
	LegacyRoot     string
	WorkerPython   string
	WorkerSrc      string
	ListenAddr     string
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
