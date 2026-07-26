-- Migration 0019: Drop legacy binding_attempts table.
--
-- The /activate <code> binding flow has been fully replaced by the official
-- iLink QR-code binding flow (spec §3-§4). The binding_attempts table stored
-- one-time binding codes for the legacy flow; it is no longer referenced by
-- any Go code (BindingService, BindingStore, binding_http.go all removed).
--
-- The bots table (also created by migration 0003) remains; only the
-- binding_attempts table and its indexes are dropped.
--
-- Marker table: migration_0019_drop_binding_attempts_marker

DROP TABLE IF EXISTS binding_attempts CASCADE;

CREATE TABLE IF NOT EXISTS migration_0019_drop_binding_attempts_marker (
    applied_at TIMESTAMPTZ NOT NULL DEFAULT (timezone('utc', now()))
);
