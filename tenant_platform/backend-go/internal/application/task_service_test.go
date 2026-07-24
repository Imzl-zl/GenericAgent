package application

import (
	"context"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/policy"
	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/postgres"
)

func foundationPolicy(t *testing.T) string {
	t.Helper()
	_, file, _, _ := runtime.Caller(0)
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", "..", "..", "contracts", "policy", "foundation.v1.json"))
}

func serviceFixture(t *testing.T) (TaskService, *postgres.Store, policy.Registry, postgres.DevelopmentContext) {
	t.Helper()
	pool := postgres.OpenTestPool(t)
	store, err := postgres.NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	reg, err := policy.LoadRegistry(foundationPolicy(t))
	if err != nil {
		t.Fatal(err)
	}
	dev, err := store.EnsureDevelopmentContext(context.Background(), 1, "dev")
	if err != nil {
		t.Fatal(err)
	}
	svc, err := NewTaskService(TaskServiceConfig{
		Store: store, Registry: reg, PlatformInstanceID: "svc-instance", ClaimLease: time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	return svc, store, reg, dev
}

func TestTaskService_RejectsUnknownPolicy(t *testing.T) {
	svc, _, _, dev := serviceFixture(t)
	_, err := svc.SubmitTask(context.Background(), domain.SubmitTaskCommand{
		SessionKey: dev.SessionKey, RequesterUserID: 1,
		Source: "web", SourceInstanceID: "i", MessageID: "m",
		Prompt: "x", PersonaSnapshot: []string{}, ToolPolicyVersion: "host-tools.v1",
	})
	if err == nil {
		t.Fatal("expected policy rejection")
	}
}

func TestTaskService_ClaimRoundTripsDurableEnvelope(t *testing.T) {
	svc, store, _, dev := serviceFixture(t)
	ctx := context.Background()
	prompt := "durable-prompt-value"
	persona := []string{"persona-A", "persona-B"}
	task, err := svc.SubmitTask(ctx, domain.SubmitTaskCommand{
		SessionKey: dev.SessionKey, RequesterUserID: 1,
		Source: "web", SourceInstanceID: "i", MessageID: "m-rt",
		Prompt: prompt, PersonaSnapshot: persona, ToolPolicyVersion: "foundation.no-host-tools.v1",
	})
	if err != nil {
		t.Fatal(err)
	}
	claimed, ok, err := store.ClaimNextTask(ctx, dev.SessionKey, "new-proc", time.Minute)
	if err != nil || !ok {
		t.Fatalf("claim: ok=%v err=%v", ok, err)
	}
	if claimed.ID != task.ID {
		t.Fatal("id")
	}
	if claimed.Prompt != prompt {
		t.Fatalf("prompt=%q", claimed.Prompt)
	}
	if len(claimed.PersonaSnapshot) != 2 || claimed.PersonaSnapshot[0] != "persona-A" {
		t.Fatalf("persona=%v", claimed.PersonaSnapshot)
	}
	if claimed.ToolPolicyVersion != "foundation.no-host-tools.v1" {
		t.Fatalf("policy=%s", claimed.ToolPolicyVersion)
	}
}

func TestNewScheduler_RejectsEmptyIDAndLease(t *testing.T) {
	_, store, reg, _ := serviceFixture(t)
	if _, err := NewScheduler(SchedulerConfig{PlatformInstanceID: "", ClaimLease: time.Second, Store: store, Registry: reg}); err == nil {
		t.Fatal("empty id")
	}
	if _, err := NewScheduler(SchedulerConfig{PlatformInstanceID: "x", ClaimLease: 0, Store: store, Registry: reg}); err == nil {
		t.Fatal("zero lease")
	}
}
