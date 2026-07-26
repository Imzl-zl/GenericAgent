package application

import (
	"context"
	"errors"
	"testing"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/transport"
)

// fakeRelayService is an in-memory RelayService for router tests.
type fakeRelayService struct {
	relayErr   error
	relayCalls []relayCall
	setErr     error
	setCalls   []setOptOutCall
}

type relayCall struct {
	fromUserID int64
	toUsername string
	text       string
}

func (f *fakeRelayService) Relay(_ context.Context, fromUserID int64, toUsername, text string) error {
	f.relayCalls = append(f.relayCalls, relayCall{fromUserID: fromUserID, toUsername: toUsername, text: text})
	return f.relayErr
}

func (f *fakeRelayService) SetOptOut(_ context.Context, userID int64, optOut bool) error {
	if f.setErr != nil {
		return f.setErr
	}
	f.setCalls = append(f.setCalls, setOptOutCall{userID: userID, optOut: optOut})
	return nil
}

// newTestRouterWithRelay builds a router with the relay service wired in.
// Returns the router, the relay fake, and the loopback transport for assertions.
func newTestRouterWithRelay(store *fakeRouterStore, tr *transport.LoopbackTransport, relay RelayService) (Router, *fakeRelayService) {
	if relay == nil {
		relay = &fakeRelayService{}
	}
	fakeRelay, _ := relay.(*fakeRelayService)
	r, _ := NewRouter(RouterConfig{
		Store:          store,
		Binding:        &fakeBindingSvc{},
		Tasks:          &fakeTaskService{},
		Transport:      tr,
		Messages:       &fakeMessageStore{},
		ToolPolicy:     "foundation.no-host-tools.v1",
		SourceInstance: "test-router",
		Relay:          relay,
	})
	return r, fakeRelay
}

func relayTestBot(uuid, ilinkUser string, ownerID int64) domain.Bot {
	return domain.Bot{
		ID:          1,
		BotUUID:     uuid,
		OwnerID:     ownerID,
		IlinkUserID: ilinkUser,
		State:       domain.BotActive,
	}
}

func approvedStoreWithBot(bot domain.Bot) *fakeRouterStore {
	s := newFakeRouterStore()
	s.bots[bot.BotUUID] = bot
	s.statuses[bot.OwnerID] = domain.UserApproved
	return s
}

func TestParseRelayMention(t *testing.T) {
	cases := []struct {
		name     string
		input    string
		wantUser string
		wantBody string
		wantOK   bool
	}{
		{"basic", "@alice hello", "alice", "hello", true},
		{"multi word body", "@alice hi there", "alice", "hi there", true},
		{"tab separator", "@bob\tgood morning", "bob", "good morning", true},
		{"chinese username", "@张三 你好", "张三", "你好", true},
		{"underscore username", "@alice_b hi", "alice_b", "hi", true},
		{"trailing whitespace trimmed", "@alice hi  ", "alice", "hi", true},
		{"no body", "@alice", "", "", false},
		{"empty body", "@alice ", "", "", false},
		{"no @ prefix", "alice hi", "", "", false},
		{"empty string", "", "", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			u, b, ok := parseRelayMention(c.input)
			if ok != c.wantOK {
				t.Fatalf("ok mismatch: got %v want %v", ok, c.wantOK)
			}
			if ok {
				if u != c.wantUser {
					t.Errorf("username: got %q want %q", u, c.wantUser)
				}
				if b != c.wantBody {
					t.Errorf("body: got %q want %q", b, c.wantBody)
				}
			}
		})
	}
}

func TestRouterRelayMentionDispatched(t *testing.T) {
	bot := relayTestBot("b1", "u1", 42)
	store := approvedStoreWithBot(bot)
	tr := transport.NewLoopbackTransport()
	r, relay := newTestRouterWithRelay(store, tr, nil)

	res, _ := r.HandleMessage(context.Background(), IncomingMessage{
		BotUUID: "b1", IlinkUserID: "u1", MessageID: "m1", Text: "@bob hello there",
	})
	if res.Action != ActionReplied {
		t.Fatalf("expected replied, got %s", res.Action)
	}
	if len(relay.relayCalls) != 1 {
		t.Fatalf("expected 1 relay call, got %d", len(relay.relayCalls))
	}
	call := relay.relayCalls[0]
	if call.fromUserID != 42 || call.toUsername != "bob" || call.text != "hello there" {
		t.Fatalf("relay call mismatch: %+v", call)
	}
	last, ok := tr.LastSentMessage()
	if !ok || !contains(last.Text, "已转发给 bob") {
		t.Fatalf("expected '已转发给 bob' reply, got %q", last.Text)
	}
}

func TestRouterRelayUserNotFound(t *testing.T) {
	bot := relayTestBot("b1", "u1", 42)
	store := approvedStoreWithBot(bot)
	tr := transport.NewLoopbackTransport()
	relay := &fakeRelayService{relayErr: domain.ErrRelayUserNotFound}
	r, _ := newTestRouterWithRelay(store, tr, relay)

	res, _ := r.HandleMessage(context.Background(), IncomingMessage{
		BotUUID: "b1", IlinkUserID: "u1", MessageID: "m1", Text: "@nobody hi",
	})
	if res.Action != ActionRejected {
		t.Fatalf("expected rejected, got %s", res.Action)
	}
	last, ok := tr.LastSentMessage()
	if !ok || !contains(last.Text, "不存在") {
		t.Fatalf("expected '不存在' reply, got %q", last.Text)
	}
}

func TestRouterRelaySelfTarget(t *testing.T) {
	bot := relayTestBot("b1", "u1", 42)
	store := approvedStoreWithBot(bot)
	tr := transport.NewLoopbackTransport()
	relay := &fakeRelayService{relayErr: domain.ErrRelaySelfTarget}
	r, _ := newTestRouterWithRelay(store, tr, relay)

	res, _ := r.HandleMessage(context.Background(), IncomingMessage{
		BotUUID: "b1", IlinkUserID: "u1", MessageID: "m1", Text: "@alice hi",
	})
	if res.Action != ActionRejected {
		t.Fatalf("expected rejected, got %s", res.Action)
	}
	last, _ := tr.LastSentMessage()
	if !contains(last.Text, "自己") {
		t.Fatalf("expected self-target reply, got %q", last.Text)
	}
}

func TestRouterRelayOptedOut(t *testing.T) {
	bot := relayTestBot("b1", "u1", 42)
	store := approvedStoreWithBot(bot)
	tr := transport.NewLoopbackTransport()
	relay := &fakeRelayService{relayErr: domain.ErrRelayOptedOut}
	r, _ := newTestRouterWithRelay(store, tr, relay)

	res, _ := r.HandleMessage(context.Background(), IncomingMessage{
		BotUUID: "b1", IlinkUserID: "u1", MessageID: "m1", Text: "@bob hi",
	})
	if res.Action != ActionRejected {
		t.Fatalf("expected rejected, got %s", res.Action)
	}
	last, _ := tr.LastSentMessage()
	if !contains(last.Text, "已关闭转发接收") {
		t.Fatalf("expected opt-out reply, got %q", last.Text)
	}
}

func TestRouterRelayNotBound(t *testing.T) {
	bot := relayTestBot("b1", "u1", 42)
	store := approvedStoreWithBot(bot)
	tr := transport.NewLoopbackTransport()
	relay := &fakeRelayService{relayErr: domain.ErrRelayUserNotBound}
	r, _ := newTestRouterWithRelay(store, tr, relay)

	res, _ := r.HandleMessage(context.Background(), IncomingMessage{
		BotUUID: "b1", IlinkUserID: "u1", MessageID: "m1", Text: "@bob hi",
	})
	if res.Action != ActionRejected {
		t.Fatalf("expected rejected, got %s", res.Action)
	}
	last, _ := tr.LastSentMessage()
	if !contains(last.Text, "未绑定微信") {
		t.Fatalf("expected not-bound reply, got %q", last.Text)
	}
}

func TestRouterRelayNotApproved(t *testing.T) {
	bot := relayTestBot("b1", "u1", 42)
	store := approvedStoreWithBot(bot)
	tr := transport.NewLoopbackTransport()
	relay := &fakeRelayService{relayErr: domain.ErrRelayUserNotApproved}
	r, _ := newTestRouterWithRelay(store, tr, relay)

	res, _ := r.HandleMessage(context.Background(), IncomingMessage{
		BotUUID: "b1", IlinkUserID: "u1", MessageID: "m1", Text: "@bob hi",
	})
	if res.Action != ActionRejected {
		t.Fatalf("expected rejected, got %s", res.Action)
	}
	last, _ := tr.LastSentMessage()
	if !contains(last.Text, "未激活") {
		t.Fatalf("expected not-approved reply, got %q", last.Text)
	}
}

func TestRouterRelayEmptyBody(t *testing.T) {
	bot := relayTestBot("b1", "u1", 42)
	store := approvedStoreWithBot(bot)
	tr := transport.NewLoopbackTransport()
	r, relay := newTestRouterWithRelay(store, tr, nil)

	res, _ := r.HandleMessage(context.Background(), IncomingMessage{
		BotUUID: "b1", IlinkUserID: "u1", MessageID: "m1", Text: "@bob",
	})
	if res.Action != ActionRejected {
		t.Fatalf("expected rejected, got %s", res.Action)
	}
	if len(relay.relayCalls) != 0 {
		t.Fatalf("expected 0 relay calls for empty body, got %d", len(relay.relayCalls))
	}
	last, _ := tr.LastSentMessage()
	if !contains(last.Text, "用法") {
		t.Fatalf("expected usage reply, got %q", last.Text)
	}
}

func TestRouterRelayNilServiceFallsThrough(t *testing.T) {
	bot := relayTestBot("b1", "u1", 42)
	store := approvedStoreWithBot(bot)
	tr := transport.NewLoopbackTransport()
	// Router with NO relay service — @-text should fall through to normal task.
	r, _, _ := newTestRouter(store, tr)

	res, _ := r.HandleMessage(context.Background(), IncomingMessage{
		BotUUID: "b1", IlinkUserID: "u1", MessageID: "m1", Text: "@bob hello",
	})
	if res.Action != ActionTaskCreated {
		t.Fatalf("expected task_created (fallback), got %s", res.Action)
	}
}

func TestRouterRelayTransportFailure(t *testing.T) {
	bot := relayTestBot("b1", "u1", 42)
	store := approvedStoreWithBot(bot)
	tr := transport.NewLoopbackTransport()
	relay := &fakeRelayService{relayErr: errors.New("ilink send failed")}
	r, _ := newTestRouterWithRelay(store, tr, relay)

	res, _ := r.HandleMessage(context.Background(), IncomingMessage{
		BotUUID: "b1", IlinkUserID: "u1", MessageID: "m1", Text: "@bob hi",
	})
	if res.Action != ActionRejected {
		t.Fatalf("expected rejected, got %s", res.Action)
	}
	last, _ := tr.LastSentMessage()
	if !contains(last.Text, "转发失败") {
		t.Fatalf("expected generic failure reply, got %q", last.Text)
	}
}

func TestRouterRelayOffCommand(t *testing.T) {
	bot := relayTestBot("b1", "u1", 42)
	store := approvedStoreWithBot(bot)
	tr := transport.NewLoopbackTransport()
	r, relay := newTestRouterWithRelay(store, tr, nil)

	res, _ := r.HandleMessage(context.Background(), IncomingMessage{
		BotUUID: "b1", IlinkUserID: "u1", MessageID: "m1", Text: "/relay_off",
	})
	if res.Action != ActionReplied {
		t.Fatalf("expected replied, got %s", res.Action)
	}
	if len(relay.setCalls) != 1 || !relay.setCalls[0].optOut || relay.setCalls[0].userID != 42 {
		t.Fatalf("opt-out call mismatch: %+v", relay.setCalls)
	}
	last, _ := tr.LastSentMessage()
	if !contains(last.Text, "已关闭") {
		t.Fatalf("expected '已关闭' reply, got %q", last.Text)
	}
}

func TestRouterRelayOnCommand(t *testing.T) {
	bot := relayTestBot("b1", "u1", 42)
	store := approvedStoreWithBot(bot)
	tr := transport.NewLoopbackTransport()
	r, relay := newTestRouterWithRelay(store, tr, nil)

	res, _ := r.HandleMessage(context.Background(), IncomingMessage{
		BotUUID: "b1", IlinkUserID: "u1", MessageID: "m1", Text: "/relay_on",
	})
	if res.Action != ActionReplied {
		t.Fatalf("expected replied, got %s", res.Action)
	}
	if len(relay.setCalls) != 1 || relay.setCalls[0].optOut || relay.setCalls[0].userID != 42 {
		t.Fatalf("opt-in call mismatch: %+v", relay.setCalls)
	}
	last, _ := tr.LastSentMessage()
	if !contains(last.Text, "已开启") {
		t.Fatalf("expected '已开启' reply, got %q", last.Text)
	}
}

func TestRouterRelayOffWhenServiceNil(t *testing.T) {
	bot := relayTestBot("b1", "u1", 42)
	store := approvedStoreWithBot(bot)
	tr := transport.NewLoopbackTransport()
	// Router with NO relay service — /relay_off should say feature disabled.
	r, _, _ := newTestRouter(store, tr)

	res, _ := r.HandleMessage(context.Background(), IncomingMessage{
		BotUUID: "b1", IlinkUserID: "u1", MessageID: "m1", Text: "/relay_off",
	})
	if res.Action != ActionReplied {
		t.Fatalf("expected replied, got %s", res.Action)
	}
	last, _ := tr.LastSentMessage()
	if !contains(last.Text, "未启用") {
		t.Fatalf("expected '未启用' reply, got %q", last.Text)
	}
}

func TestRouterRelayMentionDoesNotConflictWithCommands(t *testing.T) {
	bot := relayTestBot("b1", "u1", 42)
	store := approvedStoreWithBot(bot)
	tr := transport.NewLoopbackTransport()
	r, relay := newTestRouterWithRelay(store, tr, nil)

	// /stop is still a command, not a relay target
	res, _ := r.HandleMessage(context.Background(), IncomingMessage{
		BotUUID: "b1", IlinkUserID: "u1", MessageID: "m1", Text: "/stop",
	})
	if res.Action == ActionReplied && contains(res.Reply, "已转发") {
		t.Fatalf("/stop should not be treated as relay, got %s", res.Action)
	}
	if len(relay.relayCalls) != 0 {
		t.Fatalf("expected 0 relay calls for /stop, got %d", len(relay.relayCalls))
	}
}

func TestFormatRelayError(t *testing.T) {
	cases := []struct {
		err      error
		contains string
	}{
		{domain.ErrRelayUserNotFound, "不存在"},
		{domain.ErrRelaySelfTarget, "自己"},
		{domain.ErrRelayUserNotApproved, "未激活"},
		{domain.ErrRelayOptedOut, "已关闭"},
		{domain.ErrRelayUserNotBound, "未绑定"},
		{domain.ErrRelayEmptyMessage, "为空"},
		{domain.ErrRelaySenderUnknown, "发送者"},
		{errors.New("custom"), "转发失败"},
	}
	for _, c := range cases {
		got := formatRelayError(c.err, "bob")
		if !contains(got, c.contains) {
			t.Errorf("formatRelayError(%v): got %q, want substring %q", c.err, got, c.contains)
		}
	}
}
