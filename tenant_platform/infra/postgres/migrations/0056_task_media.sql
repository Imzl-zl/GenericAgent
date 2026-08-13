-- Migration 0056: 任务入站媒体清单（tasks.media）。
--
-- 背景: IM 多模态链路结构化（2026-08-13 定案）。此前媒体只以"prompt 文本
-- 路径引用"传递（"[Session file workspace] ... attachments/F00X.jpg"），
-- Worker/GA 需从文本逆向解析才能把图片作为多模态 content block 注入模型
-- 首轮——脆弱且依赖文本格式约定（Go 侧提示文案变更即静默失效）。
--
-- 决策:
--   * tasks.media jsonb: 本次任务入站媒体清单（路由时 ImportInbound 得到
--     的附件，含 workspace 相对路径/别名/原始名/大小），随任务持久化，
--     dispatch 时经 TaskEnvelope.media 契约结构化下发（proto MediaItem）。
--   * Worker 据此调用 GA put_task(images=...) 注入模型首轮——结构化传递，
--     不再解析 prompt 文本。
--   * 空数组默认值：存量任务/无媒体任务不产生额外存储。媒体明细审计仍
--     在 media_assets 表（入站消息维度），本列是任务执行所需的快捷清单。
--   * jsonb 列用 DO 块条件执行（0052/0053/0054 先例，重放幂等）。

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM information_schema.columns
        WHERE table_name = 'tasks' AND column_name = 'media'
    ) THEN
        ALTER TABLE tasks ADD COLUMN media jsonb NOT NULL DEFAULT '[]'::jsonb;
    END IF;
END $$;
