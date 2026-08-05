# GA Sandbox Runner 重构方案（原生租户工作区）

> 状态：已实施（2025-08，仓库 main 分支）。
> 部署形态见 [Compose 部署说明](../infra/compose/README.zh-CN.md)：六常驻服务
> postgres/platform/bot-poller/web/llm-proxy/sandbox-manager，ga-runner 按工作区
> 活跃状态动态创建。方案第 10 节验证门中需真实 Linux 主机/runsc 的项仍待部署验证。
>
> **核心决定：** 渠道只是消息入口。个人用户或团队租户拥有一份完整、持久、可编辑的 GA 原生工作区；其中包含 `memory/`、`temp/`、项目、文件、会话历史和用户 SOP。一个活跃工作区独占一个隔离 Runner，Runner 从不在不同工作区间复用。

## 1. 重构目标

GA 原生是单人本地 Agent。它将模型对话、工作记忆、L1-L4 长期记忆、SOP、项目文件、附件、输出和任务日志放在同一个本地根目录。当前 Platform 一方面在自身容器内启动 Python Worker，另一方面又通过 Document Gateway、Document Manager 和 `ga-document-tool` 处理 DOCX，拆成两条执行路径。

目标不是重新设计 GA 的个人工作区，而是将它按用户虚拟化并放入隔离 Runner：

```text
微信 / QQ / 飞书 / 钉钉 / Web
            |
            v
     canonical_user_id
            |
            v
个人 GA 工作区：memory + temp + state
            |
            v
活跃期间唯一的 GA Sandbox Runner
```

- 同一已绑定用户从任何渠道发消息，都使用同一份个人记忆、SOP、项目、文件和活跃 Runner。
- 团队上下文是一个独立共享租户：已授权成员共同使用团队工作区、记忆、SOP、项目、文件和活跃 Runner；团队数据不进入任一成员的个人工作区。
- 同一 `runner_key` 的 task 串行执行；后到消息排队，不创建第二个 Runner。
- Runner 在固定镜像内原生执行 GA、命令、脚本和文档工具。DOCX、PDF、XLSX 是镜像能力，不再走文档专用 RPC、队列或容器池。
- 工作区的 `memory/`、`temp/` 与 `state/` 直接挂载给所属 Runner；GA 保持原生 `memory/` 与 `temp/` 路径约定，Worker adapter 仅将 `state/` 用作运行态快照。不会引入 Artifact Service 或 Platform -> Manager -> Runner 的字节中转。
- Runner 空闲、异常或部署替换时销毁；工作区和记忆保留，下次创建干净 Runner 后恢复。平台现有的新会话命令沿用既有语义，不把“新会话”定义为删除用户工作区。
- Platform 保留身份、任务顺序、账号密钥和容器控制权；Runner 不持有 Docker socket、数据库凭据、Provider 原始 API Key、Sophub API Key 或宿主目录。

仓库仍处于开发阶段，未承载需要保留的生产历史数据或在线会话。本次采用清库式切换：启用新部署前删除旧 PostgreSQL 数据卷及 Document 相关运行卷，再按新 schema 启动；不实现灰度双模式、旧 checkpoint 兼容、旧 document job 排空或人工回滚。

## 2. GA 原生状态拆分

原生代码中的 `agentmain.py`、`ga.py` 和 `plugins/project_mode.py` 依赖以下路径和运行态：

| 原生状态 | 原生位置 | 多租户归属 |
| --- | --- | --- |
| 模型完整上下文 | `llmclient.backend.history` | 当前工作区活跃 Runner；持久化到该工作区 `state/` 用于重启恢复 |
| Working Memory | `handler.working` 的 `key_info`、`related_sop`、项目激活态 | 当前工作区 `state/` |
| L1 记忆索引 | `memory/global_mem_insight.txt` | 当前工作区 `memory/` |
| L2 长期事实 | `memory/global_mem.txt` | 当前工作区 `memory/` |
| L3 经验、SOP、工具脚本 | `memory/` 下 Markdown/Python 文件 | 当前工作区 `memory/`，可自行修改 |
| L4 会话归档 | `memory/L4_raw_sessions/` | 当前工作区 `memory/` |
| 默认工作目录 | `temp/` | 当前工作区 `temp/` |
| 项目模式 | `temp/projects/<name>/project_memory.md` | 当前工作区 `temp/projects/` |
| 附件、输出、任务和模型日志 | `temp/` 下各目录 | 当前工作区 `temp/` |

所有**可变 GA 状态**按工作区独占；GA 代码、`assets/`、工具 schema 和 Runner 镜像层由所有工作区共用但始终只读。

`memory/` 与 `temp/` 保持 GA 原生的写穿语义：GA 在 task 中写入的记忆、SOP、项目和文件立即生效，任务失败、取消或超时不回滚。`state/` 只承担可恢复的会话快照职责，只有成功 task 才推进当前恢复指针。

## 3. 用户身份与串行调度

Platform 保存渠道账号到 `canonical_user_id` 的绑定关系。当前先支持微信；接入 QQ、飞书、钉钉时，用户需要经受认证的绑定流程将渠道账号关联到已有用户。

GA 不会自行推断两个渠道账号属于同一人。绑定后的个人上下文使用稳定键：

```text
runner_key = personal:<canonical_user_id>
workspace_key = personal:<canonical_user_id>
```

团队上下文使用独立共享键：

```text
runner_key = team:<team_id>
workspace_key = team:<team_id>
```

只有已授权的团队成员可将消息路由到对应 `team:<team_id>`；团队成员共享该团队工作区，个人与团队工作区始终隔离。

未绑定身份必须视为不同用户，不能共享 Runner、记忆、SOP 或文件。

Platform 按 `runner_key` 维护唯一 lease、任务顺序和全局 Runner 容量：

- 同一 `runner_key` 的后到 task 等待前一个 task 完成；
- 同一 `runner_key` 最多一个活跃 Runner；
- Runner 销毁后，下一次 task 只会为相同 `runner_key` 创建干净 Runner；
- `GA_RUNNER_MAX_ACTIVE` 是全局上限，不按用户另设 Runner 数量上限；容量已满时 task 保持 `queued`，等待空闲 Runner 回收或容量释放后按既有顺序继续，不能因为容量不足被终态化为失败；
- 不同工作区的 Runner、进程、网络身份、工作区和恢复状态完全隔离。

Runner lease 是持久控制面记录，而不是进程内缓存：记录 `runner_key`、lease owner、单调递增的 `runner_generation`、不可变 container ID、健康控制端点与到期时间。创建、复用、回收、孤儿清理和 Platform/Manager 重启都必须以该 generation fencing；旧 generation 即使进程仍存活，也不能再接收 task 或提交 state。

现有按 `session_key` 缓存 Worker 和按 session 顺序调度 task 的模型应保留；本次只将 Platform 内 Python 子进程换成工作区 Runner，禁止退化为每 task 一个容器。

## 4. 原生用户工作区

每个工作区拥有以下持久化目录：

```text
workspaces/<hash(workspace_key)>/
  memory/
    global_mem_insight.txt
    global_mem.txt
    sops/
    L4_raw_sessions/
    ... 用户自己的 SOP、脚本、经验库
  temp/
    attachments/
    outputs/
    projects/
    model_responses/
    tasks/
    ... 原生 GA 工作文件
  state/
    history.json
    working.json
    runner-state.json
```

Runner 镜像保持 GA 代码只读，Sandbox Manager 将当前工作区的持久 subpath 挂载到固定位置：

```text
/ga/legacy/memory          <- workspaces/<hash(workspace_key)>/memory
/ga/legacy/temp            <- workspaces/<hash(workspace_key)>/temp
/ga/runner-state           <- workspaces/<hash(workspace_key)>/state
/ga/runner-state/committed <- workspaces/<hash(workspace_key)>/state/committed (只读)
/ga/runner-state/results   <- workspaces/<hash(workspace_key)>/state/results (只读)
/ga/runner-config          <- workspaces/<hash(workspace_key)>/config/g<generation> (只读)
```

审查 C3: `state/committed` 与 `state/results` 以只读子挂载遮蔽顶层 rw `state` 挂载——Runner 不得删除/替换已提交快照与结果文件(Platform 写 committed/results 不受影响, 挂载 ro 是容器侧视图)。

另有生命周期 subpath `/ga/runner-config <- workspaces/<hash>/config/g<generation>`（只读），承载短期 mTLS 服务证书、策略清单与 task capability runtime 文件（审查 R5-C6/C1-I6）：config 按 generation 隔离, 内容仅存在于对应 Runner 容器生命周期内，容器销毁时由 Manager 按 workspace hash + generation（进程内 map 或容器 label）清理对应 config/g<gen> 子目录（DestroyRunner 与 EnsureRunner 共享 per-workspace 锁, 旧 generation 清理不得误删新配置），创建失败路径同样清理；残留私钥/token 不随 workspace 卷快照长期保存。

这样原生相对路径继续成立：`./temp` 是当前工作区 cwd，`../memory` 是当前工作区记忆。无需为每个工作区复制 `agentmain.py`、`ga.py`、`assets/` 或整个镜像。根文件系统保持只读；`memory/`、`temp/` 与 `state/` 是当前工作区可读写挂载，其中只有 Worker adapter 使用 `/ga/runner-state` 保存运行态。容器的 mount namespace 不包含全局工作区根或其他工作区 subpath，因此 GA 即使用绝对路径、`..` 或 shell 遍历文件系统，也看不到其他工作区。

- `memory-template/` 以固定 commit 的上游原仓库已跟踪 `memory/` 树构建，而不是从某次部署机器正在使用的 `memory/` 目录复制。上游已跟踪的 SOP、脚本和模板完整保留；Git ignored 的运行时状态、用户记忆、文件访问统计、私有配置和下载内容不进入模板。
- 首次创建工作区时，从 Runner 镜像内只读的上游基线 `memory-template/` 初始化一份默认 memory。之后用户和 GA 修改的都是自己的副本，不影响其他用户。
- 模板升级不得静默覆盖用户已经修改的 SOP。升级应显式创建新文件、产生待合并版本或由用户触发合并。
- Platform 保存入站附件并从当前工作区 `temp/outputs/` 交付生成文件；Runner 对自己的 `memory/`、`temp/` 和 `state/` 直接读写，不发生 Platform -> Manager -> Runner 的文件中转。
- Manager 只能从已认证的 `workspace_key` 推导 volume subpath；task、渠道消息、SOP 和 Agent 工具调用都不能传入路径、volume 名或挂载选项。
- 创建前 Manager 预置当前工作区的 `memory/`、`temp/` 与 `state/` 子目录，并设定固定非 root Runner UID/GID 的读写所有权。
- Manager 必须使用 Docker volume subpath 或等价的预置 scope-specific volume。若不能只挂当前工作区目录则启动失败；严禁挂整个全局工作区卷、任意宿主路径或其他工作区子目录。创建后必须通过 `docker inspect` 校验容器只挂载上述预期 subpath（含 committed/results 只读遮蔽与 config/g<gen> 归属），且不存在 Docker socket、其他卷或 host bind mount。
- Platform 只交付 `outputs/` 下的最终普通文件。Runner 若以符号链接或临时文件生成结果，必须先在 `outputs/` 内复制为独立普通文件；Platform 不跟随符号链接，并以受限根目录、文件描述符校验、文件类型和大小上限拒绝设备文件、管道、目录、越界路径及交付期间被替换的文件。正常 DOCX、PDF、XLSX 的读取和交付不受影响。
- Runner 销毁时不删除工作区。成功 task 的 state 不采用“最后一次文件写入即生效”的模型：Platform 在 task 已领取且当前 Runner generation 有效时创建带 task ID、generation 和 checkpoint token 的 state staging 记录；Worker 将裁剪后的 history、working memory 和项目激活态原子写入 `state/staging/<token>.json`，采用临时文件、`fsync` 和 rename，并返回 checksum。
- `memory/` 与 `temp/` 不进入上述 state 事务，始终保留原生写穿行为；失败、取消和租约丢失 task 留下的文件或记忆修改保留，但其 staging state 永不成为恢复点。
- Platform 校验 staging 文件、task ID、Runner generation、token、checksum 和任务租约后，在同一 PostgreSQL 事务中提交 workspace 当前 state 指针、`tasks.succeeded` 与 delivery outbox。重建 Runner 时只恢复该已提交指针指向的 state；取消、租约丢失或提交失败留下的 staging state 永不恢复，可由 Manager 清理。该协议仅回传控制元数据，不把用户文件经由 Manager 中转。

原生 L2 可以保存当前工作区自己的路径、项目事实、偏好和已验证环境信息。它不得保存 Platform 密钥、Provider 原始 Key、数据库凭据或其他工作区数据。用户的第三方账号密钥同样不写入 SOP 或 L2，而由 Platform 加密保存并按需授予短时能力。

## 5. 用户 SOP 与 SOPHub

### 5.1 用户 SOP

`memory/sops/` 属于当前工作区。个人用户可以维护自己的 SOP；团队租户中的已授权成员共同维护团队 SOP。GA 可以读取、创建、修改、整理和在自身工作区内执行 SOP 指示。SOP 不是全局注册表，不自动注入其他工作区 task。

系统默认 SOP 只用作首次初始化模板。运行时不存在所有工作区共享且可写的 `memory/`；个人或团队对自身 SOP 的修改不会影响其他工作区。

SOP 是提示和工作流内容，不是安全控制。即使 SOP 被用户或 GA 修改，Runner 仍不能访问 Docker socket、数据库、宿主机或其他用户工作区；这些权限由容器 profile 和 Platform capability 强制决定。

### 5.2 SOPHub 下载

SOPHub 使用一个由部署管理员维护的平台账号，不要求每个 GA 用户注册 SOPHub：

```text
当前工作区 Runner
  -> Platform Sophub proxy（task/workspace capability）
  -> Platform 使用加密保存的 Sophub API Key
  -> https://fudankw.cn/sophub/
  -> SOP Markdown 返回给当前 Runner
  -> 写入当前工作区 memory/sops/
```

- Sophub API Key 仅由 Platform 加密保存、解密和使用，绝不写入工作区 `memory/`、SOP、`temp/` 或 Runner 环境。
- Runner 只能通过 Platform 的受控 Sophub API 搜索和安装 SOP，不能获得该 Key，也不直接以管理员身份访问 Sophub。
- 默认只允许下载 Sophub 的公开、approved、single-file Markdown SOP；私有或付费内容必须经部署配置 allowlist 后才允许下载。
- 安装目标为当前工作区的 `memory/sops/<remote-sop-id>.md`。再次下载不能静默覆盖已被用户或团队修改的本地 SOP；更新必须显式保存新版本或要求用户确认。
- 现有“下载候选 -> 管理员审核 -> 全局 SOP Registry -> task snapshot 注入”路径不符合个人工作区模型，应整体删除。

## 6. 组件边界

| 组件 | 职责 | 明确禁止 |
| --- | --- | --- |
| Platform | 渠道身份绑定、用户工作区、记忆恢复、任务串行、Runner lease、文件交付、Sophub proxy、LLM capability、经认证的 Worker RPC 控制 | Docker socket、Docker exec 或修改 Runner 进程、跨用户读写工作区 |
| Sandbox Manager | 以固定 profile 创建、检查、销毁用户 Runner；空闲回收和孤儿清理；挂载已确定的用户 subpath | 业务调度、文件字节中转、Sophub 调用、任意 Docker 参数、宿主路径或业务命令 |
| GA Runner | 运行当前工作区的 GA 与镜像内工具，直接读写自己的 `memory/`、`temp/` 和 `state/` | Docker socket、数据库网络、其他工作区目录、Platform/Sophub/Provider 原始 Key、调用其他 Runner 的控制接口 |
| LLM Proxy | 复用现有透明 LLM Proxy：校验 task capability 后调用上游 Provider，并仅在自身进程内注入真实 API Key | 向 Runner 暴露上游 API Key、直接暴露公网 |
| Sophub proxy | 以 Platform 的 Sophub 账号搜索和获取可下载 SOP | 返回 API Key、全局加载用户下载的 SOP、越权访问私有内容 |

`LLM Proxy` 是 Platform 已有的透明代理，不是新增 GA 工具，也不改变 GA 的模型协议。Scheduler 为 Runner 写入固定 `mykey.py` 加载器和 task-scoped `mykey.runtime.json`：其中 `apibase` 指向内部 Proxy，`apikey` 是短期 capability 而非 Provider 原始 Key。GA 仍按原生 OpenAI/Claude 协议调用；Proxy 校验 capability、Provider、模型、revision 和路径后才注入真实 Key 并转发上游。

原生 `web_scan`、`web_execute_js` 和 `TMWebDriver` 当前连接宿主机浏览器标签页，是单人桌面能力。V1 必须禁用；未来只有实现“一用户一浏览器 profile/远程浏览器容器”后才能重新开放。

## 7. Runner 安全与生命周期

Sandbox Manager 为每个 Runner 使用固定、部署时审核的 profile，并在创建后 `inspect` 校验：

- 固定 digest 的 `ga-runner` 镜像，包含 GA Worker 和文档工具链；
- 不可信用户生产环境使用 `gVisor/runsc`；普通 Docker 加固仅限本地开发或明确可信内部场景，不能静默回退；
- 非 root、只读根文件系统、`cap_drop=ALL`、`no-new-privileges`、受控 seccomp、AppArmor/SELinux；
- 无 Docker socket、无设备、无 `privileged`、无宿主 bind mount；唯一持久化挂载是当前工作区的 `memory/`、`temp/` 与 `state/` subpath（`config/` 为容器生命周期绑定的短期材料，随容器销毁强制清理）；
- CPU、内存、PID 和单 task 时长受部署策略限制；V1 不引入每工作区硬磁盘配额，保持原生文件写入语义，仅保留既有单文件大小限制与宿主全局磁盘监控；
- Runner 只加入 `runner-control` 网络，只能访问 Platform 的受控 Worker/恢复/Sophub 端点和内部 LLM Proxy；不加入 `application`、`database` 或公网网络。
- Worker RPC 不能把共享网络当作身份边界：Runner 仅接受 Platform control identity 的 mTLS 请求；每个 Runner 使用绑定 `runner_key` hash 与 `runner_generation` 的短期服务证书，Platform 使用独立客户端身份。Runner 不持有可调用其他 Runner 的客户端凭据；mTLS、generation 和 task capability 任一不匹配均拒绝 StartSession、ExecuteTask、CancelTask、Checkpoint 与 Shutdown。

LLM、Sophub 和控制 capability 按 task 签发，绑定 `workspace_key`、`runner_key`、操作、预算和过期时间；任务终态后撤销，过期 token 不能被下一条 task 继续使用。

内部 LLM Proxy 复用现有 `cmd/llm-proxy`：它仅加入 `runner-control` 与 `database` 网络，不映射宿主机端口；Runner 只能访问其内部地址，Proxy 从加密 Provider store 读取真实 Key 和 capability 吊销记录。

```text
1. 渠道消息到达；Platform 将渠道身份解析为个人 `canonical_user_id`，或解析为已授权的团队上下文。
2. Platform 得到 `workspace_key` 与 `runner_key`，并按 `runner_key` 串行入队；全局 Runner 容量已满时保持 queued。
3. Scheduler 为可启动 task 获取该工作区 lease(generation fencing)；任务即进程(决策 D1)：每个 task 请求 Manager 创建全新 Runner。
4. Manager 用固定 profile 创建 Runner，仅挂载该工作区 memory/、temp/ 和 state/，生成新的 Runner generation，等待经 mTLS 认证的 Worker 就绪。
5. Platform 为当前 task 签发控制、LLM 和必要的 Sophub capability，通过经认证的 Worker RPC 调用 Runner ExecuteTask。
6. Runner 原生读写工作区；SOPHub SOP 下载后保存到该工作区 `memory/sops/`。
7. 成功 task 的 Worker state 经过 token/generation 校验并与任务成功原子提交；任务终态(成功/失败/取消/超时)即销毁 Runner 并释放 lease，下一任务以 generation+1 冷启动全新容器，会话连续性由 checkpoint 快照恢复保证。
8. Runner 异常或部署替换时 Manager 销毁容器(孤儿清理兜底)；工作区保留。已有的新会话命令沿用当前行为：不恢复 history/working state，但不删除 memory/、temp/、SOP 或项目文件。
```

持有共享 Docker daemon 的普通 `docker.sock` 的 Manager 等价于宿主级受信组件。固定模板只能限制 task 输入，不能隔离 Manager 自身被攻破后的影响。高安全部署应使用专用 Runner daemon、受限 runtime proxy 或独立 Runner 主机，且该控制面不得管理 Platform、PostgreSQL、其卷或其他 Compose 服务。

## 8. 部署形态

最终 Compose 常驻服务：

```text
postgres
platform
bot-poller
web
sandbox-manager
llm-proxy
```

`ga-runner` 不是常驻服务，而是 Manager 按工作区活跃状态动态创建的容器。Manager 是唯一能访问 Runner runtime socket 的组件；Platform、Web、Bot Poller、PostgreSQL 和 Runner 不持有该权限。

`llm-proxy` 复用现有 `cmd/llm-proxy`，是常驻内部服务：连接 `database` 网络读取加密 Provider 配置和 capability 吊销记录，连接 `runner-control` 网络接受 Runner 请求，不发布宿主机端口。

拟议 `.env` 只增加部署级参数，例如：

```env
GA_WORKER_EXECUTION_MODE=user_workspace_runner
GA_RUNNER_SECURITY_PROFILE=runsc
GA_RUNNER_IMAGE=registry.example/ga-runner@sha256:...
GA_RUNNER_MAX_ACTIVE=4
GA_RUNNER_IDLE_TTL=30m
GA_RUNNER_MEMORY_BYTES=1073741824
GA_RUNNER_CPU_QUOTA=100000
GA_RUNNER_PIDS_LIMIT=128
GA_RUNNER_TASK_TIMEOUT=300s
GA_LLM_PROXY_ADDR=http://llm-proxy:8081
```

具体字段以实现阶段的配置契约为准。`GA_RUNNER_MAX_ACTIVE` 是全局容量：Scheduler 在 Runner capacity 不足时不将 task 标记失败，而是保留 queued 并在容量释放后重新调度。发布配置以固定 digest 和 allowlist 定义 profile；渠道消息、用户设置和 Agent 输入均不能选择镜像、挂载、Docker 参数、网络或 Sophub 权限。

## 9. 代码改造范围

| 阶段 | 改造内容 | 主要位置 |
| --- | --- | --- |
| 1 | 定义渠道账号绑定、`canonical_user_id`、个人/团队 `workspace_key` 与 `runner_key`、带 generation fencing 的持久 Runner lease、全局容量排队、state staging/commit 与串行调度契约；开发期清库后按新 schema 启动 | `internal/domain/`、`internal/application/`、PostgreSQL migration、Compose 发布说明 |
| 2 | 构建固定 `ga-runner` 镜像；将 GA 代码、assets 和固定上游 commit 的完整已跟踪 memory template 固化为只读层 | 新增 `infra/compose/ga-runner.Dockerfile`、调整 `worker-python/`、记录上游基线 commit |
| 3 | 以 Document Manager 的宿主自检、空闲回收和孤儿清理原则实现新的 Sandbox Manager；固定 Runner profile 不向业务层开放任意 Docker 参数，不保留 document job 队列 | `internal/infrastructure/document/`、`internal/application/document_manager.go`、新 Sandbox Manager |
| 4 | 实现 `SandboxWorkerRuntime`，保持 session-key Worker 缓存和顺序调度，只替换 Platform 本地 Python 子进程；增加 Runner mTLS control plane、generation fencing 与每 task capability 下发/撤销；将现有 `cmd/llm-proxy` 接入 Compose 内部 `runner-control` 网络 | `internal/infrastructure/worker/runtime.go`、`internal/application/scheduler_worker.go`、`scheduler_dispatch.go`、`worker_credential.go`、LLM Proxy、Worker proto、`cmd/platform/main.go`、`cmd/llm-proxy`、Compose |
| 5 | 以工作区 subpath 挂载原生 `memory/`、`temp/` 与 `state/`；`memory/`、`temp/` 保持原生写穿，Worker adapter 仅将成功 task 的 history、working memory 与项目激活态写入 token-scoped staging state 并由 Platform 提交恢复 | `worker-python/`、checkpoint、Manager mount profile |
| 6 | 将附件、输出和文件 marker 统一到工作区 `temp/`；要求 Runner 输出独立普通文件，Platform 以文件描述符安全交付，不跟随符号链接 | `session_files.go`、`delivery_service.go`、Worker 文件工具 |
| 7 | 以 Sophub proxy + 用户 `memory/sops/` 替换全局 SOP Registry、候选审核和 task snapshot 注入 | `sophub_service.go`、`sop.go`、`sop_http.go`、Sophub migration、Worker SOP 工具 |
| 8 | 开发期清库后删除 Document Gateway、Document Manager、document job 队列、DOCX 专用镜像、全局 SOP Registry 及对应旧 schema/migration 引用；在 Runner 镜像和工具 schema 中禁用宿主浏览器入口；更新 Compose | `document_*`、`cmd/document-manager`、`document-image`、`worker-python/`、工具 schema、Compose、PostgreSQL migration list |

## 10. 验证门

完成以下自动化和真实环境证据后，才允许启用用户 Runner：

- 同一已绑定用户从微信和第二个渠道提交消息，均命中同一个 `personal:<canonical_user_id>`、个人工作区、记忆和活跃 Runner；未绑定身份不能共享它们；
- 已授权团队成员提交消息均命中同一个 `team:<team_id>` 工作区和活跃 Runner；团队成员可见团队数据，但不能读取任一成员的个人工作区；
- 新用户的初始 memory 必须与记录的上游 commit 的已跟踪 `memory/` 基线一致；本机 Git ignored 的 `global_mem*`、文件访问统计、私有配置和下载内容均不能进入模板或新用户工作区；
- 同一 `runner_key` 的 task 串行执行，后到消息排队，不产生第二个 Runner；`GA_RUNNER_MAX_ACTIVE` 已满时 task 保持 queued，空闲 Runner 回收或容量释放后按既有顺序继续；不同工作区不能访问彼此的 memory、temp、SOP、文件、history 或 Runner；
- Runner 可直接读取工作区附件、生成并交付 DOCX；每个成功 task 后的 state 快照必须可被独立校验。强制杀死 Runner 后重建，必须恢复该工作区最近一个成功 task 的 L1/L2、项目记忆、history 和 working state，且不能读取部分写入或损坏的快照；取消、租约丢失或数据库提交失败后创建的 staging state 不得成为恢复点。`memory/`、`temp/` 的原生写穿修改仍保留，不要求回滚；
- Manager 只能挂载当前工作区的 `memory/`、`temp/`、`state/` subpath；创建后 `docker inspect` 必须与 server-side 推导的 source/destination、读写模式完全一致。工作区 A 在这些目录写入测试文件后，工作区 B 的 Runner 通过相对路径、绝对路径、`..`、符号链接或 shell 遍历均不能读取、修改或列出该文件；挂载全局卷、其他工作区目录、任意 host path 或任意 profile 必须失败；
- Platform 能交付正常 DOCX、PDF、XLSX 普通文件；对符号链接、特殊文件、路径逃逸、越界文件、超大文件或检查后被替换的文件必须拒绝，并且不能读取链接目标；
- LLM Proxy 只在内部 `runner-control` 网络监听；Runner 以原生 OpenAI/Claude 协议成功调用模型，运行时配置、Runner 环境、工作区、日志和 API 响应中均不存在 Provider 原始 API Key；
- Sophub API Key 只以密文存在于 Platform；Runner 环境、工作区、日志和 API 响应中均不存在该 Key；
- SOPHub 搜索与下载仅能使用公开 approved Markdown；下载结果只写当前工作区 `memory/sops/`，不出现在其他工作区或全局 task 注入中；
- `docker inspect` 验证 runtime、非 root、只读根、capability、seccomp、网络和 cgroup 限额；内部 LLM Proxy 仅连接 `database` 与 `runner-control` 网络，且不映射宿主机端口；
- 未持有 Platform control mTLS identity 的客户端，以及工作区 A 的 Runner，均不能调用工作区 B 的 Worker RPC；过期或 generation 不匹配的控制请求必须失败；
- task capability 必须包含 task ID、Runner generation、操作、预算和过期时间；任务终态后不可用于下一 task，过期、取消、Runner 崩溃、Manager 重启和 Platform 重启均能恢复或清理，不让 Runner 跨工作区复用；
- 原生宿主浏览器接口在工作区 Runner 中不可用；
- `docker compose up -d --build` 冒烟覆盖 Platform、内部 LLM Proxy、动态个人/团队 Runner、工作区持久化、SOPHub 工作区安装和 DOCX。

## 11. 实施前确认

| 决策 | 推荐 | 影响 |
| --- | --- | --- |
| 运行粒度 | 任务即进程(决策 D1)：每个 task 一个全新 Runner 容器 | 会话连续性由 checkpoint 快照恢复保证；冷启动成本远低于状态机复杂度 |
| 团队 | `team:<team_id>` 是共享租户 | 已授权成员共享团队工作区；个人与团队数据隔离 |
| Runner 容量 | `GA_RUNNER_MAX_ACTIVE` 为全局上限，满载保持 queued | 不增加每用户 Runner 上限；容量不足不使 task 失败 |
| 用户状态 | 每工作区完整原生 `memory/`、`temp/` 与 `state/` | 最大限度复用 GA 路径；`memory/`、`temp/` 写穿，只有成功 state 可恢复 |
| 新会话 | 沿用现有 GA/Platform 会话语义 | 不恢复 history/working state，不删除 `memory/`、`temp/`、SOP 或项目文件 |
| 初始 memory | 固定上游 commit 的完整已跟踪 `memory/` 基线 | 保留原生 GA SOP/脚本；不下发本机 Git ignored 的运行时状态和私有配置 |
| SOP | 每工作区独立、可修改，首次由 template 初始化 | 个人 SOP 不影响其他工作区；团队成员共享团队 SOP |
| SOPHub 账号 | 部署管理员维护一个平台账号 | Key 只在 Platform，下载结果按工作区私有安装 |
| LLM 调用 | 复用现有透明 `llm-proxy` 作为内部 Compose 服务 | GA 保持原生协议；Runner 只有短期 capability，真实 Provider Key 只在 Proxy |
| 用户身份 | 渠道账号经认证绑定到 canonical 用户；团队成员须有团队授权 | 跨渠道个人记忆必须先绑定；团队只路由到对应共享工作区 |
| 存储配额 | V1 不做每工作区硬配额 | 保持原生写入；依赖单文件限制与宿主全局磁盘监控 |
| 开发期切换 | 删除旧 PostgreSQL 数据卷和 Document 相关运行卷后启动新 schema | 无旧数据兼容、迁移排空或回滚成本 |
| 不可信生产运行时 | `gVisor/runsc` | 需要 Linux Docker host 一次性安装 runtime |
| Runner 回收 | 任务终态即销毁；异常/部署替换/孤儿由 Manager 兜底清理 | 任务容器无 idle 驻留，残留进程只来自异常路径 |
| 浏览器能力 | V1 禁用 | 宿主浏览器不能用于多租户；后续需每用户独立浏览器 |
| 高安全部署 | 专用 Runner daemon 或独立 Runner 主机 | 避免共享 Docker daemon 的宿主级控制面风险 |

## 12. 参考

- [Docker Engine Security](https://docs.docker.com/engine/security/)
- [gVisor Security Model](https://gvisor.dev/docs/architecture_guide/security/)
- [GKE Sandbox](https://docs.cloud.google.com/kubernetes-engine/docs/concepts/sandbox-pods)
- [Firecracker Production Host Setup](https://github.com/firecracker-microvm/firecracker/blob/main/docs/prod-host-setup.md)
- [Kubernetes Pod Security Standards](https://kubernetes.io/docs/concepts/security/pod-security-standards/)
- [Sophub](https://fudankw.cn/sophub/)
