package application

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/infrastructure/transport"
)

// DefaultStreamThrottleWindow 是流式转发的节流合并窗口: chunk 到达后最多
// 每 500ms 向 IM 推送一次合并文本(飞书官方频控 5 QPS, 500ms 合并天然
// ≤2 QPS 安全余量——IM_STREAMING_DELIVERY 决策 3)。
const DefaultStreamThrottleWindow = 500 * time.Millisecond

// streamBatcher 是 per-task 流式转发缓冲: chunk 文本累积 + 时间窗口节流
// 合并。非并发安全(dispatch 循环单 goroutine 使用)。
//
// 语义: Due 检查到期(缓冲非空且距上次 flush ≥ window), Flush 取出合并
// 文本, Add 累积新 chunk。首个 chunk 不触发 flush(等待窗口, 短任务直接
// 交 delivery 终态); Terminal/空心跳时无条件 Flush。
type streamBatcher struct {
	window    time.Duration
	buf       strings.Builder
	lastFlush time.Time
}

func newStreamBatcher(window time.Duration) *streamBatcher {
	if window <= 0 {
		window = DefaultStreamThrottleWindow
	}
	return &streamBatcher{window: window}
}

// Add 累积一条 chunk 文本。首条 chunk 是窗口起点(500ms 从首条到达起算;
// 短任务单条文本在 Terminal 时随 FlushDue 发出)。
func (b *streamBatcher) Add(text string, now time.Time) {
	if b.buf.Len() == 0 && b.lastFlush.IsZero() {
		b.lastFlush = now
	}
	b.buf.WriteString(text)
}

// Due 报告缓冲非空且距上次 flush 已达窗口(调用方应 Flush)。
func (b *streamBatcher) Due(now time.Time) bool {
	return b.buf.Len() > 0 && now.Sub(b.lastFlush) >= b.window
}

// Flush 取出累积文本并重置窗口。返回 (text, hasContent)。
func (b *streamBatcher) Flush(now time.Time) (string, bool) {
	if b.buf.Len() == 0 {
		return "", false
	}
	text := b.buf.String()
	b.buf.Reset()
	b.lastFlush = now
	return text, true
}

// Empty reports 缓冲为空。
func (b *streamBatcher) Empty() bool { return b.buf.Len() == 0 }

// RuntimeStreamSettings 提供管理员级 IM 流式模式开关(任务 6 实现持久化,
// 接口先立——scheduler 转发判定依赖)。
type RuntimeStreamSettings interface {
	GetIMStreamingMode(ctx context.Context) (domain.IMStreamingMode, error)
}

// StreamForwarder 编排单任务的流式转发生命周期:
//   open(BeginReply) → append* → commit/abort。
//
// 转发判定(IM_STREAMING_DELIVERY §4.4 群聊收敛 + §5 模式开关):
//   * StreamingSender 未接线 → 关闭(测试/loopback 无流式依赖)
//   * im_streaming_mode != streaming → 关闭(off/final_only 只发最终结果)
//   * 群聊桶 → 关闭(群聊统一只发最终结果, 防刷屏 + QQ 群被动回复限制)
//   * 渠道能力分档: 仅 feishu(消息编辑)、qq 单聊(原生流式)与 wecom
//     (SEND_MSG stream 帧)支持流式;
//     wechat(iLink 无流式)/dingtalk(v1 不分片)/web → 关闭
//
// 失败语义(决策 4): 转发中途失败(限流/SDK 错误) → Abort 弃流, 由既有
// delivery 路径补发最终结果(任务终态 delivery 不受流式影响); 流式片段
// 不写 messages 审计(只记终态, 与现 delivery 一致)。
//
// 非并发: 生命周期绑定单任务 dispatch goroutine。
type StreamForwarder struct {
	streaming transport.StreamingSender
	bots      ChannelResolverByOwner
	settings  RuntimeStreamSettings
	task      domain.Task
	batcher   *streamBatcher

	reply transport.StreamReply
	open  bool
	// closed 标记流已结束(Commit/Abort 后幂等, 防重复调用)。
	closed bool
	// failed 标记流式已放弃(open/append 失败): 后续文本不再转发, 等待
	// Abort 或直接忽略(未 open 过则无需 abort)。
	failed bool
}

// NewStreamForwarder 构造转发器。streaming 为 nil 时 Enabled() 恒 false
// (非流式部署零开销)。
func NewStreamForwarder(streaming transport.StreamingSender, bots ChannelResolverByOwner, settings RuntimeStreamSettings, task domain.Task) *StreamForwarder {
	return &StreamForwarder{
		streaming: streaming,
		bots:      bots,
		settings:  settings,
		task:      task,
		batcher:   newStreamBatcher(DefaultStreamThrottleWindow),
	}
}

// Enabled 判定该任务是否应走流式转发(转发判定集中于此, 单一真值)。
func (f *StreamForwarder) Enabled() bool {
	if f.streaming == nil || f.settings == nil || f.bots == nil {
		return false
	}
	if domain.IsGroupConversation(f.task.ConversationType) {
		// 群聊收敛: 群聊统一只发最终结果(IM_STREAMING_DELIVERY §4.4)。
		return false
	}
	switch f.task.Source {
	case domain.SourceFeishu, domain.SourceQQ, domain.SourceWecom:
		// 渠道能力分档: 飞书/QQ/企业微信有流式能力(群聊已在群聊收敛排除)。
	default:
		// wechat(iLink 非流) / dingtalk(v1 不分片) / web → 仅终态。
		return false
	}
	mode, err := f.settings.GetIMStreamingMode(context.Background())
	if err != nil || !mode.StreamingEnabled() {
		// 读失败 fail-closed: 不转发(与 off 一致), 终态 delivery 兜底。
		return false
	}
	return true
}

// AppendText 累积一条 chunk 文本; 窗口到期时 flush 并转发。调用方在每次
// 非空 chunk 后调用。now 用于节流判定(测试注入)。
func (f *StreamForwarder) AppendText(ctx context.Context, text string, now time.Time) {
	// TrimSpace 防护: 纯空白 chunk 不进入缓冲(open 首帧文本必须非空——
	// 空/纯空白会让 poller 以 '…' 占位开场, QQ replace 前缀契约下占位
	// 会残留进终态内容)。worker 侧清洗后文本已 strip, 此处是防御边界。
	if strings.TrimSpace(text) == "" || f.failed || !f.Enabled() {
		return
	}
	if f.batcher.Due(now) {
		f.flush(ctx, now)
		if f.failed {
			return
		}
	}
	f.batcher.Add(text, now)
}

// FlushDue 无条件 flush 剩余缓冲(Terminal/空心跳时调用)。
func (f *StreamForwarder) FlushDue(ctx context.Context, now time.Time) {
	if f.failed {
		return
	}
	f.flush(ctx, now)
}

func (f *StreamForwarder) flush(ctx context.Context, now time.Time) {
	frag, ok := f.batcher.Flush(now)
	if !ok || f.failed {
		return
	}
	if !f.open {
		// open 携带首段文本: 前缀严格渠道(QQ replace 模式)的首帧即该文本,
		// 后续 append 帧保持前缀连续(官方契约: 每帧须以已下发 SentContent
		// 开头; 首帧占位与累积不一致会 40007)。open 失败 → frag 随 failed
		// 弃置, 终态 delivery 补发完整结果。
		if err := f.openReply(ctx, frag); err != nil {
			return
		}
	} else if err := f.reply.Append(ctx, frag); err != nil {
		f.abortWith(ctx, "append failed; final result delivered by existing delivery path", err)
	}
}

// openReply 解析回复目标并开启流式回复。失败 → 整个流式放弃(日志 + 置
// failed), 终态 delivery 兜底(无 stream_final_at 标记)。
func (f *StreamForwarder) openReply(ctx context.Context, firstText string) error {
	channelType := channelTypeForTaskSource(f.task.Source)
	bot, err := f.bots.GetChannelConfigByOwnerAndType(ctx, f.task.RequesterID, channelType)
	if err != nil || !bot.IsBound() {
		f.failed = true
		slog.WarnContext(ctx, "stream: reply target resolve failed; falling back to final delivery",
			"task_id", f.task.ID, "source", f.task.Source, "error", err)
		return err
	}
	replyTarget := bot.IlinkUserID
	if bot.ChannelType != domain.ChannelWechat {
		replyTarget = f.task.ConversationKey
	}
	reply, err := f.streaming.BeginReply(ctx, bot.BotUUID, replyTarget, f.task.ID, firstText)
	if err != nil {
		f.failed = true
		slog.WarnContext(ctx, "stream: BeginReply failed; falling back to final delivery",
			"task_id", f.task.ID, "bot_uuid", bot.BotUUID, "error", err)
		return err
	}
	f.reply = reply
	f.open = true
	return nil
}

// Commit 终态收尾(任务成功): flush 剩余缓冲(含未 open 场景——缓冲有文本
// 则 open+append 后 commit, 流式消息即最终交付) + commit 流。返回是否已
// commit(调用方据此置位 stream_final_at, 抑制 delivery 文本 part)。
func (f *StreamForwarder) Commit(ctx context.Context, now time.Time) bool {
	if f.closed {
		return false
	}
	if !f.failed {
		f.flush(ctx, now)
	}
	if !f.open || f.failed {
		// 从未 open(缓冲为空, 如纯文件任务): 无流式消息存在, 直接交
		// delivery 最终结果。
		f.closed = true
		return false
	}
	if err := f.reply.Commit(ctx); err != nil {
		f.abortWith(ctx, "stream commit failed; final result delivered by existing delivery path", err)
		return false
	}
	f.closed = true
	return true
}

// Abort 失败弃流(任务失败/中断/取消): 已 open 则 abort(飞书可改"生成
// 中断"提示), 未 open 无操作。幂等。最终结果由终态 delivery 兜底。
func (f *StreamForwarder) Abort(ctx context.Context) {
	if f.closed || !f.open {
		f.closed = true
		return
	}
	f.closed = true
	if err := f.reply.Abort(ctx); err != nil {
		slog.WarnContext(ctx, "stream: abort failed",
			"task_id", f.task.ID, "error", err)
	}
}

func (f *StreamForwarder) abortWith(ctx context.Context, reason string, err error) {
	slog.WarnContext(ctx, "stream: "+reason, "task_id", f.task.ID, "error", err)
	f.failed = true
	f.Abort(ctx)
}
