# 图像能力架构设计（2026-09-13 立项）

> 状态：**能力矩阵已实测锁定（本文 §2 是证据真值）；P0 已落地；P1 直连形态已落地；§8（2026-09-14 修订）把机制从"运行时协商"改为"通道能力档案"，P0/P1/P2 见 §8.8**。
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
| `sensenova-u1.5-lite` 改图（**免费**，2026-09-13） | ✅ 可用但形态不同 | 官方文档：`POST /v1/images/edits` **JSON**（非 multipart）+ `images:[{image_url: <公网URL 或 data:image/*;base64,>}]`；`n` 只能 1；`size` 2K/4K 常量（32 倍数、≤4096、比例 ≤3:1）；`watermark` 默认 true、`prompt_extend` 默认 true。实测 200 / **49.6s** / 1024×1024，**像素验证参考图生效** |
| `sensenova-u1.5-lite` 文生图 | ⚠️ 可用但慢 | 1024 档可用；**2K（`2048x2048`）实测撞 CF 524（>120s）** → CF 后面别指望 2K |
| 参考图输入来源（SenseNova） | 仅公网 URL 或 Data-URL | 官方明示**不支持纯无前缀 base64**；我们无公网图床 → 统一 Data-URL |
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

`BaseImageGenClient` → `OpenAIImageGenClient`（现有）；改图接入时新增 **`OpenAIImageEditClient`**（multipart `/v1/images/edits`），配置用 `protocol: "images_generations" | "images_edits"` 分派（`resolve_image_gen` 已按 `kind` 分派）。**两种 edits 传输（protocol 选择，都已实测）**：
- `images_edits`（OpenAI 官方形态）：`multipart/form-data` + 文件字段 `image`（gemini/gpt-image 走这条）；
- `images_edits_json`（SenseNova 等）：`application/json` + `images:[{image_url: <URL|Data-URL>}]`
  ——**形态选错就是最常见的"参数差异"类故障**（multipart 发给 SenseNova 会被回 `invalid arguments`，反之亦然）。
返回体与 generations 同构（`data[0].b64_json` 或 `url`），复用 `_extract_images`/魔数嗅探/20MiB 前置检查。
`extra_params`（如 `{'watermark': False}`）配置透传：**不得覆盖语义参数**（model/prompt/n/images，构造即拒绝），
且只应放"确认能过网关 DTO 白名单"的键（见 §2 new-api 源码结论）。
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

**默认通道（2026-09-13 定稿）**：`image_edit` = **sensenova-u1.5-lite（免费，~46-50s，JSON 形态）**；
注释保留 `gemini-3.1-flash-image`（付费，9.0s，multipart 形态）→ **慢但免费 vs 快但付费**，按需切换。
超时预算：`read_timeout=110`（< CF 120s 窗口）+ `max_retries=1` → 最坏 ≈220s < 300s 任务预算。

**未实测（不得当作已支持）**：多图合成（网关表单只读单值 `image`，SenseNova 的 `images` 数组虽是多图形态但未实测）；`gemini-3.1-flash-image` 的纯文生图（generations 路径）；
`gpt-image-2` / `gemini-3-pro-image-preview` 的 edits；`mask`/`input_fidelity` 等其它 DTO 参数在 edits 路径上的行为。

**平台形态（待定，属契约变更）**：新增 `image.edit` 能力 provider（`capabilities` 维度 + runtime_config 增 `operations` + openapi/web/policy 同步）→ 走既有"契约先行"流程。
本轮**未做**：生产 provider 仍是 agnes（generate-only），因此**生产环境用改图会 fail-closed 并如实告知**，不会静默出错图。

### 4.7 托管形态（平台）待办与已知硬约束

平台模式要让 GA 用到改图，需把"能力维度"落进契约（本轮**未做**）：
- `capabilities` 扩为 `chat` / `image.generate` / `image.edit`（省略语义保持 `["chat"]`，存量零迁移）；
- `runtime_config` 的 image_gen 块增 `operations`（或新增独立 `image_edit` 块），GA 侧按声明 fail-closed；
- openapi/web/policy 同步（走既有"契约先行 + 生成绑定 + 契约绑定测试"流程）。

**已知硬约束（必须一起解）**：托管链路里参考图是 **Data-URL 内联进请求体**，而 `llm-proxy` 现有
**4MiB 请求体上限**（`MaxWorkerRequestBytes`）→ 参考图会被直接拒绝。可选路径（按主流做法排序）：
① 参考图**降采样到预算内**（runner 已有 pillow，可复用媒体链路的 1568px 策略）；
② 平台侧做**受控上传/对象存储**换取短时 URL（SenseNova 也接受公网 URL，这条更通用）；
③ 抬升该路由请求体上限（最差：破坏传输层不变量）。
**结论**：托管形态单独一期，且必须先定"参考图怎么进/出沙箱"。

## 6. 待用户决策

1. **改图**：能力已实测可用（渠道1 key + `gemini-3.1-flash-image` + `/images/edits`，9s，像素验证生效）。待定的是**成本与默认值**：是否启用、默认档位（flash 便宜 / pro 贵）、以及走**直连形态**（`mykey` 增 `image_edit` 配置，本轮实施）还是**平台形态**（新增 `image.edit` 能力 provider → 契约变更）。
2. **视觉自检**是否立项（P2）——这是唯一能在交付前抓住"太老/太假"的机制。

## 7. 残余风险

- 上游 reseller 行为不可控：同一 `type=1` 渠道不同模型能力差异大（本次已见 size 集合/quality/url-only 差异），**任何新模型上线前必须按 §2 的方式实测矩阵**，不得按文档假设。
- `abilities` 表与 `channels.models` 不一致 → 排障时以"实测请求"为准，不以配置表为准。
- 生成图质量仍受上游模型能力限制，SOP/自检只能减少无效交付，不能突破模型上限。
- **能力矩阵必须记录探测所用 key/分组**（本轮已因此误判一次）；接新通道/新模型必须重跑 §2 矩阵，不得按文档或按旧通道推断。
- 改图是**付费**路径（按张计费）且当前仅支持 1 张参考图；默认档位（flash/pro）与是否启用由运营侧决定。

## 8. 修订（2026-09-14）：通道能力档案（Profiles）—— 能力是数据，不是运行时协商

> 本节**取代 §4.1/§4.2 的机制描述**（配置声明 + 运行时错误文本协商）；§2 证据矩阵、§3 约束、§4.3~§4.5 仍然有效。修订留痕见 §8.9。

### 8.1 为什么现机制不够（含 2026-09-14 实证）

现机制 = 静态声明（`protocol`/`operations`/`extra_params`）+ 运行时错误文本协商（`_IMAGE_GEN_TRIMMABLE` 5 个参数名 + 8 条话术族 + 3 次裁剪预算）+ 保守集兜底一次 + 进程内记忆。四个结构性弱点：

1. **两个真值源**：能力同时由"配置声明"和"运行时学到"决定，冲突时无裁决规则；记忆在**进程内**、重启即失忆——一次任务 20+ 张图省下的白 400，下个任务重新付一遍。
2. **协商能力硬编码**：可裁剪集（5 个名字）、话术族（8 条）、模型分支（`_is_dalle()` 按模型名子串猜行为）都写死在 `llmcore.py`。每来一个新模型的参数差异，都要改代码——而本渠道（实测 `GET /v1/models`，2026-09-14）就有 **11 个图像模型**（`agnes-image-2.0/2.1/2.5-flash`、`gemini-3-pro-image(-preview)`、`gemini-3.0-pro-image-preview`、`gemini-3.1-flash-image(-preview)`、`gpt-image-2`、`gpt-image-2.5-flare`、`sensenova-u1-fast`、`sensenova-u1.5-lite`）+ 3 个视频模型。
3. **请求构造没有"按能力构造"这一步**：靠 400 反向学习。未建档的模型最坏 3 次白 400 + 1 次保守集重试 = **4 次往返全部真实计费/耗时**，而且第一次一定失败。
4. **能力边界没有单一表示**，散落四处各说一半，且会互相矛盾。**实证（2026-09-14 读 `assets/tools_schema.json` 与 `_cn.json`）**：`image_gen` 的工具描述里同时存在
   - `**No reference-image / image-editing capability exists on the configured route**` / 「**当前路由上不存在参考图/改图能力**」（旧结论，写在 description 里）
   - 「Supplying `image` … makes this an EDIT/img2img call」/「传了 `image` 就是改图，未声明则 fail-closed」（P1 新加的参数说明）

   于是模型拿到的是**自相矛盾的工具契约**（一边说没有改图能力、一边说传 image 就改图）。根因不是笔误：**能力知识被写死在自然语言里，代码改了它不会改**。§4.1 已把能力维度提为 `operations` 配置，但配置之外还有 schema 文案、`extra_params`、`_is_dalle()` 三处在各自表达能力。

### 8.2 主流做法（调研 2026-09-14；来源见"证据"列）

| 借鉴对象 | 机制 | 采纳什么 |
|---|---|---|
| **pi `packages/ai`**（本机源码 `C:\sudy\github\pi`） | `ImagesModel.api` 指定线协议，`images-api-registry` 按 api 注册/分派实现（api 不匹配直接抛 `Mismatched api`）；模型元数据声明 `input`/`output` 模态；`ImagesContext.input` 是**内容块** ⇒ 纯文本=文生图、文本+图=改图（**同一入口**）；`Model.compat` 是逐模型覆盖项（**缺省自动探测，显式值优先**）；错误是返回值（`stopReason:"error"` + `errorMessage`）不是异常 | ①能力归**模型元数据** ②入口唯一、**输入驱动** ③线协议=注册模块 ④错误是值 |
| **OpenRouter Image API**（官方文档） | 单一端点 `/api/v1/images`；逐模型发现接口给 `supported_parameters`（带类型/枚举）+ `architecture.input_modalities/output_modalities` + `supports_streaming`；统一参数集 `prompt/n/resolution/aspect_ratio/size/output_format/seed/stream/**references**`；**单图模型直接拒绝 `n>1`**；生成失败返回 502 且**不计费** | 「能力发现 + 统一参数面 + 参考图是一等字段」= 档案化的现成工业形态 |
| **Gemini 图像**（官方文档，Nano Banana） | 单一 `generateContent`：`contents=[prompt, image…]`（多参考图，官方示例 5 张）；**生成与编辑同一条路**；`generationConfig.responseFormat.image.{aspectRatio,imageSize}` | 生成 vs 编辑是**输入差异**，不是端点差异 |
| **LiteLLM `image_generation`**（官方文档） | OpenAI 参数为统一面，**非 OpenAI 参数按 provider 原样透传**；`get_supported_openai_params(model, provider)` **逐模型声明**支持集；**默认不支持的参数直接抛错**，`drop_params=True` 才丢弃 | 「声明式支持集 + 默认 fail-loud + 丢弃是显式 opt-in」——与本项目"语义参数不静默缩水"红线同构 |

三条结论：

1. 成熟的图像能力面都是**「能力发现 + 统一参数面 + 逐模型声明」**，没有一家是"发出去猜"。
2. **生成与编辑是输入差异**（参考图是一等输入字段），不是两套工具或两个 operation 名——这与本项目现状（靠 `image` 参数触发 edit）已一致，保留。
3. **不支持的参数默认报错**，静默丢弃必须是显式 opt-in——所以现在的"保守集兜底"应降级为**显式开关**，不再是默认路径。

### 8.3 设计：三层 + 一条不变量

```
┌ 意图层（唯一入口，统一参数面）
│   generate(prompt, references=[...], aspect_ratio=, resolution=, size=,
│            n=, output_format=, seed=)
│   references 非空 ⇒ edit（输入驱动，同 pi/Gemini）
│   语义参数 = prompt / references / n / size / aspect_ratio / resolution
│   装饰参数 = output_format / quality / seed / stream
├ 能力档案层（**数据**）：CHANNEL_PROFILES[(gateway_host, model)]
│   ops / 参数映射(统一参数 → 线参数) / 上限(max_refs,max_n) / 预算(read_timeout,max_retries)
│   / 分类(latency,cost) / 置信(source) / 证据(evidence)
├ 线协议层（**代码**，注册表）：images_generations ｜ images_edits_multipart ｜
│   images_edits_json ｜（未来）gemini_generate_content
└ 传输与交付（不变）：_post 重试退避 / _extract_images / 魔数嗅探 / outputs/ 落盘 / [FILE:] marker
```

**唯一不变量（红线，写进测试）**：

> **改变用户意图的参数不允许缩水**——不支持就 fail-loud，且错误文本必须附"该通道实际支持什么"，让模型能改参自愈；
> **不改变意图的参数可以省略，但必须在工具结果里明示**（`ℹ️ 上游不支持 X，已省略`）。

由此得到与现状相反的动作顺序：**构造期按档案裁干净（不发），而不是发出去被 400 退回再裁**。

### 8.4 档案数据模型

```python
# llmcore.py 内联常量（为何不能放 assets/ 见 §8.7）
_CHANNEL_PROFILES = {
  'agnes-image-2.5-flash': {
    'api': 'images_generations',
    'ops': {'generate'},
    'maps': {'size': 'size',            # 支持（含 1K/2K/3K/4K 档位与原生像素）
             'aspect_ratio': None,      # 实测：ratio 被网关静默丢弃 ⇒ 不声明
             'resolution': None,
             'n': 'n',
             'output_format': None,     # 实测 400：text image queue 不支持
             'quality': None,           # 实测 400
             'seed': None},
    'limits': {'max_n': 4, 'max_refs': 0},
    'budget': {'read_timeout': 100, 'max_retries': 1},
    'latency': 'fast', 'source': 'measured', 'evidence': '2026-09-13 newapi.myovo.cc.cd group=default',
  },
  'sensenova-u1.5-lite': {
    'api': 'images_edits_json',         # 改图走 JSON + images[{image_url: Data-URL}]
    'ops': {'generate', 'edit'},
    'maps': {'size': 'size', 'aspect_ratio': None, 'resolution': None,
             'n': None,                 # 官方：n 只能 1 ⇒ 语义参数不支持, 非 1 时 fail-loud
             'output_format': None, 'quality': None, 'seed': None},
    'limits': {'max_n': 1, 'max_refs': 1},
    'extra_params': {'watermark': False},   # 仍是声明式, 但只在档案里
    'budget': {'read_timeout': 110, 'max_retries': 1},
    'latency': 'slow', 'cost': 'free', 'source': 'measured+documented',
    'evidence': '改图 200/49.6s 像素验证 2026-09-13; 2K 文生图撞 CF 524',
  },
  'gemini-3.1-flash-image': {
    'api': 'images_edits_multipart',    # 文件字段 image
    'ops': {'generate', 'edit'},
    'maps': {'size': 'size', 'aspect_ratio': None, 'n': 'n',
             'output_format': None, 'quality': None, 'seed': None},
    'limits': {'max_n': 4, 'max_refs': 1},
    'budget': {'read_timeout': 100, 'max_retries': 1},
    'latency': 'fast', 'cost': 'paid', 'source': 'measured',
    'evidence': 'multipart edits 200/9.0s 像素验证 2026-09-13; 返回 JPEG 非请求的 png(魔数嗅探救回)',
  },
  # 其余 8 个图像模型：**未建档 ⇒ fail-closed**（错误文本给出已建档清单与建档方法，见 §8.6）
}
```

- `maps[param]` 为 `None` = 该通道不支持：语义参数 → fail-loud；装饰参数 → 省略 + 明示。
- 解析优先级：**mykey 显式 `profile`（含 `unverified: True`）> 内置 catalog（按模型名）> 协议默认最小安全集（fail-closed）**。
- `source` 三档：`measured`（真实调用验证过）/ `documented`（官方文档）/ `unverified`（仅结构推断）。

### 8.5 协商的新定位（保留，但从主机制降为显式兜底）

- **构造期优先**：档案已定（measured/documented/config）⇒ 请求里不会出现已知被拒参数，协商不触发。
- **协商作为有界自愈始终保留**（实现定稿，与初稿"只在 unverified 时启用"不同）：上限 3 次裁剪 + 1 次保守集，**且只作用于装饰参数**（`_IMAGE_GEN_TRIMMABLE`），语义参数永不裁剪。
  保留理由：上游 reseller 的队列会变——agnes 拒收 `output_format`/`quality` 就是**先能用后被拒**（08-14 能用 → 09-13 400）；静态档案会过时，而重试只动装饰参数、不违反红线。
- 需要"绝不宽松"的部署可用 `unverified: True` 显式把通道标为未验证（对应 LiteLLM `drop_params=True` 的语义：宽松是 opt-in）。
- 学到的结论**回写同一个 profile 对象**，并打一条可直接粘贴进 catalog 的日志：
  `[ImageGen Adapt] profile-proposal: {"output_format": None}`——把"运行时发现"变成**可固化的事实**，而不是躲在内存字典里。
- 保守集兜底（网关清洗错误体时的最后防线）保留，但同样只在 unverified 通道上生效一次。
- 档案已定（`measured`/`documented`）⇒ **构造期就裁干净，不发** ⇒ 白 400 归零。

### 8.6 建档协议（对齐"收费尽量少测"）

| 步骤 | 动作 | 成本 | 能判定 |
|---|---|---|---|
| P0 | `GET /v1/models` | 0 | 模型清单（**已实测**：本渠道 11 图像 + 3 视频） |
| P1 | 打 `/images/edits` **不带** image | 0（不产生生成） | 端点与适配器是否存在（`image is required` + `convert_request_failed` = 通） |
| P2 | 发**非法值**装饰参数（`output_format:"zzz"`）或未知字段 | 0~极低 | 参数是否被接受（上游话术常回合法值列表） |
| P3 | 1 张最小真图 | 1 张计费（免费模型 0） | 端到端 + 响应形态（b64/url）+ 真实格式 |

原则：**先建档案再发真图**；每步结论写进 profile 的 `evidence`。**能力矩阵必须记录探测所用 key/分组**（§7 已因此误判过一次）。

### 8.7 硬约束（决定实现落点）

- **catalog 必须内联 `llmcore.py`**：`tenant_platform/worker-python/src/ga_worker/runtime_overlay.py` 的 `LEGACY_MODULES`/`LEGACY_ASSETS` 是**固定白名单**，且 `OVERLAY_MANIFEST_ENTRIES` 参与 overlay manifest digest（`test_task_identity` 钉住）。新增 `assets/image_models.json` **平台沙箱读不到**；要放 assets 必须同步改 worker-python + 身份测试（本轮不做）。
- **schema 的能力描述改为运行期由档案渲染**（同一份数据的两个视图），消除 §8.1-4 的自相矛盾；静态描述里只保留"结构性"信息（工具语义、marker 回显、图内文字走合成）。
- 预算不变式保留并测试钉住：`read_timeout < 120s`（CF 窗口）且 `(max_retries+1) × read_timeout < 300s`（任务预算）。
- 错误前缀 `[Error: image_gen …]`、`never !!!Error:`、工具层读盘/路径安全/魔数校验、交付 ≤20MiB 全部不变。

### 8.8 分期

| 期 | 内容 | 验收 |
|---|---|---|
| **P0（本轮建议）** | 档案内联 catalog + **按档案构造 payload** + 语义/装饰二分（fail-loud vs 省略明示）+ 未建档 fail-closed + schema 描述由档案渲染 + 单测（零网络） | 给定 profile 断言 payload **精确相等**；未声明 operation / 越界 n / 不支持语义参数全部 fail-loud；现有 `tests/test_image_gen.py` 74 例不回归 |
| **P1** | 建档脚本 + 11 个模型逐个建档（先 P0~P2 零成本三步，免费模型补 P3 真图） | 每模型 profile 带 `evidence`；真实 key 下**零白 400** |
| **P2** | 平台形态：契约 `capabilities` 维度（`image.generate`/`image.edit`）+ runtime_config `operations` + 参考图进出沙箱（4MiB 请求体上限） | 契约绑定测试 + 端到端 |

### 8.9 与 §4 的关系（留痕）

| 项 | 处置 |
|---|---|
| `protocol`（线协议）、`operations`（能力门）、fail-closed 原则、工具层读盘、魔数嗅探、错误前缀 | **保留**（§4.1/§4.2 结论仍有效） |
| `extra_params` 自由透传 | **取代**：改为档案内的声明式字段（`extra_params` 仅在 `source: unverified` 时作为临时逃生口） |
| `operations` 作为配置项 | **升级**：由档案派生（配置仍可覆盖） |
| 运行时错误文本协商 | **降级**：从默认路径改为 `unverified` 通道的显式 opt-in 兜底，且结论回写档案 |
| schema 里的能力断言 | **删除**：改为运行期由档案渲染 |
| 新增 | 参数映射表（`maps`）、未建档 fail-closed、profile-proposal 日志、建档协议（§8.6） |

### 8.10 建档回填（2026-09-14，全部 0 计费）

工具：`assets/probe_image_channel.py`（P0 列表 → P1 端点探活 → P2 参数探活 → P3 真图；默认只跑 P0+P1）。
本轮对 12 个图像模型全量跑过 P0+P1，事实如下（写入 catalog 的 `evidence`）。

| 新实测事实 | 证据 | 对档案的影响 |
|---|---|---|
| **`size` 形态逐通道不同** | agnes 传档位 `1K` 通过校验（只回 `prompt is required`）；gemini/gpt-image/sensenova 传档位回「图片尺寸格式错误，应为 宽x高，例如 1024x1024」 | 新增档案字段 `size_style`（`both`/`tier`/`pixels`）；形态不符 **fail-loud**（size 是语义参数，不静默替换） |
| **该参数校验错被网关标成 `500`** | 上面这条错误响应的 `type=new_api_error` + HTTP 500（不是 400） | 不能只看状态码判定参数错；已建档通道在构造期就不发错形态，故不会触发无意义重试 |
| **sensenova 参考图是 1..5 张** | 网关原文 `invalid images, should contain between 1 and 5 items` | `max_refs` 由 1 改 **5**。注：§2 记的"仅 1 张"其实是 `n` 的限制，不是参考图上限 |
| **`image is required` 不能证明上游能改图** | multipart edits 对 gemini/gpt-image **和 agnes 都**回 `image is required`（只说明网关路由存在 + 适配器在转换）；但 agnes 真实改图 08-14 实测 **503/106s**，官方也无 `/images/edits` | **结论矛盾时不声明能力**：agnes 保持 `ops={'generate'}`，带参考图 fail-closed。这是 §2"200 就算成功不可信"的同类教训——**否定信号同样不可单独采信** |
| **改图请求形态逐模型不同** | gemini/gpt-image 的 JSON edits 被回 `image is required` 或 `failed to parse multipart form`；sensenova 的 JSON edits 明确校验 `images` 数组 | 印证"改图是**通道属性**（适配器决定）"；`api` 取值继续由档案逐模型声明 |
| **探针自身的两个坑（已修）** | ①`requests` 仅用 `data=` 不会发 multipart（网关回 `multipart boundary not found`，500）→ 必须带 `files=`；②P2 参数探活若留着合法 `prompt`，上游忽略该参数时会**真的出图（=计费）** → 必须让请求必然被拒（prompt 留空） | 探针工具已加注释与修正；`--paid` 默认关闭 |

**生产路径端到端复验（2026-09-14，免费模型，`--e2e`）**：`sensenova-u1.5-lite` 两条操作都跑通且带客观判据——
文生图 **200/42.9s/862KB/png**；改图（JSON + Data-URL 参考图）**200/70.8s/888KB/png**，
**像素级验证：参考图纯蓝 → 输出中心像素 (248,10,1) 变红（changed=True）**。
同时验证了整链：配置里**只写 model**，端点/形态/参数/参考图上限全部由档案得出（`api=images_edits_json`、`size_style=pixels`、`max_refs=5`）；
工具层 `ga.do_image_gen` 落盘 + `[FILE:]` marker + 省略明示（quality/output_format）+ 魔数改名（请求 webp → 实得 png）全通。
另：该模型单次实测 42.9s/70.8s，印证 `read_timeout≥110` 与 `max_retries=1` 的预算取舍（最坏 ≈220s < 300s）。

### 8.11 参考图输入预算：为什么不抬 4MiB（2026-09-14 查证 + 定案）

**4MiB 是自己设的**：`tenant_platform/backend-go/internal/infrastructure/llmproxy/handler.go`
`const MaxWorkerRequestBytes = 4 * 1024 * 1024`——**硬编码常量，不是配置项**（防请求体撑爆内存的读上限）。
另两处不要混淆：`api.DefaultMaxRequestBodyBytes = 1MiB`（`PLATFORM_MAX_BODY_BYTES`，只管平台 API，
而入站媒体传的是**路径**不是字节，所以 1MiB 够）；生图**响应**上限 32MiB（另一个常量）。
又因 base64 膨胀 ~33%，4MiB 请求体 ⇒ 内联图片原始字节上限 ≈ **3MiB**。

**现状（对账后的真实缺口）**：

| 路径 | 现状 | 结论 |
|---|---|---|
| 入站视觉注入（用户发照片给模型看） | `agent_loop._image_block_from_file` 已降采样最长边 1568px + JPEG，预算 3.5MB（对齐 4MiB） | **已符合行业做法**，不动 |
| **改图参考图**（`ga._load_reference_images`） | 直读原始字节（单张 ≤8MiB）→ 原样 base64 发出，**不缩放不重编码** | **缺口**：平台形态必撞 413 `BODY_TOO_LARGE`；直连形态白传带宽/延迟（8MiB 原图 base64 后 ≈10.7MB） |
| 解压炸弹 | 有字节上限 + 魔数校验，**无像素维度上限** | 8MiB 的 PNG 可解成上亿像素 → 补头部像素校验 |

**行业共识（2026-09-14 查一手文档）**：

1. **内联 data URI 只适合小文件**——fal 官方原文："Data URIs embed the entire file in the request payload. This inflates
   the request size significantly… **not recommended for files larger than a few KB**. Use CDN uploads or external URLs instead"；
   其通用形态是 **URL**（本地文件先 CDN 上传，大文件自动 10MB 分片）。Gemini 同类分档：内联 ≤100MB /
   File API ≤2GB·48h / 外部 URL / GCS URI。
2. **客户端预缩是主流建议**——Anthropic 官方明说：自己先 resize，传 4000px "the model sees the same thing either way"，
   只浪费带宽和延迟（Claude 侧自动降采样到长边 1568px ≈ 1.15MP，API 单图 5MB；OpenAI 则是 2048 框→768 短边→512 瓦片）。
3. **解码前做像素上限**（防解压炸弹）——fal 对 `image_urls` 在解码前只读头部校验 `max_image_pixels`（默认 ~8948 万，PIL `MAX_IMAGE_PIXELS` 同量级），超限回确定性 422 `image_too_large`，
   而不是 OOM 掉整个 runner。

**定案（不抬 4MiB）**：

- **P2-a（本期可做，直连/托管双受益）**：参考图在**工具层**（与路径安全/魔数/大小同层）归一化——
  长边 >1568px 等比缩放（对齐出图原生尺寸 1K/2K，再大对生成无收益）、编码 JPEG q85、剥离 EXIF；
  解码前先读头部尺寸，超 `MAX_IMAGE_PIXELS` 直接拒绝；归一化结果**明示**给模型与用户
  （"参考图已归一化 4032x3024 → 1568x1176，JPEG 218KB"）。预期把 8MiB 级原图压到 200-500KB，4MiB 不再是约束。
- **P2-b（真大图才需要）**：平台侧**受控上传→短时 URL**，把"内联字节"换成"引用"。这是主流终态，
  且 SenseNova/Gemini/fal 都把 URL 当一等输入形态（SenseNova 甚至只收公网 URL 或 Data-URL）。
- **明令禁止的修法**：直接抬 `MaxWorkerRequestBytes`（那是内存防御，与问题无关；真要大图应该换引用传输）。

### 8.12 P2-a 实施记录：参考图归一化（2026-09-14 已落地 + 真机验证）

**落点（架构未跑偏）**：工具层 `ga._load_reference_images` + `ga._normalize_reference`
（与路径安全/魔数/字节上限同层）；`llmcore` 仍只管发送，不依赖 cwd 与图像库。

实施顺序（不可换）：

1. **源文件读取上限 64MiB**（解码防御，**不是能力上限**）——超过就显式拒绝并告知；
2. **解码前校头部像素数 ≤24MP**（防解压炸弹；先拒再解，不浪费内存）；
3. 尺寸合规且源文件 ≤1MiB → **原字节透传**（不必要不重编码，不做无损转有损）；
4. 否则缩到 **长边 ≤1568**（保比例、LANCZOS；JPEG 先 `draft()` 解码期降采样）+ **JPEG q85**
   （重编码天然剥 EXIF）；有 alpha 的图**保留 PNG**（转 JPEG 会压成黑底）；
5. 归一化结果**明示**：`参考图已归一化（长边 ≤1568, 去元数据）: a.png 4000x3000/33884KB → a.jpg 1568x1176/423KB（JPEG q85）`。

**真机实测（免费模型，2026-09-14；最坏情况 = 噪声图近似真实照片）**：

| 指标 | 数值 |
|---|---|
| 参考图 | 4000x3000 噪声 PNG **33,884KB**（纯色版仅 41KB——所以必须用噪声测最坏情况） |
| 归一化后 | 1568x1176 **JPEG q85 / 423KB**（缩了 ~80 倍） |
| **实际请求体** | **577,877 bytes（564KB）vs llm-proxy 硬上限 4MiB → 余量 3.45MiB** |
| 像素客观判据 | 输出 1024x1024 中心像素 (238,8,2) → **蓝→红，参考图仍生效** |

**途中拓出的两个真缺陷（已修）**：

1. **旧实现“先整读字节再校 20MiB”会把正常大图拒掉**——33MB 噪声 PNG 直接被拒（真实手机照片/扫描件很容易超）。改为**按路径交给 PIL 解码**，读取上限只做解码防御。
2. **测试 fixture `_1PX_PNG` 是坏 PNG**（魔数对、CRC/IDAT 不合法，PIL 报 `UnidentifiedImageError`）——以前只做魔数嗅探所以一直未暴露。fixture 已换成真合法 PNG（fixture 缺陷与产品缺陷分开修）。

**两个数字为什么与 fal 不同（有意收紧，有必要理由）**：像素上限取 **24MP**（覆盖 6000x4000 这个手机/相机的实际常见上限）
而非 fal 的 89M——fal 服务多租户且只读头部，我们这里参考图进的是**出图 ≤2K** 的生成链路（24MP 已是输出像素的 60 倍），
收紧才能控住解码内存。`pillow` 缺失时**显式失败**（不能校像素就不能保证输入安全，不静默发原图）。

### 8.13 P2-1 实施记录：平台能力维度按 operation 细分（2026-09-14 已落地）

**为何必须细分**："改图"是**通道属性**（上游端点 + 网关适配器），不是模型属性——实测同一网关下 agnes 只能文生图、
sensenova 只能改图。平台如果只有一个 `image` 维度，就无法表达"这条通道能做什么"，托管形态下改图只能靠试探或静默错配。

**能力词表**（`domain.ProviderCapability`）：`chat` / `image.generate` / `image.edit`；
**`image` 保留为 `image.generate` 的别名**——0058/0059 以来的存量行与既有 API 客户端都写 `image`，
不能因为细分而失效。归一化在**写入时进行**（api 层，落库一律显式形态），**读取时也归一化**
（`EffectiveCapabilities`，所以存量行回显为 `image.generate`，前端零兼容分支）。**无数据迁移**。

**runtime_config**（GA 消费面）：

| 平台声明 | 下发块 | 块内 `operations` |
|---|---|---|
| `image.generate`（或别名 `image`） | `image_gen` | `["generate"]` |
| `image.edit` | `image_edit` | `["edit"]` |

- 块名不是自由命名：GA 的 `llmcore._edit_config_name` 约定是 `<name>_edit`，所以必须是这两个名字；
  `image_edit` 能被 GA 的 `resolve_image_gen("image_gen", operation="edit")` 直接命中。
- `operations` 是**能力声明**（GA 对未声明 operation fail-closed），不是路由参数。
- 多图像 provider 的 fail-closed **按 operation 去重**：不同 operation 各一条通道（真实部署：文生图 agnes + 改图 sensenova），
  同一 operation 两条通道仍拒签（没有路由策略前无法定序，假成功比拒签更危险）。
- `MyKeyLoader` 是 `globals().update(_config)`（无键白名单），所以新增顶层块 `image_edit` 天然能被 GA 读到。

**代理层 operation 保持不变的取舍（必须知道）**：`llmproxy` 仍只有 `llm.chat` / `llm.image` 两个 operation，
**不按 generate/edit 细分**。后果：**平台侧无法拦住"用 generate-only 的 capability token 打 `/images/edits`"**；
这一层由 GA 侧兜住（`operations` 声明 + 未声明即 fail-closed，已有单测）。
这是 **有意取舍**（要细分就得双 token/双路由映射 + 策略同步，而 GA 侧已是诚实门），
但也是**残余风险**：若将来出现非我们自己的 worker，就需要 P2-1b（token 按 operation 签发 + 路由校验）。

**契约与前端同步**：`contracts/openapi/platform.yaml` 的 `LLMProviderCapability` 枚举改为
`[chat, image.generate, image.edit]`（描述里写明 `image` 别名与为何细分）；Web 能力复选框改为三选
（`api/types.ts` 联合类型 + `LLMProviderForm.tsx`），native_claude 仍禁图像能力，旧别名在读取时已归一化。

**DB 约束**：migration `0061_provider_capabilities_operations.sql` 把 CHECK 放宽到
`<@ ["chat","image","image.generate","image.edit"]`——**保留 `image`** 是因为存量行就是它，
不做数据迁移；新写入不再产生该字面值。

**验证（2026-09-14）**：

| 检查 | 结果 |
|---|---|
| `go vet ./...` / `go build ./...` | 通过 |
| `go test ./internal/domain/...` | 通过（新增别名归一化/去重/operation 映射/非法值 4 例） |
| `go test ./internal/application/ -run "RuntimeConfig\|Capabilit\|OperationSplit\|Image"` | 通过（新增：双块拆分、按 operation 去重、别名不泄露 edit、**跨语言探针**——真 GA 子进程分别解析出 `image_gen`/`image_edit` 的 model/token/operations） |
| `npm run lint` / `npm run build`（web） | 通过 |
| `pytest tenant_platform/tests/{contract,security,smoke}` | 41 passed（1 例既存 grpcio 版本问题，与本次无关） |
| **未跑（必须补）** | `internal/api` 能力用例、`internal/infrastructure/postgres`、以及 **migration 0061 的真实应用**——需要 `TEST_DATABASE_URL`；本机 Docker 未启动。交付前必须在 CI 或带 Postgres 的环境跑一次（迁移写错是生产事故） |

### 8.14 事故与结构修复：迁移清单两处真值（2026-09-14）

**现象**：P2-1 推送后 CI 红——`new row for relation "llm_providers" violates check constraint
"llm_providers_capabilities_check" (SQLSTATE 23514)`；而**本地全绿**，因为本机没有 Postgres，
DB 类测试（`internal/api` / `postgres` 包）根本没跑。就是“**没跑过的改动不能算验证过**”的教科书案例。

**根因**：`internal/infrastructure/postgres/migrations.go` 有**两份硬编码清单**：

1. `migrationFiles()`——应用顺序（fresh schema 直跑）；
2. `pendingMigrations`——**已有 schema** 的补跑清单，靠 marker 表判断（标记表不存在就重跑该文件，
   所以近期“纯约束替换”类迁移用幂等 DO 块 + 永不存在的标记表，每次 EnsureSchema 都重跑一次）。

我的 `0061` 只落了文件，**两份清单都没加** ⇒ 新库直接用旧约束、存量库也不会补跑。

**结构修复（不是补丁）**：

- `migrationFiles()` 改为**读目录排序**（目录=单一真值；文件名即顺序，`^\d{4}_[A-Za-z0-9_]+\.sql$` 约束命名）；
- `readMigrationBatch` / `ApplyMigrations` 对**空清单显式报错**（防“静默应用零个迁移”）；
- 新增两条测试把两处清单的覆盖关系变成**硬失败**：`TestMigrationFilesCoverPendingMigrations`
  （目录里有、`pendingMigrations` 没列 → 报错）与 `TestMigrationFilesOnDiskSortedAndConventional`；
- `pendingMigrations` 补 `0061` 条目，并写清“幂等 DO 块不建标记表、每次重跑”这个模式。

**验证（真 Postgres 16 容器，两条路径都跑）**：

| 路径 | 做法 | 结果 |
|---|---|---|
| 全新库 | `CREATE DATABASE ga_verify` → `go test -p 1 ./...` | **17 个包 ok**；先前 CI 失败的 `TestAdminCreateLLMProviderCapabilities` / `...UpdateLLMProviderCapabilitiesBumpsRevision` 全部通过 |
| **存量库** | 从用户 `ga_test` 复制出 `ga_existing`（连 0059 的约束都没有）→ 跑 api 能力用例 | 通过；且事后查 `pg_get_constraintdef` 确认约束**已补成** `<@ '["chat","image","image.generate","image.edit"]'` |

（临时库已删除；仅剩一个 Windows 本机路径问题：`personal:1-g1` 带冒号在 Windows 建不了目录，CI/Linux 本就是过的。）

**两条教训（已归档）**：① 硬编码清单 = 两处真值 = 必然漏一处；② **CI 已按用户要求关闭**
（额度问题）⇒ 以后没有“推送后 CI 替我验收”这一层，交付前**必须本地真库验证**（含迁移）。

**仍未证实的（不得当作已支持）**：gemini-3-pro-image / 各 preview / gpt-image-2(.5-flare) 的**真图改图**（仅端点探活）；gpt-image 系的 `stream`/`partial_images` 在本网关的实际行为；sensenova 的 `quality`/`output_format`（本轮探针在 size 校验前即返回，得不到结论 → 保守声明为不支持）。


