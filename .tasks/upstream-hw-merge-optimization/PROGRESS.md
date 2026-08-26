# PROGRESS: 上游高价值优化合入后的清理与硬化

> 进度真值: `SUBTASKS.csv`; 背景与方案: `EPIC.md`。
> 2026-08-26 三批全部完成, 全量测试 100 passed, 已提交 main(未推送)。

## 结果概览

| 批次 | 内容 | 状态 |
|---|---|---|
| 第一批 O1-O3(+O6) | 契约硬化: turn_resps 槽位防御 / should_stop 清理实化 / socket import 上提 / put_task 契约注释 | ✅ 合入 |
| 第二批 O4-O7 | 规范化与资源治理: trim 多行化 / all_outputs 过滤 / 测试 factory 化 | ✅ 合入 |
| 第三批 O8-O10 | O9 常量提取实施; O8/O10 评估后记录理由保持 backlog | ⚠️ 部分 |

## 提交清单(main, 未 push)

- `2b09ce0c` fix(agentmain): O1-O3 契约硬化 + O6 注释补落地
- `e1562603` refactor(llmcore): O4-O7 规范化与资源治理
- `ee6e9f08` refactor(llmcore): O9 退避/重试魔法数字命名常量

验证: 每批合入前 `python -m pytest tests -q` 全绿; 最终 100 passed (92 基线 + 8 新增:
O1/O2/O4×5/O5 单测)。

## 第一批 O1-O3 细节

- **O1** run() 收文本 chunk 且 `turn_resps` 为空时自动开槽(`curr_turn += 1; turn_resps.append('')`),
  不抛 IndexError; run() 的 turn 事件处理与 `agent_loop.agent_runner_loop` 的
  `yield {'turn': turn}` 双向注释标明契约。新单测: 无 turn 事件直接 yield 文本
  → done 正常落、outputs 归属第 1 轮。
- **O2** 提取 `_backend_sessions()`(abort 注入 / run finally 清理共用同一 session 集);
  run() finally 显式 `should_stop = None`(原注释声称清理但未实现); abort 注释修正。
  新单测: 任务中注入 should_stop lambda → 完成后 backend.should_stop is None。
- **O3** `import socket as _socket` → 模块顶部 `import socket`, 函数内直接用。
- **O6** put_task shutdown 检查补契约注释(前次提交标 done 但代码实际缺失,
  本次一并落地: "shutdown 后拒收, 前端保证不调用")。

## 第二批 O4-O7 细节

- **O4** `trim_messages_history` 单行复合语句(0c235a80)多行化: 成本数组 + 索引推进,
  保持 O(n), 注释说明 pop(0) O(n²)→索引 O(n)。**等价性验证**: 新增
  `tests/test_llmcore_trim.py`, 单行版参考实现 5 场景(不触发/仅压缩/硬 trim kp2/
  kp0/尾部 tool block)深拷贝后 diff 逐字符全等。
- **O5** 模块级 `IM_CHAT_SOURCES = {'wechat','telegram','chat'}` 黑名单;
  run() 中对黑名单 source 跳过 `all_outputs.append`, 同时把 `turn_resps` 改为
  槽位引用/任务局部列表分离——IM 任务不触碰 all_outputs(无跨任务错位)。
  新单测: 交互 source(user/hub/controller)记录 3 条、IM source 不新增。
- **O7** `tests/conftest.py` 新增 `make_minimal_agent()` 工厂(覆盖 GenericAgent.__init__
  全部字段) + `minimal_agent` fixture; `test_agentmain_lifecycle.py` /
  `test_agentmain_stream_events.py` 删除本地重复 `_minimal_agent`, 全部改用 fixture。

## 第三批 O8-O10 决策

- **O9 已实施**: 提取模块级命名常量 11 个(退避 min/base/cap/sleep 粒度、
  image_gen base、trim 保留率 ×2、maxlen 系数/floor/cap、mixin base_delay),
  替换 6 处硬编码, 行为不变。
- **O8 保持 backlog(理由)**: ①STATS 仅 stapp.py:290 显示用(tps/ttft/ctx),
  无任务结果/交付正确性影响; ②改动面 = 4 个 parse 函数签名
  (`_parse_claude_sse`/`_parse_openai_sse`/`_parse_openai_json`/`_record_usage`)
  + 7 处写入 + stapp 改读实例属性, 属中等重构; ③实际并发场景少
  (single-session 主流, 多 agent 实例各自进程)。如未来需要多 session 统计隔离,
  作为独立 feature 实施(含双 session 并发 digests 对照测试)。
- **O10 保持 backlog(理由)**: ①生产容器 overlay 在 tmpfs(GA_OVERLAY_ROOT),
  容器销毁即清空, 无残留; 残留 9 个 2.7M 仅 loopback 开发环境积累(~24MB, 收益低);
  ②loopback 下 overlay 内 memory/temp 是本地实际数据(非符号链接),
  当前无 lease/活动会话注册机制可安全区分活动/过期, 误删会损坏运行中会话;
  ③恢复路径靠 digest 校验重建 overlay, 清理不带来功能收益。如后续引入
  会话注册表 + 时间戳淘汰, 可再评估。

## 剩余动作(交付后)

1. 用户统一跑 CI(分支/PR 级矩阵)后推送 origin(main 当前领先 23 commits)。
2. 更新 `memory.md`(≤120 行: 当前基线/已完成/进行中)并追加
   `memory/archive/2026-08.md` 本月归档。