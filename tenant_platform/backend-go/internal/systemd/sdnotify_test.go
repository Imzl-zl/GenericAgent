package systemd

import (
	"os"
	"testing"
	"time"
)

// TestNotifierNoopWhenSocketUnset verifies that when NOTIFY_SOCKET is unset
// (process not under systemd), all calls are safe no-ops. This is the path
// exercised on Windows, macOS, and bare-metal Linux.
func TestNotifierNoopWhenSocketUnset(t *testing.T) {
	t.Setenv("NOTIFY_SOCKET", "")
	n := NewNotifier()
	if !n.noop {
		t.Fatalf("expected noop notifier when NOTIFY_SOCKET unset")
	}
	if err := n.Notify(Ready); err != nil {
		t.Fatalf("noop Notify returned error: %v", err)
	}
	// StartWatchdog with any interval must not panic and must not leak goroutines.
	n.StartWatchdog(10 * time.Millisecond)
	n.StopWatchdog()
}

// TestWatchdogIntervalFromEnvMissing verifies the env hint parser returns 0
// when WATCHDOG_USEC is absent, so StartWatchdog becomes a no-op.
func TestWatchdogIntervalFromEnvMissing(t *testing.T) {
	t.Setenv("WATCHDOG_USEC", "")
	if got := WatchdogIntervalFromEnv(); got != 0 {
		t.Fatalf("expected 0 interval when env unset, got %v", got)
	}
}

// TestWatchdogIntervalFromEnvValid verifies the env hint parser computes half
// the configured microsecond value (so we ping well before WatchdogSec elapses).
func TestWatchdogIntervalFromEnvValid(t *testing.T) {
	t.Setenv("WATCHDOG_USEC", "30000000") // 30s
	got := WatchdogIntervalFromEnv()
	want := 15 * time.Second
	if got != want {
		t.Fatalf("interval = %v, want %v", got, want)
	}
}

// TestReadyAndServeNoSocket verifies ReadyAndServe calls serve() and returns
// its error when NOTIFY_SOCKET is unset, without blocking on systemd.
func TestReadyAndServeNoSocket(t *testing.T) {
	t.Setenv("NOTIFY_SOCKET", "")
	called := false
	err := ReadyAndServe(nil, func() error {
		called = true
		return os.ErrInvalid
	})
	if !called {
		t.Fatal("serve was not called")
	}
	if err != os.ErrInvalid {
		t.Fatalf("expected os.ErrInvalid, got %v", err)
	}
}
