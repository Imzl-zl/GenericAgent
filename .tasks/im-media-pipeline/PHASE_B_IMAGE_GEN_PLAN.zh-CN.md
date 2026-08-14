# Phase B 生图能力实施方案（image_gen）

> 状态：**方案定稿（2026-08-14）→ 二轮 fresh-context 审查：需修改后批准（无阻断，4 重要已回写，见 §9.6）**。首轮审查已回写；用户拍板 **D1：直连形态先行，托管为终态设计延后实施**（双形态统一设计，一份客户端代码，见 §3.2/§9.5）。二轮审查提示词：`PHASE_B_IMAGE_GEN_PLAN.REVIEW2_PROMPT.zh-CN.md`。
> 设计真值：`tenant_platform/docs/IM_MEDIA_ARCHITECTURE.zh-CN.md` §6（本方案定案后回写升级）。
> 任务真值：`SUBTASKS.csv` T8（本方案二轮审查通过后拆分落地）。

## 1. 背景与目标

- 用户需求：让 GA 具备**生图能力**——平时对话走多模态/文本模型，模型决定生图时调用**独立生图模型**（OpenAI `images/generations` 兼容协议优先）。
- 现状：GA 无任何生图实现（llmcore 只有 chat 类 Session；ga.py 9 个原子工具无生图；llm-proxy 仅代理 chat 三路径）。生图在 IM_MEDIA_ARCHITECTURE §6 为设计稿（决策 B1–B4 未实施）。
- 目标：以**工具化 + 独立 ImageGenClient** 落地，GA 核心循环零侵入；产物复用 Phase A 出站交付链路（`[FILE:]` → spool → `send_media`）。

## 2. 本次改动波及范围

### GA 根项目（主体，Step 2）

| 文件 | 改动 |
|---|---|
| `ga.py` | 新增 `do_image_gen` 工具（第 10 个原子工具）：解析 args（prompt/size/quality/n/output_format）→ 调 `resolve_image_gen` → b64 落盘 `outputs/` → 返回 `[FILE:outputs/<name>]` |
| `llmcore.py`（**推荐**；或新增 `imagegen.py`） | `BaseImageGenClient`（配置解析，复用 BaseSession 模式）+ `OpenAIImageGenClient`（同步 b64 + 可选 SSE 流式路径）+ `resolve_image_gen(name)` 命名分派工厂。**若新增 imagegen.py：必须同步 `runtime_overlay.py` 的 `LEGACY_MODULES`/`OVERLAY_MANIFEST_ENTRIES`（runtime_overlay.py:14-39，沙箱只物化清单内文件）+ worker 测试 fixture（test_managed_agent/test_runtime_overlay/test_task_identity），否则沙箱启动 ImportError 全平台任务失败（二轮审查 I-1）** |
| `assets/tools_schema.json` / `tools_schema_cn.json` | `image_gen` 工具条目（prompt 必填；size/quality/n/output_format 可选；`model` 可选覆盖） |
| `mykey_template.py` | 新增 `image_gen` 独立配置块（name/apibase/apikey/model/stream/超时/代理）——**命名提醒：变量名不得含 `config`/`api`/`cookie` 子串**（agentmain 会话扫描会误判为 Session 并 WARN，agentmain.py:72） |
| `tests/` | `test_image_gen.py`：mock API 单测（请求形状、b64 落盘、FILE marker、错误诚实返回、流式 SSE 解析、未配置/空响应/超限路径） |
| `tools.md` / `memory.md` | 落地闭环后更新协作文件（工具清单 9→10、配置示例、坑点）；`docs/` 安装文档按需 |

### tenant_platform（可选后置，Step 3，走托管能力体系时才需要）

| 文件 | 改动 |
|---|---|
| `backend-go/cmd/llm-proxy/.../server.go` | 路由 `POST /v1/images/generations`（+ `/images/generations`） |
| `.../llmproxy/handler.go` | `handleImageGenerations`：复用 `handleProviderPath` 能力校验/预算/白名单流程，operation 校验 `llm.image` |
| `.../llmproxy/target.go` | `nativeEndpoint` 路径映射加 `images/generations` case（target.go:31-70；egress 白名单是 host/CIDR 级，network_policy.go，无 path 级） |
| `.../application/worker_credential.go` | `Operation: "llm.chat"` 硬编码 → 按 capability 类型扩展 `llm.image`（影响面扫描已确认：`llm.chat` 仅 3 处 Go 内，worker-python 零引用） |
| `contracts/policy/foundation.v1.json` | **终态实施时**：`foundation.session-files.v1` 的 allowed_tools 补 `image_gen`。**v1 定稿有意不启用平台模式生图**（policy 保持 deny-by-default 现状 = 平台沙箱模型看不到该工具，无死工具暴露）；托管形态实施时随 policy 一起放行（过滤机制：legacy_instrument.py:93-120） |
| `.../application/runtime_config.go` | **终态（托管实施时）**：向 `mykey.runtime.json` 写 `image_gen` 块（apibase=proxy/v1 + llm.image capability token），且**不进 chat mixin**（runtime_config.go:146-148 现有 >1 provider 自动写 mixin_config 逻辑）。v1 不实施 |
| 契约/文档 | capability 枚举说明 + 安全审查（JTI 预算、**响应体大小上限**——`MaxWorkerRequestBytes` 仅限请求体 handler.go:19-20，响应无上限；`DisableCompression: true` transport.go:117 大 JSON 原样传输） |
| 部署 | ga-runner 镜像内嵌 `ga.py`/`llmcore.py`/`assets/tools_schema.json`（runtime_overlay.py:16-29）——**Step 2 上生产也必须 `make build` 重建 ga-runner**（非仅 Step 3） |

### 明确不动的

- `agent_loop.py` 核心循环：零改动（工具化原则，dispatch 泛化 agent_loop.py:18-31）
- Phase A 交付链路（delivery spool / send_media）：零改动（`[FILE:]` marker 已打通：Go `captureTaskDeliverableFiles` delivery_capture.go:55+ / `RecordOutbound` 强制 `outputs/` 前缀 session_files.go:368-370 / 根前端按 `<repo>/temp` 解析 wechatapp.py:132）
- web / worker-python / proto：**无契约变更**（llm-proxy 是 HTTP 实现，无 proto；Operation 枚举仅 Go 内部）。托管形态（终态）实施时，openapi 的 llm-providers 模型与 web 表单必改——届时撤销本条

## 3. 改动前后架构设计

### 3.1 改动前

```
对话模型(Session) → agent_loop → 9 原子工具(无生图) → 文本/文件交付
llm-proxy: /v1/chat/completions | /v1/responses | /v1/messages（仅 chat，单能力体系）
```

### 3.2 改动后

```
                    ┌─ 平时对话：既有 chat Session（多模态/文本模型，不受影响）
模型决策 ─ agent_loop ─┤
                    └─ 生图：do_image_gen（第 10 工具）
                              │
                              ▼
                    resolve_image_gen(name)   ← 命名分派，仿 llmcore resolve_session
                              │
                 ┌────────────┴─────────────┐
                 ▼                          ▼
        BaseImageGenClient          (未来协议子类: fal/sdwebui/comfyui)
        └─ OpenAIImageGenClient     ← v1 唯一实现
             ├─ 同步路径(默认): POST {apibase}/images/generations
             │    {prompt, size, quality, n, output_format, response_format?}   ← response_format 仅 dall-e 系列发送，gpt-image 恒 b64_json 不需要该参数（二轮审查 I-3）
             │    → {data:[{b64_json}]} → bytes
             └─ 流式路径(cfg stream=true, 仅 gpt-image 系列):
                  stream:true + partial_images:0-3 → SSE → 取最终帧
                              │
                              ▼
                    落盘 outputs/<name>.png
                              │
                              ▼
                   返回 [FILE:outputs/<name>]
                              │
                              ▼
         Phase A 既有链路: spool → delivery MIME 分发 → 渠道 send_media
```

**双形态设计（定稿）**：`ImageGenClient` 只认 `apibase/apikey/model` 配置——**直连/托管是配置差异而非两套实现**（对齐 `BaseSession` 先例：chat 直连与平台托管共用一份代码）：
- **直连形态（v1 实施）**：apibase = 真实上游（api.openai.com 或中转网关），apikey = 真实密钥。适用本地/自用/loopback 开发。
- **托管形态（终态，v1 不实施）**：apibase = llm-proxy，apikey = `llm.image` capability token。适用生产 IM 沙箱（密钥不进沙箱是安全硬原则）。GA 客户端协议代码零改动，仅配置下发方式不同；**但 worker 侧需补 marker 交付兜底**（见 §9.6 I-2：export_docx 有 generated_output_files 登记，image_gen 终态实施时需同款登记，否则模型最终回复忘回显 marker 则交付静默丢失）

### 3.3 配置模型（mykey.py，与 chat 完全独立）

```python
image_gen = {
    'name': 'openai',            # resolve_image_gen 分派关键字（v1: openai/oai）
    'apibase': 'https://api.openai.com/v1',   # 直连形态：真实上游/中转网关；托管形态（终态）：llm-proxy 地址 + 能力令牌，GA 侧零改动
    'apikey': 'sk-...',
    'model': 'gpt-image-1',      # 独立生图模型，与对话模型无关
    'stream': False,             # 可选：gpt-image 系列 SSE 渐进细化（见 §6.5 降级语义）
    # 可选: timeout / read_timeout / max_retries / proxy / verify
}

**未配置行为（审查定稿）**：mykey 无 `image_gen` 键时 `resolve_image_gen` 抛错，`do_image_gen` **捕获并返回错误文本**（`[Error: image_gen 未配置 ...]`）给模型，绝不抛异常穿透 dispatch（agent_loop.py:238-246 只捕 StopIteration），也不返回 marker。
```

### 3.4 工具 schema（tools_schema.json 条目要点）

```json
{ "type": "function",   ← 审查修正：必须含 OpenAI function 包装层（对齐 assets/tools_schema.json 真实条目格式）
  "function": {
  "name": "image_gen",
  "description": "Generate images via a dedicated image model (OpenAI images/generations compatible). "
                 "Returns [FILE:outputs/<name>] for delivery. Use for image creation requests.",
  "parameters": { "type": "object",
    "properties": {
      "prompt": {"type":"string","required":true},
      "size": {"type":"string","enum":["1024x1024","1536x1024","1024x1536","auto"]},
      "quality": {"type":"string","enum":["low","medium","high","auto"]},
      "n": {"type":"integer","minimum":1,"maximum":4},
      "output_format": {"type":"string","enum":["png","jpeg","webp"]},
      "model": {"type":"string","description":"optional override"}}}} }
```
注意：size/quality 合法集依上游模型而定（dall-e-3 无 1536x1024、无 output_format）——v1 目标 gpt-image 系列，描述中注明。

## 4. 官方文档参考来源

| 来源 | 关键事实 | 对设计的影响 |
|---|---|---|
| OpenAI Image API（developers.openai.com/api/reference/resources/images/methods/generate） | `POST /images/generations`：prompt ≤32000 字符；gpt-image 系列恒返回 b64_json（response_format 仅 dall-e）；size/quality/output_format/output_compression/background/moderation 参数；n 1–10；usage token 计费 | v1 客户端按此协议实现；**恒 b64_json**（不依赖 url 过期语义） |
| OpenAI Image generation guide（developers.openai.com/api/docs/guides/image-generation） | 两种接入：Image API（直接选 GPT Image 模型）/ Responses API `image_generation` tool（主模型代选）；gpt-image-2 任意分辨率（16px 倍数、≤3840px、3:1） | **选 Image API**：GA 对话模型是任意第三方，不能假设支持 Responses tool；Image API 与模型解耦 |
| OpenAI Responses `image_generation` tool（api/docs/guides/tools-image-generation + api/reference/resources/responses） | 官方"工具化"范式：主模型调用 image_generation 工具 → 工具内部用 GPT Image 模型产图 → 结果 b64 返回调用方；支持流式 partial images | 官方背书"工具化 = 模型决策 → 独立生图模型 → b64 交付"，与 B1 决策同构 |
| Azure OpenAI 图像生成（microsoftdocs/azure-ai-docs dall-e.md） | `stream: true` + `partial_images: 0–3` 流式渐进出图（gpt-image-1/2 系列） | 流式路径按此参数契约实现（B5） |

## 5. 主流方案参考来源

| 方案 | 参考 | 做法 | 与本设计对比 |
|---|---|---|---|
| Hermes Agent（Nous Research） | hermes-agent.nousresearch.com/docs/.../image-generation | `image_gen: {model: fal-ai/flux-2/klein/9b, use_gateway}`——**独立生图模型配置，与对话模型分离**；用户直接文字请求 | 配置模型与 B3 一致；差异：Hermes 生图结果直接回对话流，GA 走 `[FILE:]` 出站链（IM 场景需要文件交付） |
| OpenAI Agents SDK / Responses tool | 官方文档（见 §4） | 工具化：模型调 image_generation 工具 | 同构（B1 工具化） |
| LibreChat | librechat.ai/docs/features/image_gen | "OpenAI Image Tools" = image_gen + image_edit 两个独立工具 | 印证"生图 = 独立工具集"，且编辑也是工具（本项目 v1 不做 edit，留扩展） |
| Claude Code / Codex / OpenAI Agents SDK（既有设计稿引用） | IM_MEDIA_ARCHITECTURE §6.1 | 均工具化：生图工具 → API → 文件交付 | 设计稿定稿时的业界调研，本次细化落地 |
| ComfyUI / SD WebUI | 社区事实 | 任务提交 + 轮询进度（非 SSE） | 协议差异关在子类（未来协议扩展点），v1 不做 |
| 中转网关（new-api #4478 / llmgateway #2102） | GitHub issue | images SSE 流式转发 / 强制 upstream SSE 折叠回普通 JSON | 佐证 OpenAI 兼容协议是中转事实标准；`stream` 由客户端可选开启，兼容网关兜底 |

## 6. 是不是项目最佳设计（论证）

### 6.1 备选方案对比

| 方案 | 结论 | 理由 |
|---|---|---|
| **A. 工具化 + 独立 ImageGenClient（本方案）** | ✅ **采纳** | 业界主流（OpenAI 官方 tool / Hermes / LibreChat）；与 GA"9 原子工具 + loop"哲学同构，核心循环零侵入；独立 apibase/key/model 天然满足"平时对话模型、生图用生图模型"；OpenAI 兼容协议覆盖中转/网关 90%+ 场景 |
| B. chat 通道原生图像输出（gemini-flash-image 类） | ❌ 否决 | 仅极少数模型支持；协议不统一；llmcore 需大改（解析图像 content block 输出）；与 GA 任意对话模型的架构冲突；把"生图能力"绑定在对话模型选择上 |
| C. code_run 手写生图脚本 | ❌ 否决（保留兜底） | 模型自己写 requests 调 API 可行（这就是"别人用 GA 生图"的现状），但不可审计、不稳定、无工具 schema 约束、失败语义不可控；`image_gen` 工具落地后仍可 code_run 兜底 |
| D. Responses API `image_generation` tool | ❌ 否决 | 仅 OpenAI 主模型（gpt-5.x 系）可用；GA 对话模型为任意第三方，不可假设；且等于把生图绑死 OpenAI 全家桶 |

### 6.2 对照 IM_MEDIA_ARCHITECTURE §8 设计原则

| 原则 | 符合性 |
|---|---|
| 1. 方向对称：不造新通道 | ✅ 生图产物 = 出站媒体，走 Phase A `[FILE:]` → spool → send_media 既有链 |
| 2. 渠道/协议差异收敛为薄适配 | ✅ 协议差异关在 `ImageGenClient` 子类（`resolve_image_gen` 分派，仿 llmcore `resolve_session` 已验证模式） |
| 3. 消息模式为主、文件模式并存 | ✅ 生图 → IM 消息收图为主路径；`outputs/` 文件供工具/存档 |
| 4. 契约先行 | ✅ 工具 schema = GA 侧契约；平台侧 capability `llm.image` = 能力契约（托管形态实施时同步安全审查） |
| 5. 成本控制随架构走 | ✅ size/quality 由模型按需选（工具参数可选）；n ≤ 4 限额；gpt-image 有 usage token 计量（客户端可记录日志） |
| 6. 失败诚实 | ✅ 生图 API 错误 → 工具返回明确错误文本给模型，不静默"任务完成"（与空结果防护同原则） |

### 6.3 与既有架构的契合点（非新增发明）

- **复用 llmcore Session 抽象模式**：BaseSession 配置解析（apibase/apikey/model/超时/重试/代理/verify）→ `BaseImageGenClient` 照抄；`resolve_session` 命名分派 → `resolve_image_gen`；协议子类隔离 → 子类扩展。**同一套已被 chat 多协议验证的模式**，不发明新架构。
- **不做 MixinSession 类比**：生图是"角色分离"（生图时用生图模型）不是"同能力故障转移"，v1 单 client 即可，mixin 留未来扩展。
- **流式（B5）仅客户端内部路径**：`stream:true` + `partial_images` SSE 对工具层透明（工具永远拿最终帧落盘），不推翻基类抽象；DALL-E/中转不支持时走同步路径自动降级。

### 6.4 已知取舍（诚实声明）

- 上游兼容性：v1 只保证 OpenAI `images/generations` 兼容协议；SD WebUI/ComfyUI（`/sdapi/v1/txt2img` 等）需后续子类适配。
- 流式不稳定：社区有 gpt-image-1 partial streaming 被移除/波动报告——流式是"可选优化"，同步路径恒可用。
- 编辑（image edit / 图生图）：v1 不做（LibreChat 也是独立 image_edit 工具），留扩展。
- 平台侧 `llm.image` capability 扩展需安全审查（JTI 预算、响应上限、白名单），与 chat 同流程。

### 6.5 失败语义（审查定稿，对齐 §8 原则 6 失败诚实）

> 错误文本**统一 `[Error: image_gen ...]` 前缀**（对齐 do_code_run 工具层先例 ga.py:552）；**不得使用 `!!!Error:` 前缀**——该前缀出现在模型最终回复尾部会触发 do_no_tool 致命判定/LLM_FAILED 链（ga.py:727-728，二轮审查 I-4）。

| 场景 | 行为 |
|---|---|
| API 4xx/5xx / 连接错误 / 超时 | 重试（**仿写** `_stream_with_retry` 的重试语义：429/408/5xx 退避集合与 retry-after 上限，llmcore.py:447-487；生图返回体是 JSON 非文本流，不直接复用该函数）→ 仍失败返回 `[Error: image_gen ...]` 错误文本给模型 |
| 空响应（`data` 空数组/缺失） | 错误文本，**绝不返回 marker**（防止模型向用户谎报成功） |
| 流式中断（SSE 无最终帧） | 错误文本；`stream=true` 时自动重试一次同步路径（降级触发条件：SSE 解析失败/超时/收到非流式 JSON） |
| b64 解码失败 / 落盘失败 | 错误文本（仿 do_file_write 模式 ga.py:615-647） |
| 产物 >20MiB | 工具侧落盘前检查字节数，超限返回错误文本——Go 交付上限 20MiB 是 fail-closed 任务失败（delivery_capture.go:37-38,60-63），不能等到提交时刻才爆 |
| n>1 多图 | 命名 `outputs/<base>_<i>.<ext>`（i=1..n）；返回多行 `[FILE:...]` marker（每行一个） |
| 未配置 image_gen | 见 §3.3：错误文本 |

## 7. 实施计划（3 步，各自独立可交付）

| Step | 内容 | 交付物 | 验证 | 工作量 |
|---|---|---|---|---|
| **1. 定案** | **D1 已拍板（2026-08-14：直连先行，托管为终态设计）**；二轮审查通过 → 回写 IM_MEDIA_ARCHITECTURE **§6 + §9/§10/§11**（B1–B5 勾选、§9 生图行、§10 Phase B 行、§11 残余风险行）→ SUBTASKS T8 拆分 | 文档更新 + SUBTASKS.csv | 二轮文档评审 | 0.5 天 |
| **2. GA 侧 MVP** | `do_image_gen` + `BaseImageGenClient`/`OpenAIImageGenClient`（同步 + 流式路径）+ `resolve_image_gen` + schema ×2 + mykey 模板 + 单测（含未配置/空响应/超限/流式中断路径） | 代码 + `test_image_gen.py` | `python -m pytest tests -q` + **本地 CLI 闭环**（配置真实/中转 key → 让 GA 生图 → `outputs/` 出图） | 1 天 |
| **3. 平台集成（终态设计，v1 不实施）** | llm-proxy `/v1/images/generations` 路由 + `llm.image` capability + **provider 能力类型维度（chat/image）** + 生图 provider 排除 chat mixin + runtime_config 下发 image_gen 块 + policy 放行 + openapi/web 同步 | 设计已定稿（§3.2/§8）；Go 代码实施时补 | 实施时：`go vet/build/test ./...` + 契约绑定测试 | 2–3 天（延后，有真实需求时实施） |

残余风险：IM 端到端收图冒烟需真实渠道凭据（QQ/飞书已配置）；**Step 2 上生产需重建 ga-runner**（内嵌 ga.py/llmcore.py/tools_schema.json，runtime_overlay.py:16-29），Step 3 需平台部署（make build 全量重建）；流式路径（`stream:true` + `partial_images` SSE）以 OpenAI/Azure 文档为据，社区有稳定性波动报告——同步路径恒可用，流式标"待真实上游实测"（2026-08-14 实测保持 stream=False 走同步）；**gpt-image 参数兼容性已实测（2026-08-14 new-api 中转）**：size 必传（客户端已默认 1024x1024）、gpt-image-2/gemini-*-image 返回 b64_json、**sensenova/agnes 只回 url 直链（客户端已加直下兜底）**、sensenova size 合法集特殊（错误文本引导自愈）、n>1 未实测（保持 n≤4 透传+错误诚实）；**marker 回显依赖**（工具返回 marker ≠ 交付发生，模型须在最终回复回显 marker 才触发交付；根项目无兜底，托管兜底终态补 generated_output_files 登记仿 do_export_docx legacy_instrument.py:233）；**直连形态无 JTI 计量**——成本靠 n≤4 + 用户自觉（gpt-image token 计费，usage 日志留作后续计量基础，二轮审查盲区 4）。

## 8. 决策记录（B1–B5）

- [ ] B1：生图 = GA 工具 `image_gen`（schema §3.4），核心循环零侵入
- [ ] B2：llm-proxy 扩展 `POST /v1/images/generations` 代理（Step 3，capability `llm.image`）
- [ ] B3：`ImageGenClient` 多协议骨架，OpenAI 兼容协议第一实现；独立 apibase/apikey/model 配置
- [ ] B4：产物落 `outputs/` + `[FILE:]` 走既有出站链路
- [ ] B5：流式 = 客户端内部可选路径（`stream:true` + `partial_images` SSE，取最终帧），工具层透明；同步路径恒可用
- [x] **D1（2026-08-14 用户拍板：直连先行，托管为终态设计）**：v1 实施直连形态（ImageGenClient 双形态设计——一份代码，配置决定形态，§3.2）；托管形态（llm-proxy 路由 + `llm.image` capability + **provider 能力类型维度（chat/image）** + 排除 chat mixin + runtime_config 下发 image_gen 块 + policy 放行 + openapi/web 同步）作为终态设计定稿，**有真实需求时实施**（与 T8 "可推迟"定位一致）。

---

## 9. 独立审查结论（2026-08-14，只读审查回写）

> 审查方式：fresh-context 只读核验（读方案 + IM_MEDIA_ARCHITECTURE + llmcore.py/ga.py/agent_loop.py/agentmain.py/tools_schema/mykey_template + llmproxy 全部相关 Go 文件 + worker-python + policy 清单 + CI）。**首轮结论：需修改后批准**（2026-08-14）；用户拍板后定稿（§9.5）。阻断项已回写正文；本节留存证据。

### 9.1 阻断项（已定稿处理）

| # | 问题 | 证据 | 处理 |
|---|---|---|---|
| B-1 | **平台模式工具白名单未列**：平台工具策略 deny-by-default，`foundation.session-files.v1` allowed_tools 无 image_gen → worker `apply_tool_policy` 过滤后模型永远看不到该工具 | `contracts/policy/foundation.v1.json`；`legacy_instrument.py:93-120`；`policy/registry.go:82` | **已定稿处理（D1-b）**：v1 平台模式**有意不启用生图**（policy 保持现状，无死工具暴露）；托管形态实施时随 policy 放行（§2 已标注） |
| B-2 | **Step 3 托管机制未闭环**：capability 绑定 provider_id+model+revision，`loadBoundProvider` 强制 model 匹配（handler.go:198-201）→ 生图请求 model（gpt-image-1）与 chat provider model 必不匹配；平台 provider 模型无双类型外的"image 能力"概念（单 model 字段），也无 image-gen provider 识别/签发/下发机制 | `worker_credential.go:54-59`；`handler.go:118-127,198-201`；`domain/llm_provider.go:20-25`；`LLM_PROVIDER_ARCHITECTURE.md`；`runtime_config.go:93-153` | **已定稿处理（D1-b）**：v1 直连先行；托管机制（provider 能力类型维度等）作为终态设计定稿写入 §3.2/§7 Step 3/§8 D1，有真实需求时实施 |

### 9.2 重要项（已回写正文，此处留档）

1. 未配置 `image_gen` 时 do_image_gen 行为 → 已定稿 §3.3（错误文本，不抛异常：agent_loop.py:238-246 只捕 StopIteration）。
2. 失败语义明细（空响应/流式中断/超限/多图命名）→ 已定稿 §6.5（对齐失败诚实原则；产物 >20MiB 是 Go 侧 fail-closed 任务失败，工具侧必须前置检查）。
3. §3.4 schema 示例缺 OpenAI `function` 包装层（真实格式 `{"type":"function","function":{...}}`）→ 已修正。
4. 流式降级触发条件未定义 → §6.5 定稿（SSE 解析失败/超时/非流式 JSON → 重试一次同步路径）。
5. 文档同步范围：原方案只写回写 §6，§9/§10/§11 未列 → §7 Step 1 已扩。
6. 部署面：ga-runner 内嵌 ga.py/llmcore.py/tools_schema.json（runtime_overlay.py:16-29），Step 2 上生产也必须重建 ga-runner → §7 残余风险已补。
7. 平台自动写 mixin_config：>1 provider 时 runtime_config.go:146-148 自动写 chat mixin——未来 image provider 必须排除，终态设计已含（§7 Step 3）。
8. tools.md/memory.md 协作文件更新未列 → §2 GA 表已补。

### 9.3 建议项（已回写正文）

- BaseImageGenClient 只解析配置子集（apibase/apikey/model/proxy/verify/timeout/read_timeout/max_retries），HTTP 层复用/仿写 `_stream_with_retry`（llmcore.py:447-487，错误前缀 `!!!Error:` 与 chat 对齐）。
- llm-proxy 响应体无上限（`MaxWorkerRequestBytes` 仅限请求体 handler.go:19-20；`DisableCompression: true` transport.go:117）→ Step 3 安全审查补响应体大小上限。
- target.go 措辞：`nativeEndpoint` 是路径映射非"egress 白名单"（egress 白名单为 host/CIDR 级，network_policy.go）。
- mykey 模板命名提醒（`image_gen` 变量名不得含 config/api/cookie 子串，agentmain.py:72 会话扫描）。
- size/quality 合法集依上游模型（dall-e-3 无 1536x1024/output_format），schema 描述注明。
- 流式路径标"待真实上游实测"（社区稳定性报告，同步路径恒可用）。

### 9.4 核验为正确的方案声明（可采信）

- `[FILE:outputs/<name>]` 交付链三端真实存在且语义一致：Go `captureTaskDeliverableFiles`（delivery_capture.go:55+）→ `RecordOutbound` 强制 `outputs/` 前缀（session_files.go:368-370，image ≤20MiB/聚合 256MiB）→ 根前端按 `<repo>/temp` 解析（wechatapp.py:132）；容器内 `/ga/legacy/temp` 符号链接到工作区 temp（runtime_overlay.py:271+），与 GA handler cwd（agentmain.py:194）同目录。前提：do_image_gen 写 `self.cwd/outputs/` + `os.makedirs(exist_ok=True)`。
- "9 原子工具/第 10 个"口径准确（tools_schema.json 恰 9 条，与 AGENTS.md/tools.md/memory.md 一致）。
- agent_loop.py 零侵入声明成立（dispatch 泛化 agent_loop.py:18-31）。
- Operation 改动影响面：`llm.chat` 仅 3 处 Go 内（worker_credential.go:59 / handler.go:127 / token.go 注释），worker-python 零引用——方案"影响面扫描"预期结论正确。
- 新增 `image_gen` mykey 键不会被 agentmain 误当 Session 实例化（变量名不含 api/config/cookie）。
- 测试方案（mock API、无真实密钥）与 CI 门禁兼容（ci.yml:103 `python -m pytest tests -q`）；无新依赖（requests 已内建，ga-runner 无需补包）。

### 9.5 定稿记录（2026-08-14 用户拍板）

- **D1 定稿：直连形态先行，托管为终态设计**（v1 实施 §7 Step 2；§7 Step 3 转为"终态设计，有真实需求时实施"）。
- 双形态设计原则写入 §3.2：`ImageGenClient` 只认 apibase/apikey/model 配置，直连/托管是配置差异（对齐 BaseSession 先例），GA 侧永远一份工具代码。
- 平台模式 v1 **有意不可用生图**：policy 保持 deny-by-default（不补 image_gen），沙箱模型看不到该工具，无死工具暴露；托管形态实施时随 policy 放行。
- 首轮阻断项 B-1/B-2 状态：已定稿处理（见 §9.1 处理列）。
- 定稿后交付二轮 fresh-context 审查：提示词见 `PHASE_B_IMAGE_GEN_PLAN.REVIEW2_PROMPT.zh-CN.md`。

### 9.6 二轮审查结论（2026-08-14，fresh-context reviewer 独立执行，**无阻断项**）

**结论：需修改后批准**。D1 定稿专项核验：双形态切换不引入结构性问题（= 配置差异，BaseSession 先例实证）；平台模式 v1 有意不可用声明**真实成立**（四层防护：policy 过滤 legacy_instrument.py:107-120 + dispatch_guard :544 + runtime 无 image_gen 块 + 无 provider 绑定）；policy 保持现状无隐藏副作用（contract 测试不含工具清单计数，test_contract_sources.py:38-54）。4 项重要问题已回写正文：

| # | 问题 | 证据 | 处理 |
|---|---|---|---|
| I-1 | `runtime_overlay.py` 未列波及：沙箱 overlay 只物化固定清单（LEGACY_MODULES runtime_overlay.py:14-20），若新增 imagegen.py 且 ga.py 模块级 import → 沙箱启动 ImportError 全平台任务失败 | runtime_overlay.py:14-39 | 已回写 §2：实现进 llmcore.py（推荐），或新增文件时同步 LEGACY_MODULES/OVERLAY_MANIFEST_ENTRIES + worker 测试 fixture |
| I-2 | 托管形态"GA 侧零改动"声明不完整：marker 交付兜底不对称——export_docx 有 generated_output_files 登记（legacy_instrument.py:233 → task_terminal.py:151,164 append_missing_file_markers），image_gen 无；模型最终回复忘回显 marker 则交付静默丢失 | legacy_instrument.py:233；task_terminal.py:151-164 | 已回写 §3.2/§7：终态实施时补登记（仿 do_export_docx）；"零改动"收窄为"客户端协议代码零改动" |
| I-3 | §3.2 同步路径恒发 `response_format:b64_json` 与 §4"response_format 仅 dall-e"矛盾——若上游 400 则默认同步路径不可用（待验证） | 方案 §3.2 vs §4 | 已回写：response_format 按 model 条件发送（dall-e 才发）；gpt-image 的 n/response_format 兼容性列入 §7 残余风险待实测 |
| I-4 | 错误文本 `!!!Error:`/`[Error:` 混用：`!!!Error:` 在模型最终回复尾部触发 do_no_tool 致命判定/LLM_FAILED（ga.py:727-728），工具层先例是 `[Error]`（do_code_run ga.py:552） | ga.py:727-728 | 已回写 §6.5：统一 `[Error: image_gen ...]`；"复用 _stream_with_retry"改"仿写其重试语义" |

建议项（已记录，实施时处理）：行号引用修正（transport.go:117、runtime_config.go:146-148——已改正文）；health-cleanup D-C 黑盒豁免说明（改 ga.py/tools_schema 属本项目设计明确需要，提交 PR 时说明）；PROGRESS.md 纳入 Step 1 交付物；输出文件名唯一性（`<base>_<ts>_<i>`）；tui_v3 `_XML_TOOL_RE` 白名单补 image_gen（tui_v3.py:1676-1687，历史回放折叠用）；tools_schema_cn.json 双文件漂移说明（model_cmd.py:66 会加载 cn 版）；mykey_template 头部速查表补 image_gen（非 Session、不进 mixin、不进 /llms）；直连形态成本失控取舍补入 §6.4。
