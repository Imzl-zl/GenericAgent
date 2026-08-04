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
	// Operation 是允许的操作(方案 §7: capability 必须包含操作):
	// "llm.chat" / "sophub.search" / "sophub.install"。
	Operation string
	// Budget 是预算描述(方案 §7: capability 必须包含预算), 采用与
	// RuntimePolicy 一致的 JSON 片段(如 {"max_turns":N,"max_output_bytes":N});
	// 由签发方填充。校验方核对结构, 并强制执行调用次数上限
	// max_turns(llm-proxy 按 JTI 原子计量, 审查 R4-I9); 其余字节类预算
	// 由 Worker 侧 RuntimePolicy 执行。
	Budget string
}

type CapabilityClaims struct {
	ProviderID       int64                  `json:"provider_id"`
	ProviderRevision int64                  `json:"provider_revision"`
	ProviderType     domain.LLMProviderType `json:"provider_type"`
	Model            string                 `json:"model"`
	PolicyVersion    string                 `json:"policy_version"`
	TaskID           string                 `json:"task_id,omitempty"`
	RunnerGeneration uint64                 `json:"runner_generation,omitempty"`
	Operation        string                 `json:"operation,omitempty"`
	Budget           string                 `json:"budget,omitempty"`
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

// TaskCapabilityChecker 在调用时刻联查 capability 绑定的 task 是否仍活跃
// (round9 审查): task 未终态且 claim 未过期 + runner lease generation 仍有效
// + 团队任务 requester 仍是 approved 成员。llm-proxy/sophub proxy 每次调用
// 前执行, 把成员移除/接管/恢复的生效时间收敛到下一次调用, 而不是 token TTL。
type TaskCapabilityChecker interface {
	IsTaskCapabilityActive(ctx context.Context, taskID string, runnerGeneration uint64) (bool, error)
}

// CapabilityUsageCounter 按 JTI 原子计量 capability 调用次数(审查 R4-I9):
// ConsumeCapabilityCall 递增计数, 超过 maxCalls 时返回 (false, nil)。
// llm-proxy 在转发前调用, 防止 Runner 绕过 Worker 的 RuntimePolicy 直接
// 刷 LLM Proxy。
type CapabilityUsageCounter interface {
	ConsumeCapabilityCall(ctx context.Context, jtiHash [32]byte, maxCalls int64) (bool, error)
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
		Operation:        spec.Operation,
		Budget:           spec.Budget,
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
	// taskChecker 非 nil 时(生产), 每次调用在线联查 task/lease/成员状态。
	taskChecker TaskCapabilityChecker
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

// WithTaskChecker 注入在线 task 活跃性校验(round9 审查: 生产必须配置;
// 测试/loopback 不配置时保持仅签名+撤销校验)。
func (v *Validator) WithTaskChecker(checker TaskCapabilityChecker) *Validator {
	v.taskChecker = checker
	return v
}

func (v *Validator) Validate(ctx context.Context, tokenString string) (CapabilityClaims, error) {
	claims, err := v.validateWithAudience(ctx, tokenString, CapabilityAudience)
	if err != nil {
		return CapabilityClaims{}, err
	}
	if err := validateCapabilityClaims(claims); err != nil {
		return CapabilityClaims{}, err
	}
	return claims, nil
}

// ValidateTaskScoped 在 Validate 基础上强制 task 绑定(方案 §7): LLM capability
// 必须绑定 task_id 与 runner_generation, 终态撤销 + 绑定校验后旧 token 无法
// 被下一条 task 复用。round9 审查: 在线 task/lease/成员联查由 handler 在
// provider 语义检查之后显式调用 CheckTaskActive(错误码优先级: 签名/撤销 >
// provider 404/409 > 在线 REVOKED)。
func (v *Validator) ValidateTaskScoped(ctx context.Context, tokenString string) (CapabilityClaims, error) {
	claims, err := v.validateWithAudience(ctx, tokenString, CapabilityAudience)
	if err != nil {
		return CapabilityClaims{}, err
	}
	if err := validateCapabilityClaims(claims); err != nil {
		return CapabilityClaims{}, err
	}
	if claims.TaskID == "" {
		return CapabilityClaims{}, fmt.Errorf("%w: task_id binding required", ErrCapabilityInvalid)
	}
	if claims.RunnerGeneration == 0 {
		return CapabilityClaims{}, fmt.Errorf("%w: runner_generation binding required", ErrCapabilityInvalid)
	}
	return claims, nil
}

// CheckTaskActive 在线联查 capability 绑定的 task 是否仍活跃(round9 审查):
// 成员移除/接管/恢复的生效时间收敛到下一次调用。未配置 taskChecker(测试/
// 显式降级)时跳过。
func (v *Validator) CheckTaskActive(ctx context.Context, claims CapabilityClaims) error {
	if v.taskChecker == nil {
		return nil
	}
	active, err := v.taskChecker.IsTaskCapabilityActive(ctx, claims.TaskID, claims.RunnerGeneration)
	if err != nil {
		return fmt.Errorf("check task capability active: %w", err)
	}
	if !active {
		return fmt.Errorf("%w: task %s no longer active at generation %d",
			ErrCapabilityRevoked, claims.TaskID, claims.RunnerGeneration)
	}
	return nil
}

// SophubValidator 校验 Runner → Platform Sophub proxy 的 capability
// (audience=ga-sophub-proxy, 方案 §5.2): 不要求 provider 字段, 但要求
// session subject + jti 未撤销 + task_id/runner_generation 绑定(审查 I9:
// 终态撤销前旧 task 的 sophub token 不可被新 task 复用)。
type SophubValidator struct {
	signingKey  []byte
	revocations CapabilityRevocationSource
	taskChecker TaskCapabilityChecker
	clock       func() time.Time
}

// NewSophubValidator 构建 Sophub audience 专用校验器。
func NewSophubValidator(signingKey []byte, revocations CapabilityRevocationSource) (*SophubValidator, error) {
	if len(signingKey) < MinSigningKeyLen {
		return nil, fmt.Errorf("signing key must be at least %d bytes", MinSigningKeyLen)
	}
	if revocations == nil {
		return nil, fmt.Errorf("capability revocation source is required")
	}
	return &SophubValidator{signingKey: append([]byte(nil), signingKey...), revocations: revocations, clock: time.Now}, nil
}

// WithTaskChecker 注入在线 task 活跃性校验(round9 审查, 同 Validator)。
func (v *SophubValidator) WithTaskChecker(checker TaskCapabilityChecker) *SophubValidator {
	v.taskChecker = checker
	return v
}

func (v *SophubValidator) Validate(ctx context.Context, tokenString string) (CapabilityClaims, error) {
	claims, err := validateWithAudience(v.signingKey, v.revocations, v.clock, ctx, tokenString, SophubAudience)
	if err != nil {
		return CapabilityClaims{}, err
	}
	// 审查 I9: sophub capability 必须绑定 task 与 runner generation
	// (与 LLM ValidateTaskScoped 对齐), 防止终态后旧 token 继续可用。
	if claims.TaskID == "" {
		return CapabilityClaims{}, fmt.Errorf("%w: task_id binding required", ErrCapabilityInvalid)
	}
	if claims.RunnerGeneration == 0 {
		return CapabilityClaims{}, fmt.Errorf("%w: runner_generation binding required", ErrCapabilityInvalid)
	}
	// 方案 §7: capability 必须包含操作, sophub token 只允许 sophub 操作。
	if claims.Operation != "sophub" {
		return CapabilityClaims{}, fmt.Errorf("%w: operation must be sophub", ErrCapabilityInvalid)
	}
	// round9 审查: 在线联查 task/lease/成员状态(同 Validator)。
	if v.taskChecker != nil {
		active, checkErr := v.taskChecker.IsTaskCapabilityActive(ctx, claims.TaskID, claims.RunnerGeneration)
		if checkErr != nil {
			return CapabilityClaims{}, fmt.Errorf("check task capability active: %w", checkErr)
		}
		if !active {
			return CapabilityClaims{}, fmt.Errorf("%w: task %s no longer active at generation %d",
				ErrCapabilityRevoked, claims.TaskID, claims.RunnerGeneration)
		}
	}
	return claims, nil
}

// validateWithAudience 解析并校验 audience/签名/撤销(不含 provider/task 语义)。
func (v *Validator) validateWithAudience(ctx context.Context, tokenString, audience string) (CapabilityClaims, error) {
	return validateWithAudience(v.signingKey, v.revocations, v.clock, ctx, tokenString, audience)
}

func validateWithAudience(signingKey []byte, revocations CapabilityRevocationSource, clock func() time.Time, ctx context.Context, tokenString, audience string) (CapabilityClaims, error) {
	claims := CapabilityClaims{}
	parser := jwt.NewParser(
		jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}),
		jwt.WithIssuer(CapabilityIssuer),
		jwt.WithAudience(audience),
		jwt.WithExpirationRequired(),
		jwt.WithIssuedAt(),
		jwt.WithLeeway(validationLeeway),
		jwt.WithTimeFunc(clock),
	)
	token, err := parser.ParseWithClaims(tokenString, &claims, func(token *jwt.Token) (any, error) {
		if token.Method != jwt.SigningMethodHS256 {
			return nil, fmt.Errorf("%w: unsupported signing method %q", ErrCapabilityInvalid, token.Method.Alg())
		}
		return signingKey, nil
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
	if claims.Subject == "" || claims.ID == "" {
		return CapabilityClaims{}, fmt.Errorf("%w: subject and jti are required", ErrCapabilityInvalid)
	}
	revoked, err := revocations.IsCapabilityRevoked(ctx, HashJTI(claims.ID))
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
// (方案 §5.2: Runner 不持有 Sophub API Key)。taskID/generation 绑定后,
// 旧 task 的 sophub token 无法被新 task 复用(方案 §7 generation 墙)。
// budget 是预算描述(审查 F10: sophub 调用按 JTI 原子计量, 无预算 = 拒绝)。
func (i *Issuer) IssueSophubToken(sessionKey, taskID string, runnerGeneration uint64, ttl time.Duration, budget string) (string, CapabilityClaims, error) {
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
		TaskID:           taskID,
		RunnerGeneration: runnerGeneration,
		Operation:        "sophub",
		Budget:           budget,
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
	// 方案 §7: capability 必须包含操作。
	if spec.Operation == "" {
		return fmt.Errorf("operation is required")
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
		Operation:        claims.Operation,
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
