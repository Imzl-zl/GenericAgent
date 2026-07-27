package llmproxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
)

func testConfig() Config {
	return Config{
		Listen:     "127.0.0.1:0",
		SigningKey: testJWTKey,
		TokenTTL:   time.Minute,
		ProviderSource: &fakeProviderSource{
			provider: testProvider(domain.ProviderNativeOAI, "http://127.0.0.1:18999", "gpt-test", testUpstreamKey),
		},
		Cipher:      &fakeCipher{wantVersion: 1},
		Revocations: &fakeRevocationSource{revoked: make(map[[32]byte]bool)},
	}
}

func TestNewServerRejectsEmptyConfig(t *testing.T) {
	if _, err := NewServer(Config{}); err == nil {
		t.Fatal("expected validation error for empty config")
	}
}

func TestNewServerRejectsNonLoopbackListen(t *testing.T) {
	cfg := testConfig()
	cfg.Listen = "0.0.0.0:8081"
	if _, err := NewServer(cfg); err == nil {
		t.Fatal("expected loopback validation error")
	}
}

func TestNewServerRejectsShortSigningKey(t *testing.T) {
	cfg := testConfig()
	cfg.SigningKey = []byte("short")
	if _, err := NewServer(cfg); err == nil {
		t.Fatal("expected signing key length error")
	}
}

func TestNewServerRejectsMissingProviderSource(t *testing.T) {
	cfg := testConfig()
	cfg.ProviderSource = nil
	if _, err := NewServer(cfg); err == nil {
		t.Fatal("expected provider source validation error")
	}
}

func TestNewServerRejectsMissingCipher(t *testing.T) {
	cfg := testConfig()
	cfg.Cipher = nil
	if _, err := NewServer(cfg); err == nil {
		t.Fatal("expected cipher validation error")
	}
}

func TestNewServerRejectsMissingRevocationSource(t *testing.T) {
	cfg := testConfig()
	cfg.Revocations = nil
	if _, err := NewServer(cfg); err == nil {
		t.Fatal("expected revocation source validation error")
	}
}

func TestHealthzReturnsOk(t *testing.T) {
	srv, err := NewServer(testConfig())
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["status"] != "ok" {
		t.Fatalf("status field = %q, want ok", body["status"])
	}
}
