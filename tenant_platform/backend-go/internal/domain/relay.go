package domain

import "errors"

// RelayRecipient is the resolved target of an @username relay: the user's
// identity, relay opt-out flag, and bot binding (if any). BotID, BotUUID and
// IlinkUserID are zero/empty when the user has not bound a WeChat bot.
type RelayRecipient struct {
	UserID      int64
	Username    string
	Status      UserStatus
	OptOut      bool
	BotID       int64
	BotUUID     string
	IlinkUserID string
}

// IsBound reports whether the recipient has a bound, active bot that can
// receive iLink messages.
func (r RelayRecipient) IsBound() bool {
	return r.BotUUID != "" && r.IlinkUserID != ""
}

// Relay error sentinels. The router maps these to user-facing replies.
var (
	ErrRelayUserNotFound     = errors.New("relay: recipient username not found")
	ErrRelayUserNotApproved  = errors.New("relay: recipient is not an approved user")
	ErrRelayOptedOut         = errors.New("relay: recipient has opted out of relay")
	ErrRelayUserNotBound     = errors.New("relay: recipient has not bound a WeChat bot")
	ErrRelaySelfTarget       = errors.New("relay: cannot relay to yourself")
	ErrRelayEmptyMessage     = errors.New("relay: message body is empty")
	ErrRelaySenderNotBound   = errors.New("relay: sender has not bound a WeChat bot")
	ErrRelaySenderUnknown    = errors.New("relay: sender username could not be resolved")
)
