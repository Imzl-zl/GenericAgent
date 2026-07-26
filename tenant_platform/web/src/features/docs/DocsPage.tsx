import { useEffect, useRef, useState, type ElementType, type ReactNode } from 'react';
import {
  BookOpen,
  UserPlus,
  ShieldCheck,
  QrCode,
  MessageCircle,
  Shield,
  Users,
  Ticket,
  Cpu,
  Terminal,
  Smartphone,
  Lock,
  Clock,
  RefreshCw,
  ChevronDown,
  HelpCircle,
  FileText,
  Sparkles,
  UserCog,
  CheckCircle,
} from 'lucide-react';
import './DocsPage.css';

const userSteps = [
  {
    icon: UserPlus,
    title: '注册账号',
    body: '在登录页点击注册，输入用户名、密码和有效的邀请码。注册后账号状态为“待批准”。',
  },
  {
    icon: ShieldCheck,
    title: '等待审批',
    body: '运营者会在后台审批你的账号。审批通过后，仪表盘状态变为“已批准”，即可进行下一步。',
  },
  {
    icon: QrCode,
    title: '绑定微信',
    body: '进入“微信绑定”页，点击获取二维码，用微信扫一扫完成 iLink 官方授权，无需手动填写凭据。',
  },
  {
    icon: MessageCircle,
    title: '开始对话',
    body: '创建或选择人设后，在微信中直接发送消息，Agent 会通过官方协议接收并回复。',
  },
];

const adminSteps = [
  {
    icon: Shield,
    title: '运营者登录',
    body: '通过部署者提供的独立运营后台入口登录，使用部署时设置的 PLATFORM_DEV_TOKEN 进入控制台。',
  },
  {
    icon: Users,
    title: '审批用户',
    body: '在“用户审批”页查看待批准用户，点击“批准”激活账号；也可对已批准用户进行封禁。',
  },
  {
    icon: Ticket,
    title: '管理邀请码',
    body: '在“邀请码”页生成一次性邀请码，复制给用户，或撤销未使用的邀请码。',
  },
  {
    icon: Cpu,
    title: '配置 Provider',
    body: '在“LLM 供应”页添加上游模型（OpenAI 兼容或 Anthropic），设置默认 Provider 并加密存储 API Key。',
  },
];

const commands = [
  { cmd: '/help', desc: '查看当前可用的平台级命令列表。' },
  { cmd: '/status', desc: '查询当前会话是否有正在运行的任务。' },
  { cmd: '/stop', desc: '取消当前会话中正在运行的任务。' },
  { cmd: '/new', desc: '清空当前会话的 history 和 working，开启全新上下文。' },
  { cmd: '/llm', desc: '查看当前模型策略版本，模型由平台 LLM Proxy 统一管控。' },
  { cmd: '/我的身份', desc: '查看当前绑定的平台身份信息。' },
  { cmd: '/个人', desc: '切换回个人会话上下文。' },
  { cmd: '/团队', desc: '切换到团队会话上下文（需先加入团队）。' },
  { cmd: '/relay_on', desc: '开启 Relay 模式，@username 消息将直接转发给对应用户。' },
  { cmd: '/relay_off', desc: '关闭 Relay 模式，@username 按普通消息处理。' },
];

const bindingStatuses = [
  { label: 'wait', text: '等待扫码' },
  { label: 'scaned', text: '已扫码，等待确认' },
  { label: 'scaned_but_redirect', text: '扫码成功，正在连接' },
  { label: 'expired', text: '二维码已过期，需重新获取' },
  { label: 'confirmed', text: '绑定成功，Bot 已启动' },
];

const faqs = [
  {
    q: '为什么注册后无法立即绑定微信？',
    a: '平台采用邀请制和人工审批。注册后需要运营者在后台批准账号，状态变为“已批准”后才能发起微信绑定。',
  },
  {
    q: '微信绑定安全吗？平台会保存我的微信密码吗？',
    a: '平台使用微信官方 iLink 协议，通过扫码授权获取 Bot 凭据，不会保存你的微信密码。bot_token 会加密后存入数据库。',
  },
  {
    q: '二维码过期了怎么办？',
    a: '二维码有效期约为 4 到 5 分钟。过期后回到“微信绑定”页，点击“重新获取”即可生成新的二维码。',
  },
  {
    q: '人设的“公开”和“私有”有什么区别？',
    a: '私人设仅自己可见和使用；公开设需要提交审核，运营者批准后会进入公共商店，所有用户都可选用。',
  },
  {
    q: '如何切换当前使用的模型？',
    a: '普通用户不能自行切换模型。模型由运营者在后台的 LLM Provider 中配置，平台通过 LLM Proxy 统一调度。',
  },
  {
    q: '发送消息后多久能收到回复？',
    a: '消息进入任务队列后，Worker 会异步处理。处理时间取决于任务复杂度和当前队列长度，可在“运行状态”页查看。',
  },
];

function useReveal() {
  const ref = useRef<HTMLDivElement>(null);
  const [visible, setVisible] = useState(false);

  useEffect(() => {
    const el = ref.current;
    if (!el) return;

    const observer = new IntersectionObserver(
      ([entry]) => {
        if (entry.isIntersecting) {
          setVisible(true);
          observer.unobserve(el);
        }
      },
      { threshold: 0.12, rootMargin: '0px 0px -40px 0px' }
    );

    observer.observe(el);
    return () => observer.disconnect();
  }, []);

  return { ref, visible };
}

function RevealSection({ children, className = '' }: { children: ReactNode; className?: string }) {
  const { ref, visible } = useReveal();
  return (
    <div ref={ref} className={`docs-reveal ${visible ? 'visible' : ''} ${className}`}>
      {children}
    </div>
  );
}

function SectionHeader({
  icon: Icon,
  title,
  subtitle,
}: {
  icon: ElementType;
  title: string;
  subtitle: string;
}) {
  return (
    <div className="docs-section-header">
      <div className="docs-section-icon">
        <Icon size={22} strokeWidth={1.5} />
      </div>
      <div>
        <div className="docs-section-title">{title}</div>
        <div className="docs-section-subtitle">{subtitle}</div>
      </div>
    </div>
  );
}

export function DocsPage() {
  const [openFaq, setOpenFaq] = useState<number | null>(0);

  const toggleFaq = (idx: number) => {
    setOpenFaq((prev) => (prev === idx ? null : idx));
  };

  return (
    <div className="docs-page">
      <header className="docs-hero">
        <div className="docs-hero-label">
          <BookOpen size={14} />
          <span>Platform Manual</span>
        </div>
        <h1>使用文档</h1>
        <p>
          GenericAgent Tenant 是一个多租户 AI 平台。用户通过微信官方 iLink 协议与 Agent 对话，
          运营者通过后台管理账号、邀请码、LLM 供应和人设商店。
        </p>
      </header>

      <RevealSection className="docs-section">
        <SectionHeader
          icon={UserPlus}
          title="普通用户指南"
          subtitle="从注册到在微信中与 Agent 对话的完整流程"
        />
        <div className="docs-timeline">
          {userSteps.map((step, idx) => (
            <div key={step.title} className="docs-step">
              <div className="docs-step-number">{idx + 1}</div>
              <div className="docs-step-icon">
                <step.icon size={22} strokeWidth={1.5} />
              </div>
              <h3>{step.title}</h3>
              <p>{step.body}</p>
            </div>
          ))}
        </div>
      </RevealSection>

      <RevealSection className="docs-section">
        <SectionHeader
          icon={Shield}
          title="运营者指南"
          subtitle="管理用户、邀请码、模型供应和人设审核"
        />
        <div className="docs-timeline">
          {adminSteps.map((step, idx) => (
            <div key={step.title} className="docs-step">
              <div className="docs-step-number">{idx + 1}</div>
              <div className="docs-step-icon">
                <step.icon size={22} strokeWidth={1.5} />
              </div>
              <h3>{step.title}</h3>
              <p>{step.body}</p>
            </div>
          ))}
        </div>
      </RevealSection>

      <RevealSection className="docs-section">
        <SectionHeader
          icon={QrCode}
          title="微信绑定说明"
          subtitle="基于官方 iLink 协议的扫码登录流程"
        />
        <div className="docs-binding-flow">
          <div className="docs-binding-visual">
            <div className="docs-qr-placeholder">
              <QrCode size={72} strokeWidth={1} />
            </div>
            <div className="docs-binding-note">
              <Smartphone size={14} style={{ verticalAlign: 'middle', marginRight: 6 }} />
              使用微信“扫一扫”扫描页面中的二维码，然后在微信内点击确认授权
            </div>
            <div className="docs-tag">
              <Lock size={12} />
              <span>bot_token 加密存储</span>
            </div>
          </div>

          <div>
            <div className="docs-card" style={{ marginBottom: 20 }}>
              <h3>
                <Clock size={16} />
                有效期
              </h3>
              <p>
                二维码约 4 到 5 分钟有效。超时后会显示“expired”，需要点击“重新获取”按钮刷新二维码。
              </p>
            </div>
            <div className="docs-card">
              <h3>
                <RefreshCw size={16} />
                扫码状态
              </h3>
              <div className="docs-status-list" style={{ marginTop: 14 }}>
                {bindingStatuses.map((s) => (
                  <div key={s.label} className="docs-status-item">
                    <strong>{s.label}</strong>
                    <span>{s.text}</span>
                  </div>
                ))}
              </div>
            </div>
          </div>
        </div>
      </RevealSection>

      <RevealSection className="docs-section">
        <SectionHeader
          icon={UserCog}
          title="人设系统"
          subtitle="创建、管理和使用 Persona"
        />
        <div className="docs-grid-2">
          <div className="docs-card">
            <h3>
              <Sparkles size={16} />
              创建人设
            </h3>
            <p>
              在“人设”页填写名称、描述和系统提示词。系统提示词决定了 Agent 的回答风格、专业领域和行为边界。
            </p>
            <ul>
              <li>名称和系统提示词为必填项</li>
              <li>创建时可选“提交到公共商店”</li>
              <li>私有状态下可随时编辑和删除</li>
            </ul>
          </div>
          <div className="docs-card">
            <h3>
              <CheckCircle size={16} />
              默认与公开
            </h3>
            <p>
              点击星形图标可将某个人设设为默认，之后新任务会自动使用该人设。公开人设提交审核后，运营者批准即可进入公共商店。
            </p>
            <ul>
              <li>默认人设会高亮显示边框</li>
              <li>公共商店人设状态为“公开”</li>
              <li>被拒绝的人设可查看运营者备注</li>
            </ul>
          </div>
        </div>
      </RevealSection>

      <RevealSection className="docs-section">
        <SectionHeader
          icon={Terminal}
          title="平台命令"
          subtitle="在微信中直接发送以下命令控制会话"
        />
        <div className="docs-command-grid">
          {commands.map((c) => (
            <div key={c.cmd} className="docs-command">
              <code>{c.cmd}</code>
              <p>{c.desc}</p>
            </div>
          ))}
        </div>
      </RevealSection>

      <RevealSection className="docs-section">
        <SectionHeader
          icon={HelpCircle}
          title="常见问题"
          subtitle="使用过程中的常见疑问"
        />
        <div className="docs-faq-list">
          {faqs.map((faq, idx) => (
            <div key={idx} className={`docs-faq-item ${openFaq === idx ? 'open' : ''}`}>
              <button className="docs-faq-question" onClick={() => toggleFaq(idx)} type="button">
                <span>{faq.q}</span>
                <ChevronDown size={18} />
              </button>
              <div className="docs-faq-answer">
                <p>{faq.a}</p>
              </div>
            </div>
          ))}
        </div>
      </RevealSection>

      <RevealSection>
        <div className="docs-footer">
          <p>
            <FileText size={14} style={{ verticalAlign: 'middle', marginRight: 6 }} />
            本文档基于当前平台源码生成，反映实际前端页面与后端接口。
          </p>
        </div>
      </RevealSection>
    </div>
  );
}
