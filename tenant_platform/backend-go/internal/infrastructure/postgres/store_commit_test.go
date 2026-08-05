package postgres

import (
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

// TestClassifyCommitError_DeterministicRollback verifies that a known rollback
// commit error is passed through unwrapped (round11 C2: only a *provable*
// rollback may authorize cleanup of orphaned committed files).
func TestClassifyCommitError_DeterministicRollback(t *testing.T) {
	if err := classifyCommitError(nil); err != nil {
		t.Fatalf("nil should stay nil, got %v", err)
	}
	err := classifyCommitError(pgx.ErrTxCommitRollback)
	if !errors.Is(err, pgx.ErrTxCommitRollback) {
		t.Fatalf("rollback error must be preserved, got %v", err)
	}
	if errors.Is(err, ErrCommitOutcomeUnknown) {
		t.Fatalf("deterministic rollback must NOT be classified as unknown outcome")
	}
}

// TestClassifyCommitError_UnknownOutcome verifies that non-rollback commit
// errors (network, timeout, closed connection) are classified as outcome
// unknown, so callers keep files for reconciliation instead of deleting
// a potentially committed restore point.
func TestClassifyCommitError_UnknownOutcome(t *testing.T) {
	networkErr := errors.New("connection reset by peer")
	err := classifyCommitError(networkErr)
	if !errors.Is(err, ErrCommitOutcomeUnknown) {
		t.Fatalf("network error must be classified as unknown outcome, got %v", err)
	}
	if !strings.Contains(err.Error(), "connection reset by peer") {
		t.Fatalf("original error must be preserved in message, got %v", err)
	}
	// ErrTxClosed (already closed, e.g. after a prior rollback) is also not
	// proof of a clean rollback outcome — treat conservatively.
	closedErr := classifyCommitError(pgx.ErrTxClosed)
	if !errors.Is(closedErr, ErrCommitOutcomeUnknown) {
		t.Fatalf("ErrTxClosed must be classified as unknown outcome, got %v", closedErr)
	}
}
