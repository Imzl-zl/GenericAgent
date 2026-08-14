-- Migration 0058: llm_providers.capabilities — provider 能力类型维度(chat/image)。
--
-- 背景(Phase B 托管形态, 2026-08-14 定稿): GA 生图(image_gen 工具)在平台
-- 模式经 llm-proxy 代理 /v1/images/generations, capability operation 扩展
-- llm.image。provider 需声明能力维度:
--   * chat  = 对话(operation llm.chat, 现有路由)——默认, 存量兼容。
--   * image = 生图(operation llm.image, 新路由 images/generations)。
-- model 单值意味着 image 能力 provider 的 model 是生图模型, 实际部署中
-- image provider 通常是独立 provider; native_claude 仅支持 chat。
--
-- 约束: capabilities 非空 JSON 数组, 元素 ∈ {chat, image}; 省略 = ["chat"]。

ALTER TABLE llm_providers ADD COLUMN IF NOT EXISTS capabilities JSONB NOT NULL DEFAULT '["chat"]';

COMMENT ON COLUMN llm_providers.capabilities IS '能力维度（JSONB 数组，chat|image）；省略=chat。image 用于生图路由（llm.image），model 为生图模型';
