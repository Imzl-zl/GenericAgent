package application

import (
	"errors"
	"io"
	"regexp"
	"testing"
)

var uuidV4RE = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func TestNewPlatformInstanceID_LowercaseRFC4122(t *testing.T) {
	id, err := NewPlatformInstanceID()
	if err != nil {
		t.Fatalf("NewPlatformInstanceID: %v", err)
	}
	if !uuidV4RE.MatchString(id) {
		t.Fatalf("id %q is not lowercase RFC 4122 UUID v4", id)
	}
}

func TestNewPlatformInstanceID_Distinct(t *testing.T) {
	a, err := NewPlatformInstanceID()
	if err != nil {
		t.Fatal(err)
	}
	b, err := NewPlatformInstanceID()
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Fatalf("two process-start IDs collided: %s", a)
	}
}

type failReader struct{}

func (failReader) Read([]byte) (int, error) {
	return 0, errors.New("entropy unavailable")
}

func TestNewPlatformInstanceID_EntropyFailure(t *testing.T) {
	_, err := newPlatformInstanceID(failReader{})
	if err == nil {
		t.Fatal("expected entropy failure")
	}
	_, err = newPlatformInstanceID(nil)
	if err == nil {
		t.Fatal("expected nil reader failure")
	}
	// Partial read must also fail.
	_, err = newPlatformInstanceID(io.LimitReader(failReader{}, 0))
	if err == nil {
		t.Fatal("expected short-read failure")
	}
}
