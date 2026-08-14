审查证据收集完毕。以下为二轮独立审查报告。

---

# GenericAgent Phase B 生图实施方案（定稿版）二轮独立审查报告

审查方式：fresh-context 只读核验（未修改任何文件）。对照真实代码核验了 llmcore.py、ga.py、agent_loop.py、agentmain.py、assets/tools_schema{,_cn}.json、mykey_template.py、frontends/model_cmd.py、tui_v3.py、wechatapp.py、llmproxy 全部相关 Go 文件、application 层 4 文件、worker-python 5 文件、policy 清单、contract 测试、ci.yml、pyproject.toml、.gitignore、health-cleanup EPIC、SUBTASKS.csv/PROGRESS.md、memory.md/tools.md。

---

## 1. 结论总览

**结论：需修改后批准。**

一句话理由：双形态设计（D1）自洽、首轮阻断项 B-1/B-2 定稿处理真实有效、交付链三端与"9 工具口径"全部核验通过，无阻断项；但存在 4 项重要问题需在定稿中修订——①`runtime_overlay.py` 未列入波及范围（若按方案可选路径新增 `imagegen.py`，平台沙箱 import 即炸）；②托管形态"GA 侧零改动"声明不完整（worker 侧 marker 兜底不对称）；③§3.2 同步默认路径发送 `response_format:b64_json` 与 §4"response_format 仅 dall-e"自相矛盾（待验证）；④错误文本格式 `!!!Error:`/`[Error:` 混用存在语义风险。

---

## 2. 按五项审查重点逐项

### 2.1 整体架构（含定稿后的双形态设计）——基本成立，3 重要

**核验为正确的声明（证据）**
- "BaseSession 直连/托管共用一份代码"先例真实：`runtime_config.go` `buildRuntimeProviderConfig` 构造 apibase=proxy + apikey=capability token（runtime_config.go:100-118），与直连 mykey 同形；`LLM_PROVIDER_ARCHITECTURE.md:24-25,110-120` 明示"apikey 仅为 capability token，apibase 为 Proxy base URL"。
- `resolve_session` 命名分派先例真实：llmcore.py:1296-1301（`'native' in cfg_name` / `'claude' in cfg_name` / `'oai' in cfg_name` 分派）。方案"仿 resolve_session 命名分派"抽象成立。
- 平台终态耦合点描述准确：provider 单 `Model` 字段（llm_provider.go:191）、`ProviderType` 仅 native_oai/native_claude 两值（llm_provider.go:24-25）、`loadBoundProvider` 强制 model/revision/type 匹配（handler.go:190-201）、`Operation: "llm.chat"` 硬编码（worker_credential.go:59 + handler.go:127 比较点；`llm.chat` 生产代码仅这 2 处 + token.go:62 注释，worker-python 零引用——grep 实证）。首轮 B-2 分析正确，D1 延后托管是正确决策。
- 流式路径（B5）自洽：`stream:true` + `partial_images` SSE 取最终帧，工具层透明；降级触发条件已定义（§6.5：SSE 解析失败/超时/非流式 JSON → 同步重试一次）。

**重要-1：`runtime_overlay.py` 未列入波及范围，与"或新增 imagegen.py"选项构成条件性平台全崩风险**
- 方案 §2 明确写"llmcore.py（**或新增 imagegen.py**）"，但 tenant_platform 波及表未列 `runtime_overlay.py`；"明确不动的"又写"web / worker-python / proto：无契约变更"。
- 事实：沙箱 overlay 只物化固定清单 `LEGACY_MODULES = (agentmain.py, ga.py, llmcore.py, agent_loop.py, simphtml.py)`（runtime_overlay.py:14-20）+ `OVERLAY_MANIFEST_ENTRIES`（runtime_overlay.py:39），清单外文件不拷贝。若实施选 imagegen.py 且 ga.py 模块级 `import imagegen`，则 Step 2 重建 ga-runner 后**沙箱内每个 agent 启动即 ImportError → 全平台任务 AGENT_FAILED**（worker 已实证此失败路径：task_runner.py:93-94）。
- 修改建议：定稿明确二选一——实现进 llmcore.py（零平台改动），或若新增 imagegen.py 必须把 `"imagegen.py"` 补进 runtime_overlay.py LEGACY_MODULES + 波及表 + worker 测试 fixture（test_managed_agent.py:41-44 / test_runtime_overlay.py:28-32 / test_task_identity.py:34-38 均按清单建 fixture）。

**重要-2：托管形态"GA 侧零改动，仅配置下发方式不同"声明不完整（marker 兜底不对称）**
- 事实：平台沙箱的 marker 兜底机制 `session.generated_output_files` → `append_missing_file_markers`（task_terminal.py:151,164）只由 `do_export_docx` 登记（legacy_instrument.py:233-237）；image_gen 是 GA 根工具，托管形态下若模型最终回复忘记回显 `[FILE:outputs/<name>]` marker，则无任何兜底，交付静默丢失（文件在 workspace，用户收不到）。
- 而交付捕获只从**任务最后用户可见轮次**提取 marker（`userVisibleTaskResult` delivery_service.go:940+ → delivery_capture.go:72），工具结果里的 marker 只是"给模型看的提示"，必须靠模型回显到最终回复。
- 修改建议：§3.2 终态描述补充"托管实施时需在 worker legacy_instrument 侧为 image_gen 提供 `generated_output_files` 登记（仿 do_export_docx）或接受纯模型回显依赖并声明残余风险"；"GA 侧零改动"收窄为"客户端协议代码零改动"。

**重要-3：§3.2 同步路径与 §4 自相矛盾——`response_format: b64_json` 是否应出现在 gpt-image 默认请求里（待验证）**
- 方案 §4 自己写明"gpt-image 系列恒返回 b64_json（**response_format 仅 dall-e**）"，但 §3.2 同步默认路径请求体恒含 `response_format:b64_json`。若上游对不支持参数报 400，则**默认同步路径（v1 唯一保证路径）不可用**，"同步路径恒可用"（§6.3/§7）声明不成立。
- 修改建议：定稿写死"response_format 仅当 model 为 dall-e 系列时发送（gpt-image 恒 b64_json 无需该参数）"，并将该参数兼容性列入 §7 残余风险"待真实上游实测"。同步标记：`n` 参数（schema maximum=4）对 gpt-image-1 的支持亦需实测（官方 n 1-10 为 dall-e 语义，gpt-image 可能仅 n=1）——n>1 命名路径 `outputs/<base>_<i>.<ext>` 可能恒不触发，不影响正确性但需在测试注明。

### 2.2 边界（定稿后新出现的边界问题）——平台不可用声明真实，1 建议

**核验为正确的声明（证据）**
- **"平台模式 v1 有意不可用"声明真实成立**：①policy `foundation.session-files.v1` allowed_tools 无 image_gen（foundation.v1.json）；②`apply_tool_policy` 过滤 schema（legacy_instrument.py:107-120），模型看不到工具；③`install_dispatch_guard` 对非白名单工具二次拦截（legacy_instrument.py:544+，即使模型幻觉调用也被拒）；④平台 runtime config 无 image_gen 块（worker_credential.go `issueProviderCapabilitiesWithRuntime` + `BuildRuntimeConfig` 只写 chat provider）。四层防护下"无死工具暴露"成立。前提是 Step 2 上生产重建 ga-runner（§7 残余风险已注明）。
- 未配置 image_gen 行为（§3.3）：do_image_gen 内部捕获返回错误文本是**必须的**——agent_loop.py:239-246 dispatch 消费只捕 StopIteration，裸异常会穿透到 agentmain.run 的 except Exception → error dict（agentmain.py:273-278），语义上不是"崩溃"但会中断任务，方案设计正确。
- 存量兼容：mykey.py 无 image_gen 键 → resolve_image_gen 抛错 → 错误文本（✓）；存量会话历史无 tool_use 引用 image_gen（✓）；存量 provider 配置零改动（✓）；新增 mykey 键名 `image_gen` 不含 api/config/cookie 子串，不会触发 agentmain.py:72 会话扫描（✓ 实证）。

**建议-1：root 直连模式"死工具"体验**：工具 schema 全局常驻后，未配置 image_gen 的存量根项目用户会在 schema 里看到该工具，首次调用收到 `[Error: image_gen 未配置 ...]`。可接受（错误诚实），但方案可注明"模型应停止重试"指令写入工具描述或错误文本（如 do_no_tool 对 `!!!Error:` 的尾部检测会重试 3 次才 LLM_FAILED，配置缺失类错误建议在错误文本中明确"不要重试"）。

### 2.3 逻辑语义——闭环，2 重要

**核验为正确的声明（证据）**
- marker 消费链三端真实：Go `captureTaskDeliverableFiles`（delivery_capture.go:68-73）→ `RecordOutbound` 强制 `outputs/` 前缀（session_files.go:371）+ 文件数 ≤32/聚合 ≤256MiB（delivery_capture.go:30-31）→ spool 复制（delivery_capture.go:82-101）→ delivery MIME 分发（mediaTypeForPath: .png→"image"→20MiB，delivery_service.go:847-856）；根前端 wechatapp.py:4,132-136 按 `<repo>/temp` 解析相对 marker。
- `self.cwd/outputs/` 前提在各运行形态成立：根项目 cwd=`<repo>/temp`（agentmain.py:194）；容器沙箱 overlay `temp` 符号链接 → GA_WORKSPACE_TEMP 工作区（runtime_overlay.py:291-298 + entrypoint.py:33-46 + session_files.py:33-45），与 Go `session_sandbox_root` 同目录；`outputs/` 在容器侧由 `ensure_session_sandbox` 预建（session_files.py:55-58），根项目侧需 do_image_gen 内 `os.makedirs(exist_ok=True)`（方案 §9.4 已注明前提）。
- 20MiB 双保险：工具侧落盘前检查（§6.5）+ Go 侧 fail-closed（delivery_capture.go:60-63 `CopyFileFromBeneath` maxBytes 超限即错误）。
- 多图 marker：Go `fileMarkerRE.FindAllStringSubmatch` 支持多 marker（session_files.go:35,408-426）；wechatapp `re.findall` 同理。

**重要-4：错误文本格式混用 `!!!Error:` 与 `[Error: image_gen ...]`（§6.5）**
- 事实：`!!!Error:` 是 LLM 传输层错误前缀（llmcore.py `_stream_with_retry`），且在 `do_no_tool` 有特殊语义——模型最终回复尾部含 `!!!Error:` 会被判定致命并重试/LLM_FAILED（ga.py do_no_tool `if '!!!Error:' in content[-100:]`）。工具层错误先例是 `[Error] ...`（ga.py do_code_run `"[Error] Code missing..."`）或 `{"status":"error",...}` dict（ga.py do_file_write / legacy_instrument do_export_docx）。若 image_gen 工具结果以 `!!!Error:` 开头且模型在最终回复原样回显（低概率但可能），会误触发 LLM_FAILED 链。
- 修改建议：§6.5 统一为 `[Error: image_gen ...]` 单格式（对齐 do_code_run 工具层语义），明确不采用 `!!!Error:` 前缀；HTTP 层"复用 `_stream_with_retry` 语义"实为仿写（返回体是 JSON 非文本流），措辞改"仿写其重试语义（_RETRYABLE 集合/退避）"。

**重要-5：工具返回 marker 到最终交付之间的模型回显依赖未被方案显式描述**
- 同 2.1 重要-2：交付捕获只认最终回复文本里的 marker。do_image_gen 返回 marker 给模型 ≠ 交付发生。方案 §2/§3.2 表述"返回 `[FILE:outputs/<name>]`"容易让人误以为工具返回即交付。修改建议：§6.5 或 §3.2 补一句"工具仅向模型提示 marker；模型须在最终用户可见回复中包含该 marker 方可触发交付（既有 export_docx 同机制，session_files.go:442 系统提示已含此约定）；根项目前端无兜底，平台托管形态的兜底需 Step 3 补登记（见重要-2）"。

### 2.4 旧源码清理与兼容——基本完整，1 建议

**核验正确**
- §7 Step 1 已把 IM_MEDIA_ARCHITECTURE 回写范围扩为 §6+§9/§10/§11（首轮只列 §6 的缺陷已修复）；SUBTASKS T8 拆分已列。
- code_run 生图兜底保留（§6.1 C 备选"保留兜底"）合理，不清理。
- llmcore 既有入站图片转换（`_image_block_from_file`/`media_content_blocks` agent_loop.py / `_fix_messages`/`_claude_image_block` llmcore.py:731,751）与 ImageGenClient（出站生成）正交，无冲突；方案不动它们 ✓。
- 存量用户/会话/provider 兼容零破坏（见 2.2）。

**建议-2：`tools_schema_cn.json` 的定位与双文件漂移**：该文件并非死资产——`frontends/model_cmd.py:66` 在 GLM/MiniMax/Kimi 模型切换时 `load_tool_schema('_cn')` 会加载它；但 memory/archive/2026-08.md 实证它与 tools_schema.json "9/9 工具描述全不同"（已全量漂移）。方案列它为改动对象合理，但建议：①明确 cn 文件需同步加 image_gen 条目且描述用中文；②顺手把两文件已漂移的描述一致化或至少声明"cn 为遗留本地化副本"；③health-cleanup D-C 曾将 tools_schema_cn.json 移出清理范围（黑盒），本次改动属于"项目设计明确需要"，建议在 §2 注明豁免依据。

**建议-3：`tui_v3.py` 工具名白名单（既有漂移，非本方案引入）**：`_XML_TOOL_RE` 硬编码工具名（tui_v3.py:1676-1687）注释称"Whitelist = every name in assets/tools_schema.json"，实际缺 image_gen 且含 schema 外的 web_search。image_gen 原生 tool_use 是结构化块不受影响，仅影响历史回放日志中 `<image_gen>` 文本包装的折叠。建议实施时顺手补名或注明不回补。

### 2.5 波及范围完整性——1 重要（同重要-1）+ 若干建议

**核验正确**
- CI：`python -m pytest tests -q`（ci.yml:103）自动发现新增 test_image_gen.py；无新依赖（pyproject.toml:11-17 requests 已有）；mock 单测与 CI 无密钥约束兼容（test_no_real_key_leak 约束不冲突）。✓
- `.gitignore`：无需改动（`temp/` 已忽略 .gitignore:1，outputs/ 在其下）。✓
- llm-proxy 请求/响应体上限：`MaxWorkerRequestBytes` 仅限请求体（handler.go:19）；响应无上限 + `DisableCompression: true`（transport.go:117，**方案引用 transport.go:191 有误**）→ Step 3 安全审查项已列 ✓。
- 路由冲突：现有 mux 仅 /healthz + chat 三路径（server.go:40-46），新增 `/v1/images/generations` 无冲突 ✓。
- provider 配置模型：单 model 字段/两 provider_type（llm_provider.go:24-25,191；LLM_PROVIDER_ARCHITECTURE.md:53-58）→ 终态需加能力类型维度，方案已承认（B-2）✓。
- 平台自动写 mixin_config：实际在 runtime_config.go:146-148（**方案引用 151-153 行号偏 5 行**），内容正确 ✓。

**建议-4：行号引用修正**：transport.go:191 → 实际 117；runtime_config.go:151-153 → 实际 146-148。均为"引用漂移"不影响结论，但定稿文档应修正以免后人误导。

**建议-5：PROGRESS.md 未列入 Step 1 交付物**：§7 Step 1 只写 SUBTASKS T8 拆分 + IM_MEDIA_ARCHITECTURE 回写；PROGRESS.md 是 SUBTASKS 的配套说明（当前仍写"T8 有意推迟 D6"），应同步更新（D1 已改变 T8 定位为"v1 直连先行实施 + 托管按需"）。

---

## 3. 与设计真值/项目硬规则的冲突清单

| 冲突 | 证据 | 评估 |
|---|---|---|
| health-cleanup D-C"GA 根项目=第三方黑盒，根目录代码一律不动" | .tasks/health-cleanup/EPIC.md D-C + Non-goals | 方案改 ga.py/llmcore.py/tools_schema.json 属"我们的项目设计明确需要"（B 期生图正是本项目设计目标），可豁免；**但方案未显式声明豁免依据**（建议-2 已提），不加说明易被误读为违反 D-C |
| 安全硬原则"密钥只进 mykey.py/环境变量" | AGENTS.md §6 | 直连 apikey 进用户 mykey.py ✓（用户自有密钥）；托管 capability token 进 runtime config ✓（真实上游密钥永不进沙箱，与 LLM_PROVIDER_ARCHITECTURE.md:12 一致）——无冲突 |
| 失败诚实（IM_MEDIA_ARCHITECTURE §8 原则 6） | §6.5 | 对齐 ✓；仅错误文本格式混用（重要-4）需统一 |
| 跨语言契约单一真值源 | AGENTS.md §6 | v1 无契约变更 ✓（llm.chat 零 worker-python 引用实证）；Step 3 openapi/proto 变更已在终态项中声明 ✓ |
| 后端分层 api→application→domain | AGENTS.md §6 | v1 零 backend-go 改动 ✓；Step 3 的 provider 能力类型维度会动 domain（LLMProvider 单 model 字段），终态设计已承认该代价 ✓ |
| policy deny-by-default（工具静态白名单） | foundation.v1.json | v1 保持现状 = 有意决策（D1），四层防护无死工具暴露 ✓；终态随 policy 放行已列 ✓ |

**结论：无实质性硬规则冲突；唯一需补的是 D-C 豁免说明。**

---

## 4. 方案未覆盖的盲区

1. **gpt-image-1 参数兼容性（n / response_format / size）**：§3.2 默认请求体含 `response_format:b64_json` 与 §4"response_format 仅 dall-e"矛盾——若上游 400 则同步默认路径不可用。**待验证**，需在实施时以真实上游/中转实测确认，或按 model 裁剪参数（见重要-3）。
2. **托管形态长时生图的超时/心跳交互**：llm-proxy `ResponseHeaderTimeout`（transport.go:116，默认 120s）与 worker 心跳窗口（PROGRESS_WINDOW_S=150s，memory.md:67）+ idle reaper 300s——同步模式若生图 >120s 响应头超时，或 SSE 流式中途停顿 >150s 心跳停发。直连形态无此约束；Step 3 实施时需评估（同步模式建议加大 proxy 响应头超时或经流式路径保活）。
3. **输出文件名唯一性**：`outputs/<base>.png` / `<base>_<i>.png` 在同任务多次调用同名 base 时会覆盖前一次产物；Go 侧交付在任务终态 spool 复制（注释已防跨任务覆盖，delivery_capture.go:59-66），但工具侧建议加时间戳/随机后缀（`<base>_<ts>_<i>`）。
4. **直连形态成本失控风险**：无 JTI 预算计量，n≤4 + 用户自觉是唯一约束；方案 §6.4 未列此取舍，建议补充（gpt-image 按 token 计费，usage 日志可作为后续计量基础）。
5. **mykey_template 头部"Session 类型速查"表**：需补 image_gen 条目并注明"非 Session、不进 /llms、不进 mixin"，防用户误配成 mixin 引用名。

---

## 5. 定稿决策（D1）专项核验结论

- **双形态切换**：不引入首轮未覆盖的结构性问题。切换=配置差异（apibase/apikey），BaseSession 先例实证成立；存量用户配置/会话/provider 零迁移成本。**新增风险仅两条**：①runtime_overlay 文件清单（重要-1，条件性）；②托管形态 marker 兜底不对称（重要-2，终态描述不完整）——均为实施期可闭合的清单级问题。
- **平台模式 v1 有意不可用声明**：四层防护实证（policy 过滤 + dispatch_guard + runtime 无 image_gen 块 + 无 provider 绑定），声明**真实成立**，无死工具暴露。
- **policy 保持现状的副作用**：仅"平台用户 v1 无法生图"（D1 显式决策本身）；无隐藏副作用——contract 测试（test_contract_sources.py:38-54 只断言 no-host-tools 策略精确列表与 session-files 含 mcp:*，不含工具清单计数）不受影响，Go policy registry 无改动。

---

## 6. 首轮"待验证/遗留"项复核

| 项 | 复核结果 |
|---|---|
| 交付链三端打通 | ✓ 实证（session_files.go:35,371 / delivery_capture.go:68-101 / wechatapp.py:132-136 / delivery_service.go:847-856） |
| 9 工具口径 | ✓ tools_schema.json 恰 9 条；tools.md:7、memory.md:27 一致 |
| agent_loop 零侵入 | ✓ dispatch 泛化（agent_loop.py:18-31）+ 只捕 StopIteration（:239-246） |
| Operation 影响面仅 Go 内 | ✓ grep 实证（生产代码 2 处 + 1 注释，worker-python 零引用） |
| image_gen 键不触发会话扫描 | ✓ agentmain.py:72 实证 |
| 测试/CI 兼容、无新依赖 | ✓ ci.yml:103、pyproject.toml:11-17 |

---

## 7. 建议修改清单（按优先级）

1. （重要）波及表补 `runtime_overlay.py`：明确 image_gen 客户端实现位置——进 llmcore.py（推荐）或新增 imagegen.py 时同步 LEGACY_MODULES/OVERLAY_MANIFEST_ENTRIES/worker 测试 fixture。
2. （重要）§3.2 同步路径：`response_format` 按 model 条件发送（dall-e 才发），并把 gpt-image 的 n/response_format 兼容性列入残余风险待实测。
3. （重要）§6.5 错误文本统一 `[Error: image_gen ...]`，弃用 `!!!Error:` 前缀；"复用 _stream_with_retry"改"仿写其重试语义"。
4. （重要）§3.2/§6.5 补"marker 回显依赖 + 平台兜底不对称"说明；终态设计补 generated_output_files 登记项。
5. （建议）§2 注明 D-C 黑盒豁免依据；PROGRESS.md 纳入 Step 1；行号引用修正（transport.go:117、runtime_config.go:146-148）；输出文件名唯一性；tui_v3 白名单与 cn schema 双文件漂移说明；mykey_template 头部表格。

---