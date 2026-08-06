# 全项目健康清理（Round17）

**Goal:** 清除 Round16 后全项目健康诊断发现的 ~40 项遗留问题：废弃源码、语义漂移、文档偏移、冗余设计，全部有实据并逐项验证交付。

**Decisions:**
- D-A（user, 2026-08-06 确认）: **fudankw.cn 是用户需要使用的技能平台**（`https://fudankw.cn/sophub/` 供 GA 自动搜索技能，对应 worker-python policy 的 sophub_search/sophub_install 工具），安装脚本来源链路是有意设计，保留不动；README/脚本内表述不一致仅记录观察项，不改
- D-B（user, open）: web StatusPage.tsx 占位页处理（标注未实现/下线/补实现）——产品决策，执行前确认
- D-C（user, 2026-08-06 确认）: **GA 根项目是第三方代码（黑盒）**——根目录代码/frontends/根 docs 一律不动，除非我们的项目设计或需求明确需要；stapp2/tuiapp v1/v2/tools_schema_cn.json 等全部移出清理范围

**Constraints:**
- 零调用死代码删除以 rg 引用计数为证；删除后必须跑对应域验证
- 契约（proto/openapi）是单一真值源，本次不动任何契约表面
- 不改 GA 运行时行为（故障语义、心跳、配额等 Round15/16 已收敛真值不动）
- 平台产物（platform.exe 等）是构建产物，不在清理范围
- mykey.py 真实密钥不触碰；文档默认值漂移只改注释/文档
- `memory/` 应用运行时记忆目录不批量删改，仅标注孤儿候选
- backend-go 测试共享 Postgres 需 `-p 1` 串行；`TEST_DATABASE_URL` 缺失时 DB 套件显式失败属预期

**Non-goals:**
- **GA 根项目（第三方黑盒）全部不动**：根目录 *.py/frontends/ga_cli/根 docs/README.md/tools_schema_cn.json 等，除非我们的项目设计或需求明确需要
- 不重构（不改架构、不拆 C5 delivery_service 834 行等纯结构债）
- 不做新功能（StatusPage 补实现是选项之一，需用户确认才做）
- 不改 B3 wechat/ilink 命名债（API 契约字段，等契约变更窗口）
- 不处理 tenant_platform 根目录本地产物（platform.exe/log/runtime/ 均已 gitignore）
- 不批量加载/改写 memory/archive/
- 不动安装脚本来源（fudankw.cn 是有意使用的平台）

**Architecture:**
清理范围仅限 tenant_platform（backend-go → worker-python/bot_poller → web → 平台文档/.tasks）；GA 根项目黑盒不动。每批先删死代码（低风险、引用计数为证）再做文档校准；用户决策点（D-B）在批次内以一次一问确认。每批跑该域最小验证，收尾做全量验证。

**Final validation:**
- Go：`go vet ./...` + `go build ./...` + `go test -p 1 -count=1 -timeout 300s ./...`（DB 套件在 TEST_DATABASE_URL 下）
- Python 根：`python -m pytest tests -q`；平台：`python -m pytest tenant_platform/tests/contract tenant_platform/tests/security tenant_platform/tests/smoke -q`
- Web：`npm run lint` + `npm run build`
- 文档：grep 抽查无残留旧命名（PER_TENANT/PLATFORM_DEV/DevToken/常驻 Worker/document gateway 概念）
- 全仓库：死代码符号引用计数复检为 0

**Deliverables:**
1. backend-go：token_revoker.go + 13 死 store 方法 + 死 domain 方法/常量 + migrations 残留 + 一次性脚本
2. worker-python/bot_poller：旧 Dockerfile + 死参数 + 空块 + 旧概念注释
3. web：无引用资源 + StatusPage 决策（D-B）
4. 平台文档（tenant_platform/docs）：认证头/路径/方法表/阶段表述/历史文档标注
5. .tasks 进度真值刷新（sandbox-runner-refactor 等）
6. 收尾全量验证
