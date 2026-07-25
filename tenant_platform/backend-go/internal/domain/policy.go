// Package domain — admin-configurable platform commands and tool policies
// (migration 0004). These types back the command registry and tool policy
// store so admins can change behavior without recompiling the platform.
package domain

import (
	"time"
)

// CommandAction classifies how the router handles a command.
type CommandAction string

const (
	// CommandIntercept: the platform handles the command directly
	// (e.g. /stop cancels a task in PostgreSQL).
	CommandIntercept CommandAction = "intercept"
	// CommandPassthrough: forward the command text as a task to the Worker;
	// GA decides whether it's a valid agent command.
	CommandPassthrough CommandAction = "passthrough"
)

// PlatformCommand is an admin-configurable command entry.
// The router loads enabled commands and uses action to decide interception.
// Handler is a key (e.g. "stop", "status") mapped to a Go handler func;
// adding a brand-new handler key requires code, but
// enabling/disabling/reclassifying is admin-only.
type PlatformCommand struct {
	ID        int64
	Command   string        // "/stop", "/status", etc.
	Action    CommandAction // intercept | passthrough
	Handler   string        // handler key
	HelpText  string
	Enabled   bool
	SortOrder int
	UpdatedBy int64
	UpdatedAt time.Time
}

// ToolPolicy is an admin-configurable tool policy version.
// AllowedTools is a JSON array of tool names the Worker may invoke
// (e.g. ["update_working_checkpoint"], or ["code_run","file_read"]).
type ToolPolicy struct {
	ID           int64
	Version      string   // "foundation.no-host-tools.v1"
	AllowedTools []string // deserialized from JSONB
	Description  string
	Enabled      bool
	CreatedBy    int64
	CreatedAt    time.Time
	UpdatedAt    time.Time
}
