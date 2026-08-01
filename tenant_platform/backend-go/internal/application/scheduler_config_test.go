package application

import (
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/infrastructure/llmproxy"
)

func TestSchedulerConfigDirFor(t *testing.T) {
	cases := []struct {
		name    string
		scoped  bool
		root    string
		session string
		want    string
	}{
		{
			name:    "global loopback config",
			scoped:  false,
			root:    "/ga/config",
			session: "session-1",
			want:    "/ga/config",
		},
		{
			name:    "session-scoped container config",
			scoped:  true,
			root:    "/ga/config",
			session: "session-1",
			want:    filepath.Join("/ga/config", hashSessionKey("session-1")),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := &scheduler{cfg: SchedulerConfig{
				ConfigRoot:          tc.root,
				SessionScopedConfig: tc.scoped,
			}}
			got := s.configDirFor(tc.session)
			if got != tc.want {
				t.Fatalf("configDirFor() = %q, want %q", got, tc.want)
			}
			if tc.scoped && strings.Contains(got, tc.session) {
				t.Fatalf("session key leaked into config path: %q", got)
			}
		})
	}
}

func TestValidateSchedulerCredentialTiming(t *testing.T) {
	const wallClock = 45 * time.Minute
	const skew = 5 * time.Minute
	issuer, err := llmproxy.NewIssuer([]byte("test-signing-key-at-least-32-bytes"), wallClock+skew)
	if err != nil {
		t.Fatal(err)
	}
	valid := SchedulerConfig{
		TokenIssuer: issuer, TokenTTL: wallClock + skew,
		MaxTaskWallClock: wallClock, TokenRefreshSkew: skew,
	}
	if err := validateSchedulerCredentialTiming(valid); err != nil {
		t.Fatal(err)
	}
	mismatch := valid
	mismatch.TokenTTL += time.Minute
	if err := validateSchedulerCredentialTiming(mismatch); err == nil {
		t.Fatal("expected issuer and scheduler TTL mismatch")
	}
	tooShort := valid
	tooShort.TokenTTL = wallClock + skew - time.Second
	if err := validateSchedulerCredentialTiming(tooShort); err == nil {
		t.Fatal("expected insufficient task coverage")
	}
}

func hashSessionKey(sessionKey string) string {
	digest := sha256.Sum256([]byte(sessionKey))
	return hex.EncodeToString(digest[:])
}
