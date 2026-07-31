package documentgateway

import (
	"errors"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

var testSigningKey = []byte("test-document-gateway-signing-key-32-bytes")

func TestCapabilityRoundTripBindsSessionAndWorkspace(t *testing.T) {
	issuer, err := NewIssuer(testSigningKey, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	validator, err := NewValidator(testSigningKey)
	if err != nil {
		t.Fatal(err)
	}

	token, issued, err := issuer.Issue(CapabilitySpec{
		SessionKey:  "team:docs",
		WorkspaceID: "11111111-1111-1111-1111-111111111111",
	})
	if err != nil {
		t.Fatal(err)
	}
	claims, err := validator.Validate(token)
	if err != nil {
		t.Fatal(err)
	}

	if claims.Subject != "team:docs" || claims.WorkspaceID != "11111111-1111-1111-1111-111111111111" {
		t.Fatalf("claims = %+v", claims)
	}
	if claims.Issuer != CapabilityIssuer || !claims.VerifyAudience(CapabilityAudience, true) {
		t.Fatalf("registered claims = %+v", claims.RegisteredClaims)
	}
	if claims.ID == "" || claims.ID != issued.ID {
		t.Fatalf("jti = %q issued=%q", claims.ID, issued.ID)
	}
}

func TestCapabilityValidatorRejectsExpiredToken(t *testing.T) {
	issuer, err := NewIssuer(testSigningKey, time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	token, _, err := issuer.Issue(CapabilitySpec{
		SessionKey:  "team:docs",
		WorkspaceID: "11111111-1111-1111-1111-111111111111",
	})
	if err != nil {
		t.Fatal(err)
	}
	validator, err := NewValidator(testSigningKey)
	if err != nil {
		t.Fatal(err)
	}
	validator.clock = func() time.Time { return time.Now().UTC().Add(time.Minute) }

	if _, err := validator.Validate(token); !errors.Is(err, ErrCapabilityExpired) {
		t.Fatalf("err=%v", err)
	}
}

func TestCapabilityValidatorRejectsWrongTypeAudienceAndKey(t *testing.T) {
	now := time.Now().UTC()
	claims := CapabilityClaims{
		WorkspaceID: "11111111-1111-1111-1111-111111111111",
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer: CapabilityIssuer, Subject: "team:docs", Audience: jwt.ClaimStrings{CapabilityAudience},
			ExpiresAt: jwt.NewNumericDate(now.Add(time.Hour)), NotBefore: jwt.NewNumericDate(now),
			IssuedAt: jwt.NewNumericDate(now), ID: "jti-test",
		},
	}
	validator, err := NewValidator(testSigningKey)
	if err != nil {
		t.Fatal(err)
	}

	wrongType := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	wrongType.Header["typ"] = "ga-llm-cap+jwt"
	signedWrongType, err := wrongType.SignedString(testSigningKey)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := validator.Validate(signedWrongType); !errors.Is(err, ErrCapabilityInvalid) {
		t.Fatalf("wrong type err=%v", err)
	}

	wrongAudienceClaims := claims
	wrongAudienceClaims.Audience = jwt.ClaimStrings{"ga-llm-proxy"}
	wrongAudience := jwt.NewWithClaims(jwt.SigningMethodHS256, wrongAudienceClaims)
	wrongAudience.Header["typ"] = CapabilityType
	signedWrongAudience, err := wrongAudience.SignedString(testSigningKey)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := validator.Validate(signedWrongAudience); !errors.Is(err, ErrCapabilityAudienceMismatch) {
		t.Fatalf("wrong audience err=%v", err)
	}

	issuer, err := NewIssuer([]byte("different-document-gateway-key-32-bytes"), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	crossKey, _, err := issuer.Issue(CapabilitySpec{SessionKey: "team:docs", WorkspaceID: claims.WorkspaceID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := validator.Validate(crossKey); !errors.Is(err, ErrCapabilityInvalid) {
		t.Fatalf("cross-key err=%v", err)
	}
}

func TestCapabilityRejectsUnsafeInputs(t *testing.T) {
	issuer, err := NewIssuer(testSigningKey, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := issuer.Issue(CapabilitySpec{SessionKey: "team:docs", WorkspaceID: "not-a-uuid"}); err == nil {
		t.Fatal("expected bad workspace error")
	}
	if _, err := NewIssuer([]byte("short"), time.Hour); err == nil {
		t.Fatal("expected short key error")
	}
	if _, err := NewValidator([]byte("short")); err == nil {
		t.Fatal("expected short key error")
	}
}
