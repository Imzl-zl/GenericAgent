package application

import (
	"context"
	"testing"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/infrastructure/transport"
)

// TestRouterMultiChannelBucketRouting 验证多渠道分桶(IM_CHANNEL_BINDING §6):
// feishu/dingtalk/qq 消息 Source=渠道类型、ConversationKey=conversation_id;
// 微信 ConversationKey 恒空; 新渠道属主直判(群内任意成员消息路由到配置属主,
// 无 ilink 身份匹配)。
func TestRouterMultiChannelBucketRouting(t *testing.T) {
	store := newFakeRouterStore()
	store.bots["feishu-bot"] = domain.ChannelConfig{
		ID: 1, BotUUID: "feishu-bot", ChannelType: domain.ChannelFeishu,
		OwnerID: 42, State: domain.ChannelActive,
	}
	store.bots["wechat-bot"] = domain.ChannelConfig{
		ID: 2, BotUUID: "wechat-bot", ChannelType: domain.ChannelWechat,
		OwnerID: 42, IlinkUserID: "wx-user-1", State: domain.ChannelActive,
	}
	store.statuses[42] = domain.UserApproved
	tr := transport.NewLoopbackTransport()
	r, tasks := newTestRouter(store, tr)

	// 飞书群消息: 群成员(非配置属主本人)@触发, 属主直判 + 群桶。
	res, err := r.HandleMessage(context.Background(), IncomingMessage{
		BotUUID:          "feishu-bot",
		ChannelType:      "feishu",
		ChannelAccountID: "ou_group_member_1",
		ConversationID:   "oc_group_123",
		MessageID:        "fs-1",
		Text:             "整理一下会议纪要",
	})
	if err != nil {
		t.Fatalf("feishu message: %v", err)
	}
	if res.Action != ActionTaskCreated {
		t.Fatalf("action = %s", res.Action)
	}
	if tasks.submittedTask.Source != "feishu" {
		t.Fatalf("source = %q, want feishu", tasks.submittedTask.Source)
	}
	if tasks.submittedTask.SessionKey != "personal:42" {
		t.Fatalf("session = %q, want config owner 42", tasks.submittedTask.SessionKey)
	}

	// 微信消息回归: ConversationKey 恒空(默认桶)。
	res, err = r.HandleMessage(context.Background(), IncomingMessage{
		BotUUID:          "wechat-bot",
		ChannelType:      "wechat",
		ChannelAccountID: "wx-user-1",
		MessageID:        "wx-1",
		Text:             "你好",
	})
	if err != nil {
		t.Fatalf("wechat message: %v", err)
	}
	if res.Action != ActionTaskCreated {
		t.Fatalf("wechat action = %s", res.Action)
	}
	if tasks.submittedTask.Source != "wechat" {
		t.Fatalf("wechat source = %q", tasks.submittedTask.Source)
	}

	// 钉钉群消息: 群桶 conversation_id 透传。
	res3, err := r.HandleMessage(context.Background(), IncomingMessage{
		BotUUID:          "feishu-bot", // 错配: bot_uuid 属于飞书配置
		ChannelType:      "dingtalk",
		ChannelAccountID: "ding-member-1",
		ConversationID:   "cid_group_9",
		MessageID:        "dt-1",
		Text:             "测试",
	})
	if err != nil {
		t.Fatalf("mismatch must be a rejected action, not an error: %v", err)
	}
	if res3.Action != ActionRejected {
		t.Fatalf("feishu bot_uuid with dingtalk channel_type must be rejected, got %s", res3.Action)
	}
}

// TestRouterConversationKeyDrivesBucket 验证对话单元维度落到任务
// ConversationKey(/new /stop /status 也按当前桶)。
func TestRouterConversationKeyDrivesBucket(t *testing.T) {
	store := newFakeRouterStore()
	store.bots["qq-bot"] = domain.ChannelConfig{
		ID: 1, BotUUID: "qq-bot", ChannelType: domain.ChannelQQ,
		OwnerID: 42, State: domain.ChannelActive,
	}
	store.statuses[42] = domain.UserApproved
	tr := transport.NewLoopbackTransport()
	r, tasks := newTestRouter(store, tr)

	msg := IncomingMessage{
		BotUUID:          "qq-bot",
		ChannelType:      "qq",
		ChannelAccountID: "qq-user-1",
		ConversationID:   "group_openid_7",
		MessageID:        "qq-1",
		Text:             "整理文档",
	}
	res, err := r.HandleMessage(context.Background(), msg)
	if err != nil {
		t.Fatalf("qq message: %v", err)
	}
	if res.Action != ActionTaskCreated {
		t.Fatalf("action = %s", res.Action)
	}
	if tasks.submittedTask.Source != "qq" {
		t.Fatalf("source = %q", tasks.submittedTask.Source)
	}
	if tasks.submittedTask.ConversationKey != "group_openid_7" {
		t.Fatalf("conversation key = %q, want group_openid_7", tasks.submittedTask.ConversationKey)
	}

	// 回复目标 = 对话单元(群), 不是发送者。
	if len(tr.Sent()) != 0 {
		t.Fatalf("unexpected replies: %+v", tr.Sent())
	}
	// 另一桶同 bot 消息 → 不同 conversation key。
	res2, err := r.HandleMessage(context.Background(), IncomingMessage{
		BotUUID:          "qq-bot",
		ChannelType:      "qq",
		ChannelAccountID: "qq-user-1",
		ConversationID:   "openid_c2c_8",
		MessageID:        "qq-2",
		Text:             "/status",
	})
	if err != nil {
		t.Fatalf("qq /status: %v", err)
	}
	if res2.Action != ActionStatus {
		t.Fatalf("action = %s", res2.Action)
	}
	sent := tr.Sent()
	if len(sent) == 0 {
		t.Fatal("no reply recorded")
	}
	if sent[len(sent)-1].ChannelAccountID != "openid_c2c_8" {
		t.Fatalf("reply target = %q, want c2c conversation id", sent[len(sent)-1].ChannelAccountID)
	}
}

// TestRouterNewResetsCurrentConversationBucket 验证 /new 只清当前对话单元桶
// (微信默认桶语义不受影响)。
func TestRouterNewResetsCurrentConversationBucket(t *testing.T) {
	store := newFakeRouterStore()
	store.bots["feishu-bot"] = domain.ChannelConfig{
		ID: 1, BotUUID: "feishu-bot", ChannelType: domain.ChannelFeishu,
		OwnerID: 42, State: domain.ChannelActive,
	}
	store.statuses[42] = domain.UserApproved
	store.resetKeys = []string{}
	tr := transport.NewLoopbackTransport()
	r, _ := newTestRouter(store, tr)

	_, err := r.HandleMessage(context.Background(), IncomingMessage{
		BotUUID:          "feishu-bot",
		ChannelType:      "feishu",
		ChannelAccountID: "ou_member",
		ConversationID:   "oc_group_abc",
		MessageID:        "fs-new-1",
		Text:             "/new",
	})
	if err != nil {
		t.Fatalf("/new: %v", err)
	}
	if len(store.resetKeys) != 1 || store.resetKeys[0] != "oc_group_abc" {
		t.Fatalf("reset keys = %+v, want [oc_group_abc]", store.resetKeys)
	}
}

// TestRouterRejectsUnsupportedChannelType 验证未知渠道 fail-closed。
func TestRouterRejectsUnsupportedChannelType(t *testing.T) {
	store := newFakeRouterStore()
	store.bots["wechat-bot"] = domain.ChannelConfig{
		ID: 1, BotUUID: "wechat-bot", ChannelType: domain.ChannelWechat,
		OwnerID: 42, IlinkUserID: "wx-user-1", State: domain.ChannelActive,
	}
	store.statuses[42] = domain.UserApproved
	r, _ := newTestRouter(store, transport.NewLoopbackTransport())

	res, err := r.HandleMessage(context.Background(), IncomingMessage{
		BotUUID:          "wechat-bot",
		ChannelType:      "telegram",
		ChannelAccountID: "tg-user",
		MessageID:        "tg-1",
		Text:             "hi",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Action != ActionRejected {
		t.Fatalf("action = %s, want rejected", res.Action)
	}
	if res, err := r.HandleMessage(context.Background(), IncomingMessage{
		BotUUID:          "wechat-bot",
		ChannelType:      "",
		ChannelAccountID: "wx-user-1",
		MessageID:        "wx-2",
		Text:             "hi",
	}); err != nil || res.Action != ActionRejected {
		t.Fatalf("empty channel_type must be rejected: action=%s err=%v", res.Action, err)
	}
}
