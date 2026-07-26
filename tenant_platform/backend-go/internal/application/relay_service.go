package application

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/infrastructure/transport"
)

// relaySendTimeout bounds the iLink send of one relay message so a hung
// Poller cannot block the inbound webhook request indefinitely. Matches
// deliverySendTimeout in delivery_service.go.
const relaySendTimeout = 15 * time.Second

// RelayStore is the persistence port for relay recipient resolution and
// opt-out preference management. *postgres.Store implements it implicitly.
type RelayStore interface {
	GetRelayRecipient(ctx context.Context, username string) (domain.RelayRecipient, error)
	// GetUserByID returns (id, username, status) for the sender. Reuses the
	// existing bot_store method to avoid duplicating user lookups.
	GetUserByID(ctx context.Context, userID int64) (int64, string, domain.UserStatus, error)
	SetRelayOptOut(ctx context.Context, userID int64, optOut bool) error
}

// AuditRecorder records relay metadata (no content) to audit_events.
// *postgres.Store implements this via AppendAuditEvent.
type AuditRecorder interface {
	AppendAuditEvent(ctx context.Context, event domain.AuditEvent) error
}

// RelayService forwards @username messages between platform users via their
// bound WeChat bots, bypassing the LLM/Worker entirely. The router intercepts
// "@<username> <text>" and delegates here.
type RelayService interface {
	// Relay forwards text from fromUserID to the user named toUsername.
	// Returns nil on success. Sender must be approved with a bound bot
	// (guaranteed by the router); recipient must be approved, opted-in, and
	// bound.
	Relay(ctx context.Context, fromUserID int64, toUsername, text string) error
	// SetOptOut enables/disables relay reception for userID.
	SetOptOut(ctx context.Context, userID int64, optOut bool) error
}

// RelayServiceConfig wires the relay service dependencies.
type RelayServiceConfig struct {
	Store     RelayStore
	Transport transport.BotTransportAdapter
	Audit     AuditRecorder
}

type relayService struct {
	store     RelayStore
	transport transport.BotTransportAdapter
	audit     AuditRecorder
}

// NewRelayService constructs the service. store, transport, and audit are
// all required: relay needs recipient resolution, message delivery, and
// metadata-only audit (no content recorded, per spec §6 R6).
func NewRelayService(cfg RelayServiceConfig) (RelayService, error) {
	if cfg.Store == nil {
		return nil, fmt.Errorf("relay store is required")
	}
	if cfg.Transport == nil {
		return nil, fmt.Errorf("transport is required")
	}
	if cfg.Audit == nil {
		return nil, fmt.Errorf("audit recorder is required")
	}
	return &relayService{
		store:     cfg.Store,
		transport: cfg.Transport,
		audit:     cfg.Audit,
	}, nil
}

// Relay implements the @username forwarding path.
//
// Validation order is deliberate: cheapest checks first (inputs), then
// recipient resolution (one DB query), then state checks, then the iLink
// send. The self-relay check uses UserID equality after resolution to catch
// case variants of the sender's own username.
func (s *relayService) Relay(ctx context.Context, fromUserID int64, toUsername, text string) error {
	toUsername = strings.TrimSpace(toUsername)
	text = strings.TrimSpace(text)
	if toUsername == "" {
		return fmt.Errorf("relay: recipient username is required")
	}
	if text == "" {
		return domain.ErrRelayEmptyMessage
	}
	if fromUserID <= 0 {
		return fmt.Errorf("relay: invalid sender user id")
	}

	recipient, err := s.store.GetRelayRecipient(ctx, toUsername)
	if err != nil {
		if errors.Is(err, domain.ErrRelayUserNotFound) {
			return err
		}
		return fmt.Errorf("resolve recipient: %w", err)
	}
	if recipient.UserID == fromUserID {
		return domain.ErrRelaySelfTarget
	}
	if recipient.Status != domain.UserApproved {
		return domain.ErrRelayUserNotApproved
	}
	if recipient.OptOut {
		return domain.ErrRelayOptedOut
	}
	if !recipient.IsBound() {
		return domain.ErrRelayUserNotBound
	}

	_, fromUsername, _, err := s.store.GetUserByID(ctx, fromUserID)
	if err != nil {
		return fmt.Errorf("resolve sender username: %w", err)
	}
	if fromUsername == "" {
		return domain.ErrRelaySenderUnknown
	}

	relayText := fmt.Sprintf("[来自 %s 的消息] %s", fromUsername, text)
	// Bounded send: without a timeout a hung Poller would block the whole
	// inbound HTTP request. Mirrors deliverySendTimeout in delivery_service.go.
	sendCtx, cancel := context.WithTimeout(ctx, relaySendTimeout)
	defer cancel()
	if err := s.transport.SendMessage(sendCtx, recipient.BotUUID, recipient.IlinkUserID, relayText); err != nil {
		return fmt.Errorf("send relay: %w", err)
	}

	// Best-effort metadata-only audit (spec §6 R6: no content recorded).
	// The relay is already delivered; a missing audit row is acceptable.
	s.persistRelayAudit(ctx, fromUserID, fromUsername, recipient, len(text))
	return nil
}

// SetOptOut toggles the user's relay reception preference.
func (s *relayService) SetOptOut(ctx context.Context, userID int64, optOut bool) error {
	if userID <= 0 {
		return fmt.Errorf("invalid user id")
	}
	if err := s.store.SetRelayOptOut(ctx, userID, optOut); err != nil {
		return fmt.Errorf("set relay opt out: %w", err)
	}
	return nil
}

// persistRelayAudit records relay metadata (participants, content length) to
// audit_events. No message content is stored (spec §6 R6). Failures are logged
// but do not fail the relay (the message was already sent).
func (s *relayService) persistRelayAudit(ctx context.Context, fromUserID int64, fromUsername string, r domain.RelayRecipient, contentLen int) {
	detail, _ := json.Marshal(map[string]any{
		"from_user":    fromUsername,
		"to_user_id":   r.UserID,
		"content_len":  contentLen,
	})
	if err := s.audit.AppendAuditEvent(ctx, domain.AuditEvent{
		ActorUserID: fromUserID,
		Action:      domain.AuditRelayForwarded,
		TargetType:  "user",
		TargetID:    fmt.Sprintf("%d", r.UserID),
		Detail:      detail,
	}); err != nil {
		slog.ErrorContext(ctx, "relay: persist audit failed",
			"from_user_id", fromUserID,
			"recipient_user_id", r.UserID,
			"error", err)
	}
}
