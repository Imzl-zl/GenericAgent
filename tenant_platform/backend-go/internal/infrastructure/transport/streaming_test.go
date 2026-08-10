package transport

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/infrastructure/poller"
)

// TestILinkAdapterStreamingSenderLifecycle 验证 ILinkAdapter 的
// StreamingSender 全生命周期: open 拿 stream_id → append → commit,
// 全部走 /send stream_action(不新增端点)。
func TestILinkAdapterStreamingSenderLifecycle(t *testing.T) {
	var actions []poller.StreamActionRequest
	adapter, _ := newPollerAdapter(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/send" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		body, _ := io.ReadAll(r.Body)
		var req poller.StreamActionRequest
		if err := json.Unmarshal(body, &req); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		actions = append(actions, req)
		if req.StreamAction == poller.StreamActionOpen {
			_, _ = w.Write([]byte(`{"sent":true,"stream_id":"s-123"}`))
			return
		}
		_, _ = w.Write([]byte(`{"sent":true}`))
	})

	reply, err := adapter.BeginReply(context.Background(), "bot-1", "oc_conv_1", "task-9")
	if err != nil {
		t.Fatalf("begin reply: %v", err)
	}
	if err := reply.Append(context.Background(), "思考中"); err != nil {
		t.Fatalf("append: %v", err)
	}
	if err := reply.Commit(context.Background()); err != nil {
		t.Fatalf("commit: %v", err)
	}

	if len(actions) != 3 {
		t.Fatalf("actions=%d, want 3", len(actions))
	}
	if actions[0].StreamAction != poller.StreamActionOpen || actions[0].BotUUID != "bot-1" || actions[0].ChannelAccountID != "oc_conv_1" {
		t.Fatalf("open req=%+v", actions[0])
	}
	if actions[1].StreamAction != poller.StreamActionAppend || actions[1].StreamID != "s-123" || actions[1].Text != "思考中" {
		t.Fatalf("append req=%+v", actions[1])
	}
	if actions[2].StreamAction != poller.StreamActionCommit || actions[2].StreamID != "s-123" {
		t.Fatalf("commit req=%+v", actions[2])
	}
}

// TestILinkAdapterStreamingAbort 验证 abort 动作路由。
func TestILinkAdapterStreamingAbort(t *testing.T) {
	var actions []poller.StreamActionRequest
	adapter, _ := newPollerAdapter(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req poller.StreamActionRequest
		_ = json.Unmarshal(body, &req)
		actions = append(actions, req)
		if req.StreamAction == poller.StreamActionOpen {
			_, _ = w.Write([]byte(`{"sent":true,"stream_id":"s-9"}`))
			return
		}
		_, _ = w.Write([]byte(`{"sent":true}`))
	})

	reply, err := adapter.BeginReply(context.Background(), "bot-1", "target-1", "task-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := reply.Abort(context.Background()); err != nil {
		t.Fatalf("abort: %v", err)
	}
	if len(actions) != 2 || actions[1].StreamAction != poller.StreamActionAbort || actions[1].StreamID != "s-9" {
		t.Fatalf("abort req=%+v", actions)
	}
}

// TestILinkAdapterBeginReplyPollerFailure 验证 open 失败直接返回错误
// (scheduler 据此回退终态 delivery)。
func TestILinkAdapterBeginReplyPollerFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"boom"}`))
	}))
	t.Cleanup(server.Close)
	pollerClient, err := poller.NewClient(server.URL, "")
	if err != nil {
		t.Fatal(err)
	}
	adapter, err := NewILinkAdapter(ILinkAdapterConfig{Poller: pollerClient})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := adapter.BeginReply(context.Background(), "bot-1", "target-1", "task-1"); err == nil {
		t.Fatal("expected error from poller")
	}
}
