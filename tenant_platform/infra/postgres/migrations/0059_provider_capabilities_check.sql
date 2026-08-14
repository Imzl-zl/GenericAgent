-- Migration 0059: llm_providers.capabilities DB 级 CHECK 约束(0058 应用层校验的 DB 兜底)。
--
-- 背景(Phase B 审查 S1): 0058 的 capabilities 列只有应用层校验, 无 DB 级
-- 约束——绕过应用层直接 SQL 写入可落非法值/空数组。补 CHECK:
--   * jsonb 数组且非空
--   * 元素 ⊆ {chat, image}(<@ 子集判断; 重复元素由应用层拒绝, DB 层放行)
--
-- 存量数据合法性: 0058 以来所有写入都经应用层校验(store/api 双侧), 加
-- 约束不会失败。约束名冲突时静默跳过(DO 块, 0052/0053 先例)。

DO $$
BEGIN
    ALTER TABLE llm_providers
        ADD CONSTRAINT llm_providers_capabilities_check
        CHECK (
            jsonb_typeof(capabilities) = 'array'
            AND jsonb_array_length(capabilities) > 0
            AND capabilities <@ '["chat","image"]'::jsonb
        );
EXCEPTION
    WHEN duplicate_object THEN NULL;
END $$;
