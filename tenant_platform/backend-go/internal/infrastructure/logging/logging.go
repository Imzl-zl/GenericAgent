// Package logging initializes the process-wide structured logger.
//
// The platform uses log/slog (Go 1.21+ standard library) with a JSON handler
// writing to stdout. systemd's journal captures stdout automatically, so no
// separate logrotate is needed. Every log line carries stable fields
// (task_id, session_key, user_id, bot_id, error, duration_ms) when relevant,
// so operators can grep and aggregate without parsing free-form text.
//
// Callers use the package-level slog functions (slog.InfoContext, etc.),
// which dispatch to slog.Default(). Init() sets that default once at startup.
package logging

import (
	"log"
	"log/slog"
	"os"
	"strings"
)

// Level names accepted by LOG_LEVEL. Empty defaults to INFO.
var levelByName = map[string]slog.Level{
	"DEBUG": slog.LevelDebug,
	"INFO":  slog.LevelInfo,
	"WARN":  slog.LevelWarn,
	"ERROR": slog.LevelError,
}

// Init configures slog.Default() with a JSON handler writing to stdout.
// LOG_LEVEL (DEBUG/INFO/WARN/ERROR) controls verbosity; absent or invalid
// values fall back to INFO. The standard log package is redirected through
// slog so legacy log.Printf calls still emit JSON until they're migrated.
func Init() {
	level := levelByName[strings.ToUpper(strings.TrimSpace(os.Getenv("LOG_LEVEL")))]
	if level == 0 {
		level = slog.LevelInfo
	}
	handler := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level})
	logger := slog.New(handler)
	slog.SetDefault(logger)
	// Bridge legacy log.Printf calls through slog so they emit JSON too.
	// slog.NewLogLogger returns a *log.Logger backed by the JSON handler;
	// .Writer() exposes it as an io.Writer for log.SetOutput.
	log.SetFlags(0)
	log.SetOutput(slog.NewLogLogger(handler, level).Writer())
}
