# 图像能力架构设计（2026-09-13 立项）

> 状态：**能力矩阵已实测锁定（本文 §2 是证据真值）；P0 已落地；P1/P2 待用户决策渠道方向**。
> 上位设计：`tenant_platform/docs/IM_MEDIA_ARCHITECTURE.zh-CN.md` §6（Phase B 生图）+ `.tasks/im-media-pipeline/PHASE_B_IMAGE_GEN_PLAN.zh-CN.md`（生图方案真值）。
> 本文只解决一件事：**把"图像能力"从"一个文生图工具"扩成可长期维护、可加模型/加通道/加操作的能力面**，并且每个结论都有实测或源码/文档依据。

## 1. 结论摘要

1. **文生图已可用且稳定**（agnes-image-2.5-flash，8–11s；1K/2K/3K/4K 与原生像素尺寸均生效）。
2. **参考图/改图 = 通道属性（不是模型属性、也不是全局有无）**——两条通道结论相反：
   - **渠道1 key（付费图像模型）→ ✅ 可用**：`POST /v1/images/edits`（multipart，文件字段 `image`）+ `gemini-3.1-flash-image`，
     实测 **200 / 9.0s / 1024×1024**，并以**像素级客观验证**确认参考图生效（参考图中心蓝方块 → 输出中心变绿）；
     免费分诊法：不带 `image` 发请求，返回 `image is required` + `code:convert_request_failed` 即"路由通且适配器在转换"（不花钱）。
   - **agnes 那条（渠道2，当前生产 provider）→ ❌ 不可用**：Agnes 官方改图协议是 `extra_body.image`（没有 `/images/edits` 端点），
     而网关（new-api v1.0.0-rc.33）图片中继**只转发其 DTO 内声明的字段**，`extra_body`/`ratio`/`return_base64` 被**静默丢弃**（实测：传不存在的图 URL 仍 200 出图）。
   - ⇒ 能力必须按 **(gateway, model) 实测矩阵**声明并 fail-closed（§4.1/§4.2），**不能**按模型名或文档推断。
3. **非方形比例仍然可达**：不靠 `ratio`（被丢弃），而是**直接传上游原生尺寸**（实测 `1312x736`→原样 16:9、`864x1152`→原样 3:4、`1920x1080`→归一化到 `1312x736`）。这条已在工具契约里写死给模型。
4. **"画质不满意"的根治点不在链路上，而在两处 agent 能力**：① 交付前的**视觉自检**（当前工具结果只能文本，模型看不见自己的产出）；② **提示词与合成 SOP**（图内文字一律合成，不要交给扩散模型）。
5. 设计红线不变：**生图代码进 `llmcore.py`（沙箱 overlay 固定清单，严禁新增模块）**；契约（proto/openapi）单一真值；decorative 参数可协商、**语义参数（size/n/model/prompt）不静默缩水**。

## 2. 能力矩阵（全部实测，2026-09-13）

| 能力 | 结论 | 证据（可复现） |
|---|---|---|
| 文生图 | ✅ | `POST /v1/images/generations` agnes-image-2.5-flash → 200，8–11s |
| size 档位 | ✅ | `1K`→1024²、`2K`→2048² |
| 原生像素尺寸 | ✅ | `1312x736`→1312x736、`864x1152`→864x1152、`2624x1472`→2624x1472 |
| 尺寸归一化 | ⚠️ 会归一化 | `1920x1080`→1312x736；`1024x768`→1152x864；`1536x1024`→1248x832 |
| `ratio` | ❌ 静默丢弃 | `1K`+`ratio:16:9` → 仍 1024²；非法 `99:1` 也 200（无任何提示） |
| `extra_body` / `extra_body.image` | ❌ 静默丢弃 | 传**不存在**的参考图 URL 仍 200 出图（若真转发必然报错） |
| 顶层 `image` | ❌ 打崩路由 | 400 `LLM Provider NOT provided ... You passed model=agnes-image-2.5-flash` |
| `/v1/images/edits`（agnes 渠道） | ❌ 上游无此端点 | 503 `no available server`（106s 后） |
| `/v1/images/edits`（渠道1 key：gemini/gpt-image） | ✅ **可用** | 不带图 → `image is required`+`convert_request_failed`（路由通）；带图 `gemini-3.1-flash-image` → **200/9.0s**，像素验证参考图生效 |
| edits 请求契约 | multipart/form-data，文件字段 `image` | 普通字段 `model`/`prompt`/`size`/`n`；响应与 generations 同构 |
| `return_base64` | ❌ 静默丢弃 | 仍回 url（走既有 url 直下兜底，不影响交付） |
| `n` | ✅ | n=1 通过 |
| `quality`（agnes 文本图队列） | ❌ 400 | `quality is not supported by text image queue` → 客户端已自动协商裁剪 |
| 图内文字 | ⚠️ 模型不可靠 | 现有做法：`code_run`+PIL 合成（agent 已自发采用，效果可用） |
| 自检（模型看自己的产出） | ❌ 缺能力 | `agent_loop` 工具结果只支持文本（`tool_results.append({'content': datastr})`） |

> **修正注（2026-09-13）**：本文件上一版曾记录"gemini/gpt-image 打 edits → `No available channel ... under group free`"并据此判定"改图不可用"。
> 该次探测用的是 `mykey.image_gen` 的 key，而它在"别用 gpt-image"的处置中已被换成**渠道2** key（group=`free` 里没有这些模型）→
> **503 是 key/分组错配，不是渠道不支持**。教训：**能力矩阵必须连同探测所用 key/分组一起记录**，否则"用错凭据测出的否定结论"会被写进设计真值（§7 已把这条列为残余风险，本次即其实例）。

**new-api 侧事实（源码级，2026-09-13 抓取 `main` 分支源码）**：

| 机制 | 源码位置 | 后果 |
|---|---|---|
| JSON 图片请求被反序列化进 `dto.ImageRequest` | `relaykit/dto/openai_image.go` | **未知字段不是被丢弃，而是收进 `Extra`**（`UnmarshalJSON` 显式提取） |
| **转发时 `Extra` 不合并** | 同上 `MarshalJSON`：合并 `Extra` 的循环**被注释掉**，旁注 `不能合并ExtraFields` | `extra_body`/`ratio`/`return_base64` 等**有意不通传** → 上游 200 但参数无效（静默） |
| `dto.ImageRequest` 白名单 | 同文件字段表 | 可通过网关的图像参数**只有**：`model/prompt/n/size/quality/response_format/output_format/partial_images/stream/images/mask/input_fidelity/image/...` |
| edits 表单只读固定字段名 | `relay/helper/valid_request.go` → `GetAndValidOpenAIImageRequest` | 只读 `prompt`/`model`/`n`/`quality`/`size`/`parameters`/`stream`/`image`/`watermark`，且 **`image` 是单值**（多图形态未实现） |
| `n` 上限 | `dto.MaxImageN = 128`，越界→400 | 工具侧仍夹取 ≤4 |
| `size` 含乘号 `×` → 400 | 同上 | 必须用字母 `x` |

**版本 `v1.0.0-rc.33`，revision `eb99ab1b`，本地容器 `new-api`，即用户的 CF 隧道网关）：
- 二进制含 `images/edits` 路由与 `ImageEdit` 符号（`grep -a` 实测 4/2 处）。
- 官方文档有 `POST /v1/images/edits`（multipart）。
- 社区 PR 显示改图按渠道适配器实现（Gemini `#2321`、豆包 seedream `#2090`、xAI grok-imagine `#4546`、JSON 版编辑 `#4646`），**即"改图能不能用"取决于渠道类型适配器 + 上游是否真有该能力**。
- 本实例 `channels` 全是 `type=1`（OpenAI 协议）的第三方 reseller；`abilities` 表里图像模型只有 `agnes-image-2.0/2.1-flash`（channel 2，group `default`）。

## 3. 约束（设计必须同时满足）

| 约束 | 值/来源 | 对设计的影响 |
|---|---|---|
| 入口代理窗口 | Cloudflare proxy read timeout **120s** | 客户端 `read_timeout=100`；慢模型必须拒绝或改异步 |
| 单任务硬预算 | `TASK_TIMEOUT_SECONDS=300` | `(max_retries+1)×read_timeout < 300`；生图失败必须**如实回复**而非拖到任务被杀 |
| 沙箱模块清单 | `runtime_overlay.py` LEGACY_MODULES | 新能力只能进 `llmcore.py`/`ga.py`/`assets/`，**禁止新增模块** |
| 契约单一真值 | `contracts/proto`、`contracts/openapi` | 新增能力维度必须同步生成 Go/worker 并跑契约绑定测试 |
| 单 image provider（v1） | `runtime_config.go` fail-closed | 加第二 image provider 需先扩能力维度（见 §4.1） |
| 错误语义 | `[Error: image_gen ...]`（禁 `!!!Error:`） | 任何新失败路径沿用 |

## 4. 架构设计

### 4.1 能力词表：把"图像"做成 operation 维度（而不是一个工具一个模型）

现状：`llm_providers.capabilities = ["chat","image"]`（migration 0058），一个 provider 一个 model。
问题：一旦引入改图（不同上游/不同协议/更慢），"image"这一维不够表达"这条通道能做什么"。

设计（契约先行，向后兼容）：
- `capabilities` 扩为 **`chat` / `image.generate` / `image.edit`**（省略语义=`["chat"]` 保持不变；`image` 保留为 `image.generate` 的别名以免存量数据失效）。
- `runtime_config` 的 `image_gen` 块增加 **`"operations": ["generate","edit"]`**（只声明真实可用的；缺省=`["generate"]`）。
- GA 侧规则：**工具参数始终存在（schema 静态），但客户端对未声明的 operation fail-closed 报错**——宁可"告知做不到"，绝不允许"发出去静默无效"（本次 `extra_body.image` 教训）。
- 平台侧：`native_claude` 禁 image 的既有校验沿用；多 image provider 的 fail-closed 在引入 operation 维度后放宽为「按 (capability, operation) 去重」。

### 4.2 客户端协议分层（llmcore 内，单一扩展点）

`BaseImageGenClient` → `OpenAIImageGenClient`（现有）；改图接入时新增 **`OpenAIImageEditClient`**（multipart `/v1/images/edits`），配置用 `protocol: "images_generations" | "images_edits"` 分派（`resolve_image_gen` 已按 `kind` 分派）。**已验证的 edits 契约**（可直接据此实现）：`multipart/form-data`，文件字段名 `image`，普通字段 `model`/`prompt`/`size`/`n`；返回体与 generations 同构（`data[0].b64_json` 或 `url`），可复用既有 `_extract_images`/魔数嗅探/20MiB 前置检查。**成本提示**：改图按张计费（flash 档 ~9s），而 agnes 文生图当前免费 → 默认模型与是否启用由运营侧决定（§6）。
**必须遵守的两条新规则（本次实测换来的）**：
1. **参数有效性可验证**：任何"上游可能静默忽略"的参数（参考图、模板、mask）都不得默认发送；发送前必须有 operation 声明，发送后必须有**可判定的成功信号**（否则视为失败，不得当成功交付）。
2. **能力协商要留证据**：协商（裁剪/切换协议）必须打日志（`[ImageGen Adapt]` 已有），便于从 runner 日志/代理日志回溯"到底哪条通道做了什么"。

### 4.3 交付质量闭环（P2，架构级）

目标：把"用户回话说太老/太假"变成"交付前自己看出来"。
- 现状障碍：`agent_loop` 工具结果只支持文本；`media_content_blocks` 的图片注入只作用于**用户首轮附件**。
- 设计：允许工具结果携带**图片内容块**（`{'type':'image_url','image_url':{...}}`），仅对多模态 chat 模型生效；预算沿用附件注入那一套（降采样 1568px + ≤3.5MB + 最多 N 张），并与历史裁剪/`_flatten_prompt_content` 的降级占位逻辑对齐（协议通道仍只给占位）。
- 用法（SOP 层）：`生成 → 看一眼 → 对照用户要求自评 → 需要则改 prompt/size 重试 ≤2 次 → 交付`；重试必须换策略（改尺寸/改风格词），不许原样重发。

### 4.4 提示词与合成（P0 已落地到工具契约 + SOP）

- **原生尺寸表**（§2）写进 `size` 描述：要 16:9 就传 `1312x736`/`2624x1472`，别传 `ratio`。
- **图内文字一律合成**：背景用生图，文字/印章/排版用 `code_run`+PIL（runner 镜像已带 pillow + noto CJK）。
- **写实类要求**：明确摄影术语（natural light / street photography / 35mm / skin texture），并优先 2K；4K 慢且收益低。
- **禁止能力外承诺**：schema 已写明"无参考图/改图"，避免模型对用户乱承诺。

### 4.5 可观测性

- 客户端：`[ImageGen Adapt]`（裁剪/保守集/记忆命中）。
- 代理：`llm-proxy` 已有非成功 WARN（含上游原文）；生图 4xx 参数类错误按白名单**只取 message 重建**后透传（2026-09-13 修复，见 `PHASE_B...§9.9`）。
- 判据（排障顺序）：`task_deliveries`（是否送达）→ `bundle.backend_history`（**模型侧到底看到了什么**）→ `llm-proxy` WARN（上游到底说了什么）→ `[ImageGen Adapt]`（客户端做了什么）。

## 5. 分期计划

| 期 | 内容 | 依赖 | 验收 |
|---|---|---|---|
| **P0（已落地）** | 参数协商自愈 + 保守集兜底 + 协商记忆 + 魔数嗅探落盘 + 代理参数类错误最小透传 + 工具契约写清能力边界/原生尺寸表/合成规则 | 无 | 根 153 全绿（image_gen 54）；真实 key 原 400 参数集自适应通过；生产转录无裸 400 |
| **P1（直连形态已实施，2026-09-13）** | 改图/参考图能力：接入一条**真正支持 edits 的上游**（官方 Gemini image edit / OpenAI gpt-image edits / 豆包 seedream / xAI grok-imagine，new-api 均已适配）→ 按 §4.1/§4.2 扩 operation + `OpenAIImageEditClient` | **用户选渠道**（账号/成本） | 契约 + Go + worker + web 同步；`/images/edits` 真实出图；`operations` 未声明时 fail-closed |
| **P2（建议立项）** | 视觉自检闭环（§4.3） | 无外部依赖，跨 `agent_loop`/`llmcore` | 工具结果可携带图片块；多模态模型能自评并触发有界重试；协议通道/超预算降级不回归 |
| **P3** | 生成后精确比例（需要非原生比例时按目标比例裁剪/扩边，作为显式步骤而非默认行为）+ 异步长任务（若将来接 >100s 的模型） | P1 之后 | 裁剪不切主体（主体检测或保留边距）；异步需 new-api 侧支持（社区 issue #4514，本版本未验） |

### 4.6 P1 直连形态实施记录（2026-09-13，已落地 + 已实测）

| 层 | 实现 |
|---|---|
| llmcore | `protocol`（默认 `images_generations` / 改图 `images_edits`）+ `operations` 能力声明；**端点由 operation 决定**（generate→JSON `images/generations`；edit→multipart `images/edits`）；multipart 复用既有重试/参数协商/`_extract_images`/魔数嗅探；**operation 未声明一律 fail-closed** |
| llmcore | `resolve_image_gen(name, operation)`：edit 时优先 `image_edit`（约定：`image_gen`→`image_edit`、`foo`→`foo_edit`），否则回退基础配置并由 gate 拒绝 → **免费文生图与付费改图分开配置** |
| ga.py | 工具层负责参考图**读盘 / 路径安全（拒绝逃逸）/ 魔数校验 / 20MiB 上限**；客户端只管发送（llmcore 不依赖 cwd 语义，沙箱与单测友好） |
| schema ×2 | 新增 `image` 参数，写明"传了就是改图、需通道声明该能力否则 fail-closed、当前仅 1 张" |
| mykey（本地） | 新增 `image_edit`：渠道1 key + `gemini-3.1-flash-image` + `protocol: images_edits` + `operations: [edit]` + `read_timeout: 100` |

**已实测（真实调用，1 次付费）**：`do_image_gen(image=ref.png)` → multipart `/images/edits` → **200 / 9.0s** → **像素客观验证**参考图生效
（参考图中心蓝方块 → 输出中心变绿）。附带收获：该上游**返回 JPEG 而非请求的 png**，魔数嗅探自动改名 `.jpeg` 并打 `ℹ️`——
先前"交付扩展名跟真实字节"的设计在真实场景救了一次（否则交付"名叫 .png 的 jpeg"，IM 侧 MIME 失配）。

**未实测（不得当作已支持）**：多图合成（网关表单只读单值 `image`）；`gemini-3.1-flash-image` 的纯文生图（generations 路径）；
`gpt-image-2` / `gemini-3-pro-image-preview` 的 edits；`mask`/`input_fidelity` 等其它 DTO 参数在 edits 路径上的行为。

**平台形态（待定，属契约变更）**：新增 `image.edit` 能力 provider（`capabilities` 维度 + runtime_config 增 `operations` + openapi/web/policy 同步）→ 走既有"契约先行"流程。
本轮**未做**：生产 provider 仍是 agnes（generate-only），因此**生产环境用改图会 fail-closed 并如实告知**，不会静默出错图。

## 6. 待用户决策

1. **改图**：能力已实测可用（渠道1 key + `gemini-3.1-flash-image` + `/images/edits`，9s，像素验证生效）。待定的是**成本与默认值**：是否启用、默认档位（flash 便宜 / pro 贵）、以及走**直连形态**（`mykey` 增 `image_edit` 配置，本轮实施）还是**平台形态**（新增 `image.edit` 能力 provider → 契约变更）。
2. **视觉自检**是否立项（P2）——这是唯一能在交付前抓住"太老/太假"的机制。

## 7. 残余风险

- 上游 reseller 行为不可控：同一 `type=1` 渠道不同模型能力差异大（本次已见 size 集合/quality/url-only 差异），**任何新模型上线前必须按 §2 的方式实测矩阵**，不得按文档假设。
- `abilities` 表与 `channels.models` 不一致 → 排障时以"实测请求"为准，不以配置表为准。
- 生成图质量仍受上游模型能力限制，SOP/自检只能减少无效交付，不能突破模型上限。
- **能力矩阵必须记录探测所用 key/分组**（本轮已因此误判一次）；接新通道/新模型必须重跑 §2 矩阵，不得按文档或按旧通道推断。
- 改图是**付费**路径（按张计费）且当前仅支持 1 张参考图；默认档位（flash/pro）与是否启用由运营侧决定。
