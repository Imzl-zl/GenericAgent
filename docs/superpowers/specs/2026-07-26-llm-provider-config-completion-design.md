# LLM Provider 配置补全 - 设计文档
> [!IMPORTANT] 历史设计文档（2026-07 实施期产物，非当前真值）
> 其中部分命名/设计已随后续重构变更（如 DevToken→AdminToken、
> PER_TENANT→PER_REQUESTER、工具分级→静态 policy manifest、
> 常驻 Worker→任务即进程）。当前设计真值以
> `tenant_platform/docs/` 与 `tenant_platform/contracts/` 为准。


**日期**: 2026-07-26  
**作者**: Claude (Kiro)  
**状态**: 待审批

## 一、背景与目标

### 1.1 问题描述

当前 Tenant Platform 的 LLM Provider 管理页面只实现了部分配置字段（6 个），而后端 `domain.LLMProviderConfig` 定义了完整的 GA Core 配置（16 个字段）。这导致：

1. **功能不完整**：无法通过 Web 页面配置高级参数（reasoning_effort、context_win、stream、proxy 等）
2. **mykey.py 生成不完整**：部分字段即使数据库有值也不会生成到 mykey.py
3. **用户体验割裂**：需要手动编辑数据库或 mykey.py 来配置高级参数

### 1.2 目标

补全 LLM Provider 管理页面的 10 个缺失配置字段，使前端表单与后端 `domain.LLMProviderConfig` 完全对齐，实现：

- ✅ 所有 16 个 GA Core 配置字段可通过 Web 页面配置
- ✅ mykey.py 生成器支持所有字段
- ✅ UI 组织清晰，复杂度通过折叠分组管理

### 1.3 非目标

- ❌ 不添加其他 IM 平台（Telegram/QQ/飞书/钉钉）
- ❌ 不实现 Provider 编辑功能（当前只支持新增）
- ❌ 不改变现有数据库 schema（JSONB 已支持所有字段）

---

## 二、当前状况分析

### 2.1 已实现的 6 个字段

```typescript
// tenant_platform/web/src/features/admin/LLMProvidersPage.tsx (已有)
thinking_type: 'adaptive' | 'enabled' | 'disabled'
max_tokens: number
temperature: number (0.0-2.0)
top_p: number (0.0-1.0)
max_retries: number
timeout: number (秒)
```

### 2.2 缺失的 10 个字段

```go
// tenant_platform/backend-go/internal/domain/llm_provider.go (已定义但前端未实现)

// ── 推理 / 思考 ──
ThinkingBudgetTokens int    `json:"thinking_budget_tokens,omitempty"` // 仅 thinking_type=enabled 时
ReasoningEffort     string `json:"reasoning_effort,omitempty"`      // none|minimal|low|medium|high|xhigh

// ── 容量 / 超时 ──
ContextWin     int `json:"context_win,omitempty"`     // 默认 30000
ConnectTimeout int `json:"connect_timeout,omitempty"` // 秒，默认 5
ReadTimeout    int `json:"read_timeout,omitempty"`    // 秒，默认 30

// ── 传输 ──
Stream  *bool  `json:"stream,omitempty"`   // 默认 true
APIMode string `json:"api_mode,omitempty"` // chat_completions | responses (仅 NativeOAI)

// ── NativeClaudeSession 专属 ──
FakeCCSystemPrompt *bool  `json:"fake_cc_system_prompt,omitempty"` // CC 透传渠道必须 true
UserAgent          string `json:"user_agent,omitempty"`            // 可选 UA 覆盖

// ── 其他 ──
Proxy string `json:"proxy,omitempty"` // HTTP 代理
```

### 2.3 后端支持现状

| 组件 | 状态 | 说明 |
|------|------|------|
| 数据库 Schema | ✅ 完整 | `llm_providers.config` JSONB 字段支持所有字段 |
| Domain Model | ✅ 完整 | `LLMProviderConfig` 定义了所有 16 个字段 |
| API Handler | ✅ 完整 | `handleAdminCreateLLMProvider` 接收完整 config |
| mykey.py 生成器 | ⚠️ 部分 | 只生成 6 个字段，需补全 10 个字段 |

---

## 三、设计方案

### 3.1 架构概览

```
┌─────────────────────────────────────────────────────────────┐
│  前端 Web (React + TypeScript)                               │
│  ├─ LLMProvidersPage.tsx (扩展：+10 字段 +6 折叠分组)        │
│  └─ types.ts (扩展：补全 LLMProviderConfig)                  │
└─────────────────────────────────────────────────────────────┘
                          │ HTTP POST/GET
                          ▼
┌─────────────────────────────────────────────────────────────┐
│  后端 Go (小改)                                              │
│  └─ mykey_generator.go (补全 10 个字段的生成逻辑)            │
└─────────────────────────────────────────────────────────────┘
                          │ SQL
                          ▼
┌─────────────────────────────────────────────────────────────┐
│  PostgreSQL (无改动)                                         │
│  └─ llm_providers.config (JSONB, 已支持所有字段)             │
└─────────────────────────────────────────────────────────────┘
```

**改动汇总：**
- ✅ 前端：扩展表单 UI + 类型定义（主要工作量）
- ⚠️ 后端：补全 `mykey_generator.go` 的字段生成逻辑
- ✅ 数据库：无需改动

---

## 四、数据模型

### 4.1 类型定义扩展

```typescript
// tenant_platform/web/src/api/types.ts (补全)
export interface LLMProviderConfig {
  // ── 推理 / 思考 ──
  thinking_type?: 'adaptive' | 'enabled' | 'disabled';              // 已有
  thinking_budget_tokens?: number;                                  // 新增
  reasoning_effort?: 'none' | 'minimal' | 'low' | 'medium' | 'high' | 'xhigh'; // 新增
  
  // ── 采样 ──
  temperature?: number;                                             // 已有
  max_tokens?: number;                                              // 已有
  top_p?: number;                                                   // 已有
  
  // ── 容量 / 超时 ──
  context_win?: number;                                             // 新增
  max_retries?: number;                                             // 已有
  connect_timeout?: number;                                         // 新增
  read_timeout?: number;                                            // 新增
  
  // ── 传输 ──
  stream?: boolean;                                                 // 新增
  api_mode?: 'chat_completions' | 'responses';                      // 新增
  
  // ── Claude 专属 ──
  fake_cc_system_prompt?: boolean;                                  // 新增
  user_agent?: string;                                              // 新增
  
  // ── 网络 ──
  proxy?: string;                                                   // 新增
}
```

### 4.2 表单状态初始值

```typescript
// 新增字段的默认值（与现有代码保持一致：空值策略）
const emptyForm = {
  // ... 现有字段
  config: {
    thinking_type: 'adaptive',
    thinking_budget_tokens: undefined,
    reasoning_effort: undefined,
    // ...
    stream: true,  // 例外：GA Core 默认 true，前端默认勾选
    api_mode: undefined,
    fake_cc_system_prompt: undefined,
    user_agent: undefined,
    proxy: undefined,
  }
}
```

---

## 五、UI 设计

### 5.1 表单结构（折叠分组）

```
📋 新增 Provider 表单
├─ 基础配置（始终展开）
│   ├─ 名称 (Input)
│   ├─ 类型 (Select: native_oai | native_claude)
│   ├─ Base URL (Input)
│   ├─ 模型 (Input)
│   └─ API Key (Input[type=password])
│
└─ 高级配置（可选）──────────────────────┐
    │                                      │
    ├─ [折叠] 🧠 推理与思考                │
    │   ├─ Thinking Type (Select)         │
    │   ├─ Thinking Budget Tokens (Input) │ ← 仅 thinking_type=enabled 时显示
    │   └─ Reasoning Effort (Select)      │
    │                                      │
    ├─ [折叠] 🎛️ 采样参数                  │
    │   ├─ Temperature (Input)            │
    │   ├─ Max Tokens (Input)             │
    │   └─ Top P (Input)                  │
    │                                      │
    ├─ [折叠] ⏱️ 容量与超时                │
    │   ├─ Context Win (Input)            │
    │   ├─ Max Retries (Input)            │
    │   ├─ Connect Timeout (Input)        │
    │   └─ Read Timeout (Input)           │
    │                                      │
    ├─ [折叠] 🔄 传输与协议                │
    │   ├─ Stream (Checkbox)              │
    │   └─ API Mode (Select)              │ ← 仅 provider_type=native_oai 时显示
    │                                      │
    ├─ [折叠] 🤖 Claude 专属               │ ← 仅 provider_type=native_claude 时显示整组
    │   ├─ Fake CC System Prompt (Checkbox) │
    │   └─ User Agent (Input)            │
    │                                      │
    └─ [折叠] 🌐 网络                      │
        └─ Proxy (Input)                  │
                                           │
    [保存] 按钮                            │
────────────────────────────────────────┘
```

### 5.2 条件显示逻辑

| 字段/分组 | 显示条件 | 实现方式 |
|----------|---------|---------|
| Thinking Budget Tokens | `form.config.thinking_type === 'enabled'` | `{thinkingType === 'enabled' && <Input />}` |
| API Mode | `form.provider_type === 'native_oai'` | `{providerType === 'native_oai' && <Input />}` |
| 🤖 Claude 专属整个分组 | `form.provider_type === 'native_claude'` | `{providerType === 'native_claude' && <Collapsible>}` |

### 5.3 字段详细规格

| 字段 | 控件类型 | 验证规则 | 占位符/提示 |
|------|---------|---------|------------|
| Thinking Budget Tokens | `<Input type="number">` | 正整数 | 例如 4000 |
| Reasoning Effort | `<select>` | 枚举值 | none / minimal / low / medium / high / xhigh |
| Context Win | `<Input type="number">` | 正整数 | 例如 30000 |
| Connect Timeout | `<Input type="number">` | 正整数（秒） | 例如 5 |
| Read Timeout | `<Input type="number">` | 正整数（秒） | 例如 30 |
| Stream | `<input type="checkbox">` | 布尔 | 默认勾选 |
| API Mode | `<select>` | chat_completions / responses | 仅 OAI |
| Fake CC System Prompt | `<input type="checkbox">` | 布尔 | CC 透传必须 true |
| User Agent | `<Input type="text">` | 字符串 | 可选 |
| Proxy | `<Input type="text">` | 字符串 | http://proxy:port |

---

## 六、后端补全

### 6.1 mykey_generator.go 需要添加的逻辑

**位置**：`tenant_platform/backend-go/internal/api/mykey_generator.go`

**当前状态**：只生成 6 个字段（thinking_type, max_tokens, temperature, top_p, max_retries, timeout）

**补全代码**：

```go
// 在 GenerateMykeyPy 函数的配置生成循环中，添加以下逻辑：

// ── 推理 / 思考 ──
if p.Config.ThinkingBudgetTokens > 0 {
    sb.WriteString(fmt.Sprintf("    'thinking_budget_tokens': %d,\n", p.Config.ThinkingBudgetTokens))
}
if p.Config.ReasoningEffort != "" {
    sb.WriteString(fmt.Sprintf("    'reasoning_effort': '%s',\n", p.Config.ReasoningEffort))
}

// ── 容量 / 超时 ──
if p.Config.ContextWin > 0 {
    sb.WriteString(fmt.Sprintf("    'context_win': %d,\n", p.Config.ContextWin))
}
if p.Config.ConnectTimeout > 0 {
    sb.WriteString(fmt.Sprintf("    'connect_timeout': %d,\n", p.Config.ConnectTimeout))
}
if p.Config.ReadTimeout > 0 {
    sb.WriteString(fmt.Sprintf("    'read_timeout': %d,\n", p.Config.ReadTimeout))
}

// ── 传输 ──
if p.Config.Stream != nil {
    sb.WriteString(fmt.Sprintf("    'stream': %t,\n", *p.Config.Stream))
}
if p.Config.APIMode != "" {
    sb.WriteString(fmt.Sprintf("    'api_mode': '%s',\n", p.Config.APIMode))
}

// ── NativeClaudeSession 专属 ──
if p.Config.FakeCCSystemPrompt != nil {
    sb.WriteString(fmt.Sprintf("    'fake_cc_system_prompt': %t,\n", *p.Config.FakeCCSystemPrompt))
}
if p.Config.UserAgent != "" {
    sb.WriteString(fmt.Sprintf("    'user_agent': '%s',\n", p.Config.UserAgent))
}

// ── 网络 ──
if p.Config.Proxy != "" {
    sb.WriteString(fmt.Sprintf("    'proxy': '%s',\n", p.Config.Proxy))
}
```

---

## 七、实施计划

### 7.1 实施步骤

```
阶段 1：类型定义与 UI 组件准备
  ✅ 1.1 扩展 types.ts，添加 10 个新字段类型定义
  ✅ 1.2 实现简单的 Collapsible 折叠组件（或复用现有 UI 库）

阶段 2：前端表单扩展
  ✅ 2.1 在 LLMProvidersPage.tsx 添加 10 个新字段表单控件
  ✅ 2.2 实现 6 个折叠分组
  ✅ 2.3 实现条件显示逻辑（thinking_type, provider_type）
  ✅ 2.4 更新表单初始化和提交逻辑

阶段 3：后端补全
  ✅ 3.1 补全 mykey_generator.go 的 10 个字段生成逻辑

阶段 4：测试与验证
  ✅ 4.1 创建 native_oai Provider，验证所有字段
  ✅ 4.2 创建 native_claude Provider，验证 Claude 专属字段
  ✅ 4.3 调用 /api/admin/config/mykey 验证生成的 mykey.py
  ✅ 4.4 边界测试（空值、极值、条件显示）
```

### 7.2 预估工作量

| 阶段 | 预估时间 | 复杂度 |
|------|---------|-------|
| 类型定义与组件 | 30 分钟 | 低 |
| 前端表单扩展 | 1.5 小时 | 中 |
| 后端补全 | 30 分钟 | 低 |
| 测试验证 | 45 分钟 | 低 |
| **总计** | **~3 小时** | **低-中** |

---

## 八、测试策略

### 8.1 功能测试用例

#### TC-01：创建 native_oai Provider（完整配置）
**前置条件**：管理员登录  
**步骤**：
1. 填写基础配置（名称、Base URL、模型、API Key）
2. 展开"推理与思考"，设置 reasoning_effort = high
3. 展开"采样参数"，设置 temperature = 0.7
4. 展开"容量与超时"，设置 context_win = 50000
5. 展开"传输与协议"，选择 api_mode = responses
6. 点击保存

**期望结果**：
- Provider 创建成功
- 调用 /api/admin/config/mykey 返回的 Python 代码包含所有字段
- 数据库 config JSONB 包含所有字段

#### TC-02：创建 native_claude Provider（Claude 专属字段）
**步骤**：
1. provider_type 选择 native_claude
2. 验证"Claude 专属"分组显示
3. 勾选 fake_cc_system_prompt
4. 填写 user_agent = "Custom UA"
5. 保存

**期望结果**：
- mykey.py 包含 `'fake_cc_system_prompt': True` 和 `'user_agent': 'Custom UA'`

#### TC-03：条件显示逻辑验证
**步骤**：
1. thinking_type 选择 enabled
2. 验证 thinking_budget_tokens 字段显示
3. thinking_type 改为 adaptive
4. 验证 thinking_budget_tokens 字段隐藏

### 8.2 边界测试用例

#### TC-04：所有字段留空
**步骤**：只填写必填字段（名称、Base URL、模型、API Key），高级配置全部留空  
**期望结果**：创建成功，mykey.py 只包含必填字段，GA Core 使用默认值

#### TC-05：极值测试
- Temperature = 0.0, 2.0
- Top P = 0.0, 1.0
- Context Win = 1, 1000000

---

## 九、风险评估

| 风险 | 概率 | 影响 | 缓解措施 |
|------|------|------|---------|
| 字段名大小写不匹配（Go snake_case vs TS camelCase） | 低 | 中 | 严格对照 Go struct tag 的 json 名称 |
| 条件显示逻辑错误 | 低 | 低 | TC-03 覆盖 |
| mykey.py 生成格式错误（Python 语法） | 低 | 高 | 生成后用 Python `ast.parse()` 验证 |
| Stream/FakeCCSystemPrompt 指针类型处理 | 中 | 中 | 前端显式传 true/false，后端判空逻辑已有 |

---

## 十、未来扩展

### 10.1 短期（本次不做）
- Provider 编辑功能（当前只支持新增）
- 配置模板（预设常用 Provider 配置）

### 10.2 中期
- 添加其他 IM 平台（Telegram/QQ/飞书/钉钉）
- Provider 配置导入/导出（JSON）

### 10.3 长期
- LLM 供应商健康检查（ping base_url，验证 API Key）
- 使用统计（各 Provider 的调用次数、成功率）

---

## 附录

### A. 参考文档
- GA Core mykey.py 配置规范：`GenericAgent/llmcore/mykeys.py`
- 后端 Domain 定义：`tenant_platform/backend-go/internal/domain/llm_provider.go`
- 数据库迁移：`tenant_platform/infra/postgres/migrations/0023_llm_provider_ga_config.sql`

### B. 字段映射表（Go ↔ TypeScript）

| Go Field (domain.LLMProviderConfig) | JSON Tag | TypeScript Field | 控件类型 |
|-------------------------------------|----------|------------------|---------|
| ThinkingType | thinking_type | thinking_type | select |
| ThinkingBudgetTokens | thinking_budget_tokens | thinking_budget_tokens | number |
| ReasoningEffort | reasoning_effort | reasoning_effort | select |
| Temperature | temperature | temperature | number |
| MaxTokens | max_tokens | max_tokens | number |
| TopP (Go 中实际为 float64) | - | top_p | number |
| ContextWin | context_win | context_win | number |
| MaxRetries | max_retries | max_retries | number |
| ConnectTimeout | connect_timeout | connect_timeout | number |
| ReadTimeout | read_timeout | read_timeout | number |
| Stream | stream | stream | checkbox |
| APIMode | api_mode | api_mode | select |
| FakeCCSystemPrompt | fake_cc_system_prompt | fake_cc_system_prompt | checkbox |
| UserAgent | user_agent | user_agent | text |
| Proxy | proxy | proxy | text |

---

**设计文档完成，等待审批后进入实施阶段。**
