// Package poller is the HTTP client for the Python Bot Poller process.
//
// The Poller reuses GA Core's WxBotClient (frontends/wxbot_client.py) to
// long-poll iLink getupdates and send messages. The Go platform delegates
// all iLink message I/O to the Poller so it never re-implements the protocol.
package poller

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const (
	// defaultTimeout 是控制面/文本消息的请求硬上限(2026-08-14 事故修复后
	// 定稿的预算层次, 见 mediaTimeout): 文本发送、/start /stop /health
	// /config、流式帧都是短操作, 15s 足够且防调用方忘传 ctx 时无限阻塞。
	defaultTimeout = 15 * time.Second
	// mediaTimeout 是媒体(/send image|video|file)请求的兜底上限。媒体发送
	// 预算的单一真值源是调用方 ctx(delivery 媒体 90s), 此处 120s 仅作
	// 防御兜底(> 90s 使 ctx 恒先生效, 且未来新调用方忘传 ctx 时不会挂死
	// poller 线程)。历史: 单一 15s Timeout 曾把媒体预算架空为 15s——
	// 300KB 上传约 14-17s 贴着边界, 修复后生产靠概率成功; 本次拆分为
	// 双 client 后预算链为: poller 侧最坏 ~85s < ctx 90s < 兜底 120s。
	mediaTimeout = 120 * time.Second
	// HTTP transport tunables for the Poller client. The Poller is a single
	// downstream host, so MaxIdleConnsPerHost is the lever that matters: the
	// Go default of 2 forces connection churn under any real concurrency.
	// 32 idle conns covers 10-20 active bots with headroom for bursts.
	defaultMaxIdleConns        = 64
	defaultMaxIdleConnsPerHost = 32
	defaultIdleConnTimeout     = 90 * time.Second
	maxInboundCoalesceWindowMS = 5000
)

// Client calls the Python Bot Poller HTTP API.
type Client struct {
	baseURL   string
	quick     *http.Client // 控制面/文本/流式: Timeout 15s
	media     *http.Client // 媒体发送: Timeout 120s 兜底, 预算以调用方 ctx 为准
	apiSecret string       // HMAC-SHA256 shared secret for X-API-Signature auth
}

// NewClient validates the poller base URL and returns a client with a tuned
// transport: keep-alive is on, idle conns per host are raised above the Go
// default (2) so concurrent StartBot/SendMessage/Health calls reuse conns
// instead of churning TCP handshakes.
//
// apiSecret is the HMAC-SHA256 shared secret used to sign requests with
// X-API-Signature. Empty string disables signing (insecure; dev/test only).
func NewClient(baseURL, apiSecret string) (*Client, error) {
	if strings.TrimSpace(baseURL) == "" {
		return nil, errors.New("poller base URL is required")
	}
	transport := &http.Transport{
		MaxIdleConns:        defaultMaxIdleConns,
		MaxIdleConnsPerHost: defaultMaxIdleConnsPerHost,
		IdleConnTimeout:     defaultIdleConnTimeout,
	}
	// 双 client 设计(2026-08-14 事故修复审查): http.Client.Timeout 是整个
	// 交换的硬上限, 单一 15s 会把 delivery 媒体 90s 预算架空(poller 侧
	// CDN 上传最坏 ~85s > 15s, 生产仅靠上传恰快于 15s 的概率成功)。
	// 预算层次: 调用方 ctx(文本 15s / 媒体 90s) < client 兜底(15s / 120s)。
	return &Client{
		baseURL:   strings.TrimRight(baseURL, "/"),
		quick:     &http.Client{Timeout: defaultTimeout, Transport: transport},
		media:     &http.Client{Timeout: mediaTimeout, Transport: transport},
		apiSecret: apiSecret,
	}, nil
}

// ConfigureInboundCoalescing updates the Poller's global cross-batch IM
// coalescing window. It applies to every active bot.
func (c *Client) ConfigureInboundCoalescing(ctx context.Context, windowMS int) error {
	if windowMS < 0 || windowMS > maxInboundCoalesceWindowMS {
		return fmt.Errorf("window_ms must be between 0 and %d", maxInboundCoalesceWindowMS)
	}
	_, err := c.post(ctx, "/config", map[string]int{
		"inbound_coalesce_window_ms": windowMS,
	})
	return err
}

// StartBotRequest is the body for POST /start.
//
// ChannelType selects the adapter (wechat|feishu|dingtalk|qq); ConfigJSON is
// the decrypted channel config JSON (wechat={token}, 新渠道={app_id,
// app_secret}), never plaintext at rest but always carried over the local
// control-plane HTTP link (same trust domain as the old bot_token).
type StartBotRequest struct {
	BotUUID     string          `json:"bot_uuid"`
	ChannelType string          `json:"channel_type"`
	ConfigJSON  json.RawMessage `json:"config_json"`
	// BaseURL / UpdatesBuf are wechat-only (iLink gateway endpoint + cursor).
	BaseURL    string `json:"base_url"`
	UpdatesBuf string `json:"updates_buf"`
	WebhookURL string `json:"webhook_url"`
}

// StartBot tells the poller to begin polling for one channel config.
func (c *Client) StartBot(ctx context.Context, req StartBotRequest) error {
	if req.BotUUID == "" || req.ChannelType == "" || len(req.ConfigJSON) == 0 || req.WebhookURL == "" {
		return errors.New("bot_uuid, channel_type, config_json, and webhook_url are required")
	}
	_, err := c.post(ctx, "/start", req)
	return err
}

// StopBotResponse is returned by POST /stop.
type StopBotResponse struct {
	Stopped    bool   `json:"stopped"`
	UpdatesBuf string `json:"updates_buf"`
}

// StopBot stops a bot's long-poll and returns the final updates_buf cursor.
func (c *Client) StopBot(ctx context.Context, botUUID string) (StopBotResponse, error) {
	if botUUID == "" {
		return StopBotResponse{}, errors.New("bot_uuid is required")
	}
	body, err := c.post(ctx, "/stop", map[string]string{"bot_uuid": botUUID})
	if err != nil {
		return StopBotResponse{}, err
	}
	var resp StopBotResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return StopBotResponse{}, fmt.Errorf("decode stop response: %w", err)
	}
	return resp, nil
}

// Message type constants for SendMessageRequest.MsgType.
// iLink officially supports image/video/file media sends via the same
// /ilink/bot/sendmessage endpoint with different item_list entries.
// The Python Poller dispatches based on MsgType.
const (
	MsgTypeText  = "text"
	MsgTypeImage = "image"
	MsgTypeVideo = "video"
	MsgTypeFile  = "file"
)

// SendMessageRequest is the body for POST /send.
//
// For text messages, only Text needs to be set (MsgType defaults to "text").
// For media messages, MsgType must be one of image/video/file and FilePath
// must point to a local file accessible to the Python Poller process.
//
// ChannelAccountID 是回复目标(微信=ilink_user_id, 新渠道=conversation_id/
// 对端 ID); ChannelType 用于跨 adapter 分发(与注册表核对)。
type SendMessageRequest struct {
	BotUUID          string `json:"bot_uuid"`
	ChannelType      string `json:"channel_type"`
	ChannelAccountID string `json:"channel_account_id"`
	Text             string `json:"text,omitempty"`
	ContextToken     string `json:"context_token,omitempty"`
	MsgType          string `json:"msg_type,omitempty"`
	FilePath         string `json:"file_path,omitempty"`
	// FileName 是用户可见的文件名(审查 R5-I10): 与 file_path 分离, 快照
	// 临时文件名不得作为显示名暴露给用户。
	FileName string `json:"file_name,omitempty"`
	// ClientID 是稳定幂等键(round9 审查: delivery 重试投递同一内容时保持
	// 同一 client_id, 供 iLink 服务端去重; 空值由 Poller 回退随机)。
	ClientID string `json:"client_id,omitempty"`
}

// SendMessage delivers a text or media reply via the poller (which dispatches
// to the channel adapter). Empty MsgType defaults to "text" for backward compat.
func (c *Client) SendMessage(ctx context.Context, req SendMessageRequest) error {
	if req.BotUUID == "" || req.ChannelAccountID == "" {
		return errors.New("bot_uuid and channel_account_id are required")
	}
	msgType := req.MsgType
	if msgType == "" {
		msgType = MsgTypeText
	}
	var media bool
	switch msgType {
	case MsgTypeText:
		if req.Text == "" {
			return errors.New("text is required for msg_type=text")
		}
	case MsgTypeImage, MsgTypeVideo, MsgTypeFile:
		if req.FilePath == "" {
			return fmt.Errorf("file_path is required for msg_type=%s", msgType)
		}
		media = true
	default:
		return fmt.Errorf("unsupported msg_type: %s", msgType)
	}
	req.MsgType = msgType
	// 媒体走 media client(Timeout 120s 兜底): 发送预算由 ctx 唯一决定
	// (delivery 媒体 90s), 15s 的 quick client 会架空该预算。
	_, err := c.postWith(c.clientFor(media), ctx, "/send", req)
	return err
}

// clientFor 选择请求使用的 http.Client: 媒体发送走 media(长预算), 其余
// 走 quick(15s 短预算)。
func (c *Client) clientFor(media bool) *http.Client {
	if media {
		return c.media
	}
	return c.quick
}

// Stream action constants for StreamActionRequest.Action
// (IM_STREAMING_DELIVERY §4.2: /send 扩展 stream_action, 不新增端点)。
const (
	StreamActionOpen   = "open"
	StreamActionAppend = "append"
	StreamActionCommit = "commit"
	StreamActionAbort  = "abort"
)

// StreamActionRequest is the body for a /send stream_action call.
type StreamActionRequest struct {
	BotUUID          string `json:"bot_uuid"`
	ChannelAccountID string `json:"channel_account_id"`
	StreamID         string `json:"stream_id,omitempty"`
	StreamAction     string `json:"stream_action"`
	Text             string `json:"text,omitempty"`
}

// StreamActionResponse is returned by stream_action=open.
type StreamActionResponse struct {
	Sent     bool   `json:"sent"`
	StreamID string `json:"stream_id,omitempty"`
}

// StreamAction 执行一次 IM 流式动作(open|append|commit|abort)。open 返回
// 渠道侧 stream_id(飞书=占位消息句柄), 其余动作按 stream_id 路由。
// 非流渠道返回 NotImplementedError → 调用方回退终态 delivery。
func (c *Client) StreamAction(ctx context.Context, req StreamActionRequest) (StreamActionResponse, error) {
	if req.BotUUID == "" {
		return StreamActionResponse{}, errors.New("bot_uuid is required")
	}
	switch req.StreamAction {
	case StreamActionOpen:
		if req.ChannelAccountID == "" {
			return StreamActionResponse{}, errors.New("channel_account_id is required for stream_action=open")
		}
	case StreamActionAppend, StreamActionCommit, StreamActionAbort:
		if req.StreamID == "" {
			return StreamActionResponse{}, fmt.Errorf("stream_id is required for stream_action=%s", req.StreamAction)
		}
	default:
		return StreamActionResponse{}, fmt.Errorf("unsupported stream_action: %s", req.StreamAction)
	}
	body, err := c.post(ctx, "/send", req)
	if err != nil {
		return StreamActionResponse{}, err
	}
	var resp StreamActionResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return StreamActionResponse{}, fmt.Errorf("decode stream action response: %w", err)
	}
	return resp, nil
}

// HealthResponse is returned by GET /health.
type HealthResponse struct {
	Healthy    bool     `json:"healthy"`
	ActiveBots []string `json:"active_bots"`
	// InboundCoalesceWindowMS 是 Poller 当前入站合并窗口(ms), 供平台对账/
	// 诊断(2026-08-15: 窗口曾因 poller 重启回 0 且 /health 不可见, 静默
	// 失效 18 小时)。
	InboundCoalesceWindowMS int `json:"inbound_coalesce_window_ms,omitempty"`
}

// Health checks poller liveness.
func (c *Client) Health(ctx context.Context) (HealthResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/health", nil)
	if err != nil {
		return HealthResponse{}, fmt.Errorf("build request: %w", err)
	}
	resp, err := c.quick.Do(req)
	if err != nil {
		return HealthResponse{}, fmt.Errorf("poller health: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return HealthResponse{}, fmt.Errorf("poller health status %d: %s", resp.StatusCode, string(body))
	}
	var h HealthResponse
	if err := json.Unmarshal(body, &h); err != nil {
		return HealthResponse{}, fmt.Errorf("decode health: %w", err)
	}
	return h, nil
}

func (c *Client) post(ctx context.Context, path string, body any) ([]byte, error) {
	return c.postWith(c.quick, ctx, path, body)
}

func (c *Client) postWith(cl *http.Client, ctx context.Context, path string, body any) ([]byte, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshal body: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	// Sign request with X-API-Signature if apiSecret is set
	if c.apiSecret != "" {
		mac := hmac.New(sha256.New, []byte(c.apiSecret))
		mac.Write(payload)
		sig := hex.EncodeToString(mac.Sum(nil))
		req.Header.Set("X-API-Signature", sig)
	}

	resp, err := cl.Do(req)
	if err != nil {
		return nil, fmt.Errorf("poller request %s: %w", path, err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("poller %s status %d: %s", path, resp.StatusCode, string(respBody))
	}
	return respBody, nil
}
