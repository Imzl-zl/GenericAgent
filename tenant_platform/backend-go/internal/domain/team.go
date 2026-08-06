// Package domain defines team lifecycle types (PRD §5, architecture §5).
package domain

import (
	"errors"
	"fmt"
	"time"
)

// TeamMemberStatus is the lifecycle of a team membership (PRD §5.2).
//
//	pending_member → pending_owner → approved
//	                  ├→ rejected
//	approved → removed
type TeamMemberStatus string

const (
	// MemberPendingMember: owner directly invited the user; awaiting user's accept.
	MemberPendingMember TeamMemberStatus = "pending_member"
	// MemberPendingOwner: user applied via invite code OR accepted a direct invite;
	// awaiting owner approval.
	MemberPendingOwner TeamMemberStatus = "pending_owner"
	// MemberApproved: full member with task submission rights.
	MemberApproved TeamMemberStatus = "approved"
	// MemberRejected: owner rejected the application. Row retained for audit.
	MemberRejected TeamMemberStatus = "rejected"
	// MemberRemoved: owner removed the member. Row retained for audit.
	// A removed user may not rejoin via the same owner's invite codes.
	MemberRemoved TeamMemberStatus = "removed"
)


// IsPending reports whether the status is awaiting a state transition.
func (s TeamMemberStatus) IsPending() bool {
	return s == MemberPendingMember || s == MemberPendingOwner
}

// TeamRole distinguishes the owner from regular members.
type TeamRole string

const (
	RoleOwner  TeamRole = "owner"
	RoleMember TeamRole = "member"
)

// Team is a shared workspace owned by a user (PRD §5).
type Team struct {
	ID            string
	Name          string
	OwnerID       int64
	TeamPersonaID *string // optional, FK to personas.id
	CreatedAt     time.Time
}

// TeamMember is a user's membership in a team.
type TeamMember struct {
	ID                int64
	TeamID            string
	UserID            int64
	Role              TeamRole
	Status            TeamMemberStatus
	PersonaID         *string
	ContextNotifiedAt *time.Time
	InvitedBy         *int64
	InvitedAt         *time.Time
	ApprovedAt        *time.Time
	RemovedAt         *time.Time
	JoinedAt          time.Time
	UpdatedAt         time.Time
}

// ShortID returns the t-<id> form used in /同意 /批准 /拒绝 commands.
// Example: TeamMember{ID: 456} → "t-456".
func (m TeamMember) ShortID() string {
	return fmt.Sprintf("t-%d", m.ID)
}

// IsActive reports whether the member is currently an approved participant.
// Pending, rejected, and removed members are not active.
func (m TeamMember) IsActive() bool {
	return m.Status == MemberApproved
}

// ActiveContext is a user's current personal/team working context.
// When TeamID is nil, the user is in personal:{user_id}.
// When TeamID is set, the user is in team:{team_id}.
type ActiveContext struct {
	UserID    int64
	TeamID    *string
	UpdatedAt time.Time
}

// SessionKey returns the platform session_key for this context.
func (c ActiveContext) SessionKey(userID int64) string {
	if c.TeamID != nil {
		return "team:" + *c.TeamID
	}
	return PersonalSessionKey(userID)
}

// IsPersonal reports whether the user is currently in personal context.
func (c ActiveContext) IsPersonal() bool {
	return c.TeamID == nil
}

// TeamInviteCode is a one-time code for self-service team join (PRD T2).
type TeamInviteCode struct {
	Code      string
	TeamID    string
	CreatedBy int64
	UsedBy    *int64
	UsedAt    *time.Time
	ExpiresAt time.Time
	State     string // active | used | revoked | expired
	CreatedAt time.Time
}

// IsConsumable reports whether the code can still be used to apply.
func (c TeamInviteCode) IsConsumable(now time.Time) bool {
	return c.State == "active" && now.Before(c.ExpiresAt)
}

// TeamInviteCodeState constants.
const (
	TeamInviteActive  = "active"
	TeamInviteUsed     = "used"
	TeamInviteRevoked  = "revoked"
	TeamInviteExpired  = "expired"
)

// Team errors. Sentinel errors let the service/router distinguish user-facing
// failures from infrastructure failures without string matching.
var (
	// ErrTeamNotFound: team does not exist or user cannot see it.
	ErrTeamNotFound = errors.New("team not found")
	// ErrMemberNotFound: no team_members row for the given (team, user) or short id.
	ErrMemberNotFound = errors.New("team member not found")
	// ErrAlreadyMember: user is already an approved member of the team.
	ErrAlreadyMember = errors.New("user is already a member")
	// ErrInviteCodeInvalid: code does not exist, is used, revoked, or expired.
	ErrInviteCodeInvalid = errors.New("invite code invalid or expired")
	// ErrNotTeamOwner: caller is not the team owner for this operation.
	ErrNotTeamOwner = errors.New("only the team owner may perform this action")
	// ErrWrongMemberStatus: member is not in the status required for this transition.
	ErrWrongMemberStatus = errors.New("member is not in the expected status")
	// ErrActiveContextBlocked: user is blocked or removed and cannot enter team.
	ErrActiveContextBlocked = errors.New("user is not an approved member of this team")
)

// PersonalSessionKey is the personal session_key formatter.
// 审查 B5 收敛: 唯一真值源——application 层的重复实现已删除,
// router/api/store 统一引用本函数, 避免一处改格式另一处静默漂移。
func PersonalSessionKey(userID int64) string {
	return fmt.Sprintf("personal:%d", userID)
}
