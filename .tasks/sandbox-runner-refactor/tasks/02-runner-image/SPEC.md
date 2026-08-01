# Task 2: ga-runner 镜像 — 执行 Spec

**Goal:** 构建固定 digest 的 `ga-runner` 镜像:GA 代码、assets、worker-python 与文档工具链;上游 commit 9355c22d7 的完整已跟踪 `memory/` 树固化为只读 `memory-template/` 层;非 root、只读根文件系统。

**Decisions:** D6(基线 9355c22d7,42 文件已核实)、D13(Docker 加固为本机验证路径)、D15(镜像内禁用宿主浏览器入口)。

**Constraints:**
- Git ignored 的运行时状态、用户记忆、文件访问统计、私有配置、下载内容**不得**进入模板
- 模板升级不得静默覆盖用户已修改 SOP(本任务只固化基线,合并策略在任务 7)
- 镜像固定 digest 引用(compose 用 sha256),不适用 mutable tag

**Non-goals:** 不做挂载/子路径(任务 3/5);不做 mTLS/capability(任务 4);不修改 worker-python 的 state 逻辑(任务 5)。

**Architecture:** 新增 `infra/compose/ga-runner.Dockerfile`;构建时从 git 提取 `git ls-tree -r 9355c22d7 -- memory/` 生成 memory-template 层;worker-python 作为依赖装入;验证脚本断言模板与上游树一致且无 ignored 文件。

**Final validation:** `docker build` 成功;容器以非 root 用户、只读根启动(只读验证 `docker run --read-only`);模板与 `git ls-tree` 逐文件一致;`git check-ignore` 扫描模板内无 ignored 文件。
