-- Migration 0016: Team lifecycle (P1 slice 1)
--
-- Brings the teams/team_members tables from dev-loopback bootstrap to production
-- readiness. Adds:
--   - teams.team_persona_id: team-level default persona fallback
--   - team_members.id BIGSERIAL: stable short-id for commands (t-<id>)
--   - team_members.status: full state machine
--       pending_member: owner directly invited, awaiting member accept
--       pending_owner:  member applied via invite code OR accepted direct invite,
--                       awaiting owner approval
--       approved:       full member
--       rejected:       owner rejected application
--       removed:        owner removed member
--   - team_members.persona_id: per-member persona override
--   - team_members.context_notified_at: one-shot privacy notice dedup
--   - team_members audit fields: invited_by, invited_at, approved_at, removed_at
--   - active_contexts: user's current personal/team context switch
--   - team_invite_codes: one-time codes for the /邀请码 <code> flow
--
-- Existing dev-loopback rows are backfilled to status='approved' so the
-- bootstrap path stays idempotent.

CREATE TABLE IF NOT EXISTS migration_0016_team_lifecycle_marker ();

-- 1. teams: add team-level persona
ALTER TABLE teams ADD COLUMN IF NOT EXISTS team_persona_id UUID REFERENCES personas (id) ON DELETE SET NULL;

-- 2. team_members: switch to BIGSERIAL id for short-id commands; keep
--    (team_id, user_id) unique. Drop the composite PK first, then add id.
ALTER TABLE team_members DROP CONSTRAINT IF EXISTS team_members_pkey;
ALTER TABLE team_members ADD COLUMN IF NOT EXISTS id BIGSERIAL;
ALTER TABLE team_members ADD PRIMARY KEY (id);
ALTER TABLE team_members ADD CONSTRAINT team_members_team_user_uq UNIQUE (team_id, user_id);

-- Status machine
ALTER TABLE team_members ADD COLUMN IF NOT EXISTS status TEXT;
ALTER TABLE team_members ADD CONSTRAINT team_members_status_check CHECK (
    status IN ('pending_member', 'pending_owner', 'approved', 'rejected', 'removed')
);
-- Backfill pre-existing rows (dev-loopback owner+members) as approved.
UPDATE team_members SET status = 'approved' WHERE status IS NULL;

-- Make status NOT NULL now that all rows have a value.
ALTER TABLE team_members ALTER COLUMN status SET NOT NULL;

-- Per-member persona override (NULL → fall back to team_persona_id → platform default)
ALTER TABLE team_members ADD COLUMN IF NOT EXISTS persona_id UUID REFERENCES personas (id) ON DELETE SET NULL;

-- One-shot privacy-notice dedup: set when the user first enters team context.
ALTER TABLE team_members ADD COLUMN IF NOT EXISTS context_notified_at TIMESTAMPTZ;

-- Audit fields
ALTER TABLE team_members ADD COLUMN IF NOT EXISTS invited_by BIGINT REFERENCES users (id);
ALTER TABLE team_members ADD COLUMN IF NOT EXISTS invited_at TIMESTAMPTZ;
ALTER TABLE team_members ADD COLUMN IF NOT EXISTS approved_at TIMESTAMPTZ;
ALTER TABLE team_members ADD COLUMN IF NOT EXISTS removed_at TIMESTAMPTZ;
ALTER TABLE team_members ADD COLUMN IF NOT EXISTS updated_at TIMESTAMPTZ NOT NULL DEFAULT (timezone('utc', now()));

-- Index pending memberships by owner's team for fast polling.
CREATE INDEX IF NOT EXISTS team_members_status_idx ON team_members (status) WHERE status IN ('pending_member', 'pending_owner');

-- 3. active_contexts: a user's current working context.
--    team_id NULL  → personal:{user_id}
--    team_id set   → team:{team_id}
CREATE TABLE IF NOT EXISTS active_contexts (
    user_id     BIGINT PRIMARY KEY REFERENCES users (id) ON DELETE CASCADE,
    team_id     UUID REFERENCES teams (id) ON DELETE SET NULL,
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT (timezone('utc', now()))
);

-- 4. team_invite_codes: one-time codes for self-service join.
CREATE TABLE IF NOT EXISTS team_invite_codes (
    code            TEXT PRIMARY KEY,
    team_id         UUID NOT NULL REFERENCES teams (id) ON DELETE CASCADE,
    created_by      BIGINT NOT NULL REFERENCES users (id),
    used_by         BIGINT REFERENCES users (id),
    used_at         TIMESTAMPTZ,
    expires_at      TIMESTAMPTZ NOT NULL,
    state           TEXT NOT NULL DEFAULT 'active',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT (timezone('utc', now())),
    CONSTRAINT team_invite_codes_state_check CHECK (state IN ('active', 'used', 'revoked', 'expired')),
    CONSTRAINT team_invite_codes_code_nonempty CHECK (char_length(code) > 0)
);

CREATE INDEX IF NOT EXISTS team_invite_codes_team_idx ON team_invite_codes (team_id);
CREATE INDEX IF NOT EXISTS team_invite_codes_state_idx ON team_invite_codes (state) WHERE state = 'active';

-- 5. Seed new platform commands. Existing commands from 0004 are left as-is.
--    Uses ON CONFLICT to be idempotent across re-applies.
INSERT INTO platform_commands (command, action, handler, help_text, sort_order) VALUES
    ('/我的身份',  'intercept', 'identity',    '查看当前身份和上下文',         10),
    ('/个人',      'intercept', 'personal',    '切换到个人助手上下文',         11),
    ('/团队',      'intercept', 'team',        '进入团队上下文或列出可加入团队', 12),
    ('/邀请码',    'intercept', 'invite_code', '提交团队邀请码申请加入',       13),
    ('/同意',      'intercept', 'accept',      '同意团队直接邀请: /同意 t-456', 14),
    ('/批准',      'intercept', 'approve',     'Owner 批准成员加入: /批准 t-456', 15),
    ('/拒绝',      'intercept', 'reject',      '拒绝团队邀请: /拒绝 t-456',     16),
    ('/移除',      'intercept', 'remove',      'Owner 移除团队成员: /移除 @用户名', 17)
ON CONFLICT (command) DO NOTHING;
