// Package ilink is a minimal HTTP client for the official WeChat iLink Bot API.
// It only covers QR-code login (get_bot_qrcode / get_qrcode_status).
// Message sending lives in internal/transport.
package ilink

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const defaultTimeout = 20 * time.Second

const (
	qrRetryAttempts = 6
	qrRetryBase     = 2 * time.Second
)

// ClientConfig wires the iLink client.
type ClientConfig struct {
	BaseURL       string
	AppID         string
	ClientVersion string
	HTTPClient    *http.Client
	// RetryAttempts is the number of attempts for get_bot_qrcode. Zero defaults to 6.
	RetryAttempts int
	// RetryBaseDelay is the linear backoff base per retry. Zero defaults to 2s.
	RetryBaseDelay time.Duration
}

// QRCodeResponse is returned by get_bot_qrcode.
type QRCodeResponse struct {
	QRCode           string `json:"qrcode"`
	QRCodeImgContent string `json:"qrcode_img_content"`
	Ret              int    `json:"ret"`
}

// QRCodeStatus is the possible values returned by get_qrcode_status.
type QRCodeStatus string

const (
	StatusWait              QRCodeStatus = "wait"
	StatusScaned            QRCodeStatus = "scaned"
	StatusScanedButRedirect QRCodeStatus = "scaned_but_redirect"
	StatusExpired           QRCodeStatus = "expired"
	StatusConfirmed         QRCodeStatus = "confirmed"
)

// QRCodeStatusResponse is returned by get_qrcode_status.
//
// The official API returns credential fields at the top level when status is
// "confirmed"; they are NOT nested under a "credentials" object.
type QRCodeStatusResponse struct {
	Status       QRCodeStatus `json:"status"`
	RedirectHost string       `json:"redirect_host,omitempty"`
	Ret          int          `json:"ret"`

	// Confirmed-only fields, returned at the top level by the official API.
	ILinkBotID  string `json:"ilink_bot_id,omitempty"`
	BotToken    string `json:"bot_token,omitempty"`
	BaseURL     string `json:"baseurl,omitempty"`
	ILinkUserID string `json:"ilink_user_id,omitempty"`
}

// IsConfirmed reports whether the response represents a successful login.
func (r QRCodeStatusResponse) IsConfirmed() bool {
	return r.Status == StatusConfirmed && r.Ret == 0
}

// ConfirmedCredentials returns the credential fields when the scan succeeded.
// The second return value is false if any required field is missing.
func (r QRCodeStatusResponse) ConfirmedCredentials() (QRCodeStatusResponse, bool) {
	if !r.IsConfirmed() {
		return r, false
	}
	if r.ILinkBotID == "" || r.BotToken == "" || r.ILinkUserID == "" {
		return r, false
	}
	return r, true
}

// Client calls the official iLink API.
type Client struct {
	cfg ClientConfig
}

// NewClient validates config and returns a client.
func NewClient(cfg ClientConfig) (*Client, error) {
	if strings.TrimSpace(cfg.BaseURL) == "" {
		return nil, errors.New("ilink BaseURL is required")
	}
	if strings.TrimSpace(cfg.AppID) == "" {
		cfg.AppID = "bot"
	}
	if strings.TrimSpace(cfg.ClientVersion) == "" {
		cfg.ClientVersion = "2.1.1"
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: defaultTimeout}
	}
	if cfg.RetryAttempts <= 0 {
		cfg.RetryAttempts = qrRetryAttempts
	}
	if cfg.RetryBaseDelay <= 0 {
		cfg.RetryBaseDelay = qrRetryBase
	}
	return &Client{cfg: cfg}, nil
}

// GetBotQRCode requests a new login QR code from iLink.
// It mirrors GA Core's login_qr() retry behaviour: on network errors or
// missing fields it backs off linearly to avoid hammering the API.
func (c *Client) GetBotQRCode(ctx context.Context) (QRCodeResponse, error) {
	url := fmt.Sprintf("%s/ilink/bot/get_bot_qrcode?bot_type=3", strings.TrimRight(c.cfg.BaseURL, "/"))

	var lastErr error
	for attempt := 0; attempt < c.cfg.RetryAttempts; attempt++ {
		if attempt > 0 {
			sleep := time.Duration(attempt) * c.cfg.RetryBaseDelay
			select {
			case <-ctx.Done():
				return QRCodeResponse{}, ctx.Err()
			case <-time.After(sleep):
			}
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return QRCodeResponse{}, fmt.Errorf("build request: %w", err)
		}
		c.setCommonHeaders(req)

		var resp QRCodeResponse
		if err := c.do(req, &resp); err != nil {
			lastErr = err
			continue
		}
		if resp.Ret != 0 {
			lastErr = fmt.Errorf("ilink ret %d", resp.Ret)
			continue
		}
		if resp.QRCode == "" || resp.QRCodeImgContent == "" {
			lastErr = errors.New("ilink returned incomplete qrcode")
			continue
		}
		return resp, nil
	}
	return QRCodeResponse{}, fmt.Errorf("exhausted %d attempts: %w", c.cfg.RetryAttempts, lastErr)
}

// GetQRCodeStatus polls the scan status of a QR code.
func (c *Client) GetQRCodeStatus(ctx context.Context, qrCode string) (QRCodeStatusResponse, error) {
	if qrCode == "" {
		return QRCodeStatusResponse{}, errors.New("qrcode is required")
	}
	url := fmt.Sprintf("%s/ilink/bot/get_qrcode_status?qrcode=%s", strings.TrimRight(c.cfg.BaseURL, "/"), qrCode)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return QRCodeStatusResponse{}, fmt.Errorf("build request: %w", err)
	}
	c.setCommonHeaders(req)

	var resp QRCodeStatusResponse
	if err := c.do(req, &resp); err != nil {
		return QRCodeStatusResponse{}, err
	}
	if resp.Ret != 0 {
		return QRCodeStatusResponse{}, fmt.Errorf("ilink ret %d", resp.Ret)
	}
	if resp.Status == "" {
		return QRCodeStatusResponse{}, errors.New("ilink returned empty status")
	}
	return resp, nil
}

func (c *Client) setCommonHeaders(req *http.Request) {
	req.Header.Set("iLink-App-Id", c.cfg.AppID)
	req.Header.Set("iLink-App-ClientVersion", c.cfg.ClientVersion)
}

func (c *Client) do(req *http.Request, out any) error {
	resp, err := c.cfg.HTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("ilink request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read body: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("ilink status %d: %s", resp.StatusCode, string(body))
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("decode body: %w (body=%s)", err, string(body))
	}
	return nil
}
