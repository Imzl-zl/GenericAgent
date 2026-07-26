package postgres

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
)

// AppendAuditEventTx records a lifecycle or authorization event inside a
// transaction. detail must be bounded and sanitized; never include real keys
// or full tokens.
func (s *Store) AppendAuditEventTx(ctx context.Context, tx pgx.Tx, event domain.AuditEvent) error {
	if event.Action == "" {
		return fmt.Errorf("audit action is required")
	}
	detail := event.Detail
	if detail == nil {
		detail = []byte("{}")
	}
	_, err := tx.Exec(ctx, `
INSERT INTO audit_events (actor_user_id, action, target_type, target_id, session_key, detail, policy_version)
VALUES ($1, $2, $3, $4, $5, $6::jsonb, $7)
`, nullableInt64(event.ActorUserID), string(event.Action), event.TargetType, event.TargetID,
		event.SessionKey, detail, event.PolicyVersion)
	return err
}

// AppendAuditEvent records an audit event outside a transaction.
func (s *Store) AppendAuditEvent(ctx context.Context, event domain.AuditEvent) error {
	if event.Action == "" {
		return fmt.Errorf("audit action is required")
	}
	detail := event.Detail
	if detail == nil {
		detail = []byte("{}")
	}
	_, err := s.pool.Exec(ctx, `
INSERT INTO audit_events (actor_user_id, action, target_type, target_id, session_key, detail, policy_version)
VALUES ($1, $2, $3, $4, $5, $6::jsonb, $7)
`, nullableInt64(event.ActorUserID), string(event.Action), event.TargetType, event.TargetID,
		event.SessionKey, detail, event.PolicyVersion)
	return err
}

// CountAuditEventsByAction returns the number of audit events matching the action.
// Used by tests to verify audit coverage.
func (s *Store) CountAuditEventsByAction(ctx context.Context, action domain.AuditAction) (int64, error) {
	var n int64
	err := s.pool.QueryRow(ctx, `
SELECT COUNT(*) FROM audit_events WHERE action = $1
`, string(action)).Scan(&n)
	return n, err
}

func nullableInt64(v int64) any {
	if v <= 0 {
		return nil
	}
	return v
}
