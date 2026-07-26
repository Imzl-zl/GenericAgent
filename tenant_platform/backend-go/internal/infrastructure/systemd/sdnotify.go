// Package systemd provides a minimal sd_notify(3) client using only the Go
// standard library. It speaks the systemd notification protocol over the
// UNIX datagram socket pointed to by NOTIFY_SOCKET.
//
// Two states are notified:
//   - READY=1   once the process has finished startup and is serving traffic
//   - WATCHDOG=1 periodically to keep systemd's WatchdogSec watchdog alive
//
// When NOTIFY_SOCKET is unset (process not started under systemd Type=notify),
// all calls are no-ops, so the same binary runs unchanged under bare metal,
// containers, and systemd.
//
// Reference: systemd.service(5), sd_notify(3).
package systemd

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"time"
)

// State is a single sd_notify state directive (e.g. "READY=1", "WATCHDOG=1").
type State string

const (
	// Ready marks the service as having completed startup.
	Ready State = "READY=1"
	// Watchdog resets the systemd watchdog timer. Send before WatchdogSec elapses.
	Watchdog State = "WATCHDOG=1"
	// Stopping marks the service as beginning shutdown. Stops the watchdog.
	Stopping State = "STOPPING=1"
)

// Notifier sends sd_notify states. The zero value is a no-op notifier safe
// for concurrent use. Use NewNotifier to construct one bound to NOTIFY_SOCKET.
type Notifier struct {
	mu     sync.Mutex
	conn   net.Conn
	addr   string
	noop   bool
	stopCh chan struct{}
}

// NewNotifier returns a Notifier bound to $NOTIFY_SOCKET. When the variable is
// unset (process not running under systemd Type=notify), the returned Notifier
// silently no-ops every call. This lets the same binary run anywhere.
func NewNotifier() *Notifier {
	addr := strings.TrimSpace(os.Getenv("NOTIFY_SOCKET"))
	if addr == "" {
		return &Notifier{noop: true}
	}
	// Linux abstract namespace addresses start with '@'; replace with NUL.
	// Concrete filesystem paths are used as-is.
	if addr[0] == '@' {
		addr = "\x00" + addr[1:]
	}
	conn, err := net.Dial("unixgram", addr)
	if err != nil {
		// Don't fail startup just because we couldn't talk to systemd.
		// systemd will hit WatchdogSec and restart us, which surfaces the
		// problem visibly. Log via fmt.Fprintf so operators see it.
		fmt.Fprintf(os.Stderr, "sdnotify: dial %s failed (degraded to no-op): %v\n", addr, err)
		return &Notifier{noop: true}
	}
	return &Notifier{conn: conn, addr: addr, stopCh: make(chan struct{})}
}

// Notify sends one or more state directives to systemd. Errors are surfaced
// (no silent fallback) but never panic. A no-op Notifier returns nil.
func (n *Notifier) Notify(states ...State) error {
	if n == nil || n.noop {
		return nil
	}
	if len(states) == 0 {
		return nil
	}
	payload := make([]byte, 0, 64)
	for i, s := range states {
		if i > 0 {
			payload = append(payload, '\n')
		}
		payload = append(payload, []byte(string(s))...)
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.conn == nil {
		return errors.New("sdnotify: connection closed")
	}
	if _, err := n.conn.Write(payload); err != nil {
		return fmt.Errorf("sdnotify write: %w", err)
	}
	return nil
}

// StartWatchdog spawns a goroutine that periodically sends WATCHDOG=1. The
// interval is derived from the WatchdogUSec environment hint (systemd sets
// WATCHDOG_USEC= when WatchdogSec is configured); we use half that interval
// to leave headroom. If no hint is present, the goroutine is not started.
// Call StopWatchdog to clean up the goroutine and close the socket.
func (n *Notifier) StartWatchdog(interval time.Duration) {
	if n == nil || n.noop || interval <= 0 {
		return
	}
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-n.stopCh:
				return
			case <-ticker.C:
				_ = n.Notify(Watchdog)
			}
		}
	}()
}

// StopWatchdog stops the watchdog goroutine and closes the socket. Safe to
// call multiple times. Sends STOPPING=1 before closing so systemd knows the
// shutdown is intentional and doesn't trigger Restart=on-failure.
func (n *Notifier) StopWatchdog() {
	if n == nil || n.noop {
		return
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	select {
	case <-n.stopCh:
		// already closed
	default:
		close(n.stopCh)
	}
	if n.conn != nil {
		// Best-effort STOPPING; ignore error since we're tearing down anyway.
		_, _ = n.conn.Write([]byte("STOPPING=1"))
		_ = n.conn.Close()
		n.conn = nil
	}
}

// WatchdogIntervalFromEnv parses $WATCHDOG_USEC (microseconds, set by systemd
// when WatchdogSec is configured) and returns half the value as the recommended
// ping interval. Returns 0 when the env var is absent or unparsable.
func WatchdogIntervalFromEnv() time.Duration {
	raw := strings.TrimSpace(os.Getenv("WATCHDOG_USEC"))
	if raw == "" {
		return 0
	}
	us, err := time.ParseDuration(raw + "us")
	if err != nil {
		return 0
	}
	return us / 2
}

// ReadyAndServe notifies systemd READY=1, starts the watchdog (when
// WATCHDOG_USEC is set), and blocks until ctx is cancelled. On shutdown it
// sends STOPPING=1. Callers pass the actual serve function so this helper
// can wrap any server (HTTP, gRPC) uniformly. Errors from serve are
// surfaced unchanged.
func ReadyAndServe(ctx context.Context, serve func() error) error {
	n := NewNotifier()
	interval := WatchdogIntervalFromEnv()
	n.StartWatchdog(interval)
	defer n.StopWatchdog()
	if err := n.Notify(Ready); err != nil {
		// Log but don't fail; systemd will restart us if READY never arrives.
		fmt.Fprintf(os.Stderr, "sdnotify: READY failed: %v\n", err)
	}
	err := serve()
	// Don't bother sending STOPPING again; StopWatchdog already did it.
	return err
}
