# 任务：二轮独立审查 GenericAgent Phase B 生图实施方案（定稿版）

你是独立审查者（fresh context，不预设结论），对**定稿后**的生图实施方案做只读审查，输出结构化审查报告。首轮审查已存在，你的价值在于：核验定稿后的方案是否自洽、首轮修订是否引入新问题、定稿决策（D1）是否有遗漏的架构后果。

## 项目背景

- 项目：GenericAgent（/home/lou/code/GenericAgent）。根项目 = 极简自进化 agent（`agent_loop.py` 核心循环 + `ga.py` 原子工具 + `llmcore.py` 多协议 LLM Session 体系 + `memory/` 分层记忆）；tenant_platform = Go 后端 + llm-proxy（能力 token/JTI 预算/egress 白名单）+ worker-python（gRPC worker，沙箱 GA）+ bot_poller（IM 渠道）
- 真值优先级：`tenant_platform/docs/` 设计文档 > 当前代码 > README。先读 `AGENTS.md`、`tools.md` 了解项目硬规则
- 关键既有事实：GA 无生图；多模态仅"入站理解"（图片 image_url 块注入）；出站媒体 Phase A 已落地（`[FILE:]` marker → delivery spool → send_media）；llm-proxy 仅代理 chat 三路径（/v1/chat/completions、/v1/responses、/v1/messages）
- 首轮审查（2026-08-14）结论："需修改后批准"，2 阻断（平台工具白名单、托管机制未闭环）+ 8 重要 + 若干建议，已全部回写方案 §2/§3/§6.5/§7/§8/§9
- **用户拍板 D1（2026-08-14）：直连形态先行（v1 实施），托管为终态设计延后实施；ImageGenClient 双形态统一设计（一份代码，配置决定形态）**

## 待审查文档

- 方案（定稿版）：`.tasks/im-media-pipeline/PHASE_B_IMAGE_GEN_PLAN.zh-CN.md`（重点：§2 波及范围、§3 架构/配置/schema、§6.5 失败语义、§7 实施计划、§8 决策记录、§9 首轮审查结论与定稿记录）
- 设计真值：`tenant_platform/docs/IM_MEDIA_ARCHITECTURE.zh-CN.md`（重点 §6 生图 Phase B 旧稿、§8 设计原则、§9-§11 状态/分期/残余风险）
- 首轮审查的"待验证/遗留"项：§9 中标注的决策点与风险

## 相关代码（必须对照真实实现验证，不轻信方案自述）

- `llmcore.py`：BaseSession 配置解析（~612-700）、`resolve_session` 命名分派（~1296-1301）、`_stream_with_retry`（~447-487）、`_msgs_claude2oai`/`_claude_image_block`/`_fix_messages`（~566-612 / ~731-797，入站多模态转换）
- `ga.py`：`GenericAgentHandler`（~510+）、`do_` 工具实现模式（~521-710）、StepOutcome 语义、cwd/`_get_abs_path`、错误返回先例（do_file_write 等）
- `agent_loop.py`：`BaseHandler.dispatch`（~18-31）、`agent_runner_loop`（~184-262，工具结果/异常/退出语义）
- `agentmain.py`：`load_tool_schema`（~18-23）、会话扫描条件（~72）、handler cwd（~194）
- `assets/tools_schema.json`（9 条，OpenAI function 包装格式）
- `mykey_template.py`：session 配置块、mixin 结构、变量名分派说明
- `tenant_platform/backend-go/internal/infrastructure/llmproxy/`：handler.go / server.go / target.go / token.go / transport.go / network_policy.go
- `tenant_platform/backend-go/internal/application/`：worker_credential.go（`Operation: "llm.chat"` 硬编码）、runtime_config.go（mixin_config 自动写、BuildRuntimeConfig）、delivery_capture.go（`[FILE:]` 捕获、per-type 上限）、session_files.go（ResolveMarker/RecordOutbound outputs/ 前缀强制）
- `tenant_platform/worker-python/src/ga_worker/`：session_files.py（OUTPUTS_DIR）、legacy_instrument.py（apply_tool_policy 白名单过滤）、runtime_overlay.py（镜像内嵌清单）、entrypoint.py（GA_WORKSPACE_TEMP 推断）
- `tenant_platform/contracts/policy/foundation.v1.json`（工具白名单 deny-by-default）
- 进度真值：`.tasks/im-media-pipeline/SUBTASKS.csv`（T8 blocked）、`PROGRESS.md`

## 审查重点（逐项给出结论 + 精确证据 + 修改建议）

### 1. 整体架构（含定稿后的双形态设计）
- §3 架构图与"双形态设计"（直连 v1 / 托管终态，一份代码配置决定形态）是否自洽？对照 `BaseSession` 直连/托管共用代码的先例是否成立？
- ImageGenClient 与 chat Session 体系的边界是否清晰？"仿 resolve_session 命名分派 / 复用 BaseSession 配置解析"抽象对照真实代码是否成立？
- 平台侧终态设计（provider 能力类型维度 chat/image、排除 chat mixin、runtime_config 下发 image_gen 块、policy 放行、openapi/web 同步）作为"定稿但延后"是否有遗漏的耦合点？该设计与既有 capability/JTI/白名单流程的关系是否被正确描述？
- 流式路径（B5：stream+partial_images SSE 对工具层透明）是否自洽？同步/流式切换与降级语义是否明确？

### 2. 边界（定稿后新出现的边界问题优先）
- v1 直连形态的可用/不可用边界是否明确？平台模式 v1 有意不可用（policy 不放行）的声明是否真实成立？有无"死工具暴露"风险（如 policy 放行但未配置、或配置了但沙箱无密钥）？
- 未配置 image_gen 时工具行为、n>1 多图命名与交付语义、超限（20MiB/聚合 256MiB）行为是否闭环？
- 双形态切换对存量用户配置（mykey.py 无 image_gen 键、存量会话、存量 provider）的影响？

### 3. 逻辑语义
- 工具调用链每步输入输出是否闭环？`[FILE:outputs/<name>]` marker 在真实代码里被谁解析/消费（Go captureTaskDeliverableFiles → RecordOutbound outputs/ 前缀 → spool → send_media；根前端按 temp/ 解析）？工具返回 marker 是否真能走通交付？do_image_gen 写 `self.cwd/outputs/` 的前提是否在所有运行形态（根项目 temp/、容器 GA_WORKSPACE_TEMP 符号链接）成立？
- 失败语义：API 错误/超时/空响应/流式中断/超限分别如何处理？是否符合"失败诚实"？错误文本格式与既有 `!!!Error:`/`[Error:` 语义是否一致？
- 与既有机制的关系：MixinSession 故障转移、平台自动写 mixin_config、多模态入站链路（_fix_messages/_claude_image_block）是否会被影响或遗漏？

### 4. 旧源码清理与兼容
- IM_MEDIA_ARCHITECTURE §6 旧设计稿（B1–B4 无勾选）+ §9 表格/§10 分期/§11 残余风险中 Phase B 相关表述，定稿落地后应如何升级/清理（方案是否已列全）？
- 实施后有无遗留死代码/兜底风险：code_run 生图兜底、llmcore 现有 image 转换函数与 ImageGenClient 的关系、旧"图片路径文本给模型"降级路径是否需清理？
- 存量兼容：已有用户配置（mykey.py 无 image_gen 键）、存量会话、平台存量 provider 配置是否受影响？

### 5. 波及范围完整性
- 文件清单有无遗漏：docs/ 文档、tools.md/memory.md 协作文件、mykey_template 中英文、.gitignore、镜像依赖（ga-runner 内嵌清单 runtime_overlay.py）、CI（ci.yml 根测试）、pyproject（是否有新依赖）等
- 平台侧影响面是否准确：Operation 枚举改动波及（worker-python 是否引用）、llm-proxy 请求/响应体大小限制（MaxWorkerRequestBytes 只限请求体？）、路由冲突、provider 配置模型类型（对照 LLM_PROVIDER_ARCHITECTURE.md）
- 是否影响既有 IM 入站媒体/多模态理解链路（回归面）

## 输出格式

1. **结论总览**：建议批准 / 需修改后批准 / 否决 + 一句话理由
2. **按 1-5 逐项**：问题列表（严重度分级：阻断/重要/建议）+ 证据（文件:行号 或 文档节号）+ 具体修改建议
3. **与设计真值/项目硬规则的冲突清单**（如有）
4. **方案未覆盖的盲区**（如有）

## 要求

- 独立验证为主（读代码、读文档、追引用），所有结论必须给证据；不确定处标注"待验证"而不是猜测
- 首轮审查已核验的事实（交付链三端打通、9 工具口径、Operation 影响面仅 Go 内 3 处、agent_loop 零侵入、image_gen 变量名不触发会话扫描）可采信，但允许复核
- 特别注意：定稿决策（D1 直连先行）是否引入了首轮未覆盖的新问题（如双形态切换、平台模式不可用的声明真实性、policy 保持现状的副作用）
