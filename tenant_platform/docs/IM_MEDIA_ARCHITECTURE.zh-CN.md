# IM 媒体管道架构（Media Pipeline）

> 状态：**设计定案（2026-08-13）**。入站理解段已落地；出站发送/生图/视频为设计稿（Phase A/B/C 分期实施，见 §10）。
> 关联：`IM_CHANNEL_ARCHITECTURE.zh-CN.md`（会话模型/分桶）、`IM_CHANNEL_BINDING.zh-CN.md`（渠道绑定/Adapter 注册表）、`IM_STREAMING_DELIVERY.zh-CN.md`（流式转发）、`LLM_PROVIDER_ARCHITECTURE.md`（llm-proxy）、`contracts/proto/genericagent/worker/v1/worker.proto`（TaskEnvelope.media 契约）。
> 结论先行：**媒体 = 入站/出站对称的「文件 + 元数据」统一模型**；理解走消息块（content block，模型直接看），生成走工具+文件（模型产出 → 既有交付链路）；渠道协议差异收敛为 adapter 的两个薄接口（入站提取 / 出站上传发送），公共逻辑（URL 直下、落盘、MIME、大小限制、元数据）基类一套。

## 1. 背景与问题演进

- **2026-08-13 生产实证**：用户换多模态模型（agnes-2.5-flash）后发图问"这是啥"→ 任务 307s `TASK_INTERRUPTED` 零回复。复现闭环（webhook 注入 + runner 容器日志）定位根因有二：
  1. **模型在 `code_run` 里生成 shell 命令**（`python3 -c`/`pip install`/`&&`），GA 按纯 Python 写 `.py` 执行 → `SyntaxError` → agent 无限重试至任务超时（工具描述未声明"纯 Python" + 无执行器防护）。
  2. **GA 只把图片路径文本给模型**（`[Session file workspace] ... attachments/F00X.jpg`），多模态模型收不到图片内容——媒体只有"文件模式"（工具可读），没有"消息模式"（模型直接看）。
- **演进路径**：补丁（agent_loop 正则扫 prompt 文本逆向还原图片路径）→ 架构化（契约级结构化传递，入站段已落地，§4-§5）。
- **本稿补齐全图**：出站媒体（生成 → 用户）、生图能力、视频能力，形成对称媒体架构。

## 2. 媒体统一模型（设计真值）

```
媒体 Media = { bytes | file_ref, file_name, content_type, size_bytes }
```

- 入站与出站**同构**：入站 = 渠道事件 → 字节 → 模型上下文；出站 = 模型产出 → 字节 → 渠道消息。
- 两条既有通道复用，不造新通道：
  - **入站通道**：`TaskEnvelope.media`（proto 契约）→ GA `put_task(images=)` → 首轮 content blocks。
  - **出站通道**：GA `[FILE:outputs/<name>]` marker → delivery spool → poller `send_media` → 渠道消息。
- **文件模式与消息模式并存且正交**：媒体同时存在于 workspace（工具可读/可存档/可导出）与模型上下文（模型可感知）；工具层视觉（OCR/vision）是 fallback，不是主路径（与 Claude Code / Codex / LangChain 主流一致）。

## 3. 渠道媒体机制矩阵（官方文档实证，2026-08-13）
### 3.1 入站（事件 → 字节）

| 渠道 | 官方机制 | 获取方式 | 复杂度 |
|---|---|---|---|
| 微信 iLink | 长轮询 `item_list` 媒体项 | `wxbot_media.download_media` URL 直下（已实现） | URL 直下 |
| QQ 官方 | `C2C_MESSAGE_CREATE` / `GROUP_AT_MESSAGE_CREATE` 事件 **`attachments[]` 直接携带下载 URL**（`multimedia.nt.qq.com.cn/download?appid=...&fileid=...&rkey=...` + filename/content_type/size/width/height） | 直接 HTTP GET url | **URL 直下（与微信同模式）** |
| 飞书 | 消息 content 带 `image_key`/`file_key` | `GET /open-apis/im/v1/messages/{message_id}/resources/{file_key}?type=image`（Bearer tenant_access_token，返回二进制流；≤100MB） | API + token |
| 钉钉 | 消息 `msgtype=image` content 带 `mediaId` | `oapi/media/download` + access_token | API + token |
| 企微 | 消息 `msgtype=image` content 带 `media_id` | `qyapi/cgi-bin/media/get` + access_token | API + token |

参考：QQ https://bot.q.qq.com/wiki/develop/api-v2/autogen/event/c2c_message_create.html 、群 https://bot.q.qq.com/wiki/develop/api-v2/autogen/event/group_at_message_create.html ；飞书 https://open.feishu.cn/document/server-docs/im-v1/message/get-2

**推论**：入站协议差异只有**两种模式**——URL 直下（微信/QQ，可共用一个下载器）；API+token（飞书/钉钉/企微，各用各自 SDK client 调用）。**不是四套，是"一套公共 + 每渠道薄适配"**。

### 3.2 出站（字节 → 渠道消息）

| 渠道 | 图片 | 视频 | 文件 | 上传机制（差异点） |
|---|---|---|---|---|
| 微信 iLink | ✅ `send_image`（已实现） | ✅ `send_video`（已实现） | ✅ `send_file`（已实现） | iLink 直传，无上传 API |
| QQ | ✅ 富媒体 `msg_type=7` | ✅ 同上（video/mp4） | ✅ 同上 | **先上传拿 `file_info`**（整文件/分片上传，单聊/群聊不互通，有 TTL） |
| 飞书 | ✅ | ✅（file 类型） | ✅ | `POST im/v1/images`（图片）/ `im/v1/files`（文件）拿 key → 消息 content |
| 钉钉 | ✅ | ✅ | ✅ | `oapi/media/upload` 拿 mediaId → 消息 |
| 企微 | ✅ | ✅ | ✅ | `qyapi/cgi-bin/media/upload` 拿 media_id → 消息 |

参考：QQ https://bot.qq.com/wiki/develop/api-v2/server-inter/message/type/media.html 、https://bot.qq.com/wiki/develop/api-v2/server-inter/message/overview.html

**推论**：出站媒体能力各平台都具备，差异只在"上传 API"（QQ file_info / 飞书 key / 钉钉/企微 media_id / 微信直传）——同样是"一套公共 + 每渠道薄适配"。

**补充（2026-08-13 架构审查 B5，QQ 出站主动消息路径）**：QQ 被动回复锚点 `msg_id` **5 分钟内有效**（官方 v2_users_user_openid_messages.post：msg_id"5 分钟内有效"、错误码 304103），另有"每条用户消息 60 分钟限回 4 次"约束。本平台 delivery 是任务终态后的异步推送（任务可达 45 分钟），必然超窗——**QQ 媒体发送实际走主动消息路径**（`srv_send_msg=true` 上传直发或上传后主动 send），消耗主动频次（单聊 10 QPS / 20 QPM、日 1000/用户），且 C2C 主动消息要求用户近期有交互。实现时必须设计主动路径 + adapter 内频控预算（复用 `_TokenBucket`/`_QQRateLimited` 先例）。

## 4. 入站媒体链路（2026-08-13 已落地；渠道覆盖：**微信**为主链路实证，**QQ/飞书/钉钉/企微**入站提取 2026-08-13 架构审查后补齐——同链路复用，钉钉/企微下载 API 待真实凭据实测）

```
渠道事件 → adapter 提取媒体引用 → 落盘 media_root/bot_uuid/ → media_paths + media_items
  → webhook（已有统一转发）→ platform ImportInbound（workspace temp/attachments/F00X_*）
  → tasks.media 持久化（migration 0056，jsonb）
  → TaskEnvelope.media（proto MediaItem{alias, original_name, relative_path, size_bytes}）
  → worker put_task(prompt, images=[relative_path...])
  → GA media_content_blocks → 首轮 user content [text + image_url(base64)] → 模型直接看图
      ↳ 工具层 OCR/vision（memory/ocr_utils.py，rapidocr）＝ fallback
```

### 4.1 契约与存储（单一真值源）

- `worker.proto`：`TaskEnvelope.media = 15`（repeated MediaItem）；`MediaItem{alias=1, original_name=2, relative_path=3, size_bytes=4}`。
- `migration 0056_task_media.sql`：`tasks.media jsonb NOT NULL DEFAULT '[]'`（DO 块幂等）。
- `domain.TaskMedia` + `SubmitTaskCommand.Media` + `Task.Media`；store `submitTaskTx` 写入（`validateTaskMedia`：干净相对路径、≤16 条、非负大小），`scanTask` 读回。
- 路由时 `ImportInbound` 附件清单（`SessionFileRef`，补 `SizeBytes`）→ `toTaskMedia`（仅 inbound）→ 随任务持久化；dispatch `workerTaskEnvelope` 填 `TaskEnvelope.Media`。

### 4.2 GA 侧（原生参数补完）

- `agentmain.put_task(query, source, images=None)` 的 **images 死参数补完**（早期设计意图）：`run()` 消费 `task["images"]` → `media_content_blocks(raw_query, task_images)` → `agent_runner_loop(initial_user_content=<blocks>)`。
- `agent_loop.media_content_blocks(user_text, image_paths=None)`：
  - **显式路径（主路径）**：来自 `put_task(images=)`，相对 GA temp 或绝对路径。
  - **文本正则兜底**：无显式路径时扫描 `attachments/*.(jpg|jpeg|png|gif|webp|bmp)`（兼容旧调用方/纯文本约定）。
  - **降采样**：PIL 可用时最长边 → 1568px（对齐 Claude Code 成本控制），失败原样透传。
  - 上限：单图 ≤3MB、≤3 张；非图片扩展名跳过；扩展音频/视频/PDF 在此加分支，链路不动。
  - `_inject_attachment_images` 保留为兼容别名。

### 4.3 执行器防护（code_run）

- `ga._SHELL_STYLE_RE`：行首 shell 命令特征（`python3 -c`/`pip install`/`&&`/shebang 等）→ python 模式自动降级 bash，防 SyntaxError 死循环（生产 307s 超时根因之一）。
- `assets/tools_schema.json`：code_run 描述明确"纯 Python 3，禁 shell 语法，shell 用 subprocess"。
- ga-runner 镜像补：`pillow`/`numpy`/`rapidocr-onnxruntime` + `libgl1 libglib2.0-0`（GA 自带 OCR 工具依赖）。

### 4.4 实证

同图任务：修复前 306s 超时 → 补丁 187s → 架构化 **54s** 成功识别（"卡通小男孩角色立绘 30 动作，水印'跑AI生'"）。

## 5. 出站媒体链路（Phase A，设计稿）

```
GA 产出（生图工具/文件写入）→ outputs/ + [FILE:outputs/<name>] marker
  → delivery（既有 spool 链路，按 MIME 分发 image/file/video）
  → poller BotAdapter.send_media(target, file_path, media_type)
  → 渠道上传 + 发送（差异点收敛到 adapter 薄适配）
```

### 5.1 设计决策

- [ ] 决策 A1：**BotAdapter 基类新增统一出站媒体接口**：
  ```python
  def send_media(self, target, file_path, media_type, file_name='', client_id=''):
      """media_type: image | file | video。基类按 media_type 分发到
      _send_image/_send_file/_send_video；子类实现"上传 + 发送"薄适配。"""
  ```
  - 基类提供：MIME 推断、大小上限校验、按渠道上传 API 的公共骨架。
  - 微信：复用现有 `send_image/send_video/send_file`（iLink 直传，无上传步骤）。
  - **QQ：分片上传（定案，2026-08-13 审查 B2）**——官方"整文件上传"需传入**公网可访问 URL**（Poller 无公网入口，不可用）；分片上传（预上传 → 分片 PUT → 完成合并）无需公网 CDN，是唯一可选路径。注意单聊/群聊上传**不互通**（独立端点 `/v2/users/{openid}/files` 与 `/v2/groups/{group_openid}/files`）+ `file_info` 有 TTL，上传必须在发送时刻执行（不在任务终态捕获时）。小文件可评估 `srv_send_msg=true` 上传直发（占用主动消息频次）。
  - 飞书：`im/v1/images`（图片）/`im/v1/files`（文件）上传拿 key → 消息 content。
  - 钉钉：`media/upload` 拿 `mediaId` → 消息 `msgtype=image|file`。
  - 企微：`media/upload` 拿 `media_id` → 消息。
- [ ] 决策 A2：**delivery 按 MIME 分发**：`content_type` 前缀 `image/` → `send_media(image)`；`video/` → `send_media(video)`；其余 → `send_media(file)`。失败语义沿用 `SEND_FILE_FAILED` 兜底（delivery 重试/死信既有路径）。
- [ ] 决策 A3：**上传失败语义**：上传 API 失败 = 发送失败（fail-closed），走既有 delivery 错误路径；不静默降级文本（媒体是用户明确期望的产物）。
- [x] 决策 A4：**大小上限**：~~沿用 `defaultMaxDeliverableBytes`~~（2026-08-13 审查 B4/T5 已落地）：出站文件存储 **spool 引用化**（migration 0057）——成功事务时文件流式复制到 delivery spool 共享卷（`GA_DELIVERY_SPOOL_DIR`，Platform rw / Poller ro），`task_delivery_files` 只存 spool 相对路径 + digest，不再存 BYTEA 字节；上限按媒体类型分化：**image ≤20MiB / video ≤100MiB（Phase C 视频）/ 其余 ≤8MiB**，任务聚合 ≤256MiB；超限媒体发送失败并诚实提示（与空结果文案同原则）。

## 6. 生图能力（Phase B，设计稿）

### 6.1 定位

- **生图 ≠ 多模态理解**：理解是入站媒体 → chat 协议内 `image_url` content block；生图是模型决策 → **独立 API**（OpenAI `images/generations` 兼容协议）→ 文件 → 出站交付。
- GA 原生无生图（llmcore 只有 chat 类 Session）；主流 agent（Claude Code/Codex/OpenAI Agents SDK）均为**工具化**：模型调用生图工具 → API 产图 → 文件交付，聊天流不返回图。
- llm-proxy 目前**只代理 `/v1/chat/completions`**（handler 单路径）——生图需扩展。

### 6.2 设计决策

- [ ] 决策 B1：**生图 = GA 工具**（`image_gen`）：模型调用（prompt/尺寸/数量）→ 工具内调生图 API → 图片落盘 `outputs/` → 返回 `[FILE:outputs/<name>]` 走既有出站链路。**GA 核心循环零侵入**。
- [ ] 决策 B2：**llm-proxy 扩展 `POST /v1/images/generations` 代理**：与 chat 同能力体系——capability 校验（operation 扩展 `llm.image`）、JTI 预算计量、白名单 egress、模型与 capability 匹配校验。契约：llm-proxy 是 HTTP 实现（无 proto），扩展路由 + `CapabilitySpec.Operation` 枚举。
- [ ] 决策 B3：**生图上游模型**：provider 配置新增 model 类型/独立 provider（`provider_type` 扩展或复用 native_oai + images 端点）；GA `ImageGenClient`（OpenAI images 兼容，`/images/generations`，response_format b64_json）可配置独立 apibase/key（走 llm-proxy 能力 token 或直连）。
- [ ] 决策 B4：**生成文件管理**：生图产物写入 session outputs/（`RecordOutbound` 登记 manifest，与 docx 同生命周期），交付后随会话文件保留。

## 7. 视频能力（Phase C，设计稿）

### 7.1 定位

- IM 平台**入站/出站视频都支持**（QQ attachments 含 video/mp4；出站各渠道视频消息见 §3.2）。
- **理解**：主流做法 = **抽帧转图**（ffmpeg 抽关键帧 → 图片块注入模型；Gemini 原生视频除外，本项目不赌模型原生能力）。
- **生成**：独立视频模型 API（Veo/Sora 类），工具化，v1 不做。

### 7.2 设计决策

- [ ] 决策 C1：**入站视频抽帧**：ga-runner 镜像补 ffmpeg；adapter 提取视频后，worker/GA 侧抽帧（首帧/关键帧 N 张）→ 图片块注入；原视频保留 workspace 供工具使用。
- [ ] 决策 C2：**视频大小/时长上限**：入站注入仅抽帧图片（≤N 张），原视频字节不进模型上下文（token 爆炸控制）；出站视频走文件链路（§5）。
- [ ] 决策 C3：**出站视频**：复用 Phase A `send_media(video)`；无独立视频生成（生成视频 = Phase B 工具模式的视频版，后续按需）。

## 8. 设计原则（长期约束）

1. **方向对称**：入站/出站共用「媒体文件 + 元数据」模型，各复用一条既有通道（契约 / delivery），不造新通道。
2. **渠道差异收敛为薄适配**：adapter 只实现两个窄接口——「入站提取媒体引用」「出站上传发送」；公共部分（URL 直下、落盘、文件名清洗、MIME 推断、大小限制、元数据构造、幂等）基类一套。协议差异（各平台 API）客观存在（NoneBot/OneBot 同模式），但每渠道代码量控制在 ~20-40 行。
3. **消息模式为主、文件模式并存**：媒体进模型上下文（content block）为主路径；workspace 文件供工具/存档/导出；工具视觉（OCR/vision）是 fallback。
4. **契约先行**：跨层媒体数据一律走 proto/OpenAPI 契约（TaskEnvelope.media 先例），禁止文本约定（prompt 路径解析已降级为兜底）。
5. **成本控制随架构走**：base64 每轮重发膨胀 payload——注入层降采样（1568px）；后续可升级 file_id 引用（Anthropic Files API 模式）或上传缓存。
6. **失败诚实**：媒体缺失/超限/上传失败 → 明确提示，不静默降级为"任务完成"。

## 9. 当前状态与缺口清单（2026-08-13）

| 能力 | 状态 | 落点 |
|---|---|---|
| 入站理解（图片） | ✅ 已落地（微信 2026-08-13；QQ/飞书/钉钉/企微提取 2026-08-13 审查补齐，钉钉/企微下载 API 待实测） | §4 |
| code_run 防护 + OCR 依赖 | ✅ 已落地 | §4.3 |
| 出站发送（文件/图/视频） | ❌ 仅微信通；飞书/QQ/钉钉/企微 `send_file` 为 `NotImplementedError` | §5 Phase A |
| 生图 | ❌ GA 无原生；llm-proxy 仅 chat 路径 | §6 Phase B |
| 视频理解 | ❌ 无抽帧 | §7 Phase C |
| 视频出站 | ❌ 同出站缺口 | §5+§7 |

## 10. 落地分期与验收

| 期 | 内容 | 验收 |
|---|---|---|
| Phase A-0（前置，2026-08-13 审查阻断项） | ①四渠道入站媒体提取（QQ URL 直下 / 飞书 resources API / 钉钉 downloadCode / 企微 media_id+直链，公共下载器 `bot_poller/media_downloader.py`）——✅ 已实施；②GA 注入 base64 总量预算（对齐 llm-proxy 4MiB 上限）——✅ 已实施；③文档修订（本稿）——✅ | poller 单测 + 真实渠道冒烟（钉钉/企微下载 API 端点待凭据实测） |
| Phase A | 出站媒体统一：基类 `send_media` + 5 渠道上传适配（QQ=分片定案）+ delivery MIME 分发 | 微信/QQ/飞书各发 docx/图片/视频成功；失败路径诚实提示 |
| Phase B | 生图：`image_gen` 工具 + llm-proxy `images/generations` 代理 + capability `llm.image` + GA ImageGenClient | 模型画图 → 用户 IM 收到图（各渠道） |
| Phase C | 视频：镜像补 ffmpeg + 入站抽帧注入 + 出站 `send_media(video)` | 用户发视频可分析；GA 可发视频 |

每期独立可交付、互不阻塞；渠道真实验证需用户凭据配合（QQ/飞书已配置）。

## 11. 残余风险

- 各渠道上传 API（QQ file_info 分片、钉钉/企微 media 上传）需真实凭据实测（mock 只覆盖请求形状）。
- QQ 附件下载 URL 有时效（`rkey`），下载失败需按事件级重试语义处理（poller 已有 InboundCoalescingBuffer，媒体下载失败策略沿用：丢弃该媒体并记录，不阻塞文本消息）。
- 生图模型按 provider 配置独立，需 llm-proxy 能力扩展同步安全审查（JTI 预算、白名单、模型匹配，同 chat 路径）。
- **QQ 出站主动消息路径（审查 B5）**：异步 delivery 必超被动窗口（msg_id 5 分钟），媒体发送走主动消息——频控预算（单聊 20 QPM、日 1000/用户）与 C2C"用户近期交互"约束需 adapter 内实现并实测。
- **钉钉/企微入站下载端点（审查 S4）**：钉钉 `POST /v1.0/robot/messageFiles/download`、企微 `media/get`（access_token 来源 = 智能机器人凭据，SDK token 端点未确认）——代码已按官方文档实现，真实凭据冒烟时验证。
- **出站 8MiB 捕获上限 + task_delivery_files 存内容字节（审查 B4）**：Phase C 视频前必须改为 spool 引用模式（DB 只存路径/摘要），并按 media_type 分化上限；Phase A 图片沿用 8MiB 可接受。
- **媒体留存（审查 I4）**：poller media_root 无清理、media_assets/tasks.media 无保留期——待定 90d + 容量上限清理策略。
- **图片跨任务不保留（审查 I3）**：checkpoint 256KB 历史预算与 GA trim 会把含 base64 图片的消息从头部裁掉——图片只活单任务，为显式接受的 v1 语义（后续 file_id 引用方案再解决）；`_sanitize_leading_user_msg` 丢图时无占位提示，建议后续补 `[图片内容已省略]`。
