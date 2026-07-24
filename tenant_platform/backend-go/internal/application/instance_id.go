package application

import (
	"crypto/rand"
	"fmt"
	"io"
)

// NewPlatformInstanceID creates a lowercase RFC 4122 UUID using crypto/rand.
// Returns an error if system randomness is unavailable. No empty-ID fallback.
func NewPlatformInstanceID() (string, error) {
	return newPlatformInstanceID(rand.Reader)
}

func newPlatformInstanceID(r io.Reader) (string, error) {
	if r == nil {
		return "", fmt.Errorf("platform instance id: randomness reader is nil")
	}
	var b [16]byte
	if _, err := io.ReadFull(r, b[:]); err != nil {
		return "", fmt.Errorf("platform instance id: read entropy: %w", err)
	}
	// RFC 4122 version 4 + variant bits.
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}
