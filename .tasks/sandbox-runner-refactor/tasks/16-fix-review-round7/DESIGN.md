# Task 16: 审查发现修复第七轮（3C/5I）

- version: zhanggui/v0.4
- goal: 修复独立审查报告的 8 个真实问题（3 Critical + 5 Important），且不引入新问题
- intent: clear-change
- phase: execute
- decision_mode: Model（均为安全/生命周期缺陷修复，方向已由审查报告确定；无 user-owned 决策）
- task_root: .tasks/sandbox-runner-refactor（epic 第 16 子任务，ownership 已确认）
- depends_on: 15

## 设计真值（每项：根因 → 修复 → 验证）

### C1 交付快照文件名路径穿越（delivery_service.go:552 + session_files.go）

根因：`f.FileName` 来自 DB 快照 → captureTaskDeliverableFiles → RecordOutbound → manifest（Runner 可写，loadManifest 反序列化不校验字段）→ `filepath.Join(dir, markerHash_+f.FileName)` 可含 `../` 逃逸 snapshotDir，Platform 以自身权限 WriteFile/Remove 任意路径。

修复：
1. `session_files.go loadManifest`：反序列化后校验每个条目：`OriginalName` 必须是非空纯 basename（`filepath.Base(name)==name` 且不含 `/ \` 与 `..`）；`RelativePath` 必须无 `..` 逃逸且位于 `outputs/` 下。非法条目整体拒绝（fail-closed）。
2. `delivery_service.go buildPayload`（task_complete 分支）：tmpPath 文件名不再使用 `f.FileName`，改为 `deliveryFileMarkerKey(f.Marker)` + 服务端从 `f.RelPath` 派生的安全 basename；`displayName` 经 `sanitizeDeliveryDisplayName`（纯 basename、禁分隔符/`..`、长度上限）。
3. `delivery_capture.go`：RecordOutbound 返回后若 `ref.OriginalName` 非法，回退到 relPath basename。

验证：Go 单测——伪造 manifest（`../evil`、含 `/`、RelativePath 逃逸）被拒绝；DeliveryFile 带 `../../x` FileName 时 buildPayload 不逃逸 snapshotDir。

### C2 code_run 派生进程跨任务存活（ga.py:58）

根因：`subprocess.Popen` 无进程组，`process.kill()` 只杀直接子进程；成功/超时/取消路径都不清理；Runner 跨任务复用，后台进程可窃取下一任务凭据或继续写工作区。

修复：
1. ga.py `code_run`：POSIX 用 `start_new_session=True`；timeout/stop/异常路径改 `os.killpg(os.getpgid(pid), SIGKILL)`（Windows 保持现状）。
2. ga.py 模块级 `_code_run_pids` 注册表 + `kill_all_code_run_processes()`（对登记 pid 逐个 killpg 并清空）。
3. Worker 任务终态统一清理：`task_terminal.py` 新增 `cleanup_legacy_subprocesses(adapter)`（经 `adapter._legacy_mods["ga"]` 调用），在 emit_cancel_or_timeout_terminal / emit_final_terminal / emit_missing_terminal_if_needed / emit_exception_terminal 产出 terminal 前调用。

验证：Python 单测——code_run 超时路径调用 killpg（mock Popen）；kill_all_code_run_processes 对登记进程组 killpg；终态 emit 前清理被调用。

### C3 state/committed/results 可被 Runner 删除替换（docker_cli.go:197 + workspace.go:96 + inspect.go:401）

根因：整个 `state/` 以 rw 挂载且 committed/results 目录 02770 组写；Runner 可 unlink/rename 重建 committed 快照与 results，导致当前恢复点或待交付结果永久丢失（checksum 只能事后报错）。

修复（挂载遮蔽方案，保留顶层 rw 以满足 Worker `runtime_root.mkdir`）：
1. docker_cli.go subpaths：`{"state", RunnerStateMount, false}`（保持）+ `{"state/committed", RunnerStateMount+"/committed", true}` + `{"state/results", RunnerStateMount+"/results", true}`。子挂载 ro 遮蔽顶层 rw 的同名路径。
2. inspect.go expected 挂载表更新为 6 项（按 Destination 字典序：memory, temp, config, state, state/committed, state/results），ro 标志逐项精确校验。
3. workspace.go 目录权限：committed/results 保持 02770（容器侧 ro 遮蔽为边界）；注释更新。

验证：sandbox 包测试更新挂载集合断言；docker inspect 集成测试（真实 Docker 时）验证遮蔽；无 Docker 时单测校验 expected 表。

### I4 SetTaskCapabilityJTIs 覆盖旧 JTI（task_store.go:179）

根因：`capability_jtis = $2` 整体替换；刷新时新 JTI 覆盖旧 JTI，旧 token 在 Worker 确认前未撤销，Platform 崩溃后恢复事务只见新 JTI，旧 token 存活至 TTL。

修复：SQL 改为数组追加去重：
```sql
capability_jtis = ARRAY(SELECT DISTINCT x FROM unnest(COALESCE(capability_jtis, ARRAY[]::text[]) || $2::text[]) x)
```
终态事务 `revokeTaskCapabilityJTIs` 已读全量数组 → 历史 JTI 一并撤销。

验证：DB 测试（TEST_DATABASE_URL）——两次 SetTaskCapabilityJTIs（不同 JTI）后读取含全部；重复 JTI 去重；终态后撤销表含全部。

### I5 RemoveMember 终态化绕过 capability 撤销（team_member_store.go:169）

根因：starting 未派发任务裸 UPDATE 置 cancelled，未走 finalizeTerminal → 不撤销已签发 JTI（capability 在 MarkDispatchStarted 前已交 Runner）、不写事件/outbox；崩溃后该终态行不被恢复扫描，token 无人撤销。

修复：RemoveMember 事务内：
1. queued 分支：SELECT FOR UPDATE 任务列表 → 逐个 `finalizeTerminal(cancelled, TASK_CANCELLED, "member removed from team")`。
2. starting 未派发分支：同样 SELECT FOR UPDATE + finalizeTerminal（内部含 revokeTaskCapabilityJTIs + 事件 + task_started 取消 + delivery）。

验证：DB 测试——移除成员后 starting 未派发任务终态为 cancelled、capability_jtis 全部进入撤销表、task_events 有 status_transition。

### I6 旧 generation 销毁清理删除新 config（manager.go:353 + workspace.go writeConfigFiles + docker_cli.go config 挂载）

根因：`cleanupWorkspaceConfig(hash)` 无锁删除整个 `config/`；DestroyRunner 与 EnsureRunner 无共享锁；旧实例销毁后清理可删掉新 generation 已写入的 config（新 Runner 丢失 mTLS 材料）。

修复：
1. config 按 generation 隔离：`writeConfigFiles(root, hash, generation, files)` 写入 `config/g<generation>/`（MkdirAll 0770 + Chown uid/shareGID）；docker_cli.go config 挂载源改为 `config/g<gen>`（destination 仍 RunnerConfigMount）。
2. `cleanupWorkspaceConfig(hash, generation)` 只删 `config/g<gen>`；调用处传入 generation（CreateAndStart 失败用 spec.Generation；DestroyRunner 从容器 label/name 解析）。
3. DestroyRunner 与 EnsureRunner 共享 per-workspace lock：DestroyRunner 解析 hash 后获取 `locks[hash]`，锁内执行 Destroy + 清理（与 EnsureRunner 串行，消除"旧删新"竞态）。

验证：Manager 单测（fake CLI）——DestroyRunner 后仅删除对应 generation config 目录；EnsureRunner 创建后 config 位于 g<gen>；并发 create/destroy 不互删。

### I7 内部 timeout/Shutdown 未携带 capability JTI（task_runner.py:260 + sandbox_runtime.go:217）

根因：deadline timer 调 `cancel_task` 不带 JTI；沙箱 cleanup 的 Shutdown 恒传空 JTI → 生产会话（有活跃 JTI 集）`_assert_task_capability` 拒绝，任务超时无法取消、优雅关闭必然失败。

修复：
1. `task_runner.py _on_timeout`：`adapter.cancel_task(task.task_id, session.workspace_key, session.runner_generation, task.capability_jti)`。
2. `worker/runtime.go Instance.Cleanup` 签名改为 `func(capabilityJTI string)`；loopback/static cleanup 忽略参数；`sandbox_runtime.go` cleanup 将参数传给 `client.Shutdown`。
3. `scheduler_worker.go`：cleanup 调用点传 `firstJTI(entry.credentials)`。

验证：Python 单测——timeout 触发 cancel 携带 JTI（有活跃集时 accepted）；Go 单测——cleanup 调用 Shutdown 带 firstJTI。

### I8 idle reaper 无法识别 Agent 卡死（task_drain.py:96 + scheduler_reaper.go:27）

根因：心跳由独立 drain 线程无条件发送并刷新 last_activity_at；Agent 卡死（LLM/工具 I/O）时 drain 线程仍心跳，idle reaper 永不触发。

修复（推进点心跳）：
1. `task_drain.py`：state 增加 `last_progress_at`（monotonic），在 `_handle_next_item`/`_handle_done_item` 取到 display item 时更新；`_handle_empty_poll` 心跳条件改为 `now - state.last_progress_at <= PROGRESS_WINDOW_S`（150s），超窗不发心跳（继续 poll，等平台 reaper 收割）。
2. 长 LLM 思考由 llm-proxy ResponseHeaderTimeout（120s 默认）兜底——思考超时必然返回/报错，agent 恢复推进，不会误收割。
3. 注释同步：scheduler.go / scheduler_reaper.go 说明心跳=推进信号。

验证：Python 单测——drain 长时间无 item 时不产出 heartbeat event；有 item 后恢复心跳。

## 验证门

1. Go：`go test -p 1 -count=1 ./...`（含 TEST_DATABASE_URL 独立库 genericagent_test）+ 4 关键包 `-race` + `go vet` + GOOS=linux build。
2. Python：worker-python 全量 pytest + bot_poller。
3. compose config OK；git diff --check OK。
4. 残余（仓库已声明，非本轮范围）：runsc/mTLS 端到端、真实六服务 compose 冒烟需部署主机。
