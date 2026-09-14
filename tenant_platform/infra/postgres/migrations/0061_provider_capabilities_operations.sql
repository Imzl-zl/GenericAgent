-- Migration 0061: llm_providers.capabilities CHECK 放开到 operation 细分形态。
--
-- 背景(2026-09-14 image capability operation 细分): "改图"是**通道属性**(上游端点 +
-- 网关适配器), 不是模型属性——实测同一网关下 agnes 只能文生图、sensenova 只能改图。
-- 平台需要能表达 image.generate / image.edit, 否则只能靠 GA 端试探(或静默能力错配)。
--
-- 规则:
--   * 应用层写入前把别名 image 归一化为 image.generate(api/llm_provider.go),
--     新行不会再出现字面值 image;
--   * 但 0058/0059 以来的存量行就是 ["image"], 必须继续合法 → CHECK 保留 image。
--   * 不做数据迁移: 读取侧(EffectiveCapabilities)同样归一化, 存量行零风险。
--
-- 约束替换用 DO 块幂等(0059 先例: 约束名冲突/已存在时静默跳过)。
DO $$
BEGIN
    ALTER TABLE llm_providers DROP CONSTRAINT IF EXISTS llm_providers_capabilities_check;
    ALTER TABLE llm_providers
        ADD CONSTRAINT llm_providers_capabilities_check
        CHECK (
            jsonb_typeof(capabilities) = 'array'
            AND jsonb_array_length(capabilities) > 0
            AND capabilities <@ '["chat","image","image.generate","image.edit"]'::jsonb
        );
EXCEPTION
    WHEN duplicate_object THEN NULL;
END $$;
