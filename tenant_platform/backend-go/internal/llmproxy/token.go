package llmproxy

import (
	cryptorand "crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"crypto/hmac"
)

// TokenClaims is the validated capability_token payload. A capability_token
// binds one LLM-calling session to a short-lived, revocable credential that
// carries no real upstream key.
type TokenClaims struct {
	Jti                string `json:"jti"`
	SessionKey         string `json:"session_key"`
	IssuedAt           int64  `json:"issued_at"`     // unix seconds
	ExpiresAt          int64  `json:"expires_at"`    // unix seconds
	ModelPolicyVersion string `json:"model_policy_version"`
}

// Issuer issues signed capability_tokens bound to a session. The platform
// (scheduler) holds an Issuer; the real upstream key never leaves the Proxy.
type Issuer struct {
	signingKey []byte
	ttl        time.Duration
	clock      func() time.Time
}

// NewIssuer validates the signing key and ttl.
func NewIssuer(signingKey []byte, ttl time.Duration) (*Issuer, error) {
	if len(signingKey) < MinSigningKeyLen {
		return nil, fmt.Errorf("signing key must be at least %d bytes", MinSigningKeyLen)
	}
	if ttl <= 0 {
		return nil, fmt.Errorf("ttl must be positive")
	}
	return &Issuer{signingKey: signingKey, ttl: ttl, clock: time.Now}, nil
}

// Issue creates a signed token for sessionKey. Returns the token string and
// the validated claims.
func (i *Issuer) Issue(sessionKey, modelPolicyVersion string) (string, TokenClaims, error) {
	if sessionKey == "" {
		return "", TokenClaims{}, fmt.Errorf("session_key is required")
	}
	now := i.clock()
	jti, err := newJTI()
	if err != nil {
		return "", TokenClaims{}, err
	}
	claims := TokenClaims{
		Jti:                jti,
		SessionKey:         sessionKey,
		IssuedAt:           now.Unix(),
		ExpiresAt:          now.Add(i.ttl).Unix(),
		ModelPolicyVersion: modelPolicyVersion,
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		return "", TokenClaims{}, fmt.Errorf("marshal claims: %w", err)
	}
	payloadB64 := base64.RawURLEncoding.EncodeToString(payload)
	sig := hmacSHA256(i.signingKey, []byte(payloadB64))
	sigB64 := base64.RawURLEncoding.EncodeToString(sig)
	return payloadB64 + "." + sigB64, claims, nil
}

// Validator validates capability_tokens and checks a revocation denylist. The
// Proxy holds one Validator; revocation is in-memory (tokens are short-lived,
// so a Proxy restart is covered by TTL).
type Validator struct {
	signingKey []byte
	clock      func() time.Time
	revoked    map[string]time.Time
	mu         sync.RWMutex
}

// NewValidator validates the signing key.
func NewValidator(signingKey []byte) (*Validator, error) {
	if len(signingKey) < MinSigningKeyLen {
		return nil, fmt.Errorf("signing key must be at least %d bytes", MinSigningKeyLen)
	}
	return &Validator{signingKey: signingKey, clock: time.Now, revoked: map[string]time.Time{}}, nil
}

// Validate checks signature, expiry, session binding, and revocation.
// expectedSessionKey, if non-empty, must match the token's session_key.
func (v *Validator) Validate(token, expectedSessionKey string) (TokenClaims, error) {
	claims, err := v.parseAndVerify(token)
	if err != nil {
		return TokenClaims{}, err
	}
	if expectedSessionKey != "" && claims.SessionKey != expectedSessionKey {
		return claims, fmt.Errorf("session_key mismatch: token=%q expected=%q", claims.SessionKey, expectedSessionKey)
	}
	now := v.clock().Unix()
	if claims.ExpiresAt <= now {
		return claims, fmt.Errorf("token expired at %d (now %d)", claims.ExpiresAt, now)
	}
	if v.IsRevoked(claims.Jti) {
		return claims, fmt.Errorf("token revoked: jti=%s", claims.Jti)
	}
	return claims, nil
}

// Revoke adds jti to the denylist. Idempotent.
func (v *Validator) Revoke(jti string) {
	if jti == "" {
		return
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	v.revoked[jti] = v.clock()
}

// IsRevoked reports whether jti is on the denylist.
func (v *Validator) IsRevoked(jti string) bool {
	v.mu.RLock()
	defer v.mu.RUnlock()
	_, ok := v.revoked[jti]
	return ok
}

func (v *Validator) parseAndVerify(token string) (TokenClaims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 2 {
		return TokenClaims{}, fmt.Errorf("invalid token format")
	}
	payloadB64, sigB64 := parts[0], parts[1]
	sig, err := base64.RawURLEncoding.DecodeString(sigB64)
	if err != nil {
		return TokenClaims{}, fmt.Errorf("invalid signature encoding: %w", err)
	}
	expected := hmacSHA256(v.signingKey, []byte(payloadB64))
	if !hmac.Equal(sig, expected) {
		return TokenClaims{}, fmt.Errorf("invalid signature")
	}
	payload, err := base64.RawURLEncoding.DecodeString(payloadB64)
	if err != nil {
		return TokenClaims{}, fmt.Errorf("invalid payload encoding: %w", err)
	}
	var claims TokenClaims
	if err := json.Unmarshal(payload, &claims); err != nil {
		return TokenClaims{}, fmt.Errorf("invalid payload JSON: %w", err)
	}
	if claims.Jti == "" || claims.SessionKey == "" {
		return claims, fmt.Errorf("token missing required fields")
	}
	return claims, nil
}

func hmacSHA256(key, data []byte) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write(data)
	return mac.Sum(nil)
}

func newJTI() (string, error) {
	b := make([]byte, 16)
	if _, err := cryptorand.Read(b); err != nil {
		return "", fmt.Errorf("generate jti: %w", err)
	}
	return hex.EncodeToString(b), nil
}
