package llmproxy

import (
	"testing"
	"time"
)

func newTestIssuer(t *testing.T, ttl time.Duration) *Issuer {
	t.Helper()
	iss, err := NewIssuer([]byte("test-signing-key-0123456789ab"), ttl)
	if err != nil {
		t.Fatalf("NewIssuer: %v", err)
	}
	return iss
}

func newTestValidator(t *testing.T) *Validator {
	t.Helper()
	v, err := NewValidator([]byte("test-signing-key-0123456789ab"))
	if err != nil {
		t.Fatalf("NewValidator: %v", err)
	}
	return v
}

func TestTokenIssueAndValidate(t *testing.T) {
	iss := newTestIssuer(t, time.Hour)
	token, claims, err := iss.Issue("personal:42", "foundation.no-host-tools.v1", "openai_compatible", "gpt-test")
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if claims.SessionKey != "personal:42" {
		t.Fatalf("claims.SessionKey = %q", claims.SessionKey)
	}
	if claims.ProviderType != "openai_compatible" {
		t.Fatalf("claims.ProviderType = %q", claims.ProviderType)
	}
	if claims.Model != "gpt-test" {
		t.Fatalf("claims.Model = %q", claims.Model)
	}
	v := newTestValidator(t)
	got, err := v.Validate(token, "personal:42")
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if got.Jti != claims.Jti {
		t.Fatalf("jti mismatch %q != %q", got.Jti, claims.Jti)
	}
}

func TestTokenRejectsTamperedSignature(t *testing.T) {
	iss := newTestIssuer(t, time.Hour)
	token, _, err := iss.Issue("personal:42", "p", "openai_compatible", "gpt-test")
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	// Flip the last character of the signature.
	tampered := token[:len(token)-1]
	last := token[len(token)-1]
	if last == 'A' {
		tampered += "B"
	} else {
		tampered += "A"
	}
	v := newTestValidator(t)
	if _, err := v.Validate(tampered, "personal:42"); err == nil {
		t.Fatal("expected signature validation error")
	}
}

func TestTokenRejectsExpired(t *testing.T) {
	iss := newTestIssuer(t, time.Minute)
	// Issue in the past so ExpiresAt is already behind the validator clock.
	iss.clock = func() time.Time { return time.Now().Add(-2 * time.Minute) }
	token, _, err := iss.Issue("personal:42", "p", "openai_compatible", "gpt-test")
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	v := newTestValidator(t)
	if _, err := v.Validate(token, "personal:42"); err == nil {
		t.Fatal("expected expiry error")
	}
}

func TestTokenRejectsWrongSession(t *testing.T) {
	iss := newTestIssuer(t, time.Hour)
	token, _, err := iss.Issue("personal:42", "p", "openai_compatible", "gpt-test")
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	v := newTestValidator(t)
	if _, err := v.Validate(token, "personal:99"); err == nil {
		t.Fatal("expected session_key mismatch error")
	}
}

func TestTokenRevocation(t *testing.T) {
	iss := newTestIssuer(t, time.Hour)
	token, claims, err := iss.Issue("personal:42", "p", "openai_compatible", "gpt-test")
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	v := newTestValidator(t)
	if _, err := v.Validate(token, "personal:42"); err != nil {
		t.Fatalf("Validate before revoke: %v", err)
	}
	v.Revoke(claims.Jti)
	if _, err := v.Validate(token, "personal:42"); err == nil {
		t.Fatal("expected revocation error")
	}
}

func TestTokenRejectsMalformed(t *testing.T) {
	v := newTestValidator(t)
	for _, bad := range []string{"", "garbage", "a.b.c", "a.b"} {
		if _, err := v.ValidateUnscoped(bad); err == nil {
			t.Fatalf("expected error for %q", bad)
		}
	}
}

// TestTokenValidateRejectsEmptySession ensures callers cannot accidentally
// skip session binding by passing an empty string.
func TestTokenValidateRejectsEmptySession(t *testing.T) {
	iss := newTestIssuer(t, time.Hour)
	token, _, err := iss.Issue("personal:42", "p", "openai_compatible", "gpt-test")
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	v := newTestValidator(t)
	if _, err := v.Validate(token, ""); err == nil {
		t.Fatal("expected error when Validate is called with empty session_key")
	}
}

func TestTokenRejectsCrossKey(t *testing.T) {
	iss := newTestIssuer(t, time.Hour)
	token, _, err := iss.Issue("personal:42", "p", "openai_compatible", "gpt-test")
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	other, err := NewValidator([]byte("different-signing-key-012345"))
	if err != nil {
		t.Fatalf("NewValidator: %v", err)
	}
	if _, err := other.Validate(token, "personal:42"); err == nil {
		t.Fatal("expected signature error for token signed by different key")
	}
}

func TestIssuerRejectsShortKey(t *testing.T) {
	if _, err := NewIssuer([]byte("short"), time.Hour); err == nil {
		t.Fatal("expected signing key length error")
	}
}
