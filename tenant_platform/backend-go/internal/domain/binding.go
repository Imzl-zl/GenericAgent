// Package domain defines user lifecycle, binding, bot, and audit types
// for the platform control plane (architecture spec §5–§6).
package domain

import (
	"errors"
	"time"
)

// UserStatus is the platform user lifecycle state (spec §5: users).
type UserStatus string

const (
	UserPending  UserStatus = "pending"
	UserApproved UserStatus = "approved"
	UserBlocked  UserStatus = "blocked"
)


// User is the platform user record (spec §5: users).
type User struct {
	ID              int64
	Username        string
	PasswordHash    string
	Status          UserStatus
	BootstrapMarker string
	CreatedAt       time.Time
	ApprovedAt      *time.Time
}

// ErrChannelBindingNotFound is returned when no channel account is bound to a canonical user.
var ErrChannelBindingNotFound = errors.New("channel binding not found")

// ChannelBinding maps a channel account to a canonical_user_id (spec §3: 用户身份).
// A channel account can belong to at most one canonical user; a user may hold
// bindings on multiple channels. Team routing uses team:<team_id> and is not
// stored here.
type ChannelBinding struct {
	ChannelType      string
	ChannelAccountID string
	CanonicalUserID  int64
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

// ChannelType identifies an IM channel integration (IM_CHANNEL_BINDING §2).
type ChannelType string

const (
	// ChannelWechat is the iLink official gateway bot (个人自用, 扫码绑定).
	ChannelWechat ChannelType = "wechat"
	// ChannelFeishu is the Lark/Feishu enterprise self-built app (lark-oapi WS).
	ChannelFeishu ChannelType = "feishu"
	// ChannelDingTalk is the DingTalk open-platform app (dingtalk-stream).
	ChannelDingTalk ChannelType = "dingtalk"
	// ChannelQQ is the QQ open-platform bot (botpy WS).
	ChannelQQ ChannelType = "qq"
	// ChannelWecom is the WeCom intelligent bot (wecom_aibot_sdk WS).
	ChannelWecom ChannelType = "wecom"
)

// IsValidChannelType reports whether s is a supported channel type.
func IsValidChannelType(s string) bool {
	switch ChannelType(s) {
	case ChannelWechat, ChannelFeishu, ChannelDingTalk, ChannelQQ, ChannelWecom:
		return true
	default:
		return false
	}
}

// ChannelConfigState is the channel config lifecycle.
type ChannelConfigState string

const (
	ChannelActive   ChannelConfigState = "active"
	ChannelDisabled ChannelConfigState = "disabled"
	ChannelExpired  ChannelConfigState = "expired"
	ChannelRevoked  ChannelConfigState = "revoked"
)

// ChannelConfig is a user-owned channel connection configuration
// (IM_CHANNEL_BINDING §3; formerly Bot). One row per (owner_id, channel_type).
// ilink_user_id 是微信专用列(新渠道 NULL); 账号标识在 config JSON 内。
// ConfigCiphertext 是加密的渠道凭据 JSON(微信={token}, 新渠道={app_id,
// app_secret}), 永不明文入库。
type ChannelConfig struct {
	ID                int64
	BotUUID           string
	ChannelType       ChannelType
	IlinkBotID        string
	OwnerID           int64
	IlinkUserID       string
	BaseURL           string
	ConfigCiphertext  []byte
	ConfigKeyVersion  int
	State             ChannelConfigState
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

// IsBound reports whether the channel config is usable for routing.
// 微信: 扫码确认后才算绑定(ilink_user_id 非空); 新渠道: active 即可用
// (属主即 canonical user, 无需二次绑定)。
func (c ChannelConfig) IsBound() bool {
	if c.ChannelType == ChannelWechat {
		return c.IlinkUserID != ""
	}
	return c.State == ChannelActive
}

// WechatQRStatus is the iLink QR-code scan lifecycle.
type WechatQRStatus string

const (
	WechatQRWait      WechatQRStatus = "wait"
	WechatQRScaned    WechatQRStatus = "scaned"
	WechatQRRedirect  WechatQRStatus = "scaned_but_redirect"
	WechatQRExpired   WechatQRStatus = "expired"
	WechatQRConfirmed WechatQRStatus = "confirmed"
)

// WechatQRSession persists a QR-code login attempt for a platform user.
type WechatQRSession struct {
	ID                 string
	UserID             int64
	ILINKQRCode        string
	QRCodeImgURL       string
	Status             WechatQRStatus
	ILINKBotID         string
	ILINKUserID        string
	BotTokenCiphertext []byte
	BaseURL            string
	ExpiresAt          time.Time
	CreatedAt          time.Time
	UpdatedAt          time.Time
}

// IsExpired reports whether the QR session is past its expiry.
func (s WechatQRSession) IsExpired(now time.Time) bool { return now.After(s.ExpiresAt) }

// BotTransportState is the per-bot encrypted transport cursor (spec §5, §7.2).
type BotTransportState struct {
	BotID                  int64
	UpdateCursorCiphertext  []byte
	UpdateCursorKeyVersion int
	ReconnectState         string
	LastErrorAt            *time.Time
	LastErrorCode          string
	UpdatedAt              time.Time
}

// ContextToken is a per-(bot, ilink_user) capability credential (spec §5).
type ContextToken struct {
	ID              int64
	BotID           int64
	IlinkUserID     string
	TokenCiphertext []byte
	ExpiresAt       time.Time
	LastUsedAt      *time.Time
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// IsExpired reports whether the context token is past its expiry.
func (t ContextToken) IsExpired(now time.Time) bool { return now.After(t.ExpiresAt) }

// AuditAction is a stable audit event type (spec §5: audit_events).
type AuditAction string

const (
	AuditUserCreated          AuditAction = "user_created"
	AuditUserApproved         AuditAction = "user_approved"
	AuditUserBlocked          AuditAction = "user_blocked"
	AuditTaskCancelledByBlock AuditAction = "task_cancelled_by_block"
	AuditRelayForwarded       AuditAction = "relay_forwarded"
	AuditLLMRoutingBound      AuditAction = "llm_routing_bound"
	AuditChannelRebound       AuditAction = "channel_binding_rebound"
)

// AuditEvent is an append-only record of a lifecycle or authorization action.
// detail is bounded and sanitized; never contains real keys or full tokens.
type AuditEvent struct {
	ID           int64
	ActorUserID  int64
	Action       AuditAction
	TargetType   string
	TargetID     string
	SessionKey   string
	Detail       []byte
	PolicyVersion string
	CreatedAt    time.Time
}
