# 新会话提示词（可直接复制给新会话开头）

---

你是 GenericAgent 项目的开发 agent（fork 二开多租户：根项目 = 自进化 agent 框架；tenant_platform = Go 后端 + gRPC Python worker + React Web + bot poller 的租户平台）。

## 项目背景（关键）

- 2026-08-26 已把上游 lsdefine/GenericAgent 8 月的高价值优化（17 提交，merge/upstream-high-value → main，+504/-188）合入并验证：`python -m pytest tests -q` 92 passed + 运行时 smoke 全过。详见 `memory/archive/2026-08.md`「2026-08-26 | 上游 lsdefine/GenericAgent 高价值优化合入」。
- 合入内容：llmcore history trimming 线性化、abort 强制唤醒阻塞 recv + 可中断退避、OpenAI overload 重试、api_key_header、conductor XSS、stapp 现代化（Streamlit>=1.62）、context_win 默认提升等。
- 已保留本地版本：ga.py 空响应防护（D5 审查版）、tools_schema code_run 描述。
- 未合入（有意跳过）：上游 React Desktop 2.0、hub/p2p 手机配对、turn-summary 提取统一。

## 当前 git 状态

- `main` 分支 = 最新（含合入）。工作区应干净。
- 计划 epic 在 `.tasks/upstream-hw-merge-optimization/`（EPIC.md 计划 / SUBTASKS.csv 进度真值）。

## 你的任务：执行优化计划 `.tasks/upstream-hw-merge-optimization/EPIC.md`

按顺序做，每批合入后更新 SUBTASKS.csv + PROGRESS.md：

1. **第一批（O1-O3，契约硬化，先做）**：
   - O1: run() 的 `turn_resps[-1]` 依赖"runner 必须先发 turn 事件"的隐式契约 → 收文本时槽位为空自动开槽 + 双向注释（agentmain.py ~262 行 / agent_loop.py:197）+ 写"不先发 turn"单测不抛 IndexError
   - O2: abort() 设的 `sess.should_stop` lambda 注释说 finally 清理但没清理 → run() finally 置 None + 修正注释（防未来共享 session 误停）
   - O3: abort() 内 `import socket` 提到 agentmain 顶部
2. **第二批（O4-O7）**：trim_messages_history 单行重构（保持 O(n)）；all_outputs 按来源过滤（先确认 IM 渠道 source 取值）；IM 前端 put_task shutdown 契约；测试 _minimal_agent 工厂化（conftest）
3. **第三批（O8-O10，backlog）**：评估后实施或记录理由关闭（STATS 全局 dict、魔法数字常量、runtime_data overlay 清理）

## 硬约束

- **不改业务行为**：IM 交付/生图/多模态/租户 worker 全部保持现状；不碰 tenant_platform 契约（proto/openapi/policy）。
- **验证**：每批合入前 `python -m pytest tests -q` 全绿；改动 llmcore/agentmain 后加对应窄测试。后端单测 60s 超时。
- **环境**：Python 3.11/3.12（勿 3.14）；本地 streamlit 1.57 < pyproject 要求的 1.62——**用 stapp 前先 `pip install -U streamlit`**（纯代码改动不需要）。
- **已知事实（不需要重新排查）**：worker overlay 每次会话从 /opt/ga/legacy 动态物化（digest 漂移自动重建），根项目 llmcore 优化对新会话 worker 自动生效；should_stop 当前 single-session 安全。
- **协作规范**：开始前读项目根 `tools.md`；功能闭环后更新 `memory.md`（≤120 行）并追加 `memory/archive/2026-08.md`；提交信息保留中文任务风格。

## 交付

- O1-O3 完成并验证 → 提交 main（commit 主题如 `fix(agentmain): turn_resps 契约硬化 + should_stop 清理 + socket import 归位`）
- 更新 `.tasks/upstream-hw-merge-optimization/SUBTASKS.csv`（status: done）+ `PROGRESS.md`
- 汇报：每批改了什么、验证结果、O8-O10 的评估结论