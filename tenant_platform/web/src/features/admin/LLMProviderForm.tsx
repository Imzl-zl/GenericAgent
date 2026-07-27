import { useState, type FormEvent, type ReactNode } from 'react';
import { Save, X } from 'lucide-react';
import type { CreateProviderInput } from '../../api/providers';
import type {
  GASessionConfig,
  LLMProvider,
  LLMProviderType,
  ProviderTransportConfig,
} from '../../api/types';
import { Button } from '../../components/ui/Button';
import { Collapsible } from '../../components/ui/Collapsible';
import { Input } from '../../components/ui/Input';

export type ProviderFormValue = CreateProviderInput;

interface LLMProviderFormProps {
  provider?: LLMProvider;
  onSave: (value: ProviderFormValue) => Promise<boolean>;
  onCancel: () => void;
}

const THINKING_TYPES = ['adaptive', 'enabled', 'disabled'] as const;
const REASONING_EFFORTS = ['none', 'minimal', 'low', 'medium', 'high', 'xhigh', 'max'] as const;
const API_MODES = ['chat_completions', 'responses'] as const;
const SERVICE_TIERS = ['auto', 'default', 'priority', 'flex'] as const;
const AUTH_MODES = ['auto', 'bearer', 'x_api_key'] as const;

function initialForm(provider?: LLMProvider): ProviderFormValue {
  return {
    name: provider?.name ?? '',
    provider_type: provider?.provider_type ?? 'native_oai',
    base_url: provider?.base_url ?? '',
    model: provider?.model ?? '',
    api_key: '',
    session_config: provider ? { ...provider.session_config } : { stream: true },
    transport_config: provider
      ? { ...provider.transport_config }
      : { auth_mode: 'auto', tls_verify: true },
  };
}

function optionalNumber(value: string): number | undefined {
  return value === '' ? undefined : Number(value);
}

function optionalChoice<T extends string>(value: string, choices: readonly T[]): T | undefined {
  return choices.find((choice) => choice === value);
}

function SelectField({ label, value, onChange, children }: {
  label: string;
  value: string;
  onChange: (value: string) => void;
  children: ReactNode;
}) {
  return (
    <label className="input-wrapper">
      <span className="input-label">{label}</span>
      <select className="input-field" value={value} onChange={(event) => onChange(event.target.value)}>
        {children}
      </select>
    </label>
  );
}

export function LLMProviderForm({ provider, onSave, onCancel }: LLMProviderFormProps) {
  const [form, setForm] = useState<ProviderFormValue>(() => initialForm(provider));
  const [isSaving, setIsSaving] = useState(false);
  const [validationError, setValidationError] = useState('');
  const editing = provider !== undefined;

  const patchSession = (patch: Partial<GASessionConfig>) => {
    setForm((current) => ({
      ...current,
      session_config: { ...current.session_config, ...patch },
    }));
  };

  const patchTransport = (patch: Partial<ProviderTransportConfig>) => {
    setForm((current) => ({
      ...current,
      transport_config: { ...current.transport_config, ...patch },
    }));
  };

  const selectProviderType = (providerType: LLMProviderType) => {
    setForm((current) => ({
      ...current,
      provider_type: providerType,
      session_config: {
        ...current.session_config,
        api_mode: providerType === 'native_oai' ? current.session_config.api_mode : undefined,
        service_tier: providerType === 'native_oai' ? current.session_config.service_tier : undefined,
        fake_cc_system_prompt:
          providerType === 'native_claude' ? current.session_config.fake_cc_system_prompt : undefined,
      },
    }));
  };

  const handleSubmit = async (event: FormEvent) => {
    event.preventDefault();
    const budget = form.session_config.thinking_budget_tokens;
    if (form.session_config.thinking_type === 'enabled' && (budget === undefined || budget <= 0)) {
      setValidationError('Thinking Budget 必须大于 0');
      return;
    }
    setValidationError('');
    setIsSaving(true);
    try {
      if (await onSave(form)) {
        setForm(initialForm());
      }
    } finally {
      setIsSaving(false);
    }
  };

  return (
    <form className="provider-form" onSubmit={handleSubmit}>
      <div className="provider-form-full provider-type-switch" aria-label="Provider type">
        {(['native_oai', 'native_claude'] as const).map((providerType) => (
          <button
            className={form.provider_type === providerType ? 'is-active' : ''}
            key={providerType}
            type="button"
            aria-pressed={form.provider_type === providerType}
            onClick={() => selectProviderType(providerType)}
          >
            {providerType === 'native_oai' ? 'Native OAI' : 'Native Claude'}
          </button>
        ))}
      </div>

      {validationError && (
        <span className="provider-form-full input-error" role="alert">{validationError}</span>
      )}

      <Input
        label="名称"
        required
        value={form.name}
        onChange={(event) => setForm({ ...form, name: event.target.value })}
      />
      <Input
        label="模型"
        required
        value={form.model}
        onChange={(event) => setForm({ ...form, model: event.target.value })}
      />
      <Input
        className="provider-form-full"
        label="Base URL"
        type="url"
        required
        placeholder="https://api.openai.com/v1"
        value={form.base_url}
        onChange={(event) => setForm({ ...form, base_url: event.target.value })}
      />
      <Input
        className="provider-form-full"
        label={editing ? 'API Key（留空保留）' : 'API Key'}
        type="password"
        required={!editing}
        autoComplete="new-password"
        value={form.api_key}
        onChange={(event) => setForm({ ...form, api_key: event.target.value })}
      />

      <div className="provider-form-full provider-inline-toggles">
        <label className="provider-toggle">
          <input
            type="checkbox"
            checked={form.session_config.stream ?? true}
            onChange={(event) => patchSession({ stream: event.target.checked })}
          />
          <span>Streaming</span>
        </label>
        <label className="provider-toggle">
          <input
            type="checkbox"
            checked={form.session_config.omit_thinking ?? false}
            onChange={(event) => patchSession({ omit_thinking: event.target.checked })}
          />
          <span>Omit thinking</span>
        </label>
      </div>

      <div className="provider-form-full">
        <Collapsible title="模型行为" defaultOpen>
          <div className="provider-config-grid">
            {form.provider_type === 'native_oai' && (
              <>
                <SelectField
                  label="API Mode"
                  value={form.session_config.api_mode ?? ''}
                  onChange={(value) => patchSession({ api_mode: optionalChoice(value, API_MODES) })}
                >
                  <option value="">默认 (chat_completions)</option>
                  {API_MODES.map((mode) => <option key={mode} value={mode}>{mode}</option>)}
                </SelectField>
                <SelectField
                  label="Service Tier"
                  value={form.session_config.service_tier ?? ''}
                  onChange={(value) => patchSession({ service_tier: optionalChoice(value, SERVICE_TIERS) })}
                >
                  <option value="">默认</option>
                  {SERVICE_TIERS.map((tier) => <option key={tier} value={tier}>{tier}</option>)}
                </SelectField>
              </>
            )}
            {form.provider_type === 'native_claude' && (
              <label className="provider-toggle provider-config-span">
                <input
                  type="checkbox"
                  checked={form.session_config.fake_cc_system_prompt ?? false}
                  onChange={(event) => patchSession({ fake_cc_system_prompt: event.target.checked })}
                />
                <span>Fake Claude Code system prompt</span>
              </label>
            )}
            <Input
              label="User Agent"
              value={form.session_config.user_agent ?? ''}
              onChange={(event) => patchSession({ user_agent: event.target.value || undefined })}
            />
          </div>
        </Collapsible>
      </div>

      <div className="provider-form-full">
        <Collapsible title="推理与采样">
          <div className="provider-config-grid">
            <SelectField
              label="Thinking Type"
              value={form.session_config.thinking_type ?? ''}
              onChange={(value) => patchSession({ thinking_type: optionalChoice(value, THINKING_TYPES) })}
            >
              <option value="">默认</option>
              {THINKING_TYPES.map((type) => <option key={type} value={type}>{type}</option>)}
            </SelectField>
            {form.session_config.thinking_type === 'enabled' && (
              <Input
                label="Thinking Budget"
                type="number"
                min={1}
                required
                value={form.session_config.thinking_budget_tokens ?? ''}
                onChange={(event) => patchSession({ thinking_budget_tokens: optionalNumber(event.target.value) })}
              />
            )}
            <SelectField
              label="Reasoning Effort"
              value={form.session_config.reasoning_effort ?? ''}
              onChange={(value) => patchSession({ reasoning_effort: optionalChoice(value, REASONING_EFFORTS) })}
            >
              <option value="">默认</option>
              {REASONING_EFFORTS.map((effort) => <option key={effort} value={effort}>{effort}</option>)}
            </SelectField>
            <Input
              label="Temperature"
              type="number"
              min={0}
              max={2}
              step={0.01}
              value={form.session_config.temperature ?? ''}
              onChange={(event) => patchSession({ temperature: optionalNumber(event.target.value) })}
            />
            <Input
              label="Max Tokens"
              type="number"
              min={1}
              value={form.session_config.max_tokens ?? ''}
              onChange={(event) => patchSession({ max_tokens: optionalNumber(event.target.value) })}
            />
          </div>
        </Collapsible>
      </div>

      <div className="provider-form-full">
        <Collapsible title="上下文与重试">
          <div className="provider-config-grid">
            <Input
              label="Context Window"
              type="number"
              min={1}
              value={form.session_config.context_win ?? ''}
              onChange={(event) => patchSession({ context_win: optionalNumber(event.target.value) })}
            />
            <Input
              label="Trim Keep Prefix"
              type="number"
              min={0}
              value={form.session_config.trim_keep_prefix ?? ''}
              onChange={(event) => patchSession({ trim_keep_prefix: optionalNumber(event.target.value) })}
            />
            <Input
              label="Max Retries"
              type="number"
              min={0}
              value={form.session_config.max_retries ?? ''}
              onChange={(event) => patchSession({ max_retries: optionalNumber(event.target.value) })}
            />
            <Input
              label="Read Timeout (s)"
              type="number"
              min={5}
              value={form.session_config.read_timeout ?? ''}
              onChange={(event) => patchSession({ read_timeout: optionalNumber(event.target.value) })}
            />
          </div>
        </Collapsible>
      </div>

      <div className="provider-form-full">
        <Collapsible title="网络传输">
          <div className="provider-config-grid">
            <SelectField
              label="Auth Mode"
              value={form.transport_config.auth_mode}
              onChange={(value) => patchTransport({
                auth_mode: optionalChoice(value, AUTH_MODES) ?? 'auto',
              })}
            >
              {AUTH_MODES.map((mode) => <option key={mode} value={mode}>{mode}</option>)}
            </SelectField>
            <Input
              label="Proxy URL"
              type="url"
              value={form.transport_config.proxy_url ?? ''}
              onChange={(event) => patchTransport({ proxy_url: event.target.value || undefined })}
            />
            <Input
              label="Connect Timeout (s)"
              type="number"
              min={1}
              value={form.transport_config.connect_timeout_seconds ?? ''}
              onChange={(event) => patchTransport({ connect_timeout_seconds: optionalNumber(event.target.value) })}
            />
            <Input
              label="Response Header Timeout (s)"
              type="number"
              min={1}
              value={form.transport_config.response_header_timeout_seconds ?? ''}
              onChange={(event) => patchTransport({ response_header_timeout_seconds: optionalNumber(event.target.value) })}
            />
            <label className="provider-toggle provider-config-span">
              <input
                type="checkbox"
                checked={form.transport_config.tls_verify ?? true}
                onChange={(event) => patchTransport({ tls_verify: event.target.checked })}
              />
              <span>TLS certificate verification</span>
            </label>
          </div>
        </Collapsible>
      </div>

      <label className="provider-form-full input-wrapper">
        <span className="input-label">Extra System Prompt</span>
        <textarea
          className="input-field provider-textarea"
          rows={3}
          value={form.session_config.extra_sys_prompt ?? ''}
          onChange={(event) => patchSession({ extra_sys_prompt: event.target.value || undefined })}
        />
      </label>

      <div className="provider-form-full provider-actions">
        {editing && (
          <Button type="button" variant="secondary" onClick={onCancel}>
            <X size={15} /> 取消
          </Button>
        )}
        <Button type="submit" isLoading={isSaving}>
          <Save size={15} /> {editing ? '保存修改' : '创建 Provider'}
        </Button>
      </div>
    </form>
  );
}
