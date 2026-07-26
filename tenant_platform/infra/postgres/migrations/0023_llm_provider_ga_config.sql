-- Migration 0023: 扩展 LLM Provider 以支持 GA Core 完整配置
--
-- 问题：当前的 provider_type 只有 'openai_compatible' 和 'anthropic_messages'，
-- 无法表达 GA Core 的完整配置（native_oai / native_claude + 各种参数）。
-- 这导致我们需要重复实现协议转换，而不是直接复用 GA Core。
--
-- 设计：
--   1. 添加 config JSONB 字段存储灵活配置（thinking_type、max_tokens、temperature 等）
--   2. provider_type 改为支持 GA 的类型：'native_oai' | 'native_claude'
--   3. 兼容旧数据：'openai_compatible' -> 'native_oai', 'anthropic_messages' -> 'native_claude'
--   4. Backend 读取这些配置生成 mykey.py 文件
--   5. Worker 启动时从 Platform 拉取 mykey.py

-- 添加 config 字段（JSONB 存储灵活配置）
ALTER TABLE llm_providers ADD COLUMN IF NOT EXISTS config JSONB NOT NULL DEFAULT '{}';

-- 更新 provider_type 检查约束以支持新类型
ALTER TABLE llm_providers DROP CONSTRAINT IF EXISTS llm_providers_provider_type_check;
ALTER TABLE llm_providers ADD CONSTRAINT llm_providers_provider_type_check
    CHECK (provider_type IN ('openai_compatible', 'anthropic_messages', 'native_oai', 'native_claude'));

-- 迁移现有数据：将旧类型映射到新类型
UPDATE llm_providers SET provider_type = 'native_oai' WHERE provider_type = 'openai_compatible';
UPDATE llm_providers SET provider_type = 'native_claude' WHERE provider_type = 'anthropic_messages';

-- 更新约束只允许新类型
ALTER TABLE llm_providers DROP CONSTRAINT llm_providers_provider_type_check;
ALTER TABLE llm_providers ADD CONSTRAINT llm_providers_provider_type_check
    CHECK (provider_type IN ('native_oai', 'native_claude'));

-- 添加注释
COMMENT ON COLUMN llm_providers.config IS 'GA Core 配置字段（JSONB）：thinking_type, max_tokens, temperature, reasoning_effort, fake_cc_system_prompt, max_retries, connect_timeout, read_timeout, context_win, proxy, user_agent, stream, api_mode 等';
COMMENT ON COLUMN llm_providers.provider_type IS 'GA Session 类型：native_oai (NativeOAISession) | native_claude (NativeClaudeSession)';
