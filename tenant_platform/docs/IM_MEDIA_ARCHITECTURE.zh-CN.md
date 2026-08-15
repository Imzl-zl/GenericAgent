# IM 媒体管道架构（Media Pipeline）

> 状态：**设计定案（2026-08-13）**。入站理解段已落地；**出站发送已落地（Phase A，2026-08-14 审查后）**；**生图直连 + 托管双形态均已实施（Phase B，2026-08-14）**；视频为设计稿（Phase C 分期实施，见 §10）。
> 生图实施真值：`.tasks/im-media-pipeline/PHASE_B_IMAGE_GEN_PLAN.zh-CN.md`（两轮审查通过 + D1 拍板定稿，§3-§8）。
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

| 渠道 | 图片 | 视频 | 文件 | 上传机制（差异点） | 官方限制（2026-08-14 实测/文档对齐） |
|---|---|---|---|---|---|
| 微信 iLink | ✅ `send_image`（已实现） | ✅ `send_video`（已实现） | ✅ `send_file`（已实现） | iLink 直传，无上传 API | 无文档化大小限制；**海外服务器实测 CDN 节流 ~18KB/s、连接 ~30s 被杀，>~450KB 必死** → 交付侧 ≤300KB JPEG 适配（决策 A7） |
| QQ | ✅ 富媒体 `msg_type=7` | ✅ 同上（video/mp4） | ✅ 同上 | **先上传拿 `file_info`**（整文件/分片上传，单聊/群聊不互通，有 TTL） | 官方 upload_config.retry_timeout=300s；850031 超限；分片默认 5MB（已逐字段对齐） |
| 飞书 | ✅ | ✅（file 类型） | ✅ | `POST im/v1/images`（图片）/ `im/v1/files`（文件）拿 key → 消息 content | 图片 ≤10MB、GIF ≤2000×2000、其他 ≤12000×12000；文件 ≤30MB（全局 8MiB 更紧，安全侧） |
| 钉钉 | ✅ | ✅ | ✅ | `oapi/media/upload` 拿 mediaId → 消息 | 图片 ≤20MB 仅 jpg/gif/png/bmp（**无 webp** → 交付侧转 JPEG）；文件 ≤20MB 仅 doc/docx/xls/xlsx/ppt/pptx/zip/pdf/rar；**file/video 的 downloadCode 上传流未实现（fail-closed）** |
| 企微 | ✅ | ✅ | ✅ | `qyapi/cgi-bin/media/upload` 拿 media_id → 消息 | 图片 ≤10MB 仅 JPG/PNG（白名单外+动图 → JPEG 首帧）；视频 ≤10MB（无渠道级适配，超限被拒=已知缺口）；文件 ≤20MB；media_id 3 天有效 |

参考：QQ https://bot.qq.com/wiki/develop/api-v2/server-inter/message/type/media.html 、https://bot.qq.com/wiki/develop/api-v2/server-inter/message/overview.html

**推论**：出站媒体能力各平台都具备，差异只在"上传 API"（QQ file_info / 飞书 key / 钉钉/企微 media_id / 微信直传）——同样是"一套公共 + 每渠道薄适配"。

**补充（2026-08-13 架构审查 B5，QQ 出站主动消息路径）**：QQ 被动回复锚点 `msg_id` **5 分钟内有效**（官方 v2_users_user_openid_messages.post：msg_id"5 分钟内有效"、错误码 304103），另有"每条用户消息 60 分钟限回 4 次"约束。本平台 delivery 是任务终态后的异步推送（任务可达 45 分钟），必然超窗——**QQ 媒体发送实际走主动消息路径**（`srv_send_msg=true` 上传直发或上传后主动 send），消耗主动频次（单聊 10 QPS / 20 QPM、日 1000/用户），且 C2C 主动消息要求用户近期有交互。实现时必须设计主动路径 + adapter 内频控预算（复用 `_TokenBucket`/`_QQRateLimited` 先例）。

## 4. 入站媒体链路（2026-08-13 已落地；渠道覆盖：**微信**为主链路实证，**QQ/飞书/钉钉/企微**入站提取 2026-08-13 架构审查后补齐——同链路复用，钉钉/企微下载 API 待真实凭据实测）

```
渠道事件 → adapter 提取媒体引用 → 落盘 media_root/bot_uuid/ → media_paths + media_items
  → 窗口合并（InboundCoalescingBuffer，2026-08-14 审查 I-3 起五渠道统一：
    同一会话窗口内相邻消息〔图 + 后续文本〕合并为一组再投递——此前仅微信接线，
    QQ 图消息与后续文本会拆成两个任务、文本任务 media=null，追问语义断裂）
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
  - **预算口径（2026-08-14 审查 I-2）**：base64 总量预算按**降采样后实际注入字节**判定（3.5MB，对齐 llm-proxy 4MiB）——旧口径按原始文件大小估算会把降采样后本可注入的大图误杀；原始文件 3MB 上限保留为解码防御（防整读/解码内存峰值）。
  - 上限：单图 ≤3MB、≤3 张；非图片扩展名跳过（日志 + 不占位）；扩展音频/视频/PDF 在此加分支，链路不动。
  - `_inject_attachment_images` 保留为兼容别名。
  - 协议通道（ToolClient）无视觉：image 块拍平时降级为占位文本，base64 不进提示词/历史/日志（2026-08-14 审查 S-1）；Anthropic 协议通道经 `_fix_messages` 把 image_url 块转 Claude image 块（审查 I-4，cache_control 只落在 text 块）。

### 4.3 执行器防护（code_run）

- `ga._SHELL_STYLE_RE`：行首 shell 命令特征（`python3 -c`/`pip install`/`&&`/shebang 等）→ python 模式自动降级 bash，防 SyntaxError 死循环（生产 307s 超时根因之一）。
- `assets/tools_schema.json`：code_run 描述明确"纯 Python 3，禁 shell 语法，shell 用 subprocess"。
- ga-runner 镜像补：`pillow`/`numpy`/`rapidocr-onnxruntime` + `libgl1 libglib2.0-0`（GA 自带 OCR 工具依赖）。

### 4.4 实证

同图任务：修复前 306s 超时 → 补丁 187s → 架构化 **54s** 成功识别（"卡通小男孩角色立绘 30 动作，水印'跑AI生'"）。

## 5. 出站媒体链路（Phase A，已落地 2026-08-14）

```
GA 产出（生图工具/文件写入）→ outputs/ + [FILE:outputs/<name>] marker
  → delivery（既有 spool 链路，按 MIME 分发 image/file/video）
  → poller BotAdapter.send_media(target, file_path, media_type)
  → 渠道上传 + 发送（差异点收敛到 adapter 薄适配）
```

### 5.1 设计决策

- [x] 决策 A1：**BotAdapter 基类新增统一出站媒体接口**：
  ```python
  def send_media(self, target, file_path, media_type, file_name='', client_id=''):
      """media_type: image | file | video。基类按 media_type 分发到
      _send_image/_send_file/_send_video；子类实现"上传 + 发送"薄适配。"""
  ```
  - 基类提供：MIME 推断、大小上限校验、按渠道上传 API 的公共骨架。
  - 微信：复用现有 `send_image/send_video/send_file`（iLink 直传，无上传步骤）。
  - **QQ：分片上传（定案，2026-08-13 审查 B2；2026-08-14 按官方 4 步重写）**——官方"整文件上传"需传入**公网可访问 URL**（Poller 无公网入口，不可用）；分片上传（预上传 → 分片 PUT → 合并）无需公网 CDN，是唯一可选路径。注意单聊/群聊上传**不互通**（独立端点 `/v2/users/{openid}/files` 与 `/v2/groups/{group_openid}/files`）+ `file_info` 有 TTL，上传必须在发送时刻执行（不在任务终态捕获时）。小文件可评估 `srv_send_msg=true` 上传直发（占用主动消息频次）。
  - 飞书：`im/v1/images`（图片）/`im/v1/files`（文件）上传拿 key → 消息 content。
  - 钉钉：`media/upload` 拿 `mediaId` → 消息 `msgtype=image|file`（file/video 的 downloadCode 上传流待实测，暂 NotImplementedError fail-closed）。
  - 企微：`media/upload` 拿 `media_id` → 消息。
- [x] 决策 A2：**delivery 按 MIME 分发**：`content_type` 前缀 `image/` → `send_media(image)`；`video/` → `send_media(video)`；其余 → `send_media(file)`。失败语义沿用 `SEND_FILE_FAILED` 兜底（delivery 重试/死信既有路径）。
- [x] 决策 A3：**上传失败语义**：上传 API 失败 = 发送失败（fail-closed），走既有 delivery 错误路径；不静默降级文本（媒体是用户明确期望的产物）。
- [x] 决策 A4：**大小上限**：~~沿用 `defaultMaxDeliverableBytes`~~（2026-08-13 审查 B4/T5 已落地）：出站文件存储 **spool 引用化**（migration 0057）——成功事务时文件流式复制到 delivery spool 共享卷（`GA_DELIVERY_SPOOL_DIR`，Platform rw / Poller ro），`task_delivery_files` 只存 spool 相对路径 + digest，不再存 BYTEA 字节；上限按媒体类型分化：**image ≤20MiB / video ≤100MiB（Phase C 视频）/ 其余 ≤8MiB**，任务聚合 ≤256MiB；超限媒体发送失败并诚实提示（与空结果文案同原则）。

**2026-08-14 事故修复决策（A5-A10，微信生图交付死信全链路）**：

- [x] 决策 A5：**媒体类型分类源 = auditPath（workspace 相对路径）**。spool 分支曾漏设 relPath → `mediaTypeForPath("")` 回退 file → 生成图全部走 file_item 原始上传（死信真正元凶之一）。现分类源收敛为两个分支按构造必然设置的 auditPath（`c0e0452`），`buildPayload` 对空 auditPath fail-closed（PAYLOAD_BUILD_FAILED），结构上杜绝"静默降级 file"再次发生。
- [x] 决策 A6：**交付重试预算**：重试窗口 5min→**30min**（CDN 瞬时故障可持续 ≥8 分钟）+ `maxDeliveryAttempts=10`（退避 1s→5min 封顶，实际 ~8.5min 内耗尽，窗口是兜底非用户等待上限）；文本发送预算 15s / **媒体发送预算 90s**（`deliveryMediaSendTimeout`）。预算链（独立审查 + 子代理复审定稿）：正常/快速失败路径 poller 侧 ≈85s（CDN 30+10+3+30+10 + 控制面各 ≤10s）< ctx 90s < poller client 兜底 120s（`mediaTimeout`，拆双 client 修复单一 15s Timeout 架空）；**病理**（控制面+CDN 双侧挂起）最坏 ~103s 超 ctx——由 Go 重试 + 幂等 client_id 兜底，poller 线程占用有界。**admin 重投开启新窗口**：`requeued_at` 列（migration 0060），claim/死信/重试截止锚点 = `GREATEST(tasks.terminal_at, requeued_at)`（否则事故后数小时重投的行会在下一 tick 被原样打回死信）。
- [x] 决策 A7：**微信 C2C CDN 节流适配**：海外服务器→微信 CDN 实测限速 ~18KB/s、连接 ~30s 被杀、>~450KB 必死——交付侧 `fit_image_for_upload` 把超限静态图转 JPEG 迭代压缩到 **≤300KB**（`send_image` 透明适配，动图不转）。
- [x] 决策 A8：**官方协议对齐（ba60f26）**：`no_need_thumb=true`（openclaw 官方默认，image_item 仅 media+mid_size）；企微图片仅 JPG/PNG（白名单外+动图 → JPEG 首帧，≤10MB）；钉钉图片 jpg/gif/png/bmp（**无 webp**，≤20MB）。适配函数 `fit_image_for_upload` 模块级（微信实例方法委托，poller 企微/钉钉 adapter 直调）。
- [x] 决策 A9：**适配临时文件落可写目录**：`tempfile.mkstemp` 落系统 /tmp（生产 poller 只读 rootfs + 只读 spool 卷，写源旁目录会静默失败回退原图——`260a59d`）。
- [x] 决策 A10：**渠道官方限制与全局上限分层**：全局 `_MEDIA_SIZE_LIMITS`（image 20MiB / video 100MiB / file 8MiB，对齐 Go 捕获层）是纵深防御；渠道官方限制（企微 10MB、飞书 10MB、钉钉 20MB）在 adapter 内以 `fit_image_for_upload(max_bytes=渠道值)` 适配（飞书 2026-08-14 独立审查补齐）。**残余**：video 无渠道级适配（企微视频官方 ≤10MB < 全局 100MiB，超限会被渠道拒，属已知缺口）。

## 6. 生图能力（Phase B，2026-08-14 定稿并实施 GA 侧直连形态 v1）

### 6.1 定位

- **生图 ≠ 多模态理解**：理解是入站媒体 → chat 协议内 `image_url` content block；生图是模型决策 → **独立 API**（OpenAI `images/generations` 兼容协议）→ 文件 → 出站交付。
- GA 原生无生图（llmcore 只有 chat 类 Session）；主流 agent（Claude Code/Codex/OpenAI Agents SDK）均为**工具化**：模型调用生图工具 → API 产图 → 文件交付，聊天流不返回图。
- llm-proxy 已代理 `/v1/images/generations`（托管形态 2026-08-14 已实施，见 §6.2 B2）——operation 参数化（llm.chat/llm.image）按路由校验。

### 6.2 设计决策（2026-08-14 全部定稿；B1-B5 + D1）

- [x] 决策 B1：**生图 = GA 工具**（`image_gen`，第 10 个原子工具）：模型调用（prompt/尺寸/数量）→ 工具内调生图 API → 图片落盘 `outputs/` → 返回 `[FILE:outputs/<name>]` 走既有出站链路。**GA 核心循环零侵入**（agent_loop.py 零改动，dispatch 泛化）。**v1 已实施（2026-08-14）**。
- [x] 决策 B2：**llm-proxy 扩展 `POST /v1/images/generations` 代理**（**2026-08-14 已实施，托管形态落地**）：与 chat 同能力体系——capability 校验（operation 扩展 `llm.image`，路由层按 operation 拒绝错配 token）、JTI 预算计量（复用 max_turns 原子扣减）、白名单 egress（host/CIDR 级）、模型与 capability 匹配校验（loadBoundProvider 复用）；**provider 能力类型维度（chat/image）已实施**（migration 0058 capabilities 列，省略=[chat] 存量兼容）；生图 provider **排除 chat mixin**（image binding 写 image_gen 块，不进 llm_nos）；policy 放行（foundation.session-files.v1 allowed_tools 补 image_gen）；worker 侧 marker 回显兜底（image_gen 产物登记 generated_output_files，仿 export_docx）；安全审查项落地：生图响应体 32MiB 上限（MaxWorkerRequestBytes 仅限请求体，DisableCompression 大 JSON 原样传输——现按 Content-Length 前置拒绝 fail-closed）；openapi/web 同步（capabilities 字段 + 表单复选框）。
- [x] 决策 B3：**生图上游模型 = 独立配置 + ImageGenClient 双形态设计**（v1 实施直连形态）：GA `ImageGenClient`（OpenAI images 兼容，`/images/generations`）只认 apibase/apikey/model 配置——**直连/托管是配置差异而非两套实现**（对齐 BaseSession 先例：chat 直连与平台托管共用一份代码）；v1 的 apibase = 真实上游/中转网关 + 真实密钥；托管形态（apibase = llm-proxy + `llm.image` 能力令牌）**2026-08-14 已实施**，GA 侧协议代码零改动（平台 runtime_config 下发 image_gen 块）。实现进 `llmcore.py`（`BaseImageGenClient`/`OpenAIImageGenClient`/`resolve_image_gen`），**严禁新增 imagegen.py**——沙箱 overlay 只物化固定清单（LEGACY_MODULES），新增模块导致平台沙箱启动 ImportError。
- [x] 决策 B4：**生成文件管理**：生图产物写入 session outputs/（`outputs/<name>`，`self.cwd` 相对路径 + `os.makedirs(exist_ok=True)`，落盘前 ≤20MiB 检查——Go 交付上限 image ≤20MiB 是 fail-closed 任务失败，不能等到提交时刻才爆）；`[FILE:]` marker 走 Phase A 出站交付链。**marker 回显依赖**：工具返回 marker ≠ 交付发生，模型须在最终回复回显 marker 才触发交付（根项目无兜底；托管形态已补 generated_output_files 登记——worker `install_image_gen_marker_registry`，仿 do_export_docx，46e81b4）。
- [x] 决策 B5：**流式 = 客户端内部可选路径**（`stream:true` + `partial_images` SSE 取最终帧，工具层透明）：同步路径恒可用；SSE 解析失败/超时/收到非流式 JSON → 自动重试一次同步路径。**v1 已实施（待真实上游实测，社区有 gpt-image-1 partial streaming 波动报告）**。
- [x] 决策 D1（2026-08-14 用户拍板）：**直连形态先行（v1 实施），托管为终态设计（v1 不实施）**——ImageGenClient 双形态设计（一份代码，配置决定形态，见 B3）；平台侧（llm-proxy/policy/provider）一律不动；**平台模式 v1 有意不可用生图**（policy 保持 deny-by-default 现状，沙箱模型看不到该工具，无死工具暴露）。**实施注记（2026-08-14 当日）**：D1 定稿后同日托管形态（Step 3）即实施完成（5870cc5/46e81b4），平台模式生图现可用（policy 已放行 image_gen、runtime_config 下发 image_gen 块）。

### 6.3 失败语义（2026-08-14 审查定稿，对齐 §8 原则 6 失败诚实；方案 §6.5 摘要）

> 错误文本**统一 `[Error: image_gen ...]` 前缀**（对齐 do_code_run 工具层先例）；**不得使用 `!!!Error:` 前缀**——该前缀出现在模型最终回复尾部会触发 do_no_tool 致命判定/LLM_FAILED 链（ga.py do_no_tool）。

| 场景 | 行为 |
|---|---|
| API 4xx/5xx / 连接错误 / 超时 | 重试（仿写 `_stream_with_retry` 重试语义：429/408/5xx 退避集合 + retry-after 上限；生图返回体是 JSON 非文本流，不直接复用）→ 仍失败返回 `[Error: image_gen ...]` 给模型 |
| 空响应（`data` 空数组/缺失） | 错误文本，**绝不返回 marker**（防模型向用户谎报成功） |
| 流式中断（SSE 无最终帧） | 错误文本；`stream=true` 时自动重试一次同步路径（降级触发：SSE 解析失败/超时/收到非流式 JSON） |
| b64 解码失败 / 落盘失败 | 错误文本（仿 do_file_write 模式） |
| 产物 >20MiB | 工具侧落盘前检查字节数，超限返回错误文本（Go 交付上限 20MiB fail-closed，delivery_capture.go） |
| n>1 多图 | 命名 `outputs/<base>_<ts>_<i>.<ext>`（i=1..n）；返回多行 `[FILE:...]` marker（每行一个）；时间戳/随机后缀防同任务重复调用同名覆盖 |
| 未配置 image_gen | 错误文本（含"不要重试"提示，避免模型空转 3 次才 LLM_FAILED），绝不抛异常穿透 dispatch（agent_loop 只捕 StopIteration） |

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
| 入站合并（图+后续文本） | ✅ 已落地（2026-08-14 审查 I-3：InboundCoalescingBuffer 从微信扩展到四渠道，平台级窗口默认 4000ms，2026-08-15 实测文字+文件间隔 3752ms 后由 2500ms 调升） | §4 |
| code_run 防护 + OCR 依赖 | ✅ 已落地 | §4.3 |
| 出站发送（文件/图/视频） | ✅ 已落地（Phase A：send_media 统一接口 + QQ 分片 + 飞书/钉钉/企微上传适配 + delivery MIME 分发；**钉钉 file/video 的 downloadCode 上传流待实测**，暂 NotImplementedError fail-closed） | §5 Phase A |
| 生图 | ✅ GA 侧直连形态 v1 已落地（2026-08-14：`image_gen` 工具 + ImageGenClient 同步/流式路径 + outputs/ 落盘 + `[FILE:]` 出站）；**托管形态已实施（2026-08-14：llm-proxy `images/generations` 路由 + `llm.image` capability + provider 能力维度 chat/image + runtime_config 下发 image_gen 块 + policy 放行 + marker 兜底登记）**——平台模式生图可用 | §6 Phase B |
| 视频理解 | ❌ 无抽帧 | §7 Phase C |
| 视频出站 | ❌ 同出站缺口 | §5+§7 |

## 10. 落地分期与验收

| 期 | 内容 | 验收 |
|---|---|---|
| Phase A-0（前置，2026-08-13 审查阻断项） | ①四渠道入站媒体提取（QQ URL 直下 / 飞书 resources API / 钉钉 downloadCode / 企微 media_id+直链，公共下载器 `bot_poller/media_downloader.py`）——✅ 已实施；②GA 注入 base64 总量预算（对齐 llm-proxy 4MiB 上限）——✅ 已实施；③文档修订（本稿）——✅ | poller 单测 + 真实渠道冒烟（钉钉/企微下载 API 端点待凭据实测） |
| Phase A | 出站媒体统一：基类 `send_media` + 5 渠道上传适配（QQ=分片定案）+ delivery MIME 分发 | 微信/QQ/飞书各发 docx/图片/视频成功；失败路径诚实提示。**2026-08-14 已实施完成**（钉钉 file/video downloadCode 上传流待实测） |
| Phase B | 生图：**直连形态已实施（2026-08-14，v1）**——`image_gen` 工具（第 10 原子工具）+ ImageGenClient（同步 b64 默认路径 + gpt-image SSE 流式可选路径）+ outputs/ 落盘 + `[FILE:]` 出站；**托管形态已实施（2026-08-14）**——llm-proxy `images/generations` 代理 + capability `llm.image` + provider 能力类型维度（chat/image）+ runtime_config 下发 image_gen 块（不进 chat mixin）+ policy 放行 + openapi/web 同步 | 模型画图 → 用户 IM 收到图（各渠道）；直连本地闭环已验、托管链路 Go/worker 测试全绿；**微信端到端收图已实测（2026-08-14 生产闭环：10 张生成图送达，26/26 交付 acked 0 死信）**；其他渠道待真实凭据冒烟 |
| Phase C | 视频：镜像补 ffmpeg + 入站抽帧注入 + 出站 `send_media(video)` | 用户发视频可分析；GA 可发视频 |

每期独立可交付、互不阻塞；渠道真实验证需用户凭据配合（QQ/飞书已配置）。

## 11. 残余风险

- **微信 CDN 节流是路径约束（2026-08-14 生产实证）**：海外服务器→微信 C2C CDN 上传 ~18KB/s 且连接 ~30s 被杀。交付侧已适配（静态图 ≤300KB JPEG、预算链 85s<90s<120s），但 **>500KB 文件/GIF/视频交付仍受限**（GIF 动图不转、视频无适配）——根治是**中国侧部署/代理**（网络话题，仅记录不实施）；生成端建议控制 size（默认 1024x1024）。
- **无 admin 死信重投端点 → 已补（2026-08-14 独立审查 E2 + 复审 P1）**：`GET /v1/admin/deliveries` + `POST /v1/admin/deliveries/{delivery_id}/requeue`（AdminToken 门禁，仅 dead_letter 行可重投，attempt 预算归零；08-14 事故曾靠手动 SQL 恢复）。**重投语义（复审 P1 修复）**：requeue 置 `requeued_at=now`（migration 0060），重试窗口锚点 = `GREATEST(tasks.terminal_at, requeued_at)`——事故后数小时重投不会在下一 tick 被 DeadLetterExpiredDeliveries 原样打回；error_code 置 'REQUEUED' 标记（满足 DB CHECK + 记录事件，下次终态覆盖）。
- **gpt-image 参数兼容性（Phase B）**：**已实测（2026-08-14，new-api 中转）**——①`size` 必传（中转计费硬要求，缺省 500 "图片尺寸计费需要传 size"），客户端已默认 1024x1024；②gpt-image-2/gemini-*-image 返回 b64_json；③**sensenova/agnes 系只返回 `url` 直链**（b64_json 为空），客户端已加 url 直下兜底（≤20MiB 限流）；④sensenova-u1-fast size 合法集特殊（1664x2496/2048x2048 等，无 1024x1024）——错误文本含合法列表，模型可自愈；⑤流式路径（`stream:true` + `partial_images` SSE）仍未真实上游实测（社区有稳定性波动报告）——同步路径恒可用，建议保持 stream=False。
- **marker 回显依赖（Phase B，托管已补兜底）**：工具返回 `[FILE:]` marker ≠ 交付发生，模型须在最终回复回显 marker 才触发交付——**托管形态已补 generated_output_files 登记**（worker 包装 do_image_gen，终态 append_missing_file_markers 自动补写漏回显 marker，仿 export_docx）；直连形态仍无兜底（模型忘回显则静默丢失，可接受）。
- **直连形态无 JTI 计量（Phase B）**：成本靠 n≤4 限额 + 用户自觉（gpt-image token 计费，usage 日志留作后续计量基础）。托管形态下 image 调用走独立 llm.image token——**双能力 provider 的 chat 与 image 预算独立计量**（各持 max_turns 预算，互不占用），admin 配置时需知晓。
- **托管形态语义边界（2026-08-14 审查定稿）**：①model override 在托管形态必 409（provider 绑定强制，schema 描述已注明）；②双能力 provider 单 model 字段——image 请求用同一 model 打上游，建议 image 用独立 provider（web 表单已提示）；③image-only 部署被拒绝（至少一个 chat provider，fail-fast）；④双能力 provider 按 (ID, 能力) 去重（原按 ID 去重会误拒，已修）。
- **Phase B 上生产需重建 ga-runner**（内嵌 ga.py/llmcore.py/tools_schema.json，runtime_overlay.py LEGACY_MODULES/ASSETS 清单物化）。
- 各渠道上传 API（QQ file_info 分片、钉钉/企微 media 上传）需真实凭据实测（mock 只覆盖请求形状）。
- QQ 附件下载 URL 有时效（`rkey`），下载失败需按事件级重试语义处理（poller 已有 InboundCoalescingBuffer，媒体下载失败策略沿用：丢弃该媒体并记录，不阻塞文本消息）。
- 生图模型按 provider 配置独立——llm-proxy 能力扩展安全审查项**已全部落地（2026-08-14）**：operation 错配 401 deny-by-default、JTI 预算（max_turns 原子计量，与 chat 同路径）、egress 白名单（host/CIDR 级复用）、model 匹配（loadBoundProvider 复用）、生图响应 32MiB 上限（Content-Length 前置 + chunked 流式计数双闸）。
- **QQ 出站主动消息路径（审查 B5）**：异步 delivery 必超被动窗口（msg_id 5 分钟），媒体发送走主动消息——频控预算（单聊 20 QPM、日 1000/用户）与 C2C"用户近期交互"约束需 adapter 内实现并实测。
- **钉钉/企微入站下载端点（审查 S4）**：钉钉 `POST /v1.0/robot/messageFiles/download`、企微 `media/get`（access_token 来源 = 智能机器人凭据，SDK token 端点未确认）——代码已按官方文档实现，真实凭据冒烟时验证。
- **钉钉出站 file/video（审查 S4 同源）**：sampleFileMsg/sampleVideoMsg 需 downloadCode 上传流（端点未确认），暂 NotImplementedError fail-closed，真实凭据冒烟时验证。
- **媒体留存（审查 I4，2026-08-14 已落地）**：media_assets 90d 保留期（delivery tick 24h 节流）+ poller media_root 90d mtime 清扫（daemon 24h，BOT_POLLER_MEDIA_RETENTION_DAYS 可调）+ spool 30d——已实现，容量上限策略（按字节配额）未做，可后续按需。
- **图片跨任务不保留（审查 I3）**：checkpoint 256KB 历史预算与 GA trim 会把含 base64 图片的消息从头部裁掉——图片只活单任务，为显式接受的 v1 语义（后续 file_id 引用方案再解决）；`_sanitize_leading_user_msg` 丢图时无占位提示，建议后续补 `[图片内容已省略]`。
