DO $$
DECLARE
    unknown_fields TEXT;
    invalid_ids TEXT;
BEGIN
    SELECT string_agg(DISTINCT key, ', ' ORDER BY key)
    INTO unknown_fields
    FROM llm_providers AS provider,
         LATERAL jsonb_object_keys(provider.config) AS key
    WHERE key NOT IN (
        'thinking_type', 'thinking_budget_tokens', 'reasoning_effort',
        'temperature', 'max_tokens', 'context_win', 'trim_keep_prefix',
        'max_retries', 'read_timeout', 'stream', 'api_mode',
        'fake_cc_system_prompt', 'user_agent', 'service_tier',
        'omit_thinking', 'extra_sys_prompt', 'extra_sys_prompt_file',
        'proxy', 'verify', 'connect_timeout', 'timeout', 'top_p'
    );
    IF unknown_fields IS NOT NULL THEN
        RAISE EXCEPTION 'unknown llm provider config fields: %', unknown_fields;
    END IF;

    SELECT string_agg(id::TEXT, ', ' ORDER BY id)
    INTO invalid_ids
    FROM llm_providers
    WHERE jsonb_typeof(config) <> 'object'
       OR config ? 'top_p'
       OR config ? 'extra_sys_prompt_file'
       OR COALESCE(config->>'thinking_type', '') NOT IN ('', 'adaptive', 'enabled', 'disabled')
       OR COALESCE(config->>'reasoning_effort', '') NOT IN ('', 'none', 'minimal', 'low', 'medium', 'high', 'xhigh', 'max')
       OR COALESCE(config->>'api_mode', '') NOT IN ('', 'chat_completions', 'responses')
       OR COALESCE(config->>'service_tier', '') NOT IN ('', 'auto', 'default', 'priority', 'flex')
       OR jsonb_path_exists(config, '$.temperature ? (@.type() != "number" || @ < 0 || @ > 2)')
       OR jsonb_path_exists(config, '$.thinking_budget_tokens ? (@.type() != "number" || @ <= 0)')
       OR jsonb_path_exists(config, '$.max_tokens ? (@.type() != "number" || @ <= 0)')
       OR jsonb_path_exists(config, '$.context_win ? (@.type() != "number" || @ <= 0)')
       OR jsonb_path_exists(config, '$.trim_keep_prefix ? (@.type() != "number" || @ < 0)')
       OR jsonb_path_exists(config, '$.max_retries ? (@.type() != "number" || @ < 0)')
       OR jsonb_path_exists(config, '$.read_timeout ? (@.type() != "number" || @ < 5)')
       OR jsonb_path_exists(config, '$.connect_timeout ? (@.type() != "number" || @ <= 0)')
       OR jsonb_path_exists(config, '$.timeout ? (@.type() != "number" || @ <= 0)')
       OR jsonb_path_exists(config, '$.stream ? (@.type() != "boolean")')
       OR jsonb_path_exists(config, '$.fake_cc_system_prompt ? (@.type() != "boolean")')
       OR jsonb_path_exists(config, '$.verify ? (@.type() != "boolean")')
       OR jsonb_path_exists(config, '$.omit_thinking ? (@.type() != "boolean")')
       OR jsonb_path_exists(config, '$.user_agent ? (@.type() != "string")')
       OR jsonb_path_exists(config, '$.extra_sys_prompt ? (@.type() != "string")')
       OR jsonb_path_exists(config, '$.proxy ? (@.type() != "string")')
       OR (config->>'thinking_type' = 'enabled' AND NOT config ? 'thinking_budget_tokens')
       OR (provider_type = 'native_claude' AND config ? 'api_mode')
       OR (provider_type = 'native_claude' AND config ? 'service_tier')
       OR (provider_type = 'native_oai' AND config ? 'fake_cc_system_prompt')
       OR (config ? 'connect_timeout' AND config ? 'timeout' AND config->'connect_timeout' <> config->'timeout')
       OR octet_length(COALESCE(config->>'extra_sys_prompt', '')) > 65536;
    IF invalid_ids IS NOT NULL THEN
        RAISE EXCEPTION 'invalid llm provider config for provider ids: %', invalid_ids;
    END IF;
END $$;

ALTER TABLE llm_providers
    ADD COLUMN session_config JSONB NOT NULL DEFAULT '{}'::jsonb,
    ADD COLUMN transport_config JSONB NOT NULL DEFAULT '{"auth_mode":"auto"}'::jsonb,
    ADD COLUMN revision BIGINT NOT NULL DEFAULT 1 CHECK (revision > 0);

UPDATE llm_providers
SET session_config = config - ARRAY[
        'proxy', 'verify', 'connect_timeout', 'timeout', 'top_p',
        'extra_sys_prompt_file'
    ],
    transport_config = jsonb_strip_nulls(jsonb_build_object(
        'auth_mode', 'auto',
        'proxy_url', NULLIF(config->>'proxy', ''),
        'tls_verify', config->'verify',
        'connect_timeout_seconds', COALESCE(config->'connect_timeout', config->'timeout')
    ));

ALTER TABLE llm_providers DROP COLUMN config;

CREATE TABLE llm_capability_revocations (
    jti_hash BYTEA PRIMARY KEY CHECK (octet_length(jti_hash) = 32),
    expires_at TIMESTAMPTZ NOT NULL
);

CREATE INDEX llm_capability_revocations_expires_idx
    ON llm_capability_revocations(expires_at);

CREATE TABLE IF NOT EXISTS migration_0023_llm_provider_ga_config_marker (
    id BOOLEAN PRIMARY KEY DEFAULT TRUE
);
INSERT INTO migration_0023_llm_provider_ga_config_marker(id)
VALUES (TRUE)
ON CONFLICT DO NOTHING;

CREATE TABLE migration_0024_transparent_llm_proxy_marker (
    id BOOLEAN PRIMARY KEY DEFAULT TRUE
);
INSERT INTO migration_0024_transparent_llm_proxy_marker(id) VALUES (TRUE);
