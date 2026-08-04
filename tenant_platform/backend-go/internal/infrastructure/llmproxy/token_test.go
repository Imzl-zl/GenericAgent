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
		Operation:        "llm.chat",
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

func TestCapabilityValidatorReturnsTaskBinding(t *testing.T) {
	issuer, validator := newJWTTestPair(t, time.Hour, &fakeRevocationSource{revoked: make(map[[32]byte]bool)})
	tokenString, _, err := issuer.Issue(CapabilitySpec{
		SessionKey: "personal:1", ProviderID: 3, ProviderRevision: 5,
		ProviderType: "native_oai", Model: "gpt-4o", PolicyVersion: "v1",
		TaskID: "task-xyz", RunnerGeneration: 9, Operation: "llm.chat",
	})
	if err != nil {
		t.Fatal(err)
	}
	claims, err := validator.Validate(context.Background(), tokenString)
	if err != nil {
		t.Fatal(err)
	}
	if claims.TaskID != "task-xyz" || claims.RunnerGeneration != 9 {
		t.Fatalf("validator claims task binding = %s/%d", claims.TaskID, claims.RunnerGeneration)
	}
}

// TestSophubValidatorRequiresTaskAndGenerationBinding 验证 Sophub capability
// 必须绑定 task_id + runner_generation(审查 I9), 缺任一字段即拒绝。
func TestSophubValidatorRequiresTaskAndGenerationBinding(t *testing.T) {
	v, err := NewSophubValidator([]byte("test-signing-key-at-least-32-bytes"), &fakeRevocationSource{})
	if err != nil {
		t.Fatal(err)
	}
	issuer, err := NewIssuer([]byte("test-signing-key-at-least-32-bytes"), time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	// 无 task/generation 绑定的 sophub token 必须被拒绝。
	plain, _, err := issuer.IssueSophubToken("personal:1", "", 0, time.Hour, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := v.Validate(context.Background(), plain); err == nil {
		t.Fatal("sophub token without task/generation binding must be rejected")
	}

	// 完整绑定必须通过。
	bound, _, err := issuer.IssueSophubToken("personal:1", "task-1", 3, time.Hour, `{"max_turns":5}`)
	if err != nil {
		t.Fatal(err)
	}
	claims, err := v.Validate(context.Background(), bound)
	if err != nil {
		t.Fatalf("bound sophub token must pass: %v", err)
	}
	if claims.TaskID != "task-1" || claims.RunnerGeneration != 3 {
		t.Fatalf("claims = %+v", claims)
	}
}

type fakeTaskChecker struct {
	active bool
	err    error
}

func (f *fakeTaskChecker) IsTaskCapabilityActive(context.Context, string, uint64) (bool, error) {
	return f.active, f.err
}

// round9 审查: 在线 task 校验在 handler 层显式执行(CheckTaskActive), task
// 不再活跃(终态化/接管/成员移除)的 capability 必须被拒绝。
func TestValidateTaskScopedRejectsInactiveTask(t *testing.T) {
	issuer, validator := newJWTTestPair(t, time.Hour, &fakeRevocationSource{})
	validator.WithTaskChecker(&fakeTaskChecker{active: false})

	spec := validCapabilitySpec()
	spec.TaskID = "task-1"
	spec.RunnerGeneration = 3
	token, _, err := issuer.Issue(spec)
	if err != nil {
		t.Fatal(err)
	}
	claims, err := validator.ValidateTaskScoped(context.Background(), token)
	if err != nil {
		t.Fatalf("token validation itself must pass, got %v", err)
	}
	if err := validator.CheckTaskActive(context.Background(), claims); !errors.Is(err, ErrCapabilityRevoked) {
		t.Fatalf("inactive task must be rejected as revoked, got %v", err)
	}
}

// round9 审查: task 活跃时在线检查通过; 检查器错误时 fail-closed(不转发)。
func TestValidateTaskScopedActiveTaskAndCheckerError(t *testing.T) {
	issuer, validator := newJWTTestPair(t, time.Hour, &fakeRevocationSource{})
	validator.WithTaskChecker(&fakeTaskChecker{active: true})
	spec := validCapabilitySpec()
	spec.TaskID = "task-1"
	spec.RunnerGeneration = 3
	token, _, err := issuer.Issue(spec)
	if err != nil {
		t.Fatal(err)
	}
	claims, err := validator.ValidateTaskScoped(context.Background(), token)
	if err != nil {
		t.Fatalf("token validation must pass, got %v", err)
	}
	if err := validator.CheckTaskActive(context.Background(), claims); err != nil {
		t.Fatalf("active task must validate, got %v", err)
	}
	validator.WithTaskChecker(&fakeTaskChecker{err: errors.New("db down")})
	if err := validator.CheckTaskActive(context.Background(), claims); err == nil {
		t.Fatal("checker error must fail closed")
	}
}

// round9 审查: Sophub capability 同样受在线 task 校验约束。
func TestSophubValidatorRejectsInactiveTask(t *testing.T) {
	issuer, err := NewIssuer(testJWTKey, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	sv, err := NewSophubValidator(testJWTKey, &fakeRevocationSource{})
	if err != nil {
		t.Fatal(err)
	}
	sv.WithTaskChecker(&fakeTaskChecker{active: false})
	token, _, err := issuer.IssueSophubToken("personal:42", "task-1", 3, time.Hour, `{"max_turns":5}`)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sv.Validate(context.Background(), token); !errors.Is(err, ErrCapabilityRevoked) {
		t.Fatalf("inactive task must be rejected by sophub validator, got %v", err)
	}
}
