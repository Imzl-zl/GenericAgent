package llmproxy

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
)

var testJWTKey = []byte("test-signing-key-at-least-32-bytes")

type fakeRevocationSource struct {
	revoked map[[32]byte]bool
	err     error
}

func (f *fakeRevocationSource) IsCapabilityRevoked(_ context.Context, digest [32]byte) (bool, error) {
	return f.revoked[digest], f.err
}

func newJWTTestPair(t *testing.T, ttl time.Duration, source CapabilityRevocationSource) (*Issuer, *Validator) {
	t.Helper()
	issuer, err := NewIssuer(testJWTKey, ttl)
	if err != nil {
		t.Fatal(err)
	}
	validator, err := NewValidator(testJWTKey, source)
	if err != nil {
		t.Fatal(err)
	}
	return issuer, validator
}

func validCapabilitySpec() CapabilitySpec {
	return CapabilitySpec{
		SessionKey:       "personal:42",
		ProviderID:       7,
		ProviderRevision: 3,
		ProviderType:     domain.ProviderNativeOAI,
		Model:            "gpt-test",
		PolicyVersion:    "foundation.no-host-tools.v1",
	}
}

func TestCapabilityJWTIssueAndValidate(t *testing.T) {
	source := &fakeRevocationSource{revoked: make(map[[32]byte]bool)}
	issuer, validator := newJWTTestPair(t, time.Hour, source)
	tokenString, issued, err := issuer.Issue(validCapabilitySpec())
	if err != nil {
		t.Fatal(err)
	}
	got, err := validator.Validate(context.Background(), tokenString)
	if err != nil {
		t.Fatal(err)
	}
	if got.Subject != "personal:42" || got.Issuer != CapabilityIssuer || !got.VerifyAudience(CapabilityAudience, true) {
		t.Fatalf("registered claims mismatch: %+v", got.RegisteredClaims)
	}
	if got.ID == "" || got.ID != issued.ID {
		t.Fatalf("jti = %q, issued %q", got.ID, issued.ID)
	}
	if got.ProviderID != 7 || got.ProviderRevision != 3 || got.ProviderType != domain.ProviderNativeOAI || got.Model != "gpt-test" {
		t.Fatalf("provider claims mismatch: %+v", got)
	}

	parts := strings.Split(tokenString, ".")
	headerBytes, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		t.Fatal(err)
	}
	var header map[string]any
	if err := json.Unmarshal(headerBytes, &header); err != nil {
		t.Fatal(err)
	}
	if header["alg"] != "HS256" || header["typ"] != CapabilityType {
		t.Fatalf("header = %v", header)
	}
}

func TestCapabilityIssuerRejectsInvalidProviderSpec(t *testing.T) {
	issuer, _ := newJWTTestPair(t, time.Hour, &fakeRevocationSource{revoked: make(map[[32]byte]bool)})
	spec := validCapabilitySpec()
	spec.ProviderID = 0
	if token, _, err := issuer.Issue(spec); err == nil || token != "" {
		t.Fatalf("token=%q err=%v", token, err)
	}
}

func TestCapabilityValidatorRejectsExpiredToken(t *testing.T) {
	issuer, validator := newJWTTestPair(t, time.Minute, &fakeRevocationSource{revoked: make(map[[32]byte]bool)})
	issuer.clock = func() time.Time { return time.Now().Add(-2 * time.Minute) }
	tokenString, _, err := issuer.Issue(validCapabilitySpec())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := validator.Validate(context.Background(), tokenString); !errors.Is(err, ErrCapabilityExpired) {
		t.Fatalf("err = %v", err)
	}
}

func TestCapabilityValidatorChecksPersistentRevocation(t *testing.T) {
	source := &fakeRevocationSource{revoked: make(map[[32]byte]bool)}
	issuer, validator := newJWTTestPair(t, time.Hour, source)
	tokenString, claims, err := issuer.Issue(validCapabilitySpec())
	if err != nil {
		t.Fatal(err)
	}
	source.revoked[HashJTI(claims.ID)] = true
	if _, err := validator.Validate(context.Background(), tokenString); !errors.Is(err, ErrCapabilityRevoked) {
		t.Fatalf("err = %v", err)
	}
}

func TestCapabilityValidatorPropagatesRevocationStoreFailure(t *testing.T) {
	storeErr := errors.New("revocation database unavailable")
	source := &fakeRevocationSource{revoked: make(map[[32]byte]bool), err: storeErr}
	issuer, validator := newJWTTestPair(t, time.Hour, source)
	tokenString, _, err := issuer.Issue(validCapabilitySpec())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := validator.Validate(context.Background(), tokenString); !errors.Is(err, storeErr) {
		t.Fatalf("err = %v", err)
	}
}

func TestCapabilityValidatorRejectsWrongTypeAndAlgorithm(t *testing.T) {
	now := time.Now()
	claims := CapabilityClaims{
		ProviderID: 7, ProviderRevision: 3, ProviderType: domain.ProviderNativeOAI,
		Model: "gpt-test", PolicyVersion: "p1",
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer: CapabilityIssuer, Subject: "personal:42", Audience: jwt.ClaimStrings{CapabilityAudience},
			ExpiresAt: jwt.NewNumericDate(now.Add(time.Hour)), IssuedAt: jwt.NewNumericDate(now),
			NotBefore: jwt.NewNumericDate(now), ID: "jti-test",
		},
	}
	source := &fakeRevocationSource{revoked: make(map[[32]byte]bool)}
	_, validator := newJWTTestPair(t, time.Hour, source)

	wrongType := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	wrongType.Header["typ"] = "JWT"
	wrongTypeString, err := wrongType.SignedString(testJWTKey)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := validator.Validate(context.Background(), wrongTypeString); err == nil {
		t.Fatal("expected explicit token type rejection")
	}

	wrongAlgorithm := jwt.NewWithClaims(jwt.SigningMethodHS384, claims)
	wrongAlgorithm.Header["typ"] = CapabilityType
	wrongAlgorithmString, err := wrongAlgorithm.SignedString(testJWTKey)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := validator.Validate(context.Background(), wrongAlgorithmString); err == nil {
		t.Fatal("expected algorithm rejection")
	}
}

func TestCapabilityValidatorRejectsWrongIssuerAndAudience(t *testing.T) {
	source := &fakeRevocationSource{revoked: make(map[[32]byte]bool)}
	_, validator := newJWTTestPair(t, time.Hour, source)
	now := time.Now()
	for name, registered := range map[string]jwt.RegisteredClaims{
		"issuer": {
			Issuer: "other", Subject: "personal:42", Audience: jwt.ClaimStrings{CapabilityAudience},
			ExpiresAt: jwt.NewNumericDate(now.Add(time.Hour)), IssuedAt: jwt.NewNumericDate(now), NotBefore: jwt.NewNumericDate(now), ID: "jti-issuer",
		},
		"audience": {
			Issuer: CapabilityIssuer, Subject: "personal:42", Audience: jwt.ClaimStrings{"other"},
			ExpiresAt: jwt.NewNumericDate(now.Add(time.Hour)), IssuedAt: jwt.NewNumericDate(now), NotBefore: jwt.NewNumericDate(now), ID: "jti-audience",
		},
	} {
		t.Run(name, func(t *testing.T) {
			claims := CapabilityClaims{ProviderID: 7, ProviderRevision: 3, ProviderType: domain.ProviderNativeOAI, Model: "gpt-test", PolicyVersion: "p1", RegisteredClaims: registered}
			token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
			token.Header["typ"] = CapabilityType
			raw, err := token.SignedString(testJWTKey)
			if err != nil {
				t.Fatal(err)
			}
			_, validationErr := validator.Validate(context.Background(), raw)
			if validationErr == nil {
				t.Fatal("expected registered claim rejection")
			}
			if name == "audience" && !errors.Is(validationErr, ErrCapabilityAudienceMismatch) {
				t.Fatalf("audience error = %v", validationErr)
			}
		})
	}
}

func TestCapabilityValidatorRejectsMalformedAndCrossKeyTokens(t *testing.T) {
	source := &fakeRevocationSource{revoked: make(map[[32]byte]bool)}
	issuer, validator := newJWTTestPair(t, time.Hour, source)
	for _, malformed := range []string{"", "garbage", "a.b", "a.b.c.d"} {
		if _, err := validator.Validate(context.Background(), malformed); err == nil {
			t.Fatalf("expected malformed rejection for %q", malformed)
		}
	}
	tokenString, _, err := issuer.Issue(validCapabilitySpec())
	if err != nil {
		t.Fatal(err)
	}
	other, err := NewValidator([]byte("different-signing-key-at-least-32-b"), source)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := other.Validate(context.Background(), tokenString); err == nil {
		t.Fatal("expected cross-key rejection")
	}
}

func TestCapabilityJWTRejectsShortSigningKey(t *testing.T) {
	if _, err := NewIssuer([]byte("short"), time.Hour); err == nil {
		t.Fatal("expected issuer key length error")
	}
	if _, err := NewValidator([]byte("short"), &fakeRevocationSource{}); err == nil {
		t.Fatal("expected validator key length error")
	}
}
