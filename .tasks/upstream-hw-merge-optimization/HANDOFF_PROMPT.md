# 新会话提示词（可直接复制给新会话开头）

---

你是 GenericAgent 项目的开发 agent（fork 二开多租户：根项目 = 自进化 agent 框架；tenant_platform = Go 后端 + gRPC Python worker + React Web + bot poller 的租户平台）。

## 项目背景（关键）

- 2026-08-26 已把上游 lsdefine/GenericAgent 8 月高价值优化（17 提交 → main，+504/-188）合入并验证（92 passed + 运行时 smoke 全过）。详见 `memory/archive/2026-08.md`「2026-08-26 | 上游 lsdefine/GenericAgent 高价值优化合入」。
- 合入内容：llmcore trimming 线性化、abort 强制唤醒阻塞 recv + 可中断退避、OpenAI overload 重试、api_key_header、conductor XSS、stapp 现代化、context_win 默认提升等。
- 保留本地版：ga.py 空响应防护（D5 审查版）、tools_schema code_run 描述。
- 未合入（有意跳过）：上游 React Desktop 2.0、hub/p2p 手机配对、turn-summary 提取统一。

## 前置工作状态（已完成，直接开工，无需再确认）

- **环境**：streamlit 已升级 1.62（`pip install -U "streamlit>=1.62"`；系统 Python 3.13.12 无 .venv）、stapp bare 导入验证 OK、92 测试全绿。**不要再动环境**。
- **O5 source 全集已查清**（grep 全量证据在 `.tasks/upstream-hw-merge-optimization/EPIC.md`「前置工作」）：
  - IM 渠道（跳过）：`wechat`（wechatapp）、`telegram`（tgapp）、`chat`（AgentChatMixin 默认，chatapp_common.py:264，QQ/飞书/钉钉/Discord 继承未覆写）
  - 交互（保留）：`user`/`hub`/`controller`/`conductor`/`subagent:*`/`acp`/`func`/`reflect`
  - 租户 worker：`task.source or "user"`（task_drain.py:69），同样可跳过
  - 建议实现：模块级 `IM_CHAT_SOURCES = {'wechat','telegram','chat'}` 黑名单，`source in IM_CHAT_SOURCES` 时跳过 all_outputs.append
- **O6 已审计降级**：`agent.shutdown()` 仅 dcapp.py:167 调用，shutdown 后前端均停止收输入 → put_task 抛 RuntimeError 实际不可达。**不改前端**，只在 agentmain.put_task shutdown 检查处加契约注释。
- **O8 影响面已评估**：`_parse_claude_sse`(167)/`_parse_openai_sse`(255)/`_parse_openai_json`(394)/`_record_usage`(375) 无 sess 参数；STATS 写入 7 处、读取仅 stapp.py:290。改动中等，backlog 处理。

## git 状态

- `main` 分支最新、工作区干净、**未推送 origin（等本批次优化完成统一 CI 后再推送，勿提前 push）**。
- 计划 epic：`.tasks/upstream-hw-merge-optimization/`（EPIC.md 计划 / SUBTASKS.csv 进度真值 / 本文件交接）。

## 你的任务：按序执行优化计划（EPIC.md）

每批合入后更新 SUBTASKS.csv + 写 PROGRESS.md，全部完成后提示统一 CI + 推送：

1. **第一批（O1-O3，契约硬化）**：
   - O1 `agentmain.py` ~262 行 `turn_resps[-1] += chunk`：收文本 chunk 且 turn_resps 为空时自动开槽（`curr_turn += 1; turn_resps.append('')`）+ 在 run() 与 `agent_loop.py` ~197 `yield {'turn': turn}` 处双向注释标明契约；新增单测：runner 不先发 turn 直接 yield 文本 → 不抛 IndexError、文本正常落 done
   - O2 `abort()` ~119 行 `sess.should_stop = lambda: self.stop_sig`：注释声称 finally 清理但没清 → 在 run() finally 里对 session 显式置 `should_stop = None` + 修正注释（防未来共享 session 闭包误停）；验证 lifecycle 测试 + smoke 无残留
   - O3 `abort()` ~123 行 `import socket as _socket` 提到 agentmain 模块顶部
2. **第二批（O4-O7）**：
   - O4 `llmcore.py` ~128-132 行 trim 单行重构为多行（成本数组+索引推进，保持 O(n)）+ 等价性验证
   - O5 all_outputs 按 `IM_CHAT_SOURCES` 黑名单过滤（实现见前置）
   - O6 仅加注释（见前置）
   - O7 测试 `_minimal_agent` 提取到共享 fixture（conftest.py），覆盖 __init__ 全字段
3. **第三批（O8-O10，backlog）**：实施或记录理由关闭（O8 需先写双 session 并发 stats 对照测试；O9 常量收敛；O10 overlay 清理勿删活动会话）

## 硬约束

- **不改业务行为**：IM 交付/生图/多模态/租户 worker 保持现状；不碰 tenant_platform 契约（proto/openapi/policy）。
- **验证**：每批合入前 `python -m pytest tests -q` 全绿；llmcore/agentmain 改动加窄测试。后端单测 60s 超时。
- **已知事实（勿重复排查）**：worker overlay 每会话动态物化（digest 漂移自动重建），根项目 llmcore 优化自动生效；should_stop 当前 single-session 安全（O2 是防未来扩展）。
- **协作规范**：开始前读根 `tools.md`；功能闭环后更新 `memory.md`（≤120 行）并追加 `memory/archive/2026-08.md`；提交信息中文任务风格（`fix(agentmain): ...`）。

## 交付

- 三批完成 + 全量验证 → 提交 main（勿 push）→ 汇报：每批改动、验证结果、O8-O10 评估结论 → 提示用户统一 CI 后推送