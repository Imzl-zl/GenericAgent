package application

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/infrastructure/transport"
)

// ---------------------------------------------------------------------------
// fakes
// ---------------------------------------------------------------------------

type fakeStreamReply struct {
	appends []string
	commits int
	aborts  int
	// appendErr 注入 append 失败。
	appendErr error
	// commitErr 注入 commit 失败。
	commitErr error
}

func (r *fakeStreamReply) Append(_ context.Context, text string) error {
	if r.appendErr != nil {
		return r.appendErr
	}
	r.appends = append(r.appends, text)
	return nil
}

func (r *fakeStreamReply) Commit(_ context.Context) error {
	if r.commitErr != nil {
		return r.commitErr
	}
	r.commits++
	return nil
}

func (r *fakeStreamReply) Abort(_ context.Context) error {
	r.aborts++
	return nil
}

type fakeStreamingSender struct {
	opens     []struct{ botUUID, target, clientID string }
	replies   []*fakeStreamReply
	beginErr  error
}

func (f *fakeStreamingSender) BeginReply(_ context.Context, botUUID, target, clientID string) (transport.StreamReply, error) {
	if f.beginErr != nil {
		return nil, f.beginErr
	}
	f.opens = append(f.opens, struct{ botUUID, target, clientID string }{botUUID, target, clientID})
	r := &fakeStreamReply{}
	f.replies = append(f.replies, r)
	return r, nil
}

func taskFor(source, convType string) domain.Task {
	return domain.Task{
		ID:               "task-1",
		RequesterID:      1,
		Source:           source,
		ConversationKey:  "conv-1",
		ConversationType: convType,
	}
}

func newTestForwarder(streaming transport.StreamingSender, settings RuntimeStreamSettings, task domain.Task) *StreamForwarder {
	bot := domain.ChannelConfig{BotUUID: "bot-" + task.Source, ChannelType: domain.ChannelType(task.Source), State: domain.ChannelActive}
	return NewStreamForwarder(streaming, &fakeBotResolver{bot: bot}, settings, task)
}

// ---------------------------------------------------------------------------
// Enabled 判定矩阵
// ---------------------------------------------------------------------------

func TestStreamForwarderEnabledMatrix(t *testing.T) {
	sender := &fakeStreamingSender{}
	settings := &fakeAgentRuntimeSettings{mode: domain.IMStreamingStreaming}

	cases := []struct {
		name      string
		source    string
		convType  string
		mode      domain.IMStreamingMode
		streaming transport.StreamingSender
		want      bool
	}{
		{"feishu private streaming", domain.SourceFeishu, domain.ConversationTypePrivate, domain.IMStreamingStreaming, sender, true},
		{"qq private streaming", domain.SourceQQ, domain.ConversationTypePrivate, domain.IMStreamingStreaming, sender, true},
		{"feishu group converged", domain.SourceFeishu, domain.ConversationTypeGroup, domain.IMStreamingStreaming, sender, false},
		{"qq group converged", domain.SourceQQ, domain.ConversationTypeGroup, domain.IMStreamingStreaming, sender, false},
		{"wechat non-stream", domain.SourceWechat, domain.ConversationTypePrivate, domain.IMStreamingStreaming, sender, false},
		{"dingtalk v1 final only", domain.SourceDingTalk, domain.ConversationTypePrivate, domain.IMStreamingStreaming, sender, false},
		{"web no im", domain.SourceWeb, domain.ConversationTypePrivate, domain.IMStreamingStreaming, sender, false},
		{"mode off", domain.SourceFeishu, domain.ConversationTypePrivate, domain.IMStreamingOff, sender, false},
		{"mode final_only", domain.SourceFeishu, domain.ConversationTypePrivate, domain.IMStreamingFinalOnly, sender, false},
		{"streaming nil", domain.SourceFeishu, domain.ConversationTypePrivate, domain.IMStreamingStreaming, nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newTestForwarder(tc.streaming, settings, taskFor(tc.source, tc.convType))
			if tc.mode != "" && tc.mode != domain.IMStreamingStreaming {
				f.settings = &fakeAgentRuntimeSettings{mode: tc.mode}
			}
			if tc.streaming == nil {
				f.streaming = nil
			}
			if got := f.Enabled(); got != tc.want {
				t.Fatalf("Enabled()=%v, want %v", got, tc.want)
			}
		})
	}
}

func TestStreamForwarderSettingsNilDisabled(t *testing.T) {
	f := newTestForwarder(&fakeStreamingSender{}, nil, taskFor(domain.SourceFeishu, domain.ConversationTypePrivate))
	if f.Enabled() {
		t.Fatal("Enabled()=true with nil settings, want false")
	}
}

func TestStreamForwarderSettingsErrorFailsClosed(t *testing.T) {
	f := newTestForwarder(&fakeStreamingSender{}, &errRuntimeSettings{}, taskFor(domain.SourceFeishu, domain.ConversationTypePrivate))
	if f.Enabled() {
		t.Fatal("Enabled()=true with settings error, want fail-closed false")
	}
}

// ---------------------------------------------------------------------------
// 节流合并与生命周期
// ---------------------------------------------------------------------------

func TestStreamForwarderThrottleMergesWithinWindow(t *testing.T) {
	sender := &fakeStreamingSender{}
	f := newTestForwarder(sender, &fakeAgentRuntimeSettings{mode: domain.IMStreamingStreaming}, taskFor(domain.SourceFeishu, domain.ConversationTypePrivate))
	ctx := context.Background()
	base := time.Now()

	// 窗口内 3 条 chunk: 首条累积, 后续未到期, 无转发。
	f.AppendText(ctx, "a", base)
	f.AppendText(ctx, "b", base.Add(100*time.Millisecond))
	f.AppendText(ctx, "c", base.Add(200*time.Millisecond))
	if len(sender.replies) != 0 {
		t.Fatalf("no open expected before window expiry, got %d", len(sender.replies))
	}

	// 第 4 条已过窗口: 触发 flush 合并前 3 条为一次 append(新窗口累积 d)。
	f.AppendText(ctx, "d", base.Add(600*time.Millisecond))
	if len(sender.opens) != 1 {
		t.Fatalf("open count=%d, want 1", len(sender.opens))
	}
	reply := sender.replies[0]
	if len(reply.appends) != 1 || reply.appends[0] != "abc" {
		t.Fatalf("first append=%q, want merged %q", strings.Join(reply.appends, "|"), "abc")
	}
	if sender.opens[0].botUUID != "bot-feishu" || sender.opens[0].target != "conv-1" || sender.opens[0].clientID != "task-1" {
		t.Fatalf("open args=%+v", sender.opens[0])
	}

	// Terminal commit: flush 剩余窗口(d) + commit。
	if !f.Commit(ctx, base.Add(700*time.Millisecond)) {
		t.Fatal("Commit()=false, want true")
	}
	if len(reply.appends) != 2 || reply.appends[1] != "d" {
		t.Fatalf("appends=%q, want [abc d]", strings.Join(reply.appends, "|"))
	}
	if reply.commits != 1 || reply.aborts != 0 {
		t.Fatalf("commits=%d aborts=%d, want 1/0", reply.commits, reply.aborts)
	}
}

func TestStreamForwarderNoTextNoOpenNoCommit(t *testing.T) {
	sender := &fakeStreamingSender{}
	f := newTestForwarder(sender, &fakeAgentRuntimeSettings{mode: domain.IMStreamingStreaming}, taskFor(domain.SourceFeishu, domain.ConversationTypePrivate))
	ctx := context.Background()
	base := time.Now()

	// 无任何文本(纯文件/空任务): 缓冲空, commit 返回 false
	// (调用方不置位 stream_final_at, delivery 发完整最终结果)。
	if f.Commit(ctx, base.Add(100*time.Millisecond)) {
		t.Fatal("Commit()=true without any text, want false")
	}
	if len(sender.opens) != 0 {
		t.Fatalf("open count=%d, want 0 (no text task)", len(sender.opens))
	}
}

func TestStreamForwarderBeginReplyFailureFallsBackToFinal(t *testing.T) {
	sender := &fakeStreamingSender{beginErr: errors.New("poller down")}
	f := newTestForwarder(sender, &fakeAgentRuntimeSettings{mode: domain.IMStreamingStreaming}, taskFor(domain.SourceFeishu, domain.ConversationTypePrivate))
	ctx := context.Background()
	base := time.Now()

	f.AppendText(ctx, "x", base)
	f.AppendText(ctx, "y", base.Add(600*time.Millisecond)) // 触发 flush → BeginReply 失败
	if len(sender.opens) != 0 {
		t.Fatalf("open count=%d, want 0", len(sender.opens))
	}
	// 失败后不再转发; Commit 返回 false → delivery 兜底。
	if f.Commit(ctx, base.Add(700*time.Millisecond)) {
		t.Fatal("Commit()=true after begin failure, want false")
	}
	if !f.failed {
		t.Fatal("forwarder not marked failed")
	}
}

func TestStreamForwarderAppendFailureAborts(t *testing.T) {
	sender := &fakeStreamingSender{}
	f := newTestForwarder(sender, &fakeAgentRuntimeSettings{mode: domain.IMStreamingStreaming}, taskFor(domain.SourceFeishu, domain.ConversationTypePrivate))
	ctx := context.Background()
	base := time.Now()

	f.AppendText(ctx, "a", base)
	f.AppendText(ctx, "b", base.Add(600*time.Millisecond)) // open + append → append 失败
	reply := sender.replies[0]
	reply.appendErr = errors.New("ratelimited")
	f.AppendText(ctx, "c", base.Add(1200*time.Millisecond))
	if reply.aborts != 1 {
		t.Fatalf("aborts=%d, want 1 after append failure", reply.aborts)
	}
	if f.Commit(ctx, base.Add(1300*time.Millisecond)) {
		t.Fatal("Commit()=true after append failure, want false")
	}
}

func TestStreamForwarderAbortOnFailure(t *testing.T) {
	sender := &fakeStreamingSender{}
	f := newTestForwarder(sender, &fakeAgentRuntimeSettings{mode: domain.IMStreamingStreaming}, taskFor(domain.SourceFeishu, domain.ConversationTypePrivate))
	ctx := context.Background()
	base := time.Now()

	f.AppendText(ctx, "a", base)
	f.AppendText(ctx, "b", base.Add(600*time.Millisecond))
	f.Abort(ctx)
	reply := sender.replies[0]
	if reply.aborts != 1 || reply.commits != 0 {
		t.Fatalf("aborts=%d commits=%d, want 1/0", reply.aborts, reply.commits)
	}
}

// ---------------------------------------------------------------------------
// LoopbackTransport streaming 记录(测试断言用)
// ---------------------------------------------------------------------------

func TestLoopbackTransportStreamRecords(t *testing.T) {
	lb := transport.NewLoopbackTransport()
	ctx := context.Background()

	reply, err := lb.BeginReply(ctx, "bot-1", "conv-9", "task-9")
	if err != nil {
		t.Fatal(err)
	}
	if err := reply.Append(ctx, "frag-1"); err != nil {
		t.Fatal(err)
	}
	if err := reply.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	ops := lb.SentStreams()
	want := []transport.StreamOp{
		{BotUUID: "bot-1", Target: "conv-9", ClientID: "task-9", Op: "open"},
		{BotUUID: "bot-1", Target: "conv-9", ClientID: "task-9", Op: "append", Text: "frag-1"},
		{BotUUID: "bot-1", Target: "conv-9", ClientID: "task-9", Op: "commit"},
	}
	if len(ops) != len(want) {
		t.Fatalf("ops=%d, want %d: %+v", len(ops), len(want), ops)
	}
	for i := range want {
		if ops[i] != want[i] {
			t.Fatalf("op[%d]=%+v, want %+v", i, ops[i], want[i])
		}
	}
}
