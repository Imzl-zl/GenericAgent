package application

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
)

// ---------------------------------------------------------------------------
// round11 审查 C3: 移除全局 workerCallMu 后,
//   1) 一个 session 卡住的 StartSession 不得阻塞其他 session 的取消/派发;
//   2) 同一 session 的 StartSession 与 CancelTask 仍串行(per-entry 锁);
//   3) StartSession RPC 有独立超时, 卡住时只终止本 session 派发。
// ---------------------------------------------------------------------------

// TestCancelWorkerNotBlockedByStuckStartSessionInOtherSession is the core
// regression test for the global-lock removal: a stuck StartSession in one
// workspace must not block CancelWorker for another workspace.
func TestCancelWorkerNotBlockedByStuckStartSessionInOtherSession(t *testing.T) {
	registry := testPolicyRegistry(t)
	stuck := newControlledWorker()
	stuck.startSessionEntered = make(chan struct{})
	stuck.releaseStartSession = make(chan struct{}) // 永不释放: 模拟卡住 RPC
	other := newControlledWorker()

	stuckEntry := &workerEntry{client: stuck, sessionKey: "personal:1"}
	otherEntry := &workerEntry{client: other, sessionKey: "personal:2"}
	s := &scheduler{
		cfg:     SchedulerConfig{Registry: registry},
		workers: map[string]*workerEntry{"personal:1": stuckEntry, "personal:2": otherEntry},
	}

	startDone := make(chan error, 1)
	go func() {
		startDone <- s.startSessionOnWorker(context.Background(), domain.Task{SessionKey: "personal:1"})
	}()
	select {
	case <-stuck.startSessionEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("stuck StartSession did not enter the RPC")
	}

	cancelDone := make(chan error, 1)
	go func() {
		cancelDone <- s.CancelWorker(context.Background(), domain.Task{ID: "task-2", SessionKey: "personal:2"})
	}()
	select {
	case err := <-cancelDone:
		if err != nil {
			t.Fatalf("cancel failed: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("CancelWorker blocked by a stuck StartSession in another session")
	}

	// 收尾: 释放卡住的 StartSession, 避免 goroutine 泄漏。
	close(stuck.releaseStartSession)
	select {
	case err := <-startDone:
		if err != nil {
			t.Fatalf("StartSession should complete after release: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("stuck StartSession did not return after release")
	}
}

// TestCancelWorkerWaitsForSameSessionStartSession verifies the per-session
// serialization contract is preserved: cancel for the SAME session waits for
// its in-flight StartSession before issuing CancelTask.
func TestCancelWorkerWaitsForSameSessionStartSession(t *testing.T) {
	registry := testPolicyRegistry(t)
	worker := newControlledWorker()
	worker.startSessionEntered = make(chan struct{})
	worker.releaseStartSession = make(chan struct{})

	entry := &workerEntry{client: worker, sessionKey: "personal:1"}
	s := &scheduler{
		cfg:     SchedulerConfig{Registry: registry},
		workers: map[string]*workerEntry{"personal:1": entry},
	}

	startDone := make(chan error, 1)
	go func() {
		startDone <- s.startSessionOnWorker(context.Background(), domain.Task{SessionKey: "personal:1"})
	}()
	select {
	case <-worker.startSessionEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("StartSession did not enter")
	}

	cancelDone := make(chan error, 1)
	go func() {
		cancelDone <- s.CancelWorker(context.Background(), domain.Task{ID: "task-1", SessionKey: "personal:1"})
	}()
	select {
	case err := <-cancelDone:
		t.Fatalf("same-session cancel must wait for StartSession, returned early: %v", err)
	case <-time.After(300 * time.Millisecond):
		// expected: cancel waits
	}

	close(worker.releaseStartSession)
	if err := <-startDone; err != nil {
		t.Fatalf("StartSession failed: %v", err)
	}
	select {
	case err := <-cancelDone:
		if err != nil {
			t.Fatalf("cancel after StartSession failed: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancel did not complete after StartSession finished")
	}
}

// TestStartSessionOnWorkerHonorsTimeout verifies a hung StartSession RPC is
// bounded by StartSessionTimeout and fails the dispatch for that session only.
func TestStartSessionOnWorkerHonorsTimeout(t *testing.T) {
	registry := testPolicyRegistry(t)
	worker := newControlledWorker()
	worker.startSessionEntered = make(chan struct{})
	worker.releaseStartSession = make(chan struct{}) // 永不释放

	entry := &workerEntry{client: worker, sessionKey: "personal:t"}
	s := &scheduler{
		cfg: SchedulerConfig{
			Registry:            registry,
			StartSessionTimeout: 300 * time.Millisecond,
		},
		workers: map[string]*workerEntry{"personal:t": entry},
	}

	err := s.startSessionOnWorker(context.Background(), domain.Task{SessionKey: "personal:t"})
	if err == nil {
		t.Fatal("expected StartSession timeout error")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected context deadline exceeded, got %v", err)
	}
}
