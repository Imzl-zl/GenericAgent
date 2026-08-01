# Task 8: 删除 document 系统 + 禁用浏览器 + Compose 收尾 — 执行 Spec

**Goal:** 开发期清库后删除 Document Gateway、Document Manager、document job 队列、DOCX 专用镜像、全局 SOP Registry 及对应旧 schema/migration 引用;禁用宿主浏览器入口;Compose 更新为最终常驻形态:postgres/platform/bot-poller/web/sandbox-manager/llm-proxy;通过方案第 10 节验证门。

**Decisions:** D12(清库)、D15(浏览器禁用)、D17(删除范围)。

**Constraints:**
- 删除后代码库无 document_* / sophub registry 引用;go test 全绿
- `web_scan`、`web_execute_js`、`TMWebDriver` 在 Runner 中不可用(V1 禁用)
- Compose 最终服务:postgres/platform/bot-poller/web/sandbox-manager/llm-proxy;ga-runner 动态创建
- Manager 是唯一持有 Docker socket 的组件;platform/web/bot-poller/postgres/runner 不持有
- 方案第 10 节验证门全部通过才允许启用用户 Runner

**Non-goals:** 不做灰度/回滚(D12);不做高安全 daemon 形态实现(D16)。

**Architecture:** 删除 `cmd/document-manager`、`cmd/document-tool`、documentgateway、document 相关 application/infrastructure 代码与 migration 0031-0034 文件;删除 sophub registry 剩余引用;工具 schema 禁用浏览器工具;compose.yaml 重写;最终冒烟验证。

**Final validation:** 删除后 `go test ./... -race` 全绿;`docker compose up -d --build` 六服务健康;浏览器工具调用失败;方案第 10 节验证门清单逐项证据。
