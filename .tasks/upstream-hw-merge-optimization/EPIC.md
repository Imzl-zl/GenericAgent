# EPIC: 上游高价值优化合入后的清理与硬化（upstream-hw-merge-optimization）

> 背景：2026-08-26 已把上游 lsdefine/GenericAgent 8 月的高价值优化（17 提交）合入 main
> （详见 `memory/archive/2026-08.md`「2026-08-26 | 上游 lsdefine/GenericAgent 高价值优化合入」）。
> 合入后自查发现一批契约脆弱点/规范问题/资源治理项，本 epic 排期逐项修复。
> 进度真值：`SUBTASKS.csv`；配套说明：`PROGRESS.md`。

## 目标与不变量

- 不改变现有业务行为（IM 渠道交付、生图、多模态、租户 worker 全部保持现状），只做契约硬化与治理。
- 不触碰 tenant_platform 契约（proto/openapi/policy）——本 epic 全在根项目层。
- 每个修复必须跑窄验证（对应测试 + 根全量），合入前跑 `python -m pytest tests -q`。

## 已验证的安全事实（合入时确认过，本 epic 不需要重做）

- worker overlay 是每次会话动态物化的（digest 漂移自动重建）——llmcore 优化对新会话 worker 自动生效。
- should_stop lambda 当前 single-session 架构下安全（worker 每会话独立 agent）。
- stapp 整搬后依赖 continue_cmd/btw_cmd/export_cmd（符号全存在），import 在 try 块内可选降级。

## 前置工作（已完成，2026-08-26，新会话直接开工）

- **streamlit 已升级 1.57 → 1.62**（`pip install -U "streamlit>=1.62"`；系统 Python 3.13.12，无 .venv）——stapp bare 导入验证 OK，92 测试全绿。**新会话无需处理环境**。
- **O5 source 取值全集已查清**（grep 全量）：
  - IM 聊天渠道（应跳过 all_outputs）：`wechat`（wechatapp 显式）、`telegram`（tgapp 显式）、`chat`（AgentChatMixin 默认，chatapp_common.py:264——QQ/飞书/钉钉/Discord 均继承未覆写）
  - 交互/管理（保留）：`user`（默认/CLI/qtapp/stapp/stapp2）、`hub`、`controller`、`conductor`、`subagent:*`（conductor.py:331）、`acp`、`func`、`reflect`
  - 租户 worker：`task.source or "user"`（task_drain.py:69，source 来自 TaskEnvelope）——租户交付不走 stapp，同样可跳过
  - 过滤规则建议：黑名单 `IM_CHAT_SOURCES = {'wechat','telegram','chat'}`，其余记录（新增交互 source 自动保留）
- **O6 审计结论**：`agent.shutdown()` 仅 dcapp.py:167 停服路径调用；put_task 调用的 18 个前端点（chatapp_common/conductor/dcapp/desktop_bridge/fsapp/acp_bridge/hub/qtapp/stapp/stapp2/tgapp/wechatapp/task_drain）在 shutdown 后均已停止接收输入——**RuntimeError 实际不可达**。O6 降级为：契约注释声明 + 不非法改前端代码。
- **O8 影响面评估**：`_parse_claude_sse/_parse_openai_sse/_parse_openai_json/_record_usage` 签名均无 `sess` 参数（llmcore.py 167/255/394/375 行）；STATS 写入 7 处（116/124/142/218/392/458-503 区），读取仅 stapp.py:290。实例化需参数化 4 个函数 + stapp 改读 `agent.llmclient.backend.stats`——改动中等，backlog 保持。

---

## 第一批：契约与一致性（O1-O3，低成本防事故，先做）

### O1. agentmain.run() 与 agent_runner_loop 的隐式契约硬化
- **问题**：run() 里 `turn_resps[-1] += chunk`（agentmain.py 约 262 行）依赖"runner 必须先发 `{'turn': N}` 事件开槽"，契约在 agent_loop.py:197（`if yield_info: yield {'turn': turn}`）但两侧无关联标记、无防护。未来新 runner 或改动 agent_runner_loop 忘了先发 turn → IndexError（有 except 兜底显示 Backend Error，但任务失败）。
- **修法**：run() 收文本 chunk 且 `turn_resps` 为空时自动开槽（`curr_turn += 1; turn_resps.append('')`）；在 run() 与 agent_runner_loop 的 yield_info 处各加注释标明契约。
- **验证**：`python -m pytest tests/test_agentmain_stream_events.py tests/test_agentmain_lifecycle.py -q`；写一个"不先发 turn 直接 yield 文本"的 runner 单测断言不抛 IndexError。

### O2. abort() should_stop 幽灵 lambda 清理符实化
- **问题**：abort() 设置 `sess.should_stop = lambda: self.stop_sig`，注释写 "cleared by run()'s finally"，但 run() finally 里**没有清理**。当前 single-session 无害，但 worker 未来扩共享 session 时旧 lambda 闭包捕获旧 agent → 误停新任务。
- **修法**：run() finally 里对 session 显式清 `should_stop`（置 None），并修正 abort() 注释（"read live from agent.stop_sig; cleared in run()'s finally"）。
- **验证**：`python -m pytest tests/test_agentmain_lifecycle.py -q`（shutdown/生命周期测试）+ 手写 smoke：abort 后检查 session 无 should_stop 残留。

### O3. agentmain 函数内 import 提模块顶部
- **问题**：abort() 里 `import socket as _socket`（约 123 行），函数内 import 不规范；agentmain 顶部无 socket import。
- **修法**：`import socket` 提到顶部，函数内直接用。
- **验证**：`python -m pytest tests -q` 全量。

---

## 第二批：规范化与资源治理（O4-O7）

### O4. trim_messages_history 单行复合语句重构
- **问题**：0c235a80 的 O(n) 优化把 4 个复合操作塞进 1 行（llmcore.py 约 128-132 行），可读性差难维护。
- **修法**：重构为多行清晰版本（成本数组 + 索引推进 + 循环），保持线性复杂度，加注释说明"pop(0) O(n²) → 索引 O(n)"。
- **验证**：`python -m pytest tests/test_llmcore_http.py -q` + 写一个 trim 等价性测试（新旧逻辑对同一 history 产出相同结果，可用 diff 验证）。

### O5. all_outputs 按来源过滤（内存治理）
- **问题**：all_outputs 为 stapp 渲染设计，但所有 source（含高频 IM 渠道）都全量记录完整分轮文本，上限 5000 任务，长跑内存累计（普通任务 50-100MB，大输出任务可能数百 MB）。IM 前端不读 all_outputs。
- **修法（推荐黑名单）**：run() 里 `source` ∈ `{'wechat','telegram','chat'}` 时跳过 append（source 全集见「前置工作」）；其余记录。常量定义在 agentmain 模块级。
- **验证**：交互 source（user/hub/controller）任务后 all_outputs[-1] 有 outputs；IM source（wechat/chat/telegram）任务后不新增条目。用现有 smoke 模式跑 source 参数矩阵。

### O6. IM 前端 put_task shutdown 语义契约
- **问题**：d8d90eef 合入后 put_task 在 shutdown 后抛 RuntimeError；前端调用点均无 try。
- **已审计（前置）**：shutdown 仅 dcapp.py:167 停服路径调用，shutdown 后所有前端已停止接收输入 → RuntimeError 实际不可达。
- **修法（降级）**：不改前端代码；在 agentmain.put_task 的 shutdown 检查处加注释声明契约（"shutdown 后拒收，前端保证不调用"），避免未来误用。
- **验证**：静态审查 + 现有测试全绿。

### O7. 测试 _minimal_agent 工厂化
- **问题**：`_minimal_agent` 在 tests/test_agentmain_lifecycle.py、test_agentmain_stream_events.py（等）重复手写，新增 agent 字段要同步多处改（本次 all_outputs 踩了 5 个测试回归）。
- **修法**：提取共享 factory 到 tests/conftest.py 或 tests/helpers.py，各测试复用；factory 覆盖 __init__ 全字段。
- **验证**：`python -m pytest tests -q` 全量。

---

## 第三批：架构改进候选（O8-O10，改动面需评估）

### O8. llmcore STATS 全局 dict → session 实例属性
- **问题**：模块级 `STATS` 多 session 并发互相覆盖（tps/ttft 显示最后活跃 session）。影响面：仅 stapp 读（worker 不读），但 stapp 用 `llmcore.STATS` 全局引用。
- **前置评估（已完成）**：`_parse_claude_sse`(167)/`_parse_openai_sse`(255)/`_parse_openai_json`(394)/`_record_usage`(375) 签名均无 `sess`；STATS 写入 7 处（116/124/142/218/392 + 流区 458-503）；读取仅 stapp.py:290。
- **方案**：BaseSession 加 `self.stats = {}`；`_stream_with_retry(sess,...)` 把 stats 透传给 parse 闭包（或 parse_fn 加参数）；stapp 改读 `agent.llmclient.backend.stats`（MixinSession 需转发到当前子 session）。改动中等，backlog 保持，实施前先写 digests 对照测试。
- **验证**：llmcore http 测试 + stapp 渲染 + 双 session 并发下统计不互相污染（新测试）。

### O9. 退避/重试魔法数字命名常量
- **问题**：llmcore.py 中退避常量（0.5 min / 3.0 base / 30.0 cap / 0.2 sleep 粒度 / 0.3 trim_keep_rate / 0.75 maxlen 系数）多处硬编码。
- **修法**：模块级命名常量（如 `_BACKOFF_MIN=0.5`、`_BACKOFF_BASE=3.0`、`_BACKOFF_CAP=30.0`），引用替换；与 `tenant_platform` 预算链无关（根侧独立）。
- **验证**：`python -m pytest tests -q`。

### O10. runtime_data 历史 overlay 清理
- **问题**：每会话物化一次 legacy-overlay，runtime_data/ 下残留（当前 9 个 2.7M，长跑积累）。
- **修法**：worker 启动或定期清理过期 session overlay（保留最近 N 个）；注意别误删活动会话（用 lease/时间戳判断）。
- **验证**：手动触发清理 + 活动会话不被删。

---

## 协作规范（本 epic 已做）

- O11. memory.md 已更新（2026-08-26 合入闭环，见 memory.md「最近活跃窗口」）。

## 完成定义（Definition of Done）

- O1-O3 合入 main + 全量测试绿；O4-O7 合入 main + 全量测试绿；O8-O10 若改动面评估可控则实施，否则在 PROGRESS.md 记录理由后关闭。
- 每批合入后更新 `SUBTASKS.csv` status 与 `PROGRESS.md`。
