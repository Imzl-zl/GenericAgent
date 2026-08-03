# 审查发现修复第四轮（14-review-round4）

## 背景

独立审查第三轮（13-review-round3）之后，只读代码审查再次发现 16 项问题（4C/9I/3M），涵盖调度生命周期、能力凭据、交付安全、checkpoint、Worker 启动、部署安全、Web/OpenAPI。本轮逐项修复，每项修复必须有失败测试先行（TDD 闭环）或现有测试扩展。

## 目标

修复以下问题（严重度排序）：

| # | 严重度 | 问题 | 位置 |
|---|--------|------|------|
| 1 | C | /new 任务重复派发互相销毁 Worker | scheduler.go:365, scheduler_dispatch.go:129 |
| 2 | C | dispatch 退出后无统一终态化 + Worker fence | scheduler_dispatch.go:200, scheduler_lease.go:83 |
| 3 | C | capability JTI 持久化非原子，终态不撤销 | scheduler_dispatch.go:175, checkpoint_store.go:152, task_lifecycle.go:390 |
| 4 | C | manifest.json 无界读取 OOM | session_files.go:415 |
| 5 | I | AttachRunnerContainer 失败 fail-open | sandbox_runtime.go:169 |
| 6 | I | checkpoint restore 无界 read_bytes | checkpoint.py:95 |
| 7 | I | RecordOutbound manifest 绕过 outputs/ 检查 | session_files.go:213 |
| 8 | I | /new reset 边界提前消费 + queued 不取消 | task_store.go:60, scheduler_worker.go:336 |
| 9 | I | capability budget 未计量 | llmproxy token.go:54, handler.go:95 |
| 10 | I | 默认 runc 非 fail-closed | compose.yaml:247, sandbox-manager main.go:38 |
| 11 | I | Worker 启动竞态 + 测试失败 | session_lifecycle.py:152, test_task_identity.py:277 |
| 12 | I | 失败 checkpoint 无清理 | checkpoint_store.go:23 |
| 13 | I | overlay 复制到持久可写 state | docker_cli.go:211, runtime_overlay.py:232 |
| 14 | M | bundle 身份校验不完整 | workspace.go:273, checkpoint.py:83 |
| 15 | M | Sophub 管理页无导航入口 | routes.tsx:61, AdminLayout.tsx:6 |
| 16 | M | OpenAPI 孤儿 schema | platform.yaml:919 |

## 非目标

- 不进行方案文档未要求的架构重构。
- 不修改已确认的残余验证（真实 Linux/Docker/runsc/mTLS 冒烟）所需代码语义。
- 不提交、不推送（除非用户要求收尾）。

## 验证策略

- Go：`go build ./...`、`go vet ./...`、非 DB 包测试 + `-race`；DB 包测试若 TEST_DATABASE_URL 可用则跑。
- Python：`python -m pytest tenant_platform/worker-python/tests`。
- Web：`npm run build`（如可用）或 tsc。
- Compose：`docker compose -f tenant_platform/infra/compose/compose.yaml --env-file .env.example config -q`。
- 每个修复项单独提交局部验证结果到 PROGRESS。
