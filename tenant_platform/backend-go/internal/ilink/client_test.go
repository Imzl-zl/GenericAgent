package ilink

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestGetBotQRCode(t *testing.T) {
	wantQR := "mock-qrcode-token"
	wantURL := "https://example.com/qr.png"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/ilink/bot/get_bot_qrcode" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		if got := r.URL.Query().Get("bot_type"); got != "3" {
			t.Errorf("bot_type=%q, want 3", got)
		}
		assertCommonHeaders(t, r)
		_ = json.NewEncoder(w).Encode(QRCodeResponse{
			QRCode:           wantQR,
			QRCodeImgContent: wantURL,
			Ret:              0,
		})
	}))
	defer srv.Close()

	c, err := NewClient(ClientConfig{BaseURL: srv.URL, AppID: "bot", ClientVersion: "2.1.1"})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}

	resp, err := c.GetBotQRCode(context.Background())
	if err != nil {
		t.Fatalf("get qr code: %v", err)
	}
	if resp.QRCode != wantQR {
		t.Errorf("qrcode=%q, want %q", resp.QRCode, wantQR)
	}
	if resp.QRCodeImgContent != wantURL {
		t.Errorf("qrcode_img_content=%q, want %q", resp.QRCodeImgContent, wantURL)
	}
}

func TestGetBotQRCodeRejectsNonZeroRet(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(QRCodeResponse{Ret: 1001})
	}))
	defer srv.Close()

	c, _ := NewClient(ClientConfig{BaseURL: srv.URL, RetryAttempts: 2, RetryBaseDelay: 10 * time.Millisecond})
	_, err := c.GetBotQRCode(context.Background())
	if err == nil {
		t.Fatal("expected error for non-zero ret")
	}
}

// TestGetQRCodeStatusParsesTopLevelCredentials is the key regression guard:
// the official API returns credential fields at the top level, NOT nested.
func TestGetQRCodeStatusParsesTopLevelCredentials(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/ilink/bot/get_qrcode_status" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		if got := r.URL.Query().Get("qrcode"); got != "qr-token" {
			t.Errorf("qrcode=%q, want qr-token", got)
		}
		assertCommonHeaders(t, r)
		body := map[string]any{
			"status":          "confirmed",
			"ilink_bot_id":    "bot@im.bot",
			"bot_token":       "secret-token",
			"baseurl":         "https://ilinkai.weixin.qq.com",
			"ilink_user_id":   "wxid_abc@im.wechat",
			"ret":             0,
		}
		_ = json.NewEncoder(w).Encode(body)
	}))
	defer srv.Close()

	c, _ := NewClient(ClientConfig{BaseURL: srv.URL})
	resp, err := c.GetQRCodeStatus(context.Background(), "qr-token")
	if err != nil {
		t.Fatalf("get status: %v", err)
	}
	if !resp.IsConfirmed() {
		t.Fatalf("expected confirmed status, got %s", resp.Status)
	}
	creds, ok := resp.ConfirmedCredentials()
	if !ok {
		t.Fatal("confirmed credentials not parsed")
	}
	if creds.ILinkBotID != "bot@im.bot" {
		t.Errorf("ilink_bot_id=%q", creds.ILinkBotID)
	}
	if creds.BotToken != "secret-token" {
		t.Errorf("bot_token=%q", creds.BotToken)
	}
	if creds.BaseURL != "https://ilinkai.weixin.qq.com" {
		t.Errorf("baseurl=%q", creds.BaseURL)
	}
	if creds.ILinkUserID != "wxid_abc@im.wechat" {
		t.Errorf("ilink_user_id=%q", creds.ILinkUserID)
	}
}

func TestGetQRCodeStatusTransitions(t *testing.T) {
	states := []string{"wait", "scaned", "scaned_but_redirect", "expired"}
	for _, want := range states {
		want := want
		t.Run(want, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_ = json.NewEncoder(w).Encode(map[string]any{"status": want, "ret": 0})
			}))
			defer srv.Close()
			c, _ := NewClient(ClientConfig{BaseURL: srv.URL})
			resp, err := c.GetQRCodeStatus(context.Background(), "qr-token")
			if err != nil {
				t.Fatalf("get status: %v", err)
			}
			if string(resp.Status) != want {
				t.Errorf("status=%q, want %q", resp.Status, want)
			}
			if _, ok := resp.ConfirmedCredentials(); ok {
				t.Error("non-confirmed status should not yield credentials")
			}
		})
	}
}

func assertCommonHeaders(t *testing.T, r *http.Request) {
	t.Helper()
	if got := r.Header.Get("iLink-App-Id"); got != "bot" {
		t.Errorf("iLink-App-Id=%q, want bot", got)
	}
	if got := r.Header.Get("iLink-App-ClientVersion"); got != "2.1.1" {
		t.Errorf("iLink-App-ClientVersion=%q, want 2.1.1", got)
	}
}

func TestSetCommonHeadersDoesNotWriteBody(t *testing.T) {
	c, _ := NewClient(ClientConfig{BaseURL: "http://example.com"})
	req := httptest.NewRequest(http.MethodGet, "http://example.com/ilink/bot/get_bot_qrcode", nil)
	c.setCommonHeaders(req)
	if req.Body != nil && req.Body != http.NoBody {
		b, _ := io.ReadAll(req.Body)
		if len(b) > 0 {
			t.Errorf("unexpected body %q", b)
		}
	}
}
