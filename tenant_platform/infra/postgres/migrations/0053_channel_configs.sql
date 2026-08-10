-- Migration 0053: bots → channel_configs 统一渠道配置模型。
--
-- 背景: 渠道配置从微信专用(bots/ilink)泛化为多 IM 渠道(飞书/钉钉/QQ)。
-- 设计真值: tenant_platform/docs/IM_CHANNEL_BINDING.zh-CN.md §3。
--
-- 决策:
--   * 不建新表、不留两表并存——bots 语义泛化为"用户渠道连接配置"。
--   * RENAME TABLE 后 messages/bot_transport_state/context_tokens 的外键
--     自动跟随(PostgreSQL 语义), 零 FK 风险; 存量微信行 channel_type
--     默认 'wechat', 零数据迁移。
--   * token_ciphertext/token_key_version 改名 config_*: 凭据是 JSON 密文
--     (微信={token}, 新渠道={app_id, app_secret}), 不再只是"token"。
--   * 唯一约束从每用户一行(bots_owner_uq)放宽为每用户每渠道一行。
--   * state 语义泛化: active | disabled(解绑) | expired | revoked。
--   * ilink_user_id 保留为微信专用列(新渠道 NULL; 账号标识在 config JSON 内)。
--
-- 0003 的 marker 就是 bots 表本身(to_regclass('bots')): RENAME 后该表
-- 消失会让 EnsureSchema 把 0003 当作未应用而重放(CREATE TABLE bots 重建
-- 空表)。因此本迁移在 RENAME 后重建一个仅作 marker 的 stub bots 表
-- (与 teams/platform_commands/users 等早期 marker 表同惯例)。

-- 幂等性说明: RENAME 无 IF NOT EXISTS, 用 DO 块条件执行(0052 先例:
-- 测试删 marker 后重放场景安全)。
DO $$
BEGIN
  IF to_regclass('public.bots') IS NOT NULL
     AND to_regclass('public.channel_configs') IS NULL THEN
    ALTER TABLE bots RENAME TO channel_configs;
  END IF;
END
$$;

ALTER TABLE channel_configs
    ADD COLUMN IF NOT EXISTS channel_type TEXT NOT NULL DEFAULT 'wechat';

DO $$
BEGIN
  IF EXISTS (
    SELECT 1 FROM information_schema.columns
    WHERE table_schema = current_schema()
      AND table_name = 'channel_configs'
      AND column_name = 'token_ciphertext'
  ) THEN
    ALTER TABLE channel_configs RENAME COLUMN token_ciphertext TO config_ciphertext;
  END IF;
  IF EXISTS (
    SELECT 1 FROM information_schema.columns
    WHERE table_schema = current_schema()
      AND table_name = 'channel_configs'
      AND column_name = 'token_key_version'
  ) THEN
    ALTER TABLE channel_configs RENAME COLUMN token_key_version TO config_key_version;
  END IF;
END
$$;

-- 每用户每渠道一行(原 bots_owner_uq 仅允许一行/用户)
DROP INDEX IF EXISTS bots_owner_uq;
CREATE UNIQUE INDEX IF NOT EXISTS channel_configs_owner_type_uq
    ON channel_configs (owner_id, channel_type);

-- state 语义泛化: + disabled(解绑)。约束名随 RENAME 保留 bots_state_check,
-- 显式替换为渠道语义命名。
DO $$
BEGIN
  IF EXISTS (
    SELECT 1 FROM pg_constraint
    WHERE conname = 'bots_state_check'
      AND conrelid = 'channel_configs'::regclass
  ) THEN
    ALTER TABLE channel_configs DROP CONSTRAINT bots_state_check;
  END IF;
END
$$;
DO $$
BEGIN
  IF NOT EXISTS (
    SELECT 1 FROM pg_constraint
    WHERE conname = 'channel_configs_state_check'
      AND conrelid = 'channel_configs'::regclass
  ) THEN
    ALTER TABLE channel_configs ADD CONSTRAINT channel_configs_state_check
        CHECK (state IN ('active', 'disabled', 'expired', 'revoked'));
  END IF;
END
$$;

-- 0003 marker 存续: stub 表仅作 EnsureSchema 的已应用标记, 无数据语义。
CREATE TABLE IF NOT EXISTS bots (
    id BOOLEAN PRIMARY KEY DEFAULT TRUE CHECK (id)
);
INSERT INTO bots (id) VALUES (TRUE)
ON CONFLICT (id) DO NOTHING;

CREATE TABLE IF NOT EXISTS migration_0053_channel_configs_marker (
    id BOOLEAN PRIMARY KEY DEFAULT TRUE CHECK (id)
);
INSERT INTO migration_0053_channel_configs_marker(id)
VALUES (TRUE)
ON CONFLICT (id) DO NOTHING;
