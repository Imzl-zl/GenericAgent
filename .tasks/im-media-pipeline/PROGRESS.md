# im-media-pipeline — IM 媒体管道实施

> 进度真值：`SUBTASKS.csv`。设计真值：`tenant_platform/docs/IM_MEDIA_ARCHITECTURE.zh-CN.md`（2026-08-13 审查修订版）。

## 背景

2026-08-13 架构审查（只读）结论：媒体统一模型成立，用户拍板按推荐实施。
5 阻断项（B1 四渠道入站缺失 / B2 QQ 分片定案 / B3 llm-proxy 4MiB 冲突 /
B4 8MiB+bytea / B5 QQ 主动消息路径）与重要项（I2 content_type、I4 留存）已全部落地。

## 已完成（2026-08-13，5 个提交）

| 提交 | 内容 |
|---|---|
| `de81e5d` | **T1/T2/T3**：四渠道入站媒体提取（media_downloader 公共下载器 + QQ/飞书/钉钉/企微 adapter + 魔数嗅探 + 统一"提取失败不投递"行为）+ GA base64 预算 3.5MB 对齐 llm-proxy 4MiB + 文档修订 |
| `04a5b66` | **T4 Phase A 出站**：send_media 统一接口 + QQ 分片上传（upload_prepare→PUT→finish→msg_type=7 主动消息 + per-target 15QPM 频控）+ 飞书/钉钉/企微上传适配 + Go transport.SendFile mediaType + delivery MIME 分发 |
| `58f727b` | **T6**：MediaItem.content_type=5 proto 一次变更窗口（generate_bindings 全量重生成 + Go 链路透传 + inferMessageType 分 image/video） |
| `7ad4d04` | **T5（B4）**：出站存储 spool 引用化（migration 0057 spool_path + capture 流式复制 + per-type 上限 image 20M/video 100M/file 8M/聚合 256M + delivery 直发 spool + 30d mtime 清扫；存量 content 行兼容） |
| `6eebb6b` | **T7（I4/D7）**：媒体留存——media_assets 90d 保留期（delivery tick 24h 节流）+ poller media_root 90d mtime 清扫（daemon 24h，env 可调） |

## 待实测（真实凭据冒烟，需用户配合）

- 钉钉入站 `POST /v1.0/robot/messageFiles/download`、企微入站 `media/get`（智能机器人 token 端点未确认，探测式）
- 钉钉出站 file/video（sampleFileMsg 需 downloadCode 上传流，暂 NotImplementedError fail-closed）
- QQ 分片上传参数（upload_prepare/upload_finish 字段）与主动消息频控实测
- 飞书 media（视频）消息类型实测
- 部署：**必须 make build 全量重建**（bot-poller + platform 必重建；ga-runner 本轮无改动）

## 有意推迟（决策 D6）

- **T8 Phase B 生图**：无真实用户需求证据。llm-proxy `llm.image` capability 扩展（Operation 枚举 + 路由 + 响应上限）建议在生图需求出现时随 B 期一起做（一次变更窗口）。
- 建议项 S1 后半（GA 注入前 PIL 解码失败丢弃而非原样透传）与 S3（媒体链路日志）未做，成本低可随时补。
