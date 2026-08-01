# Task 3: Sandbox Manager — 执行 Spec

**Goal:** 实现 Sandbox Manager:固定、部署时审核的 Runner profile 创建/检查/销毁;仅挂载当前工作区 `memory/`、`temp/`、`state/` 三个确定 subpath;创建后 `docker inspect` 校验;idle 回收与孤儿清理;业务调度不进入 Manager。

**Decisions:** D13(本机 Docker 加固验证,生产 runsc 由部署配置选择,不做静默回退)、D14(限额:idle 30m/1GiB/CPU100000/PIDS128)、D15(无 Docker socket 暴露给 Runner)。

**Constraints:**
- Manager 是唯一能访问 Runner runtime socket 的组件;Platform/Web/Bot Poller 不持有
- Manager 从已认证 workspace_key 推导 volume subpath;task/渠道消息/SOP/工具调用不得传入路径、volume 名或挂载选项
- 不能只挂当前工作区目录则启动失败;严禁挂全局卷、任意宿主路径或其他工作区
- 创建后 inspect 校验:只挂三个 subpath、无 Docker socket、无 device、无 privileged、无 host bind mount、cap_drop=ALL、no-new-privileges、非 root、只读根
- 参考 `internal/application/document_manager.go` 宿主自检原则;不保留 document job 队列
- 高安全部署(专用 daemon)仅文档说明,不实现

**Non-goals:** 不做 mTLS(任务 4);不做业务调度/文件字节中转;不做 Sophub 调用。

**Architecture:** 新增 `cmd/sandbox-manager`;`internal/infrastructure/sandbox/` 包:profile 定义(固定,由部署 env 参数化但不可由业务输入选择)、docker CLI 封装、创建/校验/销毁/回收循环;系统边界保证只有 sandbox-manager 持有 Docker 权限(参考 document-manager 的 systemd 边界模式)。

**Final validation:** 单元测试(docker CLI 封装 mock);真实 Docker 环境 smoke:创建 Runner 后 inspect 与 server-side 推导的 source/destination/rw 完全一致;工作区 A/B 隔离写测试;idle 回收与孤儿清理测试。
