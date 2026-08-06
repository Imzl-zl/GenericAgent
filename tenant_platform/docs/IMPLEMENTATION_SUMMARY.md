# LLM Provider 透明代理切分 — 实现总结

## 当前架构结论

Tenant Platform 已完成 **透明 LLM 凭证代理** 切分：

- Admin 仅通过 **`/v1/admin/llm-providers`** CRUD
- Provider 类型严格 **`native_oai` | `native_claude`**
- 嵌套 **`session_config`**（GA）与 **`transport_config`**（Proxy→上游）
- API Key **加密落库**，列表/详情 **永不回传明文**
- Scheduler 生成会话级 **`mykey.runtime.json`** + 固定 **`mykey.py` 加载器**（仅 capability JWT + Proxy URL）
- Worker **委托真实 GA Core** 执行协议
- Transparent Proxy 校验 capability / provider / model / revision / path 后注入上游密钥

不再提供明文配置拉取接口，也不再依赖手工 mykey 生成测试脚本。

## 已完成能力

### 1. 数据与领域模型

- `LLMProvider`：`provider_type`、加密 key 字段、`session_config`、`transport_config`、`revision`、默认与状态
- `GASessionConfig.Validate(providerType)` / `ProviderTransportConfig.Validate()` 严格校验
- 名称或密钥-only 更新保留 routing `revision`；运行语义与 state 变化递增

### 2. Admin HTTP

- `POST/GET/PUT/DELETE /v1/admin/llm-providers`
- `GET /v1/admin/llm-providers/{provider_id}`
- `POST /v1/admin/llm-providers/{provider_id}/default`
- `POST /v1/admin/llm-providers/{provider_id}/disable|enable`
- 创建必填 `api_key`；更新省略或空 key = 不轮换
- 响应映射含嵌套配置与 `revision`，无明文 key

### 3. Scheduler 与会话凭证

- 活动 Provider 路由快照
- 按 Provider 签发 capability JWT（覆盖任务墙钟等约束）
- `BuildRuntimeConfig`：token-only JSON + 固定 loader
- 持久化 capability 吊销；会话/Worker 拆除时 best-effort revoke
- Scheduler 周期清理已过期的 capability 撤销记录
- 密钥-only 轮换不强制替换已绑定 Worker；抬升 revision / 路由变更则替换并吊销旧集

### 4. Worker / GA

- 会话目录加载 runtime JSON（经固定 `mykey.py`）
- 真实 GA `NativeOAISession` / `NativeClaudeSession`
- 出站 base 指向 Proxy，凭证为 capability，非上游 Key

### 5. Transparent LLM Proxy

- 部署单元持有解密后的上游密钥（来自加密 Provider store）
- 路径：`/v1/chat/completions`、`/v1/responses`、`/v1/messages`（及类型允许的别名）
- 流式 / SSE 透传
- 头清洗 + 按 `transport_config` 注入
- DB 级吊销在重启后仍有效

### 6. 前端

- Provider 表单：类型下拉、`session_config` / `transport_config`
- 列表支持设默认、enable/disable、编辑与删除；不展示明文 key

## 心跳/超时部署契约（审查 F4）

Worker 心跳（推进信号）、llm-proxy 响应头超时与平台 idle reaper 的取值必须保持：

```
PROGRESS_WINDOW_S (worker, 150s) > defaultResponseHeaderTimeout (llm-proxy, 120s)
                                     < TASK_IDLE_TIMEOUT_SECONDS (platform, 默认 300s)
```

- **窗口 > 代理超时**：LLM 长思考由代理超时兜底（必然返回/报错），agent 恢复推进，心跳不停；
- **代理超时 < idle 阈值**：健康长任务不会被 idle reaper 误收割；
- worker 心跳间隔 `HEARTBEAT_INTERVAL_S=30s`，平台侧 RecordHeartbeat 刷新 `tasks.last_activity_at`；
- `last_progress_at` 以 put_task 时刻为基线（任务启动到首个 display 事件之间也在推进窗口内）。

## 核心优势

1. **密钥边界清晰** — 明文 Key 只在加密存储与 Proxy 内存路径
2. **复用 GA 协议** — Platform 不重做 chat/responses/messages
3. **可吊销短时能力** — JWT + 持久吊销，替代把 Key 铺进每个 Worker
4. **修订与轮换分离** — 密钥-only 可保 revision；路由/模型等变更走新快照

## 使用指南

### 添加 Provider（UI）

1. 管理员登录 → LLM Provider 管理
2. 添加：名称、类型（`native_oai` / `native_claude`）、Base URL、Model、API Key
3. 填写 `session_config` / `transport_config`
4. 保存；需要时设为默认

### 添加 Provider（API）

```bash
curl -X POST http://127.0.0.1:8080/v1/admin/llm-providers \
  -H "Authorization: Bearer <admin_token>" \
  -H "Content-Type: application/json" \
  -d '{
    "name": "my-gpt",
    "provider_type": "native_oai",
    "base_url": "https://api.openai.com/v1",
    "model": "gpt-4o-mini",
    "api_key": "sk-...",
    "session_config": {
      "thinking_type": "adaptive",
      "max_tokens": 8192,
      "temperature": 1.0
    },
    "transport_config": {
      "auth_mode": "auto"
    }
  }'
```

### 运行时路径（无需手工拉配置）

1. 平台 Scheduler 在确保 Worker 时发 token 并写会话 `mykey.runtime.json` + loader
2. Worker 启动 GA，请求经 Proxy
3. Proxy 校验并注入真实 Key 后访问上游

运维关注：`DATABASE_URL`、`BOT_TOKEN_KEY`、capability 签名密钥、Proxy 监听地址与 Platform 侧 `llm-proxy-addr` / 进程内 Proxy 配置一致。

## 支持的类型

| `provider_type` | 说明 |
|-----------------|------|
| `native_oai` | OpenAI 兼容（含兼容网关） |
| `native_claude` | Anthropic Messages |

其他历史类型名已废弃，Admin 与领域校验只接受以上二者。

## 配置示例

### OpenAI 兼容

```json
{
  "name": "openai-gpt4",
  "provider_type": "native_oai",
  "base_url": "https://api.openai.com/v1",
  "model": "gpt-4o-mini",
  "api_key": "sk-...",
  "session_config": {
    "thinking_type": "adaptive",
    "max_tokens": 8192,
    "temperature": 1.0
  },
  "transport_config": { "auth_mode": "auto" }
}
```

### Claude

```json
{
  "name": "claude-sonnet",
  "provider_type": "native_claude",
  "base_url": "https://api.anthropic.com",
  "model": "claude-sonnet-4-20250514",
  "api_key": "sk-ant-...",
  "session_config": {
    "thinking_type": "adaptive",
    "max_tokens": 8192,
    "temperature": 1.0
  },
  "transport_config": { "auth_mode": "x_api_key" }
}
```

### 兼容网关示例

```json
{
  "name": "deepseek",
  "provider_type": "native_oai",
  "base_url": "https://api.deepseek.com/v1",
  "model": "deepseek-chat",
  "api_key": "sk-...",
  "session_config": {
    "max_tokens": 4096,
    "temperature": 0.7
  },
  "transport_config": { "auth_mode": "auto" }
}
```

## 主要代码位置（参考）

### 后端

- `backend-go/internal/domain/llm_provider.go` — 类型与嵌套配置
- `backend-go/internal/api/llm_provider.go` — Admin handlers
- `backend-go/internal/application/runtime_config.go` — token-only 会话文件
- `backend-go/internal/application/scheduler*.go` / `worker_credential.go` — 签发、快照、吊销
- `backend-go/internal/infrastructure/llmproxy/` — 透明代理
- `backend-go/cmd/llm-proxy` / `cmd/platform` — 部署入口

### 前端

- `web/src/api/types.ts` / `web/src/api/providers.ts`
- `web/src/features/admin/LLMProviderForm.tsx`（及管理页）

### 文档

- `docs/LLM_PROVIDER_ARCHITECTURE.md` — 边界与数据流
- `docs/IMPLEMENTATION_SUMMARY.md` — 本文件

## 验证清单（概念）

- [x] Admin CRUD 仅 `/v1/admin/llm-providers`
- [x] 类型仅 `native_oai` / `native_claude`
- [x] Key 加密存储且 API 不回显
- [x] 会话配置仅 capability + Proxy URL
- [x] Worker 走真实 GA
- [x] Proxy 校验并注入；支持 chat/completions、responses、messages 与流式
- [x] 密钥-only 轮换与默认变更语义分离
- [x] 遗留手工 mykey 生成脚本已移除

## 总结

系统以 **Admin 配 Provider → Scheduler 发能力并写会话配置 → Worker/GA 执行 → Proxy 持钥转发** 为唯一主路径。配置与密钥生命周期由 revision、routing snapshot 与持久吊销约束，而不是共享明文 mykey 文件分发。
