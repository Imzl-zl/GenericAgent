package application

import (
	"testing"
	"time"
)

func TestSchedulerConfigValidation(t *testing.T) {
	// Covered primarily in task_service_test; keep explicit package-local case.
	if _, err := NewScheduler(SchedulerConfig{}); err == nil {
		t.Fatal("expected validation error")
	}
	if _, err := NewScheduler(SchedulerConfig{PlatformInstanceID: "a", ClaimLease: -1}); err == nil {
		t.Fatal("expected negative lease error")
	}
	_ = time.Second
}
