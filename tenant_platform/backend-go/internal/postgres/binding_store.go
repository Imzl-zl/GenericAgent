package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
)

// BindingCodeTTL is the default lifetime of a one-time binding code.
const BindingCodeTTL = 10 * time.Minute

// CreateBindingAttempt inserts a new binding attempt in state 'requested'.
// codeHash must be SHA-256(plaintext_code); plaintext is never persisted.
func (s *Store) CreateBindingAttempt(ctx context.Context, userID int64, codeHash string, expiresAt time.Time) (domain.BindingAttempt, error) {
	if userID <= 0 {
		return domain.BindingAttempt{}, fmt.Errorf("user id must be positive")
	}
	if codeHash == "" {
		return domain.BindingAttempt{}, fmt.Errorf("code hash is required")
	}
	var b domain.BindingAttempt
	err := s.withTx(ctx, func(tx pgx.Tx) error {
		return scanBindingAttempt(tx.QueryRow(ctx, `
INSERT INTO binding_attempts (user_id, code_hash, state, expires_at)
VALUES ($1, $2, 'requested', $3)
RETURNING id, user_id, code_hash, state, bot_uuid, expires_at, created_at, updated_at, activated_at
`, userID, codeHash, expiresAt), &b)
	})
	return b, err
}

// FindConsumableBindingByCodeHash returns a binding that can still be activated.
// Returns pgx.ErrNoRows if no consumable binding exists for the hash.
func (s *Store) FindConsumableBindingByCodeHash(ctx context.Context, codeHash string) (domain.BindingAttempt, error) {
	var b domain.BindingAttempt
	err := scanBindingAttempt(s.pool.QueryRow(ctx, `
SELECT id, user_id, code_hash, state, bot_uuid, expires_at, created_at, updated_at, activated_at
FROM binding_attempts
WHERE code_hash = $1 AND state IN ('requested', 'qr_pending', 'awaiting_activation')
FOR UPDATE
`, codeHash), &b)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.BindingAttempt{}, pgx.ErrNoRows
	}
	return b, err
}

// ConsumeBinding atomically transitions a binding to 'active' and pairs it
// with the bot. Returns the updated binding. Fails if the binding is not
// consumable or has expired.
func (s *Store) ConsumeBinding(ctx context.Context, codeHash, botUUID string, now time.Time) (domain.BindingAttempt, error) {
	if codeHash == "" || botUUID == "" {
		return domain.BindingAttempt{}, fmt.Errorf("code hash and bot uuid are required")
	}
	var b domain.BindingAttempt
	err := s.withTx(ctx, func(tx pgx.Tx) error {
		if err := scanBindingAttempt(tx.QueryRow(ctx, `
SELECT id, user_id, code_hash, state, bot_uuid, expires_at, created_at, updated_at, activated_at
FROM binding_attempts
WHERE code_hash = $1 AND state IN ('requested', 'qr_pending', 'awaiting_activation')
FOR UPDATE
`, codeHash), &b); err != nil {
			return err
		}
		if now.After(b.ExpiresAt) {
			if _, e := tx.Exec(ctx, `
UPDATE binding_attempts SET state = 'expired', updated_at = $2 WHERE id = $1
`, b.ID, now); e != nil {
				return e
			}
			return fmt.Errorf("binding %d has expired", b.ID)
		}
		return scanBindingAttempt(tx.QueryRow(ctx, `
UPDATE binding_attempts
SET state = 'active', bot_uuid = $2::uuid, activated_at = $3, updated_at = $3
WHERE id = $1
RETURNING id, user_id, code_hash, state, bot_uuid, expires_at, created_at, updated_at, activated_at
`, b.ID, botUUID, now), &b)
	})
	return b, err
}

// ConsumeBindingAndBindBot atomically consumes a binding and pairs the bot with
// an ilink_user_id (spec §5.1: "bot_uuid + from_user_id + code hash" atomic bind).
// The binding state transition and the bot ilink_user_id update are in the same
// transaction. Returns the updated binding. Fails if the binding is not
// consumable, has expired, or the bot is not found/already bound.
func (s *Store) ConsumeBindingAndBindBot(ctx context.Context, codeHash, botUUID, ilinkUserID string, now time.Time) (domain.BindingAttempt, error) {
	if codeHash == "" || botUUID == "" || ilinkUserID == "" {
		return domain.BindingAttempt{}, fmt.Errorf("code hash, bot uuid, and ilink user id are required")
	}
	var b domain.BindingAttempt
	err := s.withTx(ctx, func(tx pgx.Tx) error {
		if err := scanBindingAttempt(tx.QueryRow(ctx, `
SELECT id, user_id, code_hash, state, bot_uuid, expires_at, created_at, updated_at, activated_at
FROM binding_attempts
WHERE code_hash = $1 AND state IN ('requested', 'qr_pending', 'awaiting_activation')
FOR UPDATE
`, codeHash), &b); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("no consumable binding for this code")
			}
			return err
		}
		if now.After(b.ExpiresAt) {
			if _, e := tx.Exec(ctx, `
UPDATE binding_attempts SET state = 'expired', updated_at = $2 WHERE id = $1
`, b.ID, now); e != nil {
				return e
			}
			return fmt.Errorf("binding %d has expired", b.ID)
		}
		// Lock the bot row and verify it's not already bound.
		var existingIlink *string
		if err := tx.QueryRow(ctx, `
SELECT ilink_user_id FROM bots WHERE bot_uuid = $1::uuid AND state = 'active' FOR UPDATE
`, botUUID).Scan(&existingIlink); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("no active bot for uuid %s", botUUID)
			}
			return err
		}
		if existingIlink != nil && *existingIlink != "" {
			return fmt.Errorf("bot %s is already bound to ilink user %s", botUUID, *existingIlink)
		}
		// Transition binding to active and pair the bot.
		if err := scanBindingAttempt(tx.QueryRow(ctx, `
UPDATE binding_attempts
SET state = 'active', bot_uuid = $2::uuid, activated_at = $3, updated_at = $3
WHERE id = $1
RETURNING id, user_id, code_hash, state, bot_uuid, expires_at, created_at, updated_at, activated_at
`, b.ID, botUUID, now), &b); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
UPDATE bots SET ilink_user_id = $2, updated_at = $3
WHERE bot_uuid = $1::uuid AND state = 'active'
`, botUUID, ilinkUserID, now); err != nil {
			return err
		}
		return s.AppendAuditEventTx(ctx, tx, domain.AuditEvent{
			ActorUserID: b.UserID,
			Action:      domain.AuditBindingActivated,
			TargetType:  "binding",
			TargetID:    fmt.Sprintf("%d", b.ID),
		})
	})
	return b, err
}

// ExpireDueBindings marks all consumable bindings past their expiry as 'expired'.
// Returns the number of rows affected.
func (s *Store) ExpireDueBindings(ctx context.Context, now time.Time) (int64, error) {
	tag, err := s.pool.Exec(ctx, `
UPDATE binding_attempts
SET state = 'expired', updated_at = $2
WHERE state IN ('requested', 'qr_pending', 'awaiting_activation')
  AND expires_at < $2
`, now)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// BindingByID returns a binding attempt by its ID.
func (s *Store) BindingByID(ctx context.Context, id int64) (domain.BindingAttempt, error) {
	var b domain.BindingAttempt
	err := scanBindingAttempt(s.pool.QueryRow(ctx, `
SELECT id, user_id, code_hash, state, bot_uuid, expires_at, created_at, updated_at, activated_at
FROM binding_attempts WHERE id = $1
`, id), &b)
	return b, err
}

func scanBindingAttempt(row pgx.Row, b *domain.BindingAttempt) error {
	var botUUID *string
	if err := row.Scan(&b.ID, &b.UserID, &b.CodeHash, &b.State, &botUUID, &b.ExpiresAt, &b.CreatedAt, &b.UpdatedAt, &b.ActivatedAt); err != nil {
		return err
	}
	if botUUID != nil {
		b.BotUUID = *botUUID
	}
	return nil
}
