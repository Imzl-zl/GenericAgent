-- Migration 0007: Admin-configurable LLM providers.
--
-- Problem: the platform hardcoded a single OpenAI-compatible upstream in
-- cmd/llm-proxy. GA Core supports multiple protocols (OpenAI-compatible and
-- Anthropic Messages); the platform should expose that through admin config.
--
-- Design:
--   llm_providers — one row per upstream provider. Exactly one active row
--   should be marked is_default. The scheduler reads the default provider and
--   writes the matching mykey.py variable (native_oai_config or
--   native_claude_config). The LLM Proxy decrypts the stored API key and
--   forwards to the upstream using the provider's protocol.

CREATE TABLE IF NOT EXISTS llm_providers (
    id                  BIGSERIAL PRIMARY KEY,
    name                TEXT NOT NULL UNIQUE,
    provider_type       TEXT NOT NULL CHECK (provider_type IN ('openai_compatible', 'anthropic_messages')),
    base_url            TEXT NOT NULL,
    model               TEXT NOT NULL,
    api_key_ciphertext  BYTEA NOT NULL,
    api_key_key_version TEXT NOT NULL,
    is_default          BOOLEAN NOT NULL DEFAULT FALSE,
    state               TEXT NOT NULL DEFAULT 'active' CHECK (state IN ('active', 'disabled')),
    created_at          TIMESTAMPTZ NOT NULL DEFAULT (timezone('utc', now())),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT (timezone('utc', now()))
);

CREATE UNIQUE INDEX IF NOT EXISTS llm_providers_default_uq
    ON llm_providers (is_default)
    WHERE is_default = TRUE;

CREATE INDEX IF NOT EXISTS llm_providers_state_idx
    ON llm_providers (state);
