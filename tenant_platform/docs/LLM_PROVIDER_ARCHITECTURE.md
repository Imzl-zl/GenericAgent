# LLM Provider 架构设计

## 概述

Tenant Platform 的 LLM Provider 系统设计目标：
1. **复用 GA Core 的 LLM 协议实现** - 不重复造轮子
2. **实时生效** - UI 配置修改后立即对新任务生效
3. **多租户友好** - 支持统一配置或按用户独立配置
4. **易于管理** - 管理员通过 UI 配置，用户直接使用

## 架构设计

### 数据流

```
管理员在 UI 配置 Provider
  ↓
存储到数据库（provider_type + config 字段）
  ↓
Worker 启动时从 Platform 拉取配置
  ↓
生成 mykey.py 文件
  ↓
GA Core 读取 mykey.py 并初始化 Session
  ↓
Worker 使用 GA Core 处理任务
```

### 关键优势

1. **不重复实现协议**
   - OpenAI、Anthropic、国内大模型的协议都在 GA Core 中
   - Platform 只负责配置管理和文件生成
   - 新增 Provider 类型无需改 Platform 代码

2. **配置实时生效**
   - 新 Worker 启动时拉取最新配置
   - 不需要重启整个 Platform
   - 旧 Worker 完成任务后自然消亡

3. **完全兼容 GA Core**
   - 字段格式对齐 `mykey.py`
   - 支持所有 GA Core 的 Provider 类型
   - 支持所有配置选项（thinking_type、max_tokens 等）

## 数据库 Schema

```sql
CREATE TABLE llm_providers (
    id BIGSERIAL PRIMARY KEY,
    name TEXT NOT NULL,                       -- Provider 名称
    provider_type TEXT NOT NULL,              -- 'native_oai' | 'native_claude'
    base_url TEXT NOT NULL,                   -- API 端点
    model TEXT NOT NULL,                      -- 模型名称
    api_key_ciphertext BYTEA NOT NULL,        -- 加密的 API Key
    api_key_key_version TEXT NOT NULL,        -- 加密密钥版本
    config JSONB NOT NULL DEFAULT '{}'::jsonb,-- 其他配置（thinking_type, max_tokens 等）
    is_default BOOLEAN NOT NULL DEFAULT false,-- 是否为默认 Provider
    state TEXT NOT NULL DEFAULT 'active',     -- 状态
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL
);
```

### config 字段示例

```json
{
  "thinking_type": "adaptive",  // 思考模式：adaptive | enabled | disabled
  "max_tokens": 8192,           // 最大 token 数
  "temperature": 1.0,           // 温度参数
  "top_p": 0.9,                 // Top-p 采样
  "max_retries": 10             // 最大重试次数
}
```

## API 接口

### 1. 创建 Provider

```http
POST /v1/admin/llm-providers
Authorization: Bearer <admin_token>
Content-Type: application/json

{
  "name": "my-gpt",
  "provider_type": "native_oai",
  "base_url": "https://api.openai.com/v1",
  "model": "gpt-4",
  "api_key": "sk-...",
  "config": {
    "thinking_type": "adaptive",
    "max_tokens": 8192,
    "temperature": 1.0
  }
}
```

### 2. 获取 mykey.py 配置

```http
GET /v1/config/mykey.py
Authorization: Bearer <token>
```

返回：
```python
# Auto-generated mykey.py from Tenant Platform
# DO NOT EDIT - Changes will be overwritten

mixin_config = {
    'llm_nos': ['my-gpt'],
    'max_retries': 10,
}

native_oai_config_0 = {
    'name': 'my-gpt',
    'type': 'native_oai',
    'apikey': 'sk-...',
    'apibase': 'https://api.openai.com/v1',
    'model': 'gpt-4',
    'thinking_type': 'adaptive',
    'max_tokens': 8192,
    'temperature': 1.0,
}
```

## Worker 集成

### 配置加载流程

Worker 启动时：

1. **从 Platform 拉取配置**
   ```python
   from ga_worker.config_loader import ensure_mykey
   
   ensure_mykey(
       config_root=Path("/app/config"),
       platform_url="http://platform:8080",
       token=os.environ["WORKER_TOKEN"]
   )
   ```

2. **写入 mykey.py 文件**
   ```python
   config_root/mykey.py  # GA Core 会读取这个文件
   ```

3. **GA Core 初始化**
   ```python
   from ga_worker.legacy_import import import_legacy_runtime
   
   legacy_mods = import_legacy_runtime(
       legacy_root=legacy_root,
       config_root=config_root,
       runtime_dir=runtime_dir
   )
   ```

4. **创建 Session**
   ```python
   # GA Core 自动根据 mykey.py 创建对应的 Session
   # - native_oai → NativeOAISession
   # - native_claude → NativeClaudeSession
   session = legacy_mods.session_class(...)
   ```

## 支持的 Provider 类型

### native_oai (OpenAI 兼容)

适用于：
- OpenAI GPT-4 / GPT-3.5
- Azure OpenAI
- 其他 OpenAI 兼容接口

配置字段：
```python
{
    'name': 'my-provider',
    'type': 'native_oai',
    'apikey': 'sk-...',
    'apibase': 'https://api.openai.com/v1',
    'model': 'gpt-4',
    'thinking_type': 'adaptive',  # adaptive | enabled | disabled
    'max_tokens': 8192,
    'temperature': 1.0,
    'top_p': 0.9,
}
```

### native_claude (Anthropic Claude)

适用于：
- Claude Opus / Sonnet / Haiku
- Claude API

配置字段：
```python
{
    'name': 'my-claude',
    'type': 'native_claude',
    'apikey': 'sk-ant-...',
    'apibase': 'https://api.anthropic.com',
    'model': 'claude-opus-5',
    'thinking_type': 'adaptive',
    'max_tokens': 8192,
    'temperature': 1.0,
}
```

## 前端集成

### Provider 类型选择

```typescript
const PROVIDER_TYPES = [
  { value: 'native_oai', label: 'OpenAI Compatible' },
  { value: 'native_claude', label: 'Anthropic Claude' }
];

// 根据类型动态显示配置字段
const configFields = {
  native_oai: [
    { name: 'thinking_type', type: 'select', options: ['adaptive', 'enabled', 'disabled'] },
    { name: 'max_tokens', type: 'number', default: 8192 },
    { name: 'temperature', type: 'number', min: 0, max: 2, step: 0.1, default: 1.0 },
  ],
  native_claude: [
    { name: 'thinking_type', type: 'select', options: ['adaptive', 'enabled', 'disabled'] },
    { name: 'max_tokens', type: 'number', default: 8192 },
    { name: 'temperature', type: 'number', min: 0, max: 2, step: 0.1, default: 1.0 },
  ]
};
```

## 配置示例

### 示例 1：OpenAI GPT-4

```json
{
  "name": "openai-gpt4",
  "provider_type": "native_oai",
  "base_url": "https://api.openai.com/v1",
  "model": "gpt-4",
  "api_key": "sk-...",
  "config": {
    "thinking_type": "adaptive",
    "max_tokens": 8192,
    "temperature": 1.0
  }
}
```

### 示例 2：Claude Opus

```json
{
  "name": "claude-opus",
  "provider_type": "native_claude",
  "base_url": "https://api.anthropic.com",
  "model": "claude-opus-5",
  "api_key": "sk-ant-...",
  "config": {
    "thinking_type": "adaptive",
    "max_tokens": 8192,
    "temperature": 1.0
  }
}
```

### 示例 3：国内大模型（OpenAI 兼容）

```json
{
  "name": "deepseek",
  "provider_type": "native_oai",
  "base_url": "https://api.deepseek.com/v1",
  "model": "deepseek-chat",
  "api_key": "sk-...",
  "config": {
    "max_tokens": 4096,
    "temperature": 0.7
  }
}
```

## 测试

运行测试脚本：

```bash
# 启动后端
./start-backend.ps1

# 在另一个终端运行测试
bash test-mykey-generation.sh
```

测试会：
1. 创建一个测试 Provider
2. 获取生成的 mykey.py
3. 验证内容正确性
4. 保存到 config/mykey.py

## 未来扩展

### 1. 多租户配置

每个用户独立的 Provider 配置：

```http
GET /v1/config/mykey.py?user_id=123
```

生成 `config/mykey_123.py`，Worker 根据用户 ID 加载对应配置。

### 2. 配置热重载

Worker 运行时检测配置变更：

```python
# Worker 收到通知后
def on_config_changed(self):
    if not self._is_busy:
        self._reload_config()
        self._recreate_session()
```

### 3. 更多 Provider 类型

GA Core 支持的其他类型：
- `kimi` - 月之暗面 Kimi
- `glm` - 智谱 GLM
- `minimax` - MiniMax

只需在前端添加类型选项，无需修改后端代码。

## 常见问题

### Q: 为什么不直接用 LLM Proxy 转发？

A: 
1. 避免重复实现协议（OpenAI/Anthropic/国内大模型）
2. GA Core 已经实现了所有细节（重试、错误处理、流式等）
3. 减少维护成本

### Q: 配置什么时候生效？

A:
- **新 Worker**：启动时立即生效
- **旧 Worker**：任务完成后消亡，下次创建新 Worker

### Q: 如何支持新的 LLM Provider？

A:
1. 如果是 OpenAI 兼容接口：使用 `native_oai` 类型
2. 如果是 GA Core 支持的类型：前端添加类型选项
3. 如果是全新类型：需要先在 GA Core 中实现

### Q: API Key 如何保护？

A:
- 存储时使用 AES-256-GCM 加密
- 传输时使用 HTTPS
- Worker 启动时解密并写入 mykey.py
- mykey.py 文件仅在 Worker 容器内部可见
