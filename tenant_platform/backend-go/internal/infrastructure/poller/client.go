// Package poller is the HTTP client for the Python Bot Poller process.
//
// The Poller reuses GA Core's WxBotClient (frontends/wxbot_client.py) to
// long-poll iLink getupdates and send messages. The Go platform delegates
// all iLink message I/O to the Poller so it never re-implements the protocol.
package poller

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const (
	defaultTimeout = 15 * time.Second
	// HTTP transport tunables for the Poller client. The Poller is a single
	// downstream host, so MaxIdleConnsPerHost is the lever that matters: the
	// Go default of 2 forces connection churn under any real concurrency.
	// 32 idle conns covers 10-20 active bots with headroom for bursts.
	defaultMaxIdleConns        = 64
	defaultMaxIdleConnsPerHost = 32
	defaultIdleConnTimeout     = 90 * time.Second
)

// Client calls the Python Bot Poller HTTP API.
type Client struct {
	baseURL string
	http    *http.Client
}

// NewClient validates the poller base URL and returns a client with a tuned
// transport: keep-alive is on, idle conns per host are raised above the Go
// default (2) so concurrent StartBot/SendMessage/Health calls reuse conns
// instead of churning TCP handshakes.
func NewClient(baseURL string) (*Client, error) {
	if strings.TrimSpace(baseURL) == "" {
		return nil, errors.New("poller base URL is required")
	}
	transport := &http.Transport{
		MaxIdleConns:        defaultMaxIdleConns,
		MaxIdleConnsPerHost: defaultMaxIdleConnsPerHost,
		IdleConnTimeout:     defaultIdleConnTimeout,
	}
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		http:    &http.Client{Timeout: defaultTimeout, Transport: transport},
	}, nil
}

// StartBotRequest is the body for POST /start.
type StartBotRequest struct {
	BotUUID    string `json:"bot_uuid"`
	BotToken   string `json:"bot_token"`
	ILinkBotID string `json:"ilink_bot_id"`
	BaseURL    string `json:"base_url"`
	UpdatesBuf string `json:"updates_buf"`
	WebhookURL string `json:"webhook_url"`
}

// StartBot tells the poller to begin long-polling for one bot.
func (c *Client) StartBot(ctx context.Context, req StartBotRequest) error {
	if req.BotUUID == "" || req.BotToken == "" || req.WebhookURL == "" {
		return errors.New("bot_uuid, bot_token, and webhook_url are required")
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
type SendMessageRequest struct {
	BotUUID      string `json:"bot_uuid"`
	ILinkUserID  string `json:"ilink_user_id"`
	Text         string `json:"text,omitempty"`
	ContextToken string `json:"context_token,omitempty"`
	MsgType      string `json:"msg_type,omitempty"`
	FilePath     string `json:"file_path,omitempty"`
}

// SendMessage delivers a text or media reply via the poller (which calls
// iLink sendmessage). Empty MsgType defaults to "text" for backward compat.
func (c *Client) SendMessage(ctx context.Context, req SendMessageRequest) error {
	if req.BotUUID == "" || req.ILinkUserID == "" {
		return errors.New("bot_uuid and ilink_user_id are required")
	}
	msgType := req.MsgType
	if msgType == "" {
		msgType = MsgTypeText
	}
	switch msgType {
	case MsgTypeText:
		if req.Text == "" {
			return errors.New("text is required for msg_type=text")
		}
	case MsgTypeImage, MsgTypeVideo, MsgTypeFile:
		if req.FilePath == "" {
			return fmt.Errorf("file_path is required for msg_type=%s", msgType)
		}
	default:
		return fmt.Errorf("unsupported msg_type: %s", msgType)
	}
	req.MsgType = msgType
	_, err := c.post(ctx, "/send", req)
	return err
}

// HealthResponse is returned by GET /health.
type HealthResponse struct {
	Healthy    bool     `json:"healthy"`
	ActiveBots []string `json:"active_bots"`
}

// Health checks poller liveness.
func (c *Client) Health(ctx context.Context) (HealthResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/health", nil)
	if err != nil {
		return HealthResponse{}, fmt.Errorf("build request: %w", err)
	}
	resp, err := c.http.Do(req)
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
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshal body: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
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
