# LLM Provider 架构设计

## 概述

Tenant Platform 的 LLM Provider 系统职责边界：

1. **Admin** 通过 `/v1/admin/llm-providers` 管理上游 Provider
2. **Scheduler** 为会话签发 capability JWT，并生成仅含 token + 代理地址的会话运行时配置
3. **Worker** 加载该配置，把协议执行委托给真实 GA Core
4. **Transparent LLM Proxy** 校验 capability / provider / model / revision / path，注入真实上游密钥后转发

真实上游 API Key 仅由 Proxy 持有（从加密存储解密）；Worker 与会话配置永不接触明文 Key。

## 端到端数据流

```
管理员在 UI / Admin API 配置 Provider
  ↓
DB 持久化：provider_type、session_config、transport_config、revision；
api_key 以 ciphertext 存盘，API 响应永不回传明文
  ↓
Scheduler 选路 → 为每个路由 Provider 签发 capability JWT
  ↓
写入会话目录：固定 mykey.py 加载器 + mykey.runtime.json
（JSON 内 apikey = capability token，apibase = Proxy URL）
  ↓
Worker 委托 GA Core 按 native_oai / native_claude 建 Session 并执行
  ↓
GA 请求打到 Transparent Proxy：
校验 token、provider、model、revision、允许 path → 注入真实 Key → 上游
```

### 关键优势

1. **密钥不进 Worker** — 会话配置只有 capability JWT 与 Proxy 地址
2. **协议仍在 GA Core** — Platform 不重实现 chat / responses / messages
3. **路由可修订** — `revision` 绑定 capability；密钥-only 轮换可保留 revision 与既有会话
4. **撤销持久化** — Proxy 依赖 DB 级 capability 吊销，进程重启仍生效

## 所有权边界

| 组件 | 负责 | 不负责 |
|------|------|--------|
| Admin CRUD | Provider 元数据、嵌套 session/transport、加密入库、设默认 | 签发 token、转发上游 |
| Scheduler | 路由快照、发 capability、写 `mykey.runtime.json` + 固定 loader、会话替换策略 | 持有明文上游 Key、执行 LLM 协议 |
| Worker / GA Core | 读会话配置、真实 GA Session 执行 | 解密上游 Key、决定注入头 |
| Transparent Proxy | 校验 capability/provider/model/revision/path；provider 作用域 transport；注入 Key；流式/SSE 透传 | 业务选路、生成会话配置 |

## 数据库要点

Provider 记录（概念字段）：

- `provider_type`：仅 `native_oai` | `native_claude`
- `base_url` / `model`
- `api_key_ciphertext` / `api_key_key_version`（AES-GCM；列表/详情不返回明文）
- `session_config` JSONB — GA Session 行为（thinking、tokens、temperature、api_mode 等，按类型校验）
- `transport_config` JSONB — Proxy→上游传输（auth_mode、超时、可选 proxy_url 等）
- `revision` — 路由/能力绑定版本；名称或密钥-only 更新保持不变，type/base/model/session/transport/state 变化递增
- `is_default` / `state`

### 已知行为：routing 变更与在途任务（残余风险确认，Round15）

capability JWT 的 `provider_revision` 绑定签发时刻的 provider 配置快照；llm-proxy 在每次调用时校验 `provider.Revision == claims.ProviderRevision`（`handler.go`，不匹配返回 409 `PROVIDER_REVISION_MISMATCH`）。

因此 **provider routing 变更（PUT/disable/enable，revision 递增）与 scheduler 并发 dispatch 存在竞争窗口**：capability 签发（rev=N）之后、任务实际调用 LLM 之前发生变更（rev=N+1）→ 在途任务的旧 capability 被 409 拒绝 → GA `LLM_FAILED` → 任务以 TASK_FAILED 结束。

这是**有意的安全设计，不是缺陷**：revision 绑定保证 routing 变更立即生效，旧配置不会被继续使用；失败方向为 fail-closed（409 → 任务失败可重试），不产生静默错误或越权。生产影响小（routing 变更低频），**不要**为消除竞争窗口而放宽 revision 校验。

## Admin API

基路径：`/v1/admin/llm-providers`（需管理员认证）。

| 方法 | 路径 | 说明 |
|------|------|------|
| POST | `/v1/admin/llm-providers` | 创建（必填 `api_key`，加密入库） |
| GET | `/v1/admin/llm-providers` | 列表（无明文 key） |
| GET | `/v1/admin/llm-providers/{id}` | 详情 |
| PUT | `/v1/admin/llm-providers/{id}` | 更新；省略/空 `api_key` 表示不轮换密钥 |
| DELETE | `/v1/admin/llm-providers/{id}` | 删除 |
| POST | `/v1/admin/llm-providers/{id}/default` | 设为默认 |
| POST | `/v1/admin/llm-providers/{id}/disable` | 禁用非默认 Provider，并递增 revision |
| POST | `/v1/admin/llm-providers/{id}/enable` | 重新启用 Provider，并递增 revision |

创建/更新体形状：

```json
{
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
}
```

响应含 `session_config`、`transport_config`、`revision` 等，**不含**明文 `api_key`。

默认 Provider 不能直接禁用；先将另一个 active Provider 设为默认。重复 enable/disable 是幂等操作，不重复递增 revision。

不存在明文配置下发路由（例如旧的公开 mykey 拉取接口）；Worker 只消费 Scheduler 写入的会话目录文件。

## 会话运行时配置（Scheduler）

对每个绑定 Worker 的会话，Scheduler：

1. 按当前活动 Provider 与默认规则构建 **routing snapshot**
2. 为快照内每个 Provider 签发 **capability JWT**（含 provider id、model、revision、路径策略等声明）
3. 调用运行时配置构建，写出：
   - **`mykey.runtime.json`**：GA 可消费的变量表；`apikey` 仅为 capability token，`apibase` 为 Proxy base URL；附带 routing / generation 元数据
   - **`mykey.py`**：固定加载器（读同目录 JSON 并 `globals().update`），内容不随 Provider 变

多 Provider 时生成 `mixin_config.llm_nos` 供 GA 故障转移。真实上游密钥从不进入这些文件。

### 轮换与默认变更

- **密钥-only 轮换**：可保留 `revision` 与既有会话绑定；新请求经 Proxy 用新密钥，不必因密钥本身替换 Worker
- **session / model / type / transport / state / active 集合变更**：抬高 revision 或改变快照，在下一任务边界替换受影响 Worker 并吊销旧能力
- **默认 Provider 变更**：影响之后新建的路由快照，不回溯改写已绑定会话的历史快照语义

## Worker 与 GA Core

Worker 在创建 Agent/Session 前使用会话目录中的 loader + runtime JSON。GA Core 按 `native_oai` → NativeOAISession、`native_claude` → NativeClaudeSession 初始化。出站 HTTP 指向 Proxy，Authorization（或等价头）携带 capability token，而非上游 Key。

## Transparent LLM Proxy

职责：

- 校验 capability JWT（签名、过期、**持久化吊销**）
- Scheduler 周期删除已过 `expires_at` 的撤销记录，避免长期运行时表和索引无界增长
- 校验声明与请求一致：provider、model、**revision**、允许的 path
- 按 `provider_type` 映射上游路径：
  - `native_oai`：`/v1/chat/completions`、`/v1/responses`（及兼容别名）
  - `native_claude`：`/v1/messages`
- 清洗入站头后 **注入** 解密后的上游凭证（`transport_config.auth_mode`：auto / bearer / x_api_key）
- 支持流式与 SSE 透传；transport 超时与可选上游 HTTP proxy 按 Provider 作用域配置

Proxy 是唯一解密并使用上游 Key 的运行时组件。

## 支持的 Provider 类型

严格两种：

| 类型 | 用途 |
|------|------|
| `native_oai` | OpenAI 兼容（含 Azure / 兼容网关等） |
| `native_claude` | Anthropic Messages API |

`session_config` 字段按类型校验（例如 `api_mode` 仅 `native_oai`；Claude 专属字段仅 `native_claude`）。

## 前端集成要点

- 类型下拉仅 `native_oai` / `native_claude`
- 表单编辑嵌套 `session_config` 与 `transport_config`
- 创建必须提交 `api_key`；更新时留空表示不轮换
- 展示 `revision`、默认标记、状态；永不期望 API 回显明文 Key

## 配置示例

### native_oai

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
  "transport_config": {
    "auth_mode": "auto"
  }
}
```

### native_claude

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
  "transport_config": {
    "auth_mode": "x_api_key"
  }
}
```

## 安全

- 静态：API Key AES-GCM 加密；Admin 读路径不返回明文
- 动态：Worker 仅持 capability JWT；Proxy 校验 + 吊销 + 注入
- 传输：管理面与 Proxy 建议 HTTPS / 本机回环按部署约束
- 审计：避免在日志与审计详情中记录 token、JTI 明文密钥材料

## 常见问题

### 为什么还要 Proxy，而不是把 Key 写进 Worker？

避免真实密钥进入会话文件系统与 Worker 进程；吊销与 path/model 约束集中在 Proxy。

### 配置何时生效？

- 新路由快照 / 新 Worker：按当前默认与活动 Provider 立即生效
- 密钥-only 轮换：可保留 revision 与现有会话，上游改用新密钥
- 改变路由语义的更新：新会话用新快照；旧 capability 按吊销/替换策略失效

### 如何加新上游？

在 Admin 创建 `native_oai` 或 `native_claude` Provider，填 base_url、model、密钥与嵌套配置，并按需设默认。无需也不应再维护手工明文 mykey 下发流程。
