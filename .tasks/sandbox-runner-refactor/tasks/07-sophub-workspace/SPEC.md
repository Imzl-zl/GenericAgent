# Task 7: Sophub proxy + 用户 memory/sops/ — 执行 Spec

**Goal:** 以 Sophub proxy + 用户 `memory/sops/` 替换全局 SOP Registry、候选审核和 task snapshot 注入;Runner 经 Platform 受控 proxy 搜索/下载 SOP 到当前工作区;Sophub API Key 只以密文存在于 Platform。

**Decisions:** D7、D8、D17(旧 registry 删除)。

**Constraints:**
- Sophub API Key 由 Platform 加密保存/解密/使用,绝不写入工作区 memory/SOP/temp/Runner 环境
- Runner 只能经 Platform 受控 Sophub proxy,不能获得 Key
- 默认只允许公开 approved single-file Markdown;私有/付费需部署 allowlist
- 安装目标 `memory/sops/<remote-sop-id>.md`;再次下载不静默覆盖已修改 SOP
- 删除"下载候选→管理员审核→全局 Registry→task snapshot 注入"路径

**Non-goals:** 不做 SOPHub 多账号/用户注册;不做跨工作区 SOP 共享。

**Architecture:** 保留 `internal/infrastructure/sophub/client.go` 搜索能力;Platform 增加 task/workspace capability 限定的 proxy 端点;Worker SOP 工具改为写工作区;migration 删除 0035 相关表(sophub_bindings/sop_candidates/sop_versions 等,连同任务 8)。

**Final validation:** 下载写工作区测试;Key 不泄露断言;重复下载不覆盖已修改 SOP;旧表删除后代码无引用。
