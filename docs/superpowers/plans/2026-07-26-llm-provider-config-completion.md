# LLM Provider 配置补全实施计划
> [!IMPORTANT] 历史设计文档（2026-07 实施期产物，非当前真值）
> 其中部分命名/设计已随后续重构变更（如 DevToken→AdminToken、
> PER_TENANT→PER_REQUESTER、工具分级→静态 policy manifest、
> 常驻 Worker→任务即进程）。当前设计真值以
> `tenant_platform/docs/` 与 `tenant_platform/contracts/` 为准。


> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 补全 LLM Provider 管理页面的 10 个缺失配置字段，使前端表单与后端完全对齐

**Architecture:** 
- 前端：扩展 React 表单组件，添加 10 个新字段并通过 6 个折叠分组组织
- 后端：补全 mykey.py 生成器的字段输出逻辑
- 无需数据库改动（JSONB 已支持所有字段）

**Tech Stack:** 
- 前端：React 18, TypeScript, 现有 UI 组件库
- 后端：Go 1.21+, domain.LLMProviderConfig

## Global Constraints

- TypeScript strict mode 启用
- 所有字段为可选（undefined 表示使用后端默认值）
- 字段名严格遵循后端 JSON tag（snake_case）
- 条件显示：thinking_budget_tokens 仅在 thinking_type=enabled 时显示
- 条件显示：api_mode 仅在 provider_type=native_oai 时显示
- 条件显示：Claude 专属分组仅在 provider_type=native_claude 时显示
- 所有提交必须包含 Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>

---

## File Structure

### Files to Modify

1. **tenant_platform/web/src/api/types.ts**
   - 扩展 `LLMProviderConfig` 接口，添加 10 个新字段类型定义
   - 责任：定义前后端数据契约

2. **tenant_platform/web/src/components/ui/Collapsible.tsx** (新建)
   - 简单的折叠/展开组件
   - 责任：提供可复用的折叠 UI 容器

3. **tenant_platform/web/src/features/admin/LLMProvidersPage.tsx**
   - 添加 10 个新字段的表单控件
   - 实现 6 个折叠分组
   - 实现条件显示逻辑
   - 责任：完整的 LLM Provider 配置表单

4. **tenant_platform/backend-go/internal/api/mykey_generator.go**
   - 补全 10 个字段的 mykey.py 生成逻辑
   - 责任：生成 GA Core 可用的 Python 配置文件

### No New Dependencies

所有功能使用现有依赖实现，无需添加新的 npm 包或 Go 模块。

---

### Task 1: 扩展 TypeScript 类型定义

**Files:**
- Modify: `tenant_platform/web/src/api/types.ts:89-96`

**Interfaces:**
- Consumes: 无（独立任务）
- Produces: 完整的 `LLMProviderConfig` 接口，包含所有 16 个字段

- [ ] **Step 1: 打开 types.ts 并定位到 LLMProviderConfig 接口**

Run: 
```bash
code tenant_platform/web/src/api/types.ts
```

找到第 89 行的 `export interface LLMProviderConfig`

- [ ] **Step 2: 添加 10 个新字段类型定义**

在现有 `timeout?: number;` 后添加：

```typescript
  // ── 推理 / 思考（新增）──
  thinking_budget_tokens?: number;
  reasoning_effort?: 'none' | 'minimal' | 'low' | 'medium' | 'high' | 'xhigh';
  
  // ── 容量 / 超时（新增）──
  context_win?: number;
  connect_timeout?: number;
  read_timeout?: number;
  
  // ── 传输（新增）──
  stream?: boolean;
  api_mode?: 'chat_completions' | 'responses';
  
  // ── Claude 专属（新增）──
  fake_cc_system_prompt?: boolean;
  user_agent?: string;
  
  // ── 网络（新增）──
  proxy?: string;
```

- [ ] **Step 3: 验证 TypeScript 编译无错误**

Run:
```bash
cd tenant_platform/web
npm run type-check
```

Expected: No errors

- [ ] **Step 4: Commit**

```bash
git add tenant_platform/web/src/api/types.ts
git commit -m "feat: 扩展 LLMProviderConfig 类型定义，添加 10 个新字段

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

---

### Task 2: 实现折叠组件

**Files:**
- Create: `tenant_platform/web/src/components/ui/Collapsible.tsx`

**Interfaces:**
- Consumes: 无
- Produces: `Collapsible` 组件：`(props: { title: string; defaultOpen?: boolean; children: React.ReactNode }) => JSX.Element`

- [ ] **Step 1: 创建 Collapsible 组件文件**

```tsx
import { useState } from 'react';
import './Collapsible.css';

interface CollapsibleProps {
  title: string;
  defaultOpen?: boolean;
  children: React.ReactNode;
}

export function Collapsible({ title, defaultOpen = false, children }: CollapsibleProps) {
  const [isOpen, setIsOpen] = useState(defaultOpen);

  return (
    <div className="collapsible">
      <button
        type="button"
        className="collapsible-header"
        onClick={() => setIsOpen(!isOpen)}
        aria-expanded={isOpen}
      >
        <span className="collapsible-icon">{isOpen ? '▼' : '▶'}</span>
        <span className="collapsible-title">{title}</span>
      </button>
      {isOpen && <div className="collapsible-content">{children}</div>}
    </div>
  );
}
```

- [ ] **Step 2: 创建样式文件**

Create: `tenant_platform/web/src/components/ui/Collapsible.css`

```css
.collapsible {
  border: 1px solid var(--border);
  border-radius: 6px;
  margin-bottom: 12px;
  background: var(--surface);
}

.collapsible-header {
  width: 100%;
  display: flex;
  align-items: center;
  gap: 8px;
  padding: 12px 16px;
  background: none;
  border: none;
  cursor: pointer;
  font-size: 14px;
  font-weight: 500;
  color: var(--text);
  text-align: left;
  transition: background 0.15s;
}

.collapsible-header:hover {
  background: var(--hover);
}

.collapsible-icon {
  font-size: 10px;
  color: var(--text-muted);
}

.collapsible-title {
  flex: 1;
}

.collapsible-content {
  padding: 0 16px 16px 16px;
  display: grid;
  grid-template-columns: 1fr 1fr;
  gap: 16px;
}

.collapsible-content > .provider-form-full {
  grid-column: 1 / -1;
}
```

- [ ] **Step 3: 验证组件编译**

Run:
```bash
cd tenant_platform/web
npm run type-check
```

Expected: No errors

- [ ] **Step 4: Commit**

```bash
git add tenant_platform/web/src/components/ui/Collapsible.tsx tenant_platform/web/src/components/ui/Collapsible.css
git commit -m "feat: 添加 Collapsible 折叠组件

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

---

### Task 3: 扩展 LLMProvidersPage - 添加新字段到表单状态

**Files:**
- Modify: `tenant_platform/web/src/features/admin/LLMProvidersPage.tsx:18-32`

**Interfaces:**
- Consumes: `LLMProviderConfig` from Task 1
- Produces: 扩展的 `form` state，包含所有 16 个字段

- [ ] **Step 1: 导入 Collapsible 组件**

在文件顶部 imports 中添加：

```typescript
import { Collapsible } from '../../components/ui/Collapsible';
```

- [ ] **Step 2: 扩展 form state 初始值**

找到第 18 行的 `const [form, setForm] = useState({`，将 config 部分扩展为：

```typescript
const [form, setForm] = useState({
  name: '',
  provider_type: 'native_oai' as 'native_oai' | 'native_claude',
  base_url: '',
  model: '',
  api_key: '',
  config: {
    // ── 推理 / 思考 ──
    thinking_type: 'adaptive' as 'adaptive' | 'enabled' | 'disabled' | undefined,
    thinking_budget_tokens: undefined as number | undefined,
    reasoning_effort: undefined as 'none' | 'minimal' | 'low' | 'medium' | 'high' | 'xhigh' | undefined,
    
    // ── 采样 ──
    max_tokens: undefined as number | undefined,
    temperature: undefined as number | undefined,
    top_p: undefined as number | undefined,
    
    // ── 容量 / 超时 ──
    context_win: undefined as number | undefined,
    max_retries: undefined as number | undefined,
    connect_timeout: undefined as number | undefined,
    read_timeout: undefined as number | undefined,
    
    // ── 传输 ──
    stream: true as boolean | undefined,
    api_mode: undefined as 'chat_completions' | 'responses' | undefined,
    
    // ── Claude 专属 ──
    fake_cc_system_prompt: undefined as boolean | undefined,
    user_agent: undefined as string | undefined,
    
    // ── 网络 ──
    proxy: undefined as string | undefined,
  } as LLMProviderConfig,
});
```

- [ ] **Step 3: 同步更新 handleSubmit 中的 form 重置逻辑**

找到第 58 行 `setForm({`，用相同的结构替换（复制 Step 2 的代码）

- [ ] **Step 4: 验证编译**

Run:
```bash
cd tenant_platform/web
npm run type-check
```

Expected: No errors

- [ ] **Step 5: Commit**

```bash
git add tenant_platform/web/src/features/admin/LLMProvidersPage.tsx
git commit -m "feat: 扩展 LLMProvidersPage 表单状态，添加 10 个新字段

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

---

### Task 4: 添加"推理与思考"分组的表单字段

**Files:**
- Modify: `tenant_platform/web/src/features/admin/LLMProvidersPage.tsx:150-211`

**Interfaces:**
- Consumes: `Collapsible` from Task 2, expanded `form.config` from Task 3
- Produces: "推理与思考"折叠分组，包含 3 个字段（thinking_type, thinking_budget_tokens, reasoning_effort）

- [ ] **Step 1: 找到"高级配置"区域起点**

定位到第 151 行的 `<div className="provider-form-full" ...>` 开始的高级配置区域

- [ ] **Step 2: 将现有的 Thinking Type 字段移入折叠组**

删除原有的独立 Thinking Type select（第 156-168 行），替换为：

```tsx
<Collapsible title="🧠 推理与思考" defaultOpen={true}>
  <div>
    <label className="input-label">Thinking Type</label>
    <select
      className="input-field"
      value={form.config.thinking_type || 'adaptive'}
      onChange={(e) => setForm({ ...form, config: { ...form.config, thinking_type: e.target.value as any } })}
      style={{ width: '100%', padding: '8px 12px' }}
    >
      <option value="adaptive">Adaptive（自适应）</option>
      <option value="enabled">Enabled（启用）</option>
      <option value="disabled">Disabled（禁用）</option>
    </select>
  </div>

  {form.config.thinking_type === 'enabled' && (
    <Input
      label="Thinking Budget Tokens"
      type="number"
      placeholder="例如 4000"
      value={form.config.thinking_budget_tokens || ''}
      onChange={(e) => setForm({ ...form, config: { ...form.config, thinking_budget_tokens: e.target.value ? parseInt(e.target.value) : undefined } })}
    />
  )}

  <div>
    <label className="input-label">Reasoning Effort</label>
    <select
      className="input-field"
      value={form.config.reasoning_effort || ''}
      onChange={(e) => setForm({ ...form, config: { ...form.config, reasoning_effort: e.target.value as any || undefined } })}
      style={{ width: '100%', padding: '8px 12px' }}
    >
      <option value="">默认</option>
      <option value="none">None</option>
      <option value="minimal">Minimal</option>
      <option value="low">Low</option>
      <option value="medium">Medium</option>
      <option value="high">High</option>
      <option value="xhigh">XHigh</option>
    </select>
  </div>
</Collapsible>
```

- [ ] **Step 3: 验证条件显示逻辑**

Run dev server:
```bash
cd tenant_platform/web
npm run dev
```

在浏览器中：
1. 访问 LLM Providers 页面
2. Thinking Type 选择 "Enabled"
3. 验证 "Thinking Budget Tokens" 字段显示
4. 切换回 "Adaptive"
5. 验证字段隐藏

Expected: 条件显示正常工作

- [ ] **Step 4: Commit**

```bash
git add tenant_platform/web/src/features/admin/LLMProvidersPage.tsx
git commit -m "feat: 添加'推理与思考'折叠分组，包含条件显示逻辑

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

---

### Task 5: 添加"采样参数"分组的表单字段

**Files:**
- Modify: `tenant_platform/web/src/features/admin/LLMProvidersPage.tsx:170-194`

**Interfaces:**
- Consumes: `Collapsible` from Task 2, expanded `form.config` from Task 3
- Produces: "采样参数"折叠分组，包含 3 个已有字段（temperature, max_tokens, top_p）

- [ ] **Step 1: 将现有的 Temperature、Max Tokens、Top P 移入折叠组**

找到现有的这 3 个 Input 组件（约第 170-194 行），用折叠组包裹：

```tsx
<Collapsible title="🎛️ 采样参数">
  <Input
    label="Temperature"
    type="number"
    step="0.01"
    placeholder="0.0 - 2.0"
    value={form.config.temperature || ''}
    onChange={(e) => setForm({ ...form, config: { ...form.config, temperature: e.target.value ? parseFloat(e.target.value) : undefined } })}
  />

  <Input
    label="Max Tokens"
    type="number"
    placeholder="例如 8192"
    value={form.config.max_tokens || ''}
    onChange={(e) => setForm({ ...form, config: { ...form.config, max_tokens: e.target.value ? parseInt(e.target.value) : undefined } })}
  />

  <Input
    label="Top P"
    type="number"
    step="0.01"
    placeholder="0.0 - 1.0"
    value={form.config.top_p || ''}
    onChange={(e) => setForm({ ...form, config: { ...form.config, top_p: e.target.value ? parseFloat(e.target.value) : undefined } })}
  />
</Collapsible>
```

- [ ] **Step 2: 验证 UI 渲染**

Run:
```bash
cd tenant_platform/web
npm run dev
```

在浏览器中验证"采样参数"分组可以正常展开/折叠

- [ ] **Step 3: Commit**

```bash
git add tenant_platform/web/src/features/admin/LLMProvidersPage.tsx
git commit -m "feat: 将采样参数字段组织到折叠分组

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

---

### Task 6: 添加"容量与超时"分组的表单字段

**Files:**
- Modify: `tenant_platform/web/src/features/admin/LLMProvidersPage.tsx:196-210`

**Interfaces:**
- Consumes: `Collapsible` from Task 2, expanded `form.config` from Task 3
- Produces: "容量与超时"折叠分组，包含 4 个字段（context_win, max_retries, connect_timeout, read_timeout）

- [ ] **Step 1: 将现有 Max Retries 和 Timeout 移入折叠组并添加新字段**

找到现有的 Max Retries 和 Timeout Input（约第 196-210 行），替换为：

```tsx
<Collapsible title="⏱️ 容量与超时">
  <Input
    label="Context Win"
    type="number"
    placeholder="例如 30000"
    value={form.config.context_win || ''}
    onChange={(e) => setForm({ ...form, config: { ...form.config, context_win: e.target.value ? parseInt(e.target.value) : undefined } })}
  />

  <Input
    label="Max Retries"
    type="number"
    placeholder="例如 3"
    value={form.config.max_retries || ''}
    onChange={(e) => setForm({ ...form, config: { ...form.config, max_retries: e.target.value ? parseInt(e.target.value) : undefined } })}
  />

  <Input
    label="Connect Timeout (秒)"
    type="number"
    placeholder="例如 5"
    value={form.config.connect_timeout || ''}
    onChange={(e) => setForm({ ...form, config: { ...form.config, connect_timeout: e.target.value ? parseInt(e.target.value) : undefined } })}
  />

  <Input
    label="Read Timeout (秒)"
    type="number"
    placeholder="例如 30"
    value={form.config.read_timeout || ''}
    onChange={(e) => setForm({ ...form, config: { ...form.config, read_timeout: e.target.value ? parseInt(e.target.value) : undefined } })}
  />
</Collapsible>
```

- [ ] **Step 2: 验证编译**

Run:
```bash
cd tenant_platform/web
npm run type-check
```

Expected: No errors

- [ ] **Step 3: Commit**

```bash
git add tenant_platform/web/src/features/admin/LLMProvidersPage.tsx
git commit -m "feat: 添加'容量与超时'折叠分组，包含 4 个字段

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

---

### Task 7: 添加"传输与协议"分组的表单字段

**Files:**
- Modify: `tenant_platform/web/src/features/admin/LLMProvidersPage.tsx` (在 Task 6 之后)

**Interfaces:**
- Consumes: `Collapsible` from Task 2, expanded `form.config` from Task 3
- Produces: "传输与协议"折叠分组，包含 2 个字段（stream, api_mode）

- [ ] **Step 1: 在"容量与超时"分组后添加新的折叠组**

```tsx
<Collapsible title="🔄 传输与协议">
  <div>
    <label className="input-label">
      <input
        type="checkbox"
        checked={form.config.stream !== false}
        onChange={(e) => setForm({ ...form, config: { ...form.config, stream: e.target.checked } })}
        style={{ marginRight: '8px' }}
      />
      Stream（流式传输）
    </label>
    <p style={{ fontSize: '12px', color: 'var(--text-muted)', marginTop: '4px' }}>
      默认启用，实时返回生成内容
    </p>
  </div>

  {form.provider_type === 'native_oai' && (
    <div>
      <label className="input-label">API Mode（仅 OpenAI）</label>
      <select
        className="input-field"
        value={form.config.api_mode || ''}
        onChange={(e) => setForm({ ...form, config: { ...form.config, api_mode: e.target.value as any || undefined } })}
        style={{ width: '100%', padding: '8px 12px' }}
      >
        <option value="">默认（chat_completions）</option>
        <option value="chat_completions">Chat Completions</option>
        <option value="responses">Responses</option>
      </select>
    </div>
  )}
</Collapsible>
```

- [ ] **Step 2: 验证条件显示逻辑**

Run dev server:
```bash
cd tenant_platform/web
npm run dev
```

在浏览器中：
1. provider_type 选择 "native_oai"
2. 验证 "API Mode" 字段显示
3. 切换为 "native_claude"
4. 验证 "API Mode" 字段隐藏

Expected: 条件显示正常工作

- [ ] **Step 3: Commit**

```bash
git add tenant_platform/web/src/features/admin/LLMProvidersPage.tsx
git commit -m "feat: 添加'传输与协议'分组，包含条件显示的 API Mode

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

---

### Task 8: 添加"Claude 专属"和"网络"分组

**Files:**
- Modify: `tenant_platform/web/src/features/admin/LLMProvidersPage.tsx` (在 Task 7 之后)

**Interfaces:**
- Consumes: `Collapsible` from Task 2, expanded `form.config` from Task 3
- Produces: "Claude 专属"折叠分组（条件显示）和"网络"折叠分组

- [ ] **Step 1: 添加 Claude 专属分组（条件显示）**

```tsx
{form.provider_type === 'native_claude' && (
  <Collapsible title="🤖 Claude 专属">
    <div>
      <label className="input-label">
        <input
          type="checkbox"
          checked={form.config.fake_cc_system_prompt || false}
          onChange={(e) => setForm({ ...form, config: { ...form.config, fake_cc_system_prompt: e.target.checked } })}
          style={{ marginRight: '8px' }}
        />
        Fake CC System Prompt
      </label>
      <p style={{ fontSize: '12px', color: 'var(--text-muted)', marginTop: '4px' }}>
        CC 透传渠道必须启用
      </p>
    </div>

    <Input
      label="User Agent"
      type="text"
      placeholder="可选，自定义 UA"
      value={form.config.user_agent || ''}
      onChange={(e) => setForm({ ...form, config: { ...form.config, user_agent: e.target.value || undefined } })}
    />
  </Collapsible>
)}
```

- [ ] **Step 2: 添加网络分组**

```tsx
<Collapsible title="🌐 网络">
  <div className="provider-form-full">
    <Input
      label="Proxy"
      type="text"
      placeholder="http://proxy:port"
      value={form.config.proxy || ''}
      onChange={(e) => setForm({ ...form, config: { ...form.config, proxy: e.target.value || undefined } })}
    />
  </div>
</Collapsible>
```

- [ ] **Step 3: 验证条件显示**

Run dev server 并测试：
1. provider_type 选择 "native_claude"
2. 验证"Claude 专属"分组显示
3. 切换为 "native_oai"
4. 验证"Claude 专属"分组隐藏
5. 验证"网络"分组始终显示

Expected: 所有条件显示正常工作

- [ ] **Step 4: Commit**

```bash
git add tenant_platform/web/src/features/admin/LLMProvidersPage.tsx
git commit -m "feat: 添加 Claude 专属和网络配置分组

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

---

### Task 9: 补全后端 mykey_generator.go

**Files:**
- Modify: `tenant_platform/backend-go/internal/api/mykey_generator.go:60-90`

**Interfaces:**
- Consumes: 完整的 `domain.LLMProviderConfig`（已有所有 16 个字段）
- Produces: 完整的 mykey.py 生成逻辑，输出所有字段

- [ ] **Step 1: 找到字段生成循环位置**

打开 `mykey_generator.go`，定位到第 60 行附近的 config 字段生成代码（在 `if p.Config.ThinkingType != ""` 之后）

- [ ] **Step 2: 添加 10 个新字段的生成逻辑**

在现有的 `timeout` 字段生成代码之后，添加：

```go
		// ── 推理 / 思考（新增）──
		if p.Config.ThinkingBudgetTokens > 0 {
			sb.WriteString(fmt.Sprintf("    'thinking_budget_tokens': %d,\n", p.Config.ThinkingBudgetTokens))
		}
		if p.Config.ReasoningEffort != "" {
			sb.WriteString(fmt.Sprintf("    'reasoning_effort': '%s',\n", p.Config.ReasoningEffort))
		}

		// ── 容量 / 超时（新增）──
		if p.Config.ContextWin > 0 {
			sb.WriteString(fmt.Sprintf("    'context_win': %d,\n", p.Config.ContextWin))
		}
		if p.Config.ConnectTimeout > 0 {
			sb.WriteString(fmt.Sprintf("    'connect_timeout': %d,\n", p.Config.ConnectTimeout))
		}
		if p.Config.ReadTimeout > 0 {
			sb.WriteString(fmt.Sprintf("    'read_timeout': %d,\n", p.Config.ReadTimeout))
		}

		// ── 传输（新增）──
		if p.Config.Stream != nil {
			sb.WriteString(fmt.Sprintf("    'stream': %t,\n", *p.Config.Stream))
		}
		if p.Config.APIMode != "" {
			sb.WriteString(fmt.Sprintf("    'api_mode': '%s',\n", p.Config.APIMode))
		}

		// ── Claude 专属（新增）──
		if p.Config.FakeCCSystemPrompt != nil {
			sb.WriteString(fmt.Sprintf("    'fake_cc_system_prompt': %t,\n", *p.Config.FakeCCSystemPrompt))
		}
		if p.Config.UserAgent != "" {
			sb.WriteString(fmt.Sprintf("    'user_agent': '%s',\n", p.Config.UserAgent))
		}

		// ── 网络（新增）──
		if p.Config.Proxy != "" {
			sb.WriteString(fmt.Sprintf("    'proxy': '%s',\n", p.Config.Proxy))
		}
```

- [ ] **Step 3: 编译验证**

Run:
```bash
cd tenant_platform/backend-go
go build ./cmd/backend
```

Expected: No errors

- [ ] **Step 4: Commit**

```bash
git add tenant_platform/backend-go/internal/api/mykey_generator.go
git commit -m "feat: 补全 mykey.py 生成器，支持所有 16 个配置字段

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

---

### Task 10: 端到端测试验证

**Files:**
- 测试所有变更的集成

**Interfaces:**
- Consumes: 所有前面任务的产出
- Produces: 验证通过的完整功能

- [ ] **Step 1: 启动后端服务**

Run:
```bash
cd tenant_platform
./start-backend.ps1
```

Expected: Backend starts on port 8080

- [ ] **Step 2: 启动前端开发服务器**

Run:
```bash
cd tenant_platform/web
npm run dev
```

Expected: Frontend starts on port 5173

- [ ] **Step 3: 测试创建 native_oai Provider（完整配置）**

在浏览器中：
1. 访问 http://localhost:5173/admin/llm-providers
2. 填写基础配置：
   - 名称: "OpenAI Test"
   - 类型: native_oai
   - Base URL: https://api.openai.com/v1
   - 模型: gpt-4
   - API Key: sk-test-xxx
3. 展开"推理与思考"，设置 reasoning_effort = high
4. 展开"采样参数"，设置 temperature = 0.7
5. 展开"容量与超时"，设置 context_win = 50000
6. 展开"传输与协议"，验证 Stream 默认勾选，设置 api_mode = responses
7. 展开"网络"，设置 proxy = http://proxy:8080
8. 点击"保存"

Expected: Provider 创建成功，显示成功消息

- [ ] **Step 4: 验证 mykey.py 生成包含所有字段**

Run:
```bash
curl http://localhost:8080/api/admin/config/mykey | grep -E "(reasoning_effort|context_win|stream|api_mode|proxy)"
```

Expected: 输出包含所有设置的字段：
```python
'reasoning_effort': 'high',
'context_win': 50000,
'stream': True,
'api_mode': 'responses',
'proxy': 'http://proxy:8080',
```

- [ ] **Step 5: 测试创建 native_claude Provider（Claude 专属字段）**

在浏览器中：
1. 创建新 Provider
2. 类型选择 native_claude
3. 验证"Claude 专属"分组显示
4. 勾选 Fake CC System Prompt
5. 填写 User Agent = "Custom UA"
6. 保存

Expected: Provider 创建成功

- [ ] **Step 6: 验证 Claude 专属字段生成**

Run:
```bash
curl http://localhost:8080/api/admin/config/mykey | grep -E "(fake_cc_system_prompt|user_agent)"
```

Expected:
```python
'fake_cc_system_prompt': True,
'user_agent': 'Custom UA',
```

- [ ] **Step 7: 测试条件显示逻辑**

在浏览器中：
1. thinking_type 选择 "enabled"，验证 thinking_budget_tokens 显示
2. thinking_type 改为 "adaptive"，验证字段隐藏
3. provider_type 切换 native_oai/native_claude，验证对应专属字段显隐

Expected: 所有条件显示逻辑正常工作

- [ ] **Step 8: 边界测试 - 所有字段留空**

1. 创建只填必填字段的 Provider（名称、类型、Base URL、模型、API Key）
2. 所有高级配置留空
3. 保存

Expected: 创建成功，mykey.py 只包含必填字段

- [ ] **Step 9: 最终验证提交**

Run:
```bash
git status
```

Expected: 应该有以下提交：
- feat: 扩展 LLMProviderConfig 类型定义
- feat: 添加 Collapsible 折叠组件
- feat: 扩展 LLMProvidersPage 表单状态
- feat: 添加'推理与思考'折叠分组
- feat: 将采样参数字段组织到折叠分组
- feat: 添加'容量与超时'折叠分组
- feat: 添加'传输与协议'分组
- feat: 添加 Claude 专属和网络配置分组
- feat: 补全 mykey.py 生成器

All 9 commits should have Co-Authored-By tag

---

## 完成标准

- [ ] 所有 16 个配置字段可通过 Web 页面配置
- [ ] 条件显示逻辑正确（thinking_budget_tokens, api_mode, Claude 专属分组）
- [ ] mykey.py 生成器输出所有配置字段
- [ ] TypeScript 类型检查通过
- [ ] Go 编译通过
- [ ] 端到端测试通过
- [ ] 所有提交包含 Co-Authored-By 标签

---

## 故障排查

**问题：Collapsible 组件样式不生效**
- 检查 CSS 变量定义（--border, --surface, --text 等）
- 如果缺失，在全局 CSS 中添加默认值

**问题：mykey.py 生成的 Python 语法错误**
- 运行 `python -m py_compile mykey.py` 验证语法
- 检查字段值中的特殊字符是否需要转义

**问题：TypeScript 类型错误**
- 确保 `types.ts` 中的字段名与 Go struct tag 完全一致（snake_case）
- 确保可选字段使用 `?:` 语法


