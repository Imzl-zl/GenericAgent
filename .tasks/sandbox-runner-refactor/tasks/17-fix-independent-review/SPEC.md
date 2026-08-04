# [17] 独立审查发现修复（Round 8）

**Goal:** 修复独立 reviewer 确认的 8 个真实问题（1 凭据泄露 / 2 入站消息消费时机 / 1 授权前写库 / 2 Runner 进程生命周期 / 1 Manager 死锁与配置残留 / 1 附件链路 / 1 交付快照碰撞），并用 dbx + 本机环境完成验证。

**Decisions:**
- D1 (user, 来自用户指令"都优化一下"): 8 个已确认问题全部修复，不讨论取舍。
- D2 (model): 幂等标记从"处理前"改为"成功处理后"（Check/Mark 拆分），失败路径让 Poller 重试；任务提交先于消息入库，避免重试丢任务。
- D3 (model): Manager 销毁重构为持锁私有变体 destroyRunnerLocked，EnsureRunner 内部销毁不再重入公开 DestroyRunner；所有销毁路径统一清理 config/g<gen>。
- D4 (model): media_paths 在 Router 入口绑定 BotMediaRoot（EvalSymlinks + rel 检查）；源文件复制 O_NOFOLLOW + 普通文件 + 大小上限。
- D5 (model): ga.py 注册表保存 PGID，终态直接 killpg；inline_eval 在 Runner 镜像禁用（GA_DISABLE_INLINE_EVAL=1）。
- D6 (model): 附件落盘名用内容 hash 前缀 + 原始名，Poller 恢复原始显示名；delivery 快照 key 用 8 字节 hash。
- D7 (model): PROGRESS.md 中的 TEST_DATABASE_URL 真实密码替换为占位符；提醒用户轮换（已推送 GitHub）。

**Constraints:**
- 不改变既有 API 契约（transport adapter 方法名变化需同步全部实现与测试）。
- 保持 fail-closed 语义；不引入静默兜底。
- 兼容现有测试（router_test / poller test / manager_test 的既有断言按新语义更新）。
- Windows 本机无法验证 POSIX 进程组行为，ga.py 清理逻辑用单测 + Linux Docker 冒烟（如可用）验证。

**Non-goals:**
- 不实现 bot_media 磁盘配额/清理策略（方案 V1 明确不做每工作区配额）。
- 不重写既有幂等 DB 唯一键设计（任务行唯一键保留为跨实例兜底）。
- 不修改 checkpoint/lease/fencing 核心逻辑（审查未发现新缺陷）。

**Architecture:** 每个问题一个独立任务，TDD 循环（先失败测试后实现）。Go 侧改动集中在 router/transport（入站顺序）、sandbox/manager（销毁路径）、session_files/safefs（附件源加固）、delivery_service（快照 key）；Python 侧在 ga.py（进程清理）与 bot_poller/wxbot_media（附件命名）。验证用 dbx 跑 DB 套件 + 全量单测 + vet + compose config。

**Final validation:** `go test -p 1 -count=1 ./...`（含 TEST_DATABASE_URL）+ `go vet ./...` + worker-python 单测 + bot_poller 单测 + 新增回归测试全部通过；`git diff --check` 干净；残余风险（runsc/真实 Sophub）如实记录。
