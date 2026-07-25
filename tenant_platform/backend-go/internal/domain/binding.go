// Package domain defines user lifecycle, binding, bot, and audit types
// for the platform control plane (architecture spec §5–§6).
package domain

import (
	"time"
)

// UserStatus is the platform user lifecycle state (spec §5: users).
type UserStatus string

const (
	UserPending  UserStatus = "pending"
	UserApproved UserStatus = "approved"
	UserBlocked  UserStatus = "blocked"
)

// IsValidUserStatus reports whether s is an allowed user status.
func IsValidUserStatus(s UserStatus) bool {
	return s == UserPending || s == UserApproved || s == UserBlocked
}

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

// BindingState is the binding attempt lifecycle (spec §5.1).
//
//	requested → qr_pending → awaiting_activation → active
//	                            ├→ expired
//	                            └→ revoked
type BindingState string

const (
	BindingRequested           BindingState = "requested"
	BindingQrPending           BindingState = "qr_pending"
	BindingAwaitingActivation  BindingState = "awaiting_activation"
	BindingActive              BindingState = "active"
	BindingExpired             BindingState = "expired"
	BindingRevoked             BindingState = "revoked"
)

// IsConsumable reports whether a binding in this state can be activated.
func (s BindingState) IsConsumable() bool {
	return s == BindingRequested || s == BindingQrPending || s == BindingAwaitingActivation
}

// BindingAttempt is a one-time WeChat binding code (spec §5.1).
// code_hash stores SHA-256(plaintext_code); plaintext is never persisted.
type BindingAttempt struct {
	ID          int64
	UserID      int64
	CodeHash    string
	State       BindingState
	BotUUID     string
	ExpiresAt   time.Time
	CreatedAt   time.Time
	UpdatedAt   time.Time
	ActivatedAt *time.Time
}

// BotState is the bot lifecycle (spec §5: bots).
type BotState string

const (
	BotActive  BotState = "active"
	BotExpired BotState = "expired"
	BotRevoked BotState = "revoked"
)

// Bot is a WeChat bot owned by a platform user (spec §5).
// ilink_user_id is bound after /activate; token_ciphertext is the encrypted
// upstream bot token, never plaintext.
type Bot struct {
	ID                int64
	BotUUID           string
	IlinkBotID        string
	OwnerID           int64
	IlinkUserID       string
	BaseURL           string
	TokenCiphertext   []byte
	TokenKeyVersion   int
	State             BotState
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

// IsBound reports whether the bot has a paired ilink_user_id.
func (b Bot) IsBound() bool { return b.IlinkUserID != "" }

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
	AuditUserCreated         AuditAction = "user_created"
	AuditUserApproved        AuditAction = "user_approved"
	AuditUserBlocked         AuditAction = "user_blocked"
	AuditBindingGenerated    AuditAction = "binding_generated"
	AuditBindingActivated    AuditAction = "binding_activated"
	AuditBindingExpired      AuditAction = "binding_expired"
	AuditBindingRevoked      AuditAction = "binding_revoked"
	AuditTaskCancelledByBlock AuditAction = "task_cancelled_by_block"
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
