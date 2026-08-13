# im-media-pipeline — IM 媒体管道实施

> 进度真值：`SUBTASKS.csv`。设计真值：`tenant_platform/docs/IM_MEDIA_ARCHITECTURE.zh-CN.md`（2026-08-13 审查修订版）。

## 背景

2026-08-13 架构审查（只读）结论：媒体统一模型成立，按推荐实施。5 个阻断项：
B1 四渠道入站媒体缺失（已实施，本轮）、B2 QQ 分片定案、B3 llm-proxy 4MiB 与 GA 注入上限冲突（已实施）、
B4 出站 8MiB+DB bytea（Phase C 前置）、B5 QQ 主动消息路径（Phase A 内）。

## 已完成（2026-08-13）

- **T1 四渠道入站媒体提取**：
  - 新增 `bot_poller/media_downloader.py`：URL 直下（https+host 白名单 fail-closed+Content-Length 预检+流式累计上限 100MB+原子落盘）、
    `save_bytes_bounded`（飞书/钉钉/企微 API 字节落盘）、图片魔数嗅探（飞书 image 无扩展名，落盘必须带 ext 否则 GA 注入层跳过）、
    `build_media_item`（与微信 `_collect_media_items` 同构）、`guess_content_type`（原 `_EXT_CONTENT_TYPES` 迁移）。
  - `poller_server.py`：sys.path 补 `_POLLER_DIR`；QQ（attachments[].url 直下，`QQ_MEDIA_HOSTS`）、
    飞书（image_key/file_key → `GET /im/v1/messages/{id}/resources/{key}`，message_id+file_key 匹配）、
    钉钉（downloadCode → `POST /v1.0/robot/messageFiles/download`，语音官方 ASR recognition 注入 prompt）、
    企微（直链 URL / media_id → `media/get`，token 探测式）。
  - 统一行为（审查 B1）：无文本且媒体提取失败 → 丢弃不投递，不再回误导性 "empty message ignored"。
  - 测试：`test_poller_inbound_media.py` 新增 17 项；poller 全量 70 passed。
- **T2 限额对齐**：`agent_loop.py` 图片 base64 总量预算 `_ATTACH_IMG_B64_BUDGET=3.5MB`（llm-proxy `MaxWorkerRequestBytes=4MiB`），贪婪选取 + 跳过日志。
- **T3 文档修订**：IM_MEDIA_ARCHITECTURE §3.2 补 QQ 主动消息路径、§4 渠道限定、A1 分片定案、§10 Phase A-0、§11 残余风险（B4/B5/I3/I4/S4）。

## 残余风险 / 待实测

- 钉钉 `messageFiles/download`、企微 `media/get`（智能机器人 token 端点）——官方文档实证，真实凭据冒烟验证。
- QQ 出站主动消息路径频控（B5）、QQ 分片上传参数——Phase A 真实凭据实测。
- B4（8MiB/bytea → spool 引用）是 Phase C 前置；D2 待拍板（推荐：改）。
- D5（MediaItem.content_type）建议与 B4 同期做（proto 一次变更窗口）。

## 部署提醒（memory.md 教训）

- 生产部署必须 `make build` 全量重建（bot-poller 镜像必重建；选择性重建已两次出事故）。
- `__pycache__` 为 root 所有（历史遗留），本地测试用 `python -B -m pytest`。
