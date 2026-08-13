// Package domain holds platform task and result types owned by the control plane.
package domain

import (
	"errors"
	"time"
)

// SubmitTaskCommand is the validated carrier for enqueuing a task.
// MessageID becomes message_idempotency_key; callers cannot supply a second dedupe value.
// TaskMedia 是任务入站媒体清单的一项（2026-08-13 多模态链路定案）:
// 路由时 ImportInbound 得到的附件元数据，随任务持久化并经
// TaskEnvelope.media 契约结构化下发 Worker，使 GA 能把图片作为多模态
// content block 注入模型首轮——不再依赖 prompt 文本路径解析。
// RelativePath 相对会话沙箱根（workspace temp/）。
type TaskMedia struct {
	Alias        string `json:"alias"`
	OriginalName string `json:"original_name"`
	RelativePath string `json:"relative_path"`
	SizeBytes    int64  `json:"size_bytes"`
}

type SubmitTaskCommand struct {
	SessionKey        string
	RequesterUserID   int64
	Source            string
	SourceInstanceID  string
	MessageID         string
	Prompt            string
	PersonaSnapshot   []string
	ToolPolicyVersion string
	// Media 是本次任务入站媒体清单（结构化传递，见 TaskMedia）。
	Media []TaskMedia
	// ConversationKey 是渠道内对话单元标识(对端/群 ID); 空 = 该渠道默认桶
	// (微信个人自用单桶 / 非渠道入口)。分桶见 IM_CHANNEL_ARCHITECTURE §3。
	ConversationKey string
	// ConversationType 是对话单元类型('private'|'group'): IM 流式转发
	// 判定维度(群聊统一只发最终结果, IM_STREAMING_DELIVERY §4.4)。
	// 微信恒 private; web/非 IM 入口默认 private。
	ConversationType string
}

// TaskStatus is the durable task lifecycle state.
type TaskStatus string

const (
	TaskQueued      TaskStatus = "queued"
	TaskStarting    TaskStatus = "starting"
	TaskRunning     TaskStatus = "running"
	TaskSucceeded   TaskStatus = "succeeded"
	TaskFailed      TaskStatus = "failed"
	TaskCancelled   TaskStatus = "cancelled"
	TaskInterrupted TaskStatus = "interrupted"
)

// Source values are the protocol-level origins allowed for task submission
// (architecture §4.2: wechat|web; IM_CHANNEL_BINDING §5: feishu|dingtalk|qq|wecom).
const (
	SourceWechat   = "wechat"
	SourceWeb      = "web"
	SourceFeishu   = "feishu"
	SourceDingTalk = "dingtalk"
	SourceQQ       = "qq"
	SourceWecom    = "wecom"
)

// IsValidSource reports whether s is an allowed task source.
func IsValidSource(s string) bool {
	switch s {
	case SourceWechat, SourceWeb, SourceFeishu, SourceDingTalk, SourceQQ, SourceWecom:
		return true
	default:
		return false
	}
}

// ConversationType values identify the conversation bucket kind for IM
// streaming delivery decisions (IM_STREAMING_DELIVERY §4.4: 群聊收敛)。
const (
	ConversationTypePrivate = "private"
	ConversationTypeGroup   = "group"
)

// NormalizeConversationType 校验并归一 conversation_type: 空/非法回退
// 'private'(web 入口、存量任务、微信均无群概念)。
func NormalizeConversationType(t string) string {
	if t == ConversationTypeGroup {
		return ConversationTypeGroup
	}
	return ConversationTypePrivate
}

// IsGroupConversation reports whether the conversation is a group bucket.
func IsGroupConversation(t string) bool {
	return t == ConversationTypeGroup
}

// IMStreamingMode is the administrator-managed IM streaming switch
// (IM_STREAMING_DELIVERY §5).
//
//	off:        不转发任何流式片段(全渠道只发最终结果)
//	final_only: 同上——保留语义别名(与 off 行为一致, 供 web 下拉区分
//	            "完全关闭"与"只发最终结果"的展示; 实现上两者均为最终结果)
//	streaming:  私聊按渠道能力转发流式片段(群聊恒定只发最终结果)
//
// 注: final_only 与 off 在 v1 行为相同(钉钉/微信本就不流式), 保留两个值
// 是为了 web 设置项的用户语义清晰与后续扩展空间。
type IMStreamingMode string

const (
	IMStreamingOff        IMStreamingMode = "off"
	IMStreamingFinalOnly  IMStreamingMode = "final_only"
	IMStreamingStreaming  IMStreamingMode = "streaming"
	DefaultIMStreamingMode               = IMStreamingStreaming
)

// ValidIMStreamingMode reports whether m is an allowed mode value.
func ValidIMStreamingMode(m IMStreamingMode) bool {
	switch m {
	case IMStreamingOff, IMStreamingFinalOnly, IMStreamingStreaming:
		return true
	default:
		return false
	}
}

// NormalizeIMStreamingMode 归一非法值到默认 streaming(设计: 私聊默认开)。
func NormalizeIMStreamingMode(m IMStreamingMode) IMStreamingMode {
	if ValidIMStreamingMode(m) {
		return m
	}
	return DefaultIMStreamingMode
}

// StreamingEnabled reports whether the mode forwards streaming fragments.
func (m IMStreamingMode) StreamingEnabled() bool {
	return m == IMStreamingStreaming
}

// Task is the durable task envelope stored in PostgreSQL.
type Task struct {
	ID                    string
	SessionKey            string
	WorkspaceID           string
	RequesterID           int64
	Source                string
	SourceInstanceID      string
	MessageID             string
	MessageIdempotencyKey string
	// ConversationKey 是该任务的对话单元桶键(渠道内对端/群 ID; 空=默认桶)。
	ConversationKey string
	// ConversationType 是该任务的对话单元类型('private'|'group'; 默认
	// 'private')——IM 流式转发判定维度(群聊只发最终结果)。
	ConversationType string
	// StreamFinalAt 非空 = 该任务的流式回复已 commit 成功(最终文本已
	// 交付 IM), delivery 发送文本 part 时跳过(文件照发); 失败路径无
	// 标记 → delivery 照发兜底。
	StreamFinalAt *time.Time
	Prompt        string
	PersonaSnapshot       []string
	ToolPolicyVersion     string
	// Media 是本次任务入站媒体清单（2026-08-13 多模态链路，见 TaskMedia）。
	Media []TaskMedia
	ClaimOwner            string
	ClaimLeaseUntil       time.Time
	SessionSequence       int64
	Status                TaskStatus
	SnapshotID            string
	SnapshotChecksum      string
	ResultRef             string
	ResultDigest          string
	WorkerInstanceID      string
	// LastActivityAt is updated on every chunk event and Worker heartbeat
	// (drain_display_queue empty poll). Reaper uses it to detect "Worker alive
	// but deadlocked" (LLM HTTP call hung, GIL deadlock) — the scenario gRPC
	// stream errors + heartbeat lease loss cannot catch. Pattern: Temporal
	// HeartbeatTimeout / Kubernetes liveness probe.
	LastActivityAt time.Time
	// FreshSession is set when /new was issued since the last committed
	// snapshot. The scheduler stops the old Worker so the next task starts
	// with cleared history and working state (spec §7 /new).
	FreshSession bool
	// WorkerDispatchStartedAt is set when the scheduler records dispatch intent.
	WorkerDispatchStartedAt *time.Time
	CancelRequestedAt       *time.Time
	TerminalErrorCode       string
	TerminalErrorMessage    string
	TerminalErrorTraceID    string
	CreatedAt               time.Time
	UpdatedAt               time.Time
	StartedAt               *time.Time
	SucceededAt             *time.Time
	TerminalAt              *time.Time
	PromptBytes             int
	PersonaBytes            int
}

// ResultPayload is the bounded committed result body with its opaque ref and digest.
type ResultPayload struct {
	Ref    string
	Digest string
	Body   []byte
}

// Terminal states never re-enter claim.
func (s TaskStatus) IsTerminal() bool {
	switch s {
	case TaskSucceeded, TaskFailed, TaskCancelled, TaskInterrupted:
		return true
	default:
		return false
	}
}

// ErrPerUserQueueFull signals that a requester has reached the per-user
// queued-task cap. Defined in domain so both application and postgres layers
// can return/test against the same sentinel without import cycles.
var ErrPerUserQueueFull = errors.New("per-user queue limit reached")

// ErrSessionAccessDenied signals that a requester may not submit to the
// session (personal mismatch / not an approved team member / unapproved
// user / malformed session key). Defined in domain so the application layer
// can wrap it and the API layer can map it to 403 without import cycles.
var ErrSessionAccessDenied = errors.New("session access denied")

// ErrWorkspaceNotFound signals that no workspace row exists for the session
// key. Defined in domain so the postgres store can wrap it and the API layer
// can map it to 404 without import cycles.
var ErrWorkspaceNotFound = errors.New("workspace not found")

// ErrLeaseExpired signals that a claim heartbeat updated 0 rows because the
// caller no longer owns the task (lease expired or was stolen by recovery).
// Defined in domain so both the application (scheduler tick) and the postgres
// store can reference it without an import cycle.
var ErrLeaseExpired = errors.New("claim lease expired or lost")
