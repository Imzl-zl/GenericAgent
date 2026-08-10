-- Migration 0054: IM 流式输出任务维度（conversation_type + stream_final_at）。
--
-- 背景: IM 流式输出（IM_STREAMING_DELIVERY）按渠道分档转发，转发判定
-- 需要"群聊/私聊"维度——入站契约只有 conversation_id 字符串（钉钉/飞书
-- 群私聊同格式），无法判定群聊桶。设计真值:
--   tenant_platform/docs/IM_STREAMING_DELIVERY.zh-CN.md §4.4。
--
-- 决策:
--   * conversation_type: 'private' | 'group'——poller 各 adapter 有现成
--     信息（QQ is_group、飞书 chat_type、钉钉 conversation_type=='2'）；
--     微信恒 private；web/非 IM 入口默认 private。群聊统一只发最终结果
--     （收敛策略），私聊按 im_streaming_mode 转发。
--   * stream_final_at: scheduler 流式 commit 成功时置位（当前时间）。
--     delivery 发送文本 part 前检查：已流式交付最终文本则跳过文本
--     （文件照发）；失败路径无标记 → delivery 照发兜底（"失败补发最终
--     结果"，走既有 delivery 路径）。
--   * 两列均用 IF NOT EXISTS / DO 块条件执行（0052/0053 先例，重放幂等）。

ALTER TABLE tasks
    ADD COLUMN IF NOT EXISTS conversation_type TEXT NOT NULL DEFAULT 'private';

ALTER TABLE tasks
    ADD COLUMN IF NOT EXISTS stream_final_at TIMESTAMPTZ;

DO $$
BEGIN
  IF NOT EXISTS (
    SELECT 1 FROM pg_constraint
    WHERE conname = 'tasks_conversation_type_check'
      AND conrelid = 'tasks'::regclass
  ) THEN
    ALTER TABLE tasks ADD CONSTRAINT tasks_conversation_type_check
        CHECK (conversation_type IN ('private', 'group'));
  END IF;
END
$$;

-- im_streaming_mode 是字符串开关(off|final_only|streaming): 表原有
-- int_value 列, 追加 text_value 列承载字符串设置。默认 streaming(设计:
-- 私聊默认开, 群聊由转发判定收敛)。
ALTER TABLE platform_runtime_settings
    ADD COLUMN IF NOT EXISTS text_value TEXT;

INSERT INTO platform_runtime_settings (setting_key, int_value, text_value, updated_by)
VALUES ('im_streaming_mode', 0, 'streaming', 0)
ON CONFLICT (setting_key) DO NOTHING;

CREATE TABLE IF NOT EXISTS migration_0054_im_streaming_marker (
    id BOOLEAN PRIMARY KEY DEFAULT TRUE CHECK (id)
);
INSERT INTO migration_0054_im_streaming_marker(id)
VALUES (TRUE)
ON CONFLICT (id) DO NOTHING;
