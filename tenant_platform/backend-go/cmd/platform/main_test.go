package main

import (
	"testing"
	"time"
)

func TestDefaultBotReconcileIntervalEnv(t *testing.T) {
	t.Setenv("BOT_RECONCILE_INTERVAL", "30s")
	if d := defaultBotReconcileInterval(); d != 30*time.Second {
		t.Fatalf("expected 30s, got %v", d)
	}
	t.Setenv("BOT_RECONCILE_INTERVAL", "garbage")
	if d := defaultBotReconcileInterval(); d != 60*time.Second {
		t.Fatalf("invalid env must fall back to 60s, got %v", d)
	}
	t.Setenv("BOT_RECONCILE_INTERVAL", "-5s")
	if d := defaultBotReconcileInterval(); d != 60*time.Second {
		t.Fatalf("negative env must fall back to 60s, got %v", d)
	}
	t.Setenv("BOT_RECONCILE_INTERVAL", "")
	if d := defaultBotReconcileInterval(); d != 60*time.Second {
		t.Fatalf("unset env must default to 60s, got %v", d)
	}
}
