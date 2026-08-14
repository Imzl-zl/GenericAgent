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

## 有意推迟（决策 D6，已被 D1 取代）

- ~~T8 Phase B 生图：无真实用户需求证据，llm-proxy `llm.image` capability 扩展建议随 B 期一起做~~。
- **2026-08-14 D1 用户拍板（取代 D6）**：T8 不再整体推迟——**v1 实施直连形态**（GA 侧 image_gen 工具 + ImageGenClient，平台侧零改动）；**托管形态**（llm-proxy `images/generations` 代理 + `llm.image` capability + provider 能力类型维度 + policy 放行 + openapi/web 同步）转为**终态设计，有真实需求时实施**。**实施注记：D1 定稿当日托管形态即实施完成（T8.5 done，见下方 Step 3 明细），"终态设计延后"已被实际需求取代。**
- 建议项 S1 后半（GA 注入前 PIL 解码失败丢弃而非原样透传）与 S3（媒体链路日志）未做，成本低可随时补。

## Phase B：生图（2026-08-14 定稿 + GA 侧直连形态 v1 实施）

> 方案真值：`PHASE_B_IMAGE_GEN_PLAN.zh-CN.md`（两轮 fresh-context 审查通过 + D1 拍板，§9.5/§9.6 审查结论）。
> 设计真值：`tenant_platform/docs/IM_MEDIA_ARCHITECTURE.zh-CN.md` §6（B1-B5 + D1 已勾选，§6.3 失败语义摘要）。

**D1 定位更新（替代原"有意推迟 D6"表述）**：直连形态先行（v1 实施），托管为终态设计（v1 不实施）——ImageGenClient 双形态设计（一份代码，配置决定形态）；平台侧（llm-proxy/policy/provider）一律不动，平台模式 v1 有意不可用生图（policy deny-by-default 无死工具暴露）。**2026-08-14 当日托管形态已实施（T8.5），本节为决策历史，当前状态以 T8.5 行为准。**

| 子任务 | 内容 | 验证 |
|---|---|---|
| T8.1 | 文档回写（§6/§9/§10/§11 + PROGRESS） | 二轮文档评审结论已回写 |
| T8.2 | llmcore：BaseImageGenClient + OpenAIImageGenClient + resolve_image_gen（配置子集解析/同步 b64 路径/gpt-image SSE 流式路径/仿 _stream_with_retry 重试/流式失败自动降级同步一次） | tests/test_image_gen.py |
| T8.3 | ga.py do_image_gen（第 10 原子工具）+ tools_schema ×2 + mykey_template 配置块 | 工具形态仿 do_file_write/do_code_run |
| T8.4 | mock 单测 + 本地 CLI 闭环（配置真实/中转 key → ga CLI 生图 → temp/outputs/ 出图；错误路径未配置时模型收到错误文本） | `python -m pytest tests -q` 全绿 |
| T8.5 | **托管形态（Step 3，2026-08-14 实施）**：llm-proxy `images/generations` 路由 + `llm.image` capability + provider 能力维度（chat/image）+ runtime_config 下发 image_gen 块 + policy 放行 + openapi/web + worker marker 兜底 | Go 全量 + race 5 关键包 + worker 146 + 契约 41 全绿；**待部署冒烟** |

**Step 3 实施明细（T8.5，2026-08-14 已落地）**：

| 层 | 改动 |
|---|---|
| domain | `LLMProvider.Capabilities`（chat/image）+ `EffectiveCapabilities`/`HasCapability`；migration 0058 `capabilities JSONB DEFAULT '["chat"]'`（省略=[chat] 存量兼容）；store 读写 + 校验（非法值/重复/native_claude 禁 image） |
| llmproxy | 路由 `/v1/images/generations`（+别名）；`handleProviderPath` operation 参数化（`OperationChat`/`OperationImage` 常量，错配 401）；`nativeEndpoint` 加 images/generations 映射（claude provider 拒绝）；**生图响应 32MiB 上限**（安全审查项：MaxWorkerRequestBytes 仅限请求体，现按 Content-Length 前置拒绝 fail-closed） |
| 签发 | `worker_credential` 按 provider 能力签发（chat→`llm.chat`、image→`llm.image`，双能力双 token）；`issueProviderCapability` 抽公共签发/撤销/有效期逻辑 |
| runtime_config | `RuntimeProviderBinding.Capability`；image binding → `image_gen` 块（apibase=proxy/v1 + 能力令牌，**不进 chat mixin**，多 image provider fail-closed）；GA 兼容探针实测（真实 llmcore.resolve_image_gen 消费 runtime 配置） |
| policy | `foundation.session-files.v1` allowed_tools 补 `image_gen` |
| worker | `install_image_gen_marker_registry`（I-2 兜底：包装 do_image_gen 登记产物到 generated_output_files，终态 append_missing_file_markers 自动补写漏回显 marker） |
| openapi/web | LLMProviderCapability schema + capabilities 字段（读写）；web 表单能力复选框（image 仅 native_oai 可选） |

**实施约束（定稿硬要求）**：①实现进 llmcore.py，严禁新增 imagegen.py（沙箱 overlay 固定清单 LEGACY_MODULES，新增模块 = 平台沙箱 ImportError）；②错误文本统一 `[Error: image_gen ...]`，绝不用 `!!!Error:`（触发 do_no_tool 致命判定链）；③response_format 仅 dall-e 系列发送（gpt-image 恒 b64_json，发该参数可能 400）；④落盘走 self.cwd/outputs/（不硬编码 script_dir），前置 ≤20MiB 检查；⑤agent_loop/Phase A 交付链/worker-python/backend-go/contracts 零改动。

**验证**：根 pytest 全绿（78 项，含 test_image_gen.py 30 项）。**真实 key 闭环已实测（2026-08-14）**：new-api 中转（newapi.myovo.cc.cd）双渠道五模型——gpt-image-2 完整 CLI 闭环成功（模型自动调 image_gen → temp/outputs/ 出图 2.6MB PNG → 最终回复回显 [FILE:] marker）；agnes-image-2.1-flash 验证 url 直下兜底（1.8MB PNG）；探测确认 gemini-3-pro-image / gemini-3.1-flash-image-preview 返回 b64_json、sensenova-u1-fast 只回 url（size 集合特殊）。**实测驱动的客户端适配**：①size 缺省默认 1024x1024（中转计费必传）；②b64_json 为空时 url 直下兜底（≤20MiB 限流）。**托管链路验证**：Go 全量 + race（application/worker/llmproxy）绿、worker 146、契约 41、web build 绿。**残余风险（待实测）**：流式 SSE 稳定性（保持 stream=False）、n>1 兼容性（透传+错误诚实）、IM 端到端收图需真实渠道凭据冒烟；**上生产需 make build 全量重建**（ga-runner 内嵌 ga.py/llmcore.py/tools_schema.json + platform/llm-proxy 带托管路由）。

## 2026-08-14 二次审查优化（IM_MEDIA_ARCHITECTURE 审查结论落地）

只读全量审查发现 5 重要 + 7 建议, 按推荐全部落地(除需真实凭据的渠道冒烟):

| 项 | 内容 | 落点 |
|---|---|---|
| I-1 | media_downloader `_validate_url` 空白名单 = 拒绝全部(fail-closed)——docstring 承诺与实现曾相反(漏传白名单即放行任意 https 主机) | media_downloader.py + fail-closed 回归测试 |
| I-2 | 图片注入预算按**降采样后实际字节**判定(旧口径按原始文件大小估算, 大图降采样后本可注入却被误杀, 与 1568px 降采样意图互相抵消); 原始 3MB 上限保留为解码防御 | agent_loop.py media_content_blocks + 2 回归测试 |
| I-3 | InboundCoalescingBuffer 从微信扩展到 QQ/飞书/钉钉/企微(基类统一 `deliver_inbound` + 事件渠道定时器 flush + 锁防双投)——修复"图消息与后续文本拆两任务、文本任务 media=null"追问语义断裂; 命令消息不延迟 | poller_server.py BotAdapter/五渠道 + 5 测试 |
| I-4 | `_fix_messages` 把 image_url 块转 Claude image 块(Anthropic 协议通道此前原样发送必 400/丢图); cache_control 只落 text 块(3 处) | llmcore.py + 6 测试 |
| I-5 | 设计文档勾选态/状态表/残余风险同步(A1-A3 勾选、§5 标题、§9 出站行、§11 已解决项移除) | IM_MEDIA_ARCHITECTURE.zh-CN.md |
| S-1 | 协议通道(ToolClient)拍平时 image 块降级占位——不再把 base64 文本垃圾注入提示词/历史/日志(每轮重发最多 3.5MB) | llmcore.py `_flatten_prompt_content` + 2 测试 |
| S-2 | llm-proxy `image_blocks` 计数兼容 Claude `{"type":"image"}` 块(原只数 image_url, native_claude 通道恒计 0 误导排障) | llmproxy/handler.go |
| S-3 | `load_llm_sessions` 对未匹配 session 类型的配置名显式 WARN(原静默跳过, /llms 缺项无提示) | agentmain.py |
| S-4 | 非图片扩展名媒体跳过时打日志(原静默 continue) | agent_loop.py |

验证: 根 48 + bot_poller 91 + worker-python 144 + Go 全量(真实 TEST_DATABASE_URL) 全绿。
残余风险(需真实凭据, 未变): 钉钉 file/video downloadCode 上传流、企微 token 端点、QQ 分片 4 步冒烟、飞书 media 视频。
