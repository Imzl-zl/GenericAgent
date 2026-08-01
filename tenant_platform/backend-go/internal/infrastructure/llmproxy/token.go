package llmproxy

import (
	"context"
	cryptorand "crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
)

const (
	CapabilityIssuer   = "ga-platform"
	CapabilityAudience = "ga-llm-proxy"
	// SophubAudience 是 Runner → Platform Sophub proxy 的 capability audience。
	// 与 LLM capability 同签发体系但独立用途(方案 §5.2: Runner 不持有 Sophub Key)。
	SophubAudience = "ga-sophub-proxy"
	CapabilityType = "ga-llm-cap+jwt"
	validationLeeway = 30 * time.Second
)

var (
	ErrCapabilityInvalid          = errors.New("capability token invalid")
	ErrCapabilityExpired          = errors.New("capability token expired")
	ErrCapabilityRevoked          = errors.New("capability token revoked")
	ErrCapabilityAudienceMismatch = errors.New("capability token audience mismatch")
)

type CapabilitySpec struct {
	// Audience 目标 audience; 空表示默认 LLM Proxy audience。
	Audience         string
	SessionKey       string
	ProviderID       int64
	ProviderRevision int64
	ProviderType     domain.LLMProviderType
	Model            string
	PolicyVersion    string
	// TaskID 与 RunnerGeneration 将 capability 绑定到单个 task 与 Runner
	// generation(方案 §7): 终态后的 token 不能被下一条 task 继续使用。
	TaskID           string
	RunnerGeneration uint64
}

type CapabilityClaims struct {
	ProviderID       int64                  `json:"provider_id"`
	ProviderRevision int64                  `json:"provider_revision"`
	ProviderType     domain.LLMProviderType `json:"provider_type"`
	Model            string                 `json:"model"`
	PolicyVersion    string                 `json:"policy_version"`
	TaskID           string                 `json:"task_id,omitempty"`
	RunnerGeneration uint64                 `json:"runner_generation,omitempty"`
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

type CapabilityRevocationSource interface {
	IsCapabilityRevoked(ctx context.Context, jtiHash [32]byte) (bool, error)
}

type Issuer struct {
	signingKey []byte
	ttl        time.Duration
	clock      func() time.Time
}

func NewIssuer(signingKey []byte, ttl time.Duration) (*Issuer, error) {
	if len(signingKey) < MinSigningKeyLen {
		return nil, fmt.Errorf("signing key must be at least %d bytes", MinSigningKeyLen)
	}
	if ttl <= 0 {
		return nil, fmt.Errorf("ttl must be positive")
	}
	return &Issuer{signingKey: append([]byte(nil), signingKey...), ttl: ttl, clock: time.Now}, nil
}

func (i *Issuer) TTL() time.Duration {
	if i == nil {
		return 0
	}
	return i.ttl
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
		ProviderID:       spec.ProviderID,
		ProviderRevision: spec.ProviderRevision,
		ProviderType:     spec.ProviderType,
		Model:            spec.Model,
		PolicyVersion:    spec.PolicyVersion,
		TaskID:           spec.TaskID,
		RunnerGeneration: spec.RunnerGeneration,
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    CapabilityIssuer,
			Subject:   spec.SessionKey,
			Audience:  jwt.ClaimStrings{effectiveAudience(spec.Audience)},
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
		return "", CapabilityClaims{}, fmt.Errorf("sign capability token: %w", err)
	}
	return signed, claims, nil
}

type Validator struct {
	signingKey  []byte
	revocations CapabilityRevocationSource
	clock       func() time.Time
}

func NewValidator(signingKey []byte, revocations CapabilityRevocationSource) (*Validator, error) {
	if len(signingKey) < MinSigningKeyLen {
		return nil, fmt.Errorf("signing key must be at least %d bytes", MinSigningKeyLen)
	}
	if revocations == nil {
		return nil, fmt.Errorf("capability revocation source is required")
	}
	return &Validator{
		signingKey:  append([]byte(nil), signingKey...),
		revocations: revocations,
		clock:       time.Now,
	}, nil
}

func (v *Validator) Validate(ctx context.Context, tokenString string) (CapabilityClaims, error) {
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
	revoked, err := v.revocations.IsCapabilityRevoked(ctx, HashJTI(claims.ID))
	if err != nil {
		return CapabilityClaims{}, fmt.Errorf("check capability revocation: %w", err)
	}
	if revoked {
		return CapabilityClaims{}, fmt.Errorf("%w: jti=%s", ErrCapabilityRevoked, claims.ID)
	}
	return claims, nil
}

func HashJTI(jti string) [32]byte {
	return sha256.Sum256([]byte(jti))
}

// effectiveAudience 返回 audience(空 = LLM Proxy 默认)。
func effectiveAudience(audience string) string {
	if strings.TrimSpace(audience) == "" {
		return CapabilityAudience
	}
	return audience
}

// IssueSophubToken 签发 Runner → Platform Sophub proxy 的短期 capability
// (方案 §5.2: Runner 不持有 Sophub API Key)。
func (i *Issuer) IssueSophubToken(sessionKey string, ttl time.Duration) (string, CapabilityClaims, error) {
	if strings.TrimSpace(sessionKey) == "" {
		return "", CapabilityClaims{}, fmt.Errorf("session key is required")
	}
	if ttl <= 0 {
		ttl = i.ttl
	}
	jti, err := newJTI()
	if err != nil {
		return "", CapabilityClaims{}, err
	}
	now := i.clock().UTC()
	claims := CapabilityClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    CapabilityIssuer,
			Subject:   sessionKey,
			Audience:  jwt.ClaimStrings{SophubAudience},
			ExpiresAt: jwt.NewNumericDate(now.Add(ttl)),
			NotBefore: jwt.NewNumericDate(now),
			IssuedAt:  jwt.NewNumericDate(now),
			ID:        jti,
		},
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	token.Header["typ"] = CapabilityType
	signed, err := token.SignedString(i.signingKey)
	if err != nil {
		return "", CapabilityClaims{}, fmt.Errorf("sign sophub capability token: %w", err)
	}
	return signed, claims, nil
}

func validateCapabilitySpec(spec CapabilitySpec) error {
	if spec.SessionKey == "" {
		return fmt.Errorf("session key is required")
	}
	if spec.ProviderID <= 0 || spec.ProviderRevision <= 0 {
		return fmt.Errorf("provider id and revision must be positive")
	}
	if spec.ProviderType != domain.ProviderNativeOAI && spec.ProviderType != domain.ProviderNativeClaude {
		return fmt.Errorf("unsupported provider type %q", spec.ProviderType)
	}
	if spec.Model == "" || spec.PolicyVersion == "" {
		return fmt.Errorf("model and policy version are required")
	}
	return nil
}

func validateCapabilityClaims(claims CapabilityClaims) error {
	if claims.Subject == "" || claims.ID == "" {
		return fmt.Errorf("%w: subject and jti are required", ErrCapabilityInvalid)
	}
	return wrapCapabilitySpecError(validateCapabilitySpec(CapabilitySpec{
		SessionKey:       claims.Subject,
		ProviderID:       claims.ProviderID,
		ProviderRevision: claims.ProviderRevision,
		ProviderType:     claims.ProviderType,
		Model:            claims.Model,
		PolicyVersion:    claims.PolicyVersion,
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
