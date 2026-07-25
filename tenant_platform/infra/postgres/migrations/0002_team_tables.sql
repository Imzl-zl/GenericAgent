-- Minimal team support: teams, team_members, and workspace kind expansion.
-- Also fixes Bug D: the 0001 users_bootstrap_marker_uq unique index prevented
-- multiple dev-loopback users (required for multi-session testing). It is
-- replaced with a non-unique index so bootstrap_marker remains a tag, not a
-- singleton constraint.

CREATE TABLE teams (
    id                  UUID PRIMARY KEY,
    name                TEXT NOT NULL,
    owner_user_id       BIGINT NOT NULL REFERENCES users (id),
    bootstrap_marker    TEXT,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT (timezone('utc', now())),
    CONSTRAINT teams_name_nonempty CHECK (char_length(name) > 0),
    CONSTRAINT teams_bootstrap_marker_check CHECK (
        bootstrap_marker IS NULL OR bootstrap_marker = 'dev-loopback'
    )
);

CREATE UNIQUE INDEX teams_owner_name_uq ON teams (owner_user_id, name);

CREATE TABLE team_members (
    team_id             UUID NOT NULL REFERENCES teams (id) ON DELETE CASCADE,
    user_id             BIGINT NOT NULL REFERENCES users (id),
    role                TEXT NOT NULL,
    joined_at           TIMESTAMPTZ NOT NULL DEFAULT (timezone('utc', now())),
    CONSTRAINT team_members_role_check CHECK (role IN ('owner', 'member')),
    PRIMARY KEY (team_id, user_id)
);

CREATE INDEX team_members_user_idx ON team_members (user_id);

-- Expand workspace kind to allow team workspaces. The original 0001 constraints
-- (kind = 'personal', team_id IS NULL) are replaced with conditional rules:
-- personal workspaces must have team_id NULL; team workspaces must have team_id
-- pointing at teams.id.
ALTER TABLE workspaces DROP CONSTRAINT workspaces_kind_check;
ALTER TABLE workspaces DROP CONSTRAINT workspaces_personal_no_team;
ALTER TABLE workspaces
    ADD CONSTRAINT workspaces_kind_check CHECK (kind IN ('personal', 'team'));
ALTER TABLE workspaces
    ADD CONSTRAINT workspaces_personal_no_team CHECK (
        kind <> 'personal' OR team_id IS NULL
    );
ALTER TABLE workspaces
    ADD CONSTRAINT workspaces_team_requires_team CHECK (
        kind <> 'team' OR team_id IS NOT NULL
    );
ALTER TABLE workspaces
    ADD CONSTRAINT workspaces_team_fk
    FOREIGN KEY (team_id) REFERENCES teams (id);

-- Bug D fix: allow multiple dev-loopback users by dropping the singleton
-- unique index on bootstrap_marker. The marker is a tag, not a unique key.
DROP INDEX IF EXISTS users_bootstrap_marker_uq;
CREATE INDEX users_bootstrap_marker_idx ON users (bootstrap_marker)
    WHERE bootstrap_marker IS NOT NULL;

-- Same fix for workspaces: multiple dev-loopback workspaces are allowed.
DROP INDEX IF EXISTS workspaces_bootstrap_marker_uq;
CREATE INDEX workspaces_bootstrap_marker_idx ON workspaces (bootstrap_marker)
    WHERE bootstrap_marker IS NOT NULL;
