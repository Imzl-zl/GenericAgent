package application

import (
	"context"
	"testing"
	"time"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
	workerv1 "github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/gen/worker/v1"
	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/infrastructure/postgres"
	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/infrastructure/transport"
	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/infrastructure/workerclient"
)

// feishuConfigForTest 插入一条 owner=1 的飞书渠道配置(流式转发目标解析用)。
func feishuConfigForTest(t *testing.T, store *postgres.Store) domain.ChannelConfig {
	t.Helper()
	cfg, err := store.UpsertChannelConfigCredentials(context.Background(), 1, domain.ChannelFeishu, []byte("{}"), 1)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

// TestDispatchStreamsFeishuChunksAndCommits 端到端: feishu 私聊任务 chunk
// 流 → LoopbackTransport open/append(500ms 合并)/commit; 任务成功且
// stream_final_at 置位(delivery 文本 part 将跳过)。
func TestDispatchStreamsFeishuChunksAndCommits(t *testing.T) {
	_, store, reg, dev := serviceFixture(t)
	_ = feishuConfigForTest(t, store)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	task, err := store.SubmitTask(ctx, domain.SubmitTaskCommand{
		SessionKey: dev.SessionKey, RequesterUserID: 1,
		Source: domain.SourceFeishu, SourceInstanceID: "stream-owner", MessageID: "stream-owner-m",
		Prompt: "run", PersonaSnapshot: []string{}, ToolPolicyVersion: "foundation.no-host-tools.v1",
		ConversationKey: "oc_stream_1", ConversationType: domain.ConversationTypePrivate,
	})
	if err != nil {
		t.Fatal(err)
	}
	claimed, ok, err := store.ClaimNextTask(ctx, dev.SessionKey, "stream-owner", time.Second)
	if err != nil || !ok {
		t.Fatalf("claim: ok=%v err=%v", ok, err)
	}

	worker := newControlledWorker()
	worker.checkpointReady = &workerv1.CheckpointReady{
		StagingRef: "staging-success", Checksum: "sha256:bundle", ResultDigest: "sha256:result",
	}
	loopback := transport.NewLoopbackTransport()
	schedulerAPI, err := NewScheduler(SchedulerConfig{
		PlatformInstanceID: "stream-owner", ClaimLease: time.Second,
		Store: store, Registry: reg,
		Coordinator: &successfulCoordinator{store: store, owner: "stream-owner"},
		Streaming:   loopback, Bots: store,
		RuntimeSettings: &fakeAgentRuntimeSettings{maxTurns: 80, mode: domain.IMStreamingStreaming},
		DialWorker: func(context.Context, string) (workerclient.WorkerClient, func(string), error) {
			return worker, func(_ string) {}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	sched := schedulerAPI.(*scheduler)

	dispatchDone := make(chan error, 1)
	go func() { dispatchDone <- sched.dispatch(ctx, claimed) }()
	select {
	case <-worker.executeStarted:
	case <-ctx.Done():
		t.Fatal("worker execution did not start")
	}
	// 3 条连续 chunk(同窗口合并) + 成功终态。
	worker.events <- workerclient.WorkerEvent{Kind: workerclient.KindChunk, Chunk: &workerv1.Chunk{TaskId: task.ID, Text: "思考中"}}
	worker.events <- workerclient.WorkerEvent{Kind: workerclient.KindChunk, Chunk: &workerv1.Chunk{TaskId: task.ID, Text: "..."}}
	worker.events <- workerclient.WorkerEvent{Kind: workerclient.KindChunk, Chunk: &workerv1.Chunk{TaskId: task.ID, Text: "结果"}}
	worker.succeed()
	if err := <-dispatchDone; err != nil {
		t.Fatalf("dispatch: %v", err)
	}

	// 流式记录: open(携带合并首段) + commit, 无 append/abort。
	ops := loopback.SentStreams()
	var opsText []string
	for _, op := range ops {
		opsText = append(opsText, op.Op+":"+op.Text)
	}
	if len(ops) != 2 || ops[0].Op != "open" || ops[1].Op != "commit" {
		t.Fatalf("stream ops=%v, want open/commit (merged text carried by open)", opsText)
	}
	if ops[0].BotUUID == "" || ops[0].Target != "oc_stream_1" {
		t.Fatalf("open args=%+v, want target oc_stream_1", ops[0])
	}
	if ops[0].Text != "思考中...结果" {
		t.Fatalf("open firstText=%q, want merged %q", ops[0].Text, "思考中...结果")
	}

	// 任务成功 + stream_final_at 置位(delivery 文本 part 跳过依据)。
	final, err := store.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if final.Status != domain.TaskSucceeded {
		t.Fatalf("status=%s, want succeeded", final.Status)
	}
	if final.StreamFinalAt == nil {
		t.Fatal("stream_final_at not set after stream commit")
	}
	assertNoWorkerLeaks(t, sched)
}

// TestDispatchStreamAbortsOnFailureTerminal 失败终态: open/append 后 abort,
// 无 commit; stream_final_at 保持 nil(delivery 兜底补发最终结果)。
func TestDispatchStreamAbortsOnFailureTerminal(t *testing.T) {
	_, store, reg, dev := serviceFixture(t)
	_ = feishuConfigForTest(t, store)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	task, err := store.SubmitTask(ctx, domain.SubmitTaskCommand{
		SessionKey: dev.SessionKey, RequesterUserID: 1,
		Source: domain.SourceFeishu, SourceInstanceID: "stream-fail", MessageID: "stream-fail-m",
		Prompt: "run", PersonaSnapshot: []string{}, ToolPolicyVersion: "foundation.no-host-tools.v1",
		ConversationKey: "oc_stream_2", ConversationType: domain.ConversationTypePrivate,
	})
	if err != nil {
		t.Fatal(err)
	}
	claimed, ok, err := store.ClaimNextTask(ctx, dev.SessionKey, "stream-fail", time.Second)
	if err != nil || !ok {
		t.Fatalf("claim: ok=%v err=%v", ok, err)
	}

	worker := newControlledWorker()
	loopback := transport.NewLoopbackTransport()
	schedulerAPI, err := NewScheduler(SchedulerConfig{
		PlatformInstanceID: "stream-fail", ClaimLease: time.Second,
		Store: store, Registry: reg,
		Streaming: loopback, Bots: store,
		RuntimeSettings: &fakeAgentRuntimeSettings{maxTurns: 80, mode: domain.IMStreamingStreaming},
		DialWorker: func(context.Context, string) (workerclient.WorkerClient, func(string), error) {
			return worker, func(_ string) {}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	sched := schedulerAPI.(*scheduler)

	dispatchDone := make(chan error, 1)
	go func() { dispatchDone <- sched.dispatch(ctx, claimed) }()
	select {
	case <-worker.executeStarted:
	case <-ctx.Done():
		t.Fatal("worker execution did not start")
	}
	worker.events <- workerclient.WorkerEvent{Kind: workerclient.KindChunk, Chunk: &workerv1.Chunk{TaskId: task.ID, Text: "开头"}}
	// 等窗口到期(500ms)后发心跳(空 chunk)——心跳驱动缓冲 flush → open+append。
	select {
	case <-time.After(700 * time.Millisecond):
	case <-ctx.Done():
		t.Fatal("ctx done while waiting for throttle window")
	}
	worker.events <- workerclient.WorkerEvent{Kind: workerclient.KindChunk, Chunk: &workerv1.Chunk{TaskId: task.ID, Text: ""}}
	worker.fail("TASK_FAILED", "boom")
	if err := <-dispatchDone; err != nil {
		t.Fatalf("dispatch: %v", err)
	}

	ops := loopback.SentStreams()
	if len(ops) != 2 || ops[0].Op != "open" || ops[1].Op != "abort" {
		t.Fatalf("stream ops=%+v, want open(firstText)/abort", ops)
	}
	final, err := store.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if final.Status != domain.TaskFailed {
		t.Fatalf("status=%s, want failed", final.Status)
	}
	if final.StreamFinalAt != nil {
		t.Fatal("stream_final_at must stay nil on failure (delivery fallback)")
	}
	assertNoWorkerLeaks(t, sched)
}

// TestDispatchSkipsStreamForGroupConversation 群聊收敛: feishu 群任务
// chunk 不触发任何流式转发, 全部走终态 delivery。
func TestDispatchSkipsStreamForGroupConversation(t *testing.T) {
	_, store, reg, dev := serviceFixture(t)
	_ = feishuConfigForTest(t, store)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	task, err := store.SubmitTask(ctx, domain.SubmitTaskCommand{
		SessionKey: dev.SessionKey, RequesterUserID: 1,
		Source: domain.SourceFeishu, SourceInstanceID: "stream-group", MessageID: "stream-group-m",
		Prompt: "run", PersonaSnapshot: []string{}, ToolPolicyVersion: "foundation.no-host-tools.v1",
		ConversationKey: "oc_group_1", ConversationType: domain.ConversationTypeGroup,
	})
	if err != nil {
		t.Fatal(err)
	}
	claimed, ok, err := store.ClaimNextTask(ctx, dev.SessionKey, "stream-group", time.Second)
	if err != nil || !ok {
		t.Fatalf("claim: ok=%v err=%v", ok, err)
	}

	worker := newControlledWorker()
	worker.checkpointReady = &workerv1.CheckpointReady{
		StagingRef: "staging-success", Checksum: "sha256:bundle", ResultDigest: "sha256:result",
	}
	loopback := transport.NewLoopbackTransport()
	schedulerAPI, err := NewScheduler(SchedulerConfig{
		PlatformInstanceID: "stream-group", ClaimLease: time.Second,
		Store: store, Registry: reg,
		Coordinator: &successfulCoordinator{store: store, owner: "stream-group"},
		Streaming:   loopback, Bots: store,
		RuntimeSettings: &fakeAgentRuntimeSettings{maxTurns: 80, mode: domain.IMStreamingStreaming},
		DialWorker: func(context.Context, string) (workerclient.WorkerClient, func(string), error) {
			return worker, func(_ string) {}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	sched := schedulerAPI.(*scheduler)

	dispatchDone := make(chan error, 1)
	go func() { dispatchDone <- sched.dispatch(ctx, claimed) }()
	select {
	case <-worker.executeStarted:
	case <-ctx.Done():
		t.Fatal("worker execution did not start")
	}
	worker.events <- workerclient.WorkerEvent{Kind: workerclient.KindChunk, Chunk: &workerv1.Chunk{TaskId: task.ID, Text: "群聊文本"}}
	worker.succeed()
	if err := <-dispatchDone; err != nil {
		t.Fatalf("dispatch: %v", err)
	}

	if ops := loopback.SentStreams(); len(ops) != 0 {
		t.Fatalf("group conversation must not stream, got ops=%+v", ops)
	}
	final, err := store.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if final.Status != domain.TaskSucceeded || final.StreamFinalAt != nil {
		t.Fatalf("status=%s stream_final_at=%v, want succeeded + nil", final.Status, final.StreamFinalAt)
	}
	assertNoWorkerLeaks(t, sched)
}
