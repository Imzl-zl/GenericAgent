//go:build !linux

package processguard

// DisablePeerInspection is enforced only by the Linux deployment target.
func DisablePeerInspection() error {
	return nil
}
