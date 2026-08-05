# 架构重构"任务即进程" P1: 删除凭证热刷新协议 — Execution Spec

**Goal:** 按决策 D1（PROGRESS.md "架构决策 D1" 段）删除封装层凭证热刷新协议。凭证语义改为**每任务签发、TTL 覆盖任务墙钟上限、任务终态撤销、无刷新**。

**Background（核查事实，2026-08-05）:**
- `credentialsNeedRefresh` 触发条件为 `ExpiresAt <= now + MaxTaskWallClock(45min) + TokenRefreshSkew(5min)`；签发 TTL 恒为 `DefaultTokenTTL = 1h`。**默认配置下刷新路径永不触发**（60min > 50min），热刷新协议在默认配置下是死路径，仅为管理员把 TokenTTL 调到 < 50min 的错误配置服务。
- 热刷新相关代码：Go 侧 `worker_credential.go` 6 个函数（`credentialsNeedRefresh` / `refreshWorkerCredentials` / `acknowledgePendingCredentialRefresh` / `isDefinitiveReloadRejection` / `rollbackPendingCredentialRefresh` / `flushPendingCredentialRevocations`）+ `pendingCredentialRefresh` 类型 + `workerCredentialSet.Generation/Checksum` + `pendingRevocations` 队列；调度侧 `scheduler_worker.go` 3 处调用点；proto `ReloadCredentials` RPC；Python 侧 `managed_agent.py reload_credentials` + `credential_config.py` generation/checksum 校验。

**Decisions（D1，用户已确认）:**
- D1.1 删除 ReloadCredentials RPC（proto + 生成的 Go/Python bindings + client 方法 + server handler）。
- D1.2 删除凭证 generation 与 config checksum 机制（Go 侧字段 + Python 侧校验）。**保留 RunnerGeneration**（Runner lease generation fencing，与凭证版本无关，属"不做什么"清单）。
- D1.3 凭证每任务签发（现状已是，`issueInitialWorkerCredentials` 绑定 task.ID），TTL 恒覆盖墙钟：有效 TTL = max(TokenTTL, MaxTaskWallClock + TokenRefreshSkew)，保证不变式 TTL ≥ 墙钟上限 + skew，任务墙钟内凭证永不过期。
- D1.4 终态撤销保留（现状已有，属"不做什么"清单）；`TokenRefreshSkew` 保留为 TTL 余量，`TokenTTL` 保留为可调下限。

**Constraints:**
- 只删热刷新协议，不动 checkpoint、JTI 签发/撤销（含 ctrl: control JTI）、Runner lease + generation fencing、消息/任务/交付事务链、政策/session files/交付安全、compose 拓扑。
- 每行先写测试（红）再删代码（绿）；删除场景的红 = 新不变式测试先行 + 删除后旧引用编译失败需同步清理。
- 后端测试 60s 超时；验证基线见 Final validation。
- proto 改动需重新生成 `backend-go/internal/gen/worker/v1`（protoc-gen-go v1.34.2 匹配）与 `worker-python/src/genericagent/worker/v1`（grpc_tools）。

**Non-goals:**
- 不做 P2（删常驻 Worker 生命周期）与 P3（容器生命周期简化）——分别独立子任务（SUBTASKS 22/23）。
- 不改 RunnerGeneration / capability JTI / checkpoint / lease fencing 语义。

**Architecture:** proto 删 RPC + 重生成 bindings → Go 凭证层删协议 + TTL 不变式测试 → 调度层删调用点 → 测试清理 → Python 侧删除 → 全量验证。

**Sync targets:** worker.proto ReloadCredentials 引用点（Go client/测试、Python handler/测试、契约测试 test_contract_sources.py / test_generated_bindings.py）、TokenTTL 语义（scheduler.go / worker_credential.go 签发 TTL）、credential_config.py metadata 字段、managed_agent.py session 状态。

**Final validation（P1 完成门）:**
```
cd tenant_platform/backend-go && TEST_DATABASE_URL='postgres://ga_r11_test:REDACTED@127.0.0.1:5432/genericagent_test?sslmode=disable' go test -p 1 -count=1 -timeout 600s ./...
go test -race ./internal/application ./internal/infrastructure/worker ./internal/infrastructure/workerclient ./internal/infrastructure/llmproxy
cd tenant_platform/worker-python && python -m pytest tests -q
TEST_DATABASE_URL=<同上> python -m pytest tenant_platform/tests/contract tenant_platform/tests/security tenant_platform/tests/integration tenant_platform/tests/smoke/test_foundation_smoke_unit.py -q
docker compose --env-file tenant_platform/infra/compose/.env.example.dev -f tenant_platform/infra/compose/compose.yaml config --quiet
GOOS=linux go build ./...
```
