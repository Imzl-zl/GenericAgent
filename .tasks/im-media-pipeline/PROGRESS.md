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
- **QQ 分片上传（2026-08-14 审查已按官方 4 步修正：upload_prepare → 逐片 PUT → upload_part_finish → /files 合并）与主动消息频控实测（重点验：分片 index 从 0 起、逐片 block_size 切片偏移）**
- 飞书 media（视频）消息类型实测
- 部署：**必须 make build 全量重建**（bot-poller + platform 必重建；ga-runner 内嵌 worker-python/src——2026-08-14 起 content_type 已透传下发，ga-runner 需一并重建以消费新契约）

## 2026-08-14 审查修复（上轮变更集复查结论落地）

| 项 | 内容 | 提交前验证 |
|---|---|---|
| B1 | bot-poller 镜像补 `COPY media_downloader.py`（此前容器启动 ModuleNotFoundError 即崩） | Dockerfile 已补 |
| B2 | QQ 分片上传按官方 4 步重写：逐片 PUT 后 `upload_part_finish` + `POST .../files` 合并（upload_id/file_type/file_name/srv_send_msg=false）；`md5_10m` 官方值 10002432 字节确认正确；未知目标端点组先群后单聊兜底（I3）；分片 PUT 3 次退避重试 + 逐片 block_size（S3） | test_poller_outbound_media 4 步 mock + 兜底 + 切片单测 |
| I1 | `toTaskMedia`/`workerTaskEnvelope` 补 ContentType 透传（此前契约字段空转，Phase C 前置失效） | scheduler_dispatch_test.go 透传断言 |
| I2 | poller `send_media` 大小上限按 media_type 分档（image 20M/video 100M/file 8M，对齐 Go per-type；固定值覆盖兼容） | per-type 上限单测 |
| I4 | `download_url_bounded` 改流式落盘（内存峰值=缓冲块，失败无 tmp 残留）；QQ 上传单遍流式哈希 | 流式/嗅探/清理单测 |
| S1 | GA 图片超预算跳过时向用户显式占位（失败诚实） | 根测试占位断言 |
| S2 | `buildPayload` spool 路径 Clean + 逃逸前缀校验（纵深防御） | 逃逸死信单测 |

## 有意推迟（决策 D6）

- **T8 Phase B 生图**：无真实用户需求证据。llm-proxy `llm.image` capability 扩展（Operation 枚举 + 路由 + 响应上限）建议在生图需求出现时随 B 期一起做（一次变更窗口）。
- 建议项 S1 后半（GA 注入前 PIL 解码失败丢弃而非原样透传）与 S3（媒体链路日志）未做，成本低可随时补。
