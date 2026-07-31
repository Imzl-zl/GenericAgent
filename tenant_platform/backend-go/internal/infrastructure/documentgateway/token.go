package documentgateway

import (
	"context"
	cryptorand "crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/infrastructure/llmproxy"
)

const (
	CapabilityIssuer   = "ga-platform"
	CapabilityAudience = "ga-document-gateway"
	CapabilityType     = "ga-document-cap+jwt"
	validationLeeway   = 30 * time.Second
)

var (
	ErrCapabilityInvalid          = errors.New("document gateway capability token invalid")
	ErrCapabilityExpired          = errors.New("document gateway capability token expired")
	ErrCapabilityAudienceMismatch = errors.New("document gateway capability token audience mismatch")
)

type CapabilitySpec struct {
	SessionKey  string
	WorkspaceID string
}

type CapabilityClaims struct {
	WorkspaceID string `json:"workspace_id"`
	jwt.RegisteredClaims
}

func (c CapabilityClaims) VerifyAudience(expected string, required bool) bool {
	for _, audience := range c.Audience {
		if audience == expected {
			return true
		}
	}
	return !required
}

type Issuer struct {
	signingKey []byte
	ttl        time.Duration
	clock      func() time.Time
}

func NewIssuer(signingKey []byte, ttl time.Duration) (*Issuer, error) {
	if len(signingKey) < llmproxy.MinSigningKeyLen {
		return nil, fmt.Errorf("signing key must be at least %d bytes", llmproxy.MinSigningKeyLen)
	}
	if ttl <= 0 {
		return nil, fmt.Errorf("ttl must be positive")
	}
	return &Issuer{signingKey: append([]byte(nil), signingKey...), ttl: ttl, clock: time.Now}, nil
}

func (i *Issuer) Issue(spec CapabilitySpec) (string, CapabilityClaims, error) {
	if err := validateCapabilitySpec(spec); err != nil {
		return "", CapabilityClaims{}, err
	}
	jti, err := newJTI()
	if err != nil {
		return "", CapabilityClaims{}, err
	}
	now := i.clock().UTC()
	claims := CapabilityClaims{
		WorkspaceID: strings.TrimSpace(spec.WorkspaceID),
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    CapabilityIssuer,
			Subject:   strings.TrimSpace(spec.SessionKey),
			Audience:  jwt.ClaimStrings{CapabilityAudience},
			ExpiresAt: jwt.NewNumericDate(now.Add(i.ttl)),
			NotBefore: jwt.NewNumericDate(now),
			IssuedAt:  jwt.NewNumericDate(now),
			ID:        jti,
		},
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	token.Header["typ"] = CapabilityType
	signed, err := token.SignedString(i.signingKey)
	if err != nil {
		return "", CapabilityClaims{}, fmt.Errorf("sign document gateway capability token: %w", err)
	}
	return signed, claims, nil
}

func (i *Issuer) IssueDocumentGatewayToken(_ context.Context, sessionKey, workspaceID string) (string, error) {
	token, _, err := i.Issue(CapabilitySpec{SessionKey: sessionKey, WorkspaceID: workspaceID})
	return token, err
}

type Validator struct {
	signingKey []byte
	clock      func() time.Time
}

func NewValidator(signingKey []byte) (*Validator, error) {
	if len(signingKey) < llmproxy.MinSigningKeyLen {
		return nil, fmt.Errorf("signing key must be at least %d bytes", llmproxy.MinSigningKeyLen)
	}
	return &Validator{signingKey: append([]byte(nil), signingKey...), clock: time.Now}, nil
}

func (v *Validator) Validate(tokenString string) (CapabilityClaims, error) {
	claims := CapabilityClaims{}
	parser := jwt.NewParser(
		jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}),
		jwt.WithIssuer(CapabilityIssuer),
		jwt.WithAudience(CapabilityAudience),
		jwt.WithExpirationRequired(),
		jwt.WithIssuedAt(),
		jwt.WithLeeway(validationLeeway),
		jwt.WithTimeFunc(v.clock),
	)
	token, err := parser.ParseWithClaims(tokenString, &claims, func(token *jwt.Token) (any, error) {
		if token.Method != jwt.SigningMethodHS256 {
			return nil, fmt.Errorf("%w: unsupported signing method %q", ErrCapabilityInvalid, token.Method.Alg())
		}
		return v.signingKey, nil
	})
	if err != nil {
		if errors.Is(err, jwt.ErrTokenExpired) {
			return CapabilityClaims{}, fmt.Errorf("%w: %v", ErrCapabilityExpired, err)
		}
		if errors.Is(err, jwt.ErrTokenInvalidAudience) {
			return CapabilityClaims{}, fmt.Errorf("%w: %v", ErrCapabilityAudienceMismatch, err)
		}
		return CapabilityClaims{}, fmt.Errorf("%w: %v", ErrCapabilityInvalid, err)
	}
	if token == nil || !token.Valid {
		return CapabilityClaims{}, ErrCapabilityInvalid
	}
	if token.Header["typ"] != CapabilityType {
		return CapabilityClaims{}, fmt.Errorf("%w: token type must be %q", ErrCapabilityInvalid, CapabilityType)
	}
	if err := validateCapabilityClaims(claims); err != nil {
		return CapabilityClaims{}, err
	}
	return claims, nil
}

func validateCapabilitySpec(spec CapabilitySpec) error {
	if strings.TrimSpace(spec.SessionKey) == "" {
		return fmt.Errorf("session key is required")
	}
	if strings.ContainsRune(spec.SessionKey, '\x00') || len(spec.SessionKey) > 256 {
		return fmt.Errorf("session key is invalid")
	}
	if _, err := uuid.Parse(strings.TrimSpace(spec.WorkspaceID)); err != nil {
		return fmt.Errorf("workspace id must be a UUID: %w", err)
	}
	return nil
}

func validateCapabilityClaims(claims CapabilityClaims) error {
	if claims.Subject == "" || claims.ID == "" {
		return fmt.Errorf("%w: subject and jti are required", ErrCapabilityInvalid)
	}
	return wrapCapabilitySpecError(validateCapabilitySpec(CapabilitySpec{
		SessionKey: claims.Subject, WorkspaceID: claims.WorkspaceID,
	}))
}

func wrapCapabilitySpecError(err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%w: %v", ErrCapabilityInvalid, err)
}

func newJTI() (string, error) {
	bytes := make([]byte, 16)
	if _, err := cryptorand.Read(bytes); err != nil {
		return "", fmt.Errorf("generate jti: %w", err)
	}
	return hex.EncodeToString(bytes), nil
}
