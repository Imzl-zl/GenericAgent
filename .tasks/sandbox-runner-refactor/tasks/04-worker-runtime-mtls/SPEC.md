# Task 4: SandboxWorkerRuntime + mTLS control plane — 执行 Spec

**Goal:** 实现 `SandboxWorkerRuntime`(worker.WorkerRuntime 的生产实现):通过 Manager 创建 Runner 容器并接入 Worker gRPC;Runner mTLS control plane(Platform control identity + Runner 短期服务证书);generation fencing;per-task capability 签发/撤销;llm-proxy 接入内部 `runner-control` 网络。

**Decisions:** D9(复用 llm-proxy)、D13、D14、D15(Runner 不持有任何原始 Key)。

**Constraints:**
- Runner 只加入 `runner-control` 网络;只能访问 Platform 受控 Worker/恢复/Sophub 端点和内部 LLM Proxy
- mTLS、generation、task capability 任一不匹配均拒绝 StartSession/ExecuteTask/CancelTask/Checkpoint/Shutdown
- 每个 Runner 使用绑定 runner_key hash + runner_generation 的短期服务证书;Platform 独立客户端身份;Runner 不持有可调用其他 Runner 的客户端凭据
- 凭证不共享:Worker 不获取 Provider 原始 Key(已有 capability 机制,扩展 task-scoped)
- 保留 session-key Worker 缓存与顺序调度(scheduler_worker.go 骨架)

**Non-goals:** 不做 staging state(任务 5);不做挂载细节(任务 3 已建,任务 5 补);不做 delivery(任务 6)。

**Architecture:** worker.proto 扩展(或新增握手协议);`internal/infrastructure/worker/runtime.go` 新增 `SandboxWorkerRuntime`,内部调用 Sandbox Manager client 创建/获取 Runner 并建立 mTLS gRPC;`worker_credential.go` 扩展 per-task capability(含 task ID/generation/操作/预算/过期);`cmd/llm-proxy` 保持复用,compose 网络调整。

**Final validation:** go test -race 全绿;mTLS 拒绝测试(无证书/错误身份/过期/generation 不匹配);capability 终态后不可复用测试;llm-proxy 仅监听 runner-control 且不映射宿主端口。
