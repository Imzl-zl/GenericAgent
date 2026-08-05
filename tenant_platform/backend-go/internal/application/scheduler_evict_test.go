package application

import (
	"testing"
	"time"
)

// TestEvictWorkerAfterFailureLockedNoDeadlock 回归测试: completeSuccess 在
// 持有 entry.lifecycleMu 的错误分支调用 evictWorkerAfterFailureLocked,
// 不得再次获取同一把锁(否则自死锁, 永久卡死该工作区)。
func TestEvictWorkerAfterFailureLockedNoDeadlock(t *testing.T) {
	s := &scheduler{workers: map[string]*workerEntry{
		"session-a": {sessionKey: "session-a"},
	}}
	entry := s.workers["session-a"]

	done := make(chan struct{})
	go func() {
		// 模拟 completeSuccess: 先持锁, 再在锁内 evict。
		entry.lifecycleMu.Lock()
		defer entry.lifecycleMu.Unlock()
		s.destroyTaskWorkerLocked("session-a", entry)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("destroyTaskWorkerLocked deadlocked while holding lifecycleMu")
	}
	s.mu.Lock()
	_, stillPresent := s.workers["session-a"]
	s.mu.Unlock()
	if stillPresent {
		t.Fatal("worker entry was not removed after locked evict")
	}
}

// TestEvictWorkerAfterFailurePublicUncontended 公开版在未持锁时正常移除。
func TestEvictWorkerAfterFailurePublicUncontended(t *testing.T) {
	s := &scheduler{workers: map[string]*workerEntry{
		"session-b": {sessionKey: "session-b"},
	}}
	done := make(chan struct{})
	go func() {
		s.destroyTaskWorker("session-b")
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("destroyTaskWorker deadlocked")
	}
}
