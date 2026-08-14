# 任务：实施 GenericAgent Phase B 生图能力（image_gen）——Step 1 定案 + Step 2 GA 侧 MVP

你是实施者（fresh context）。项目根目录：/home/lou/code/GenericAgent。先读 `AGENTS.md`、`tools.md` 了解项目硬规则与验证命令。

## 背景与真值

- 方案**已定稿**（两轮独立审查通过，无未决阻断项）：`.tasks/im-media-pipeline/PHASE_B_IMAGE_GEN_PLAN.zh-CN.md`（**必读全文**，§1-§9；§6.5 失败语义、§8 决策记录、§9.5/§9.6 审查结论是硬要求）
- 设计真值：`tenant_platform/docs/IM_MEDIA_ARCHITECTURE.zh-CN.md`（§5 出站链路、§6 生图 Phase B、§8 设计原则）
- 决策 D1（用户已拍板）：**直连形态先行（v1 实施），托管为终态设计（v1 不实施）**。ImageGenClient 双形态设计（一份代码，配置决定形态），但平台侧（llm-proxy/policy/provider）**一律不动**
- 任务真值：`.tasks/im-media-pipeline/SUBTASKS.csv` T8（blocked，本次拆分）

## Step 1：文档与任务回写（0.5 天）

1. 回写 `tenant_platform/docs/IM_MEDIA_ARCHITECTURE.zh-CN.md`：
   - §6：B1–B5 勾选 + 细化（§6.2 决策列表逐条标注已定稿，含 D1 直连先行、双形态设计、§6.5 失败语义摘要）
   - §9 状态表"生图"行：❌ → ✅（GA 侧直连形态 v1）
   - §10 分期表 Phase B 行：更新为"直连形态已实施；托管形态按需（终态设计）"
   - §11 残余风险：补生图相关（gpt-image 参数兼容性待实测、marker 回显依赖、直连无 JTI 计量）
2. `SUBTASKS.csv`：T8 拆分（如 T8.1 文档回写 / T8.2 GA 客户端 / T8.3 工具+schema / T8.4 单测+本地闭环），状态从 blocked 改为进行中/拆分子项
3. `PROGRESS.md`：补 Phase B 段（T8 定位更新：v1 直连先行实施 + 托管按需，替代原"有意推迟 D6"表述）

## Step 2：GA 侧代码（1 天）

### 文件清单（就这些，不多不少）

| 文件 | 改动 |
|---|---|
| `llmcore.py` | `BaseImageGenClient` + `OpenAIImageGenClient` + `resolve_image_gen(name)`。**实现进 llmcore.py，严禁新增 imagegen.py**——沙箱 overlay 只物化固定清单（runtime_overlay.py:14-20 LEGACY_MODULES），新增文件导致平台沙箱启动 ImportError 全平台任务失败（二轮审查 I-1） |
| `ga.py` | `do_image_gen`（第 10 个原子工具） |
| `assets/tools_schema.json` + `tools_schema_cn.json` | `image_gen` 条目（**OpenAI function 包装格式**：`{"type":"function","function":{...}}`，见方案 §3.4 修正版；cn 版描述用中文） |
| `mykey_template.py` | `image_gen` 独立配置块 + **头部"Session 类型速查"表补条目**（注明：非 Session、不进 /llms、不进 mixin） |
| `tests/test_image_gen.py` | mock 单测（见下） |

### 实施要求（逐条，来自定稿 §3.3/§6.5 与两轮审查）

1. **配置解析**：`BaseImageGenClient` 只解析子集 {apibase/apikey/model/stream/timeout/read_timeout/max_retries/proxy/verify}，不照抄 BaseSession 的 chat 专属字段（context_win/thinking 等）
2. **分派**：`resolve_image_gen(name)` 读 `mykeys['image_gen']`（经 `reload_mykeys()`）；未配置抛 ValueError，**由 do_image_gen 捕获并返回错误文本**（`[Error: image_gen 未配置 ...]`），绝不裸抛穿透 dispatch（agent_loop.py:238-246 只捕 StopIteration）
3. **同步路径**：POST `{apibase}/images/generations`，body {prompt, size, quality, n, output_format}；**`response_format` 仅当 model 为 dall-e 系列时发送**——gpt-image 恒返回 b64_json，发该参数可能 400（二轮审查 I-3）
4. **流式路径**（cfg stream=true）：`stream:true` + `partial_images:0-3` → SSE 解析 → 取最终帧落盘；**SSE 解析失败/超时/收到非流式 JSON → 自动重试一次同步路径**
5. **重试**：**仿写** `_stream_with_retry` 的重试语义（429/408/5xx 退避集合 + retry-after 上限，llmcore.py:447-487）；生图返回体是 JSON 非文本流，**不直接复用**该函数
6. **失败语义（§6.5，全部）**：错误一律返回 `[Error: image_gen ...]` 文本给模型；**绝不用 `!!!Error:` 前缀**——模型最终回复尾部含它会触发 do_no_tool 致命判定/LLM_FAILED（ga.py:727-728，二轮审查 I-4）；**空响应（data 空数组）绝不返回 marker**；b64 解码/落盘失败 → 错误文本（仿 do_file_write ga.py:615-647）
7. **落盘**：写 `self.cwd/outputs/<name>`（**必须用 `self.cwd` 相对路径，不能硬编码 script_dir**）；先 `os.makedirs(..., exist_ok=True)`（根项目模式无人预置 outputs/）；**落盘前检查字节数 ≤20MiB**，超限返回错误文本（Go 交付上限 20MiB 是 fail-closed 任务失败，delivery_capture.go:37-38）
8. **n>1 多图**：命名 `outputs/<base>_<i>.<ext>`（i=1..n），返回多行 `[FILE:]` marker；建议文件名带时间戳/随机后缀（`<base>_<ts>_<i>`）防同任务重复调用同名覆盖（二轮审查盲区 3）
9. **schema 描述**：告知模型"**在最终回复中包含 `[FILE:outputs/<name>]` 才触发交付**"（marker 回显依赖——工具返回 marker ≠ 交付发生，二轮审查 I-2）；注明 size/quality 合法集依上游模型而定（dall-e-3 无 1536x1024、无 output_format）
10. **工具形态**：仿 do_file_write/do_code_run 模式（yield `[Action]` 行 + 返回 `StepOutcome(data, next_prompt)`）；配置缺失类错误文本中明确"不要重试"（避免模型空转 3 次才 LLM_FAILED）
11. **mykey 模板**：`image_gen` 变量名**不得含 config/api/cookie 子串**（agentmain.py:72 会话扫描会误判为 Session 并 WARN）；注释给出两种形态配置示例（直连=真实上游+密钥 / 托管=llm-proxy+能力令牌，仅文档说明不实施）
12. **单测** `tests/test_image_gen.py`：mock API（monkeypatch，**无真实密钥**，CI 安全约束）；覆盖：请求形状（含 response_format 按 model 裁剪断言）、b64 落盘路径、FILE marker 格式、API 4xx/5xx、空响应、流式中断、超限 >20MiB、未配置 image_gen、n=2 命名与多 marker

## 明确不做（范围外，动一处即越界）

- `agent_loop.py`：零改动
- Phase A 交付链（delivery spool/send_media）、`worker-python`、`backend-go`、`contracts/`（openapi/proto/policy）：零改动
- **`contracts/policy/foundation.v1.json` 不改**——平台模式 v1 有意不可用生图（保持 deny-by-default，无死工具暴露；首轮 B-1 已定稿处理）
- llm-proxy 路由 / `llm.image` capability / provider 能力类型维度：终态设计，不实施
- image edit / 图生图：不做

## 验证

1. `python -m pytest tests -q`（CI 门禁 ci.yml:103 同命令，必须全绿）
2. **本地 CLI 闭环**（关键验收）：配置真实/中转 key（写进本地 mykey.py，**绝不提交**）→ `ga` CLI 发起生图请求 → 模型调 image_gen → `temp/outputs/` 出图；顺带验证错误路径（未配置时模型收到错误文本）
3. 不改 Go 侧，无需 go test

## 协作文件（落地闭环后）

- `memory.md`：能力清单"9 原子工具"→"10 原子工具"、进行中段更新
- `tools.md`：IM 渠道能力段补 image_gen 坑点（错误前缀 `[Error:`、response_format 按 model、overlay 清单不许新增模块、`self.cwd` 落盘）
- `memory/archive/2026-08.md`：追加（格式：`## [日期 | 标题]` + Events/Changes/Insights）
- 提交 PR 时注明 health-cleanup D-C 黑盒豁免依据：改 ga.py/tools_schema.json 属本项目设计明确需要（二轮审查建议项）

## 实施顺序

Step 1（文档/SUBTASKS）→ Step 2 代码（llmcore → ga.py → schema → mykey 模板）→ 单测 → 本地 CLI 闭环 → 协作文件 → 汇总。

## 可顺手处理的小项（非必须，低风险）

- `frontends/tui_v3.py:1676-1687` `_XML_TOOL_RE` 工具名白名单补 `image_gen`（历史回放文本折叠用；缺它只影响旧日志回放展示）
- 行号引用提醒（方案正文已修正，实施时勿再引用旧值）：`transport.go:117`（DisableCompression）、`runtime_config.go:146-148`（mixin_config 自动写）
