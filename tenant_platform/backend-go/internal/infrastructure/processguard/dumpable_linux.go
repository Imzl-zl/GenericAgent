//go:build linux

package processguard

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// DisablePeerInspection prevents same-UID child processes from reading the
// control plane's memory, environment, or /proc root after they are spawned.
func DisablePeerInspection() error {
	if err := unix.Prctl(unix.PR_SET_DUMPABLE, 0, 0, 0, 0); err != nil {
		return fmt.Errorf("set process non-dumpable: %w", err)
	}
	return nil
}
