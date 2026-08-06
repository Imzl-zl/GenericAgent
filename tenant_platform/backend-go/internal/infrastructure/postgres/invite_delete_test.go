package postgres

import (
	"context"
	"testing"
	"time"
)

func TestDeleteInviteCodesPreservesRegisteredUserAndSession(t *testing.T) {
	pool := OpenTestPool(t)
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	dev, err := store.EnsureAdminContext(ctx, 9, "invite-delete-dev")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if _, err := store.CreateInviteCode(ctx, "USED-CODE", dev.UserID, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	registered, err := store.CreateUserWithInvite(
		ctx,
		"invite-user",
		"password-hash",
		"USED-CODE",
		"registered-session-token",
		now,
		now.Add(time.Hour),
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateInviteCode(ctx, "ACTIVE-CODE", dev.UserID, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}

	deleted, err := store.DeleteInviteCodes(ctx, []string{"USED-CODE", "ACTIVE-CODE", "MISSING-CODE"})
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 2 {
		t.Fatalf("deleted=%d, want 2", deleted)
	}

	var userCount, sessionCount, inviteCount int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM users WHERE id = $1`, registered.ID).Scan(&userCount); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM user_sessions WHERE token_hash = $1`, "registered-session-token").Scan(&sessionCount); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM invite_codes WHERE code = ANY($1::text[])`, []string{"USED-CODE", "ACTIVE-CODE"}).Scan(&inviteCount); err != nil {
		t.Fatal(err)
	}
	if userCount != 1 || sessionCount != 1 || inviteCount != 0 {
		t.Fatalf("user_count=%d session_count=%d invite_count=%d", userCount, sessionCount, inviteCount)
	}
}

func TestCreateUserWithInviteRollsBackUserAndConsumptionWhenSessionInsertFails(t *testing.T) {
	pool := OpenTestPool(t)
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	dev, err := store.EnsureAdminContext(ctx, 9, "invite-rollback-dev")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if _, err := store.CreateInviteCode(ctx, "ROLLBACK-CODE", dev.UserID, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateUserSession(ctx, "duplicate-token", dev.UserID, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}

	if _, err := store.CreateUserWithInvite(
		ctx,
		"rolled-back-user",
		"password-hash",
		"ROLLBACK-CODE",
		"duplicate-token",
		now,
		now.Add(time.Hour),
	); err == nil {
		t.Fatal("duplicate session token must fail registration")
	}

	var userCount int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM users WHERE username = $1`, "rolled-back-user").Scan(&userCount); err != nil {
		t.Fatal(err)
	}
	var state string
	var usedBy *int64
	if err := pool.QueryRow(ctx, `SELECT state, used_by_user_id FROM invite_codes WHERE code = $1`, "ROLLBACK-CODE").Scan(&state, &usedBy); err != nil {
		t.Fatal(err)
	}
	if userCount != 0 || state != "active" || usedBy != nil {
		t.Fatalf("user_count=%d invite_state=%s used_by=%v", userCount, state, usedBy)
	}
}
