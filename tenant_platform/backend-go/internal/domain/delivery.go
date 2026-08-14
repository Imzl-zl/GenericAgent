package domain

import "time"

// DeliveryType is a platform-owned terminal delivery classification.
type DeliveryType string

const (
	DeliveryTaskStarted     DeliveryType = "task_started" // Initial "processing..." notification
	DeliveryTaskComplete    DeliveryType = "task_complete"
	DeliveryTaskFailed      DeliveryType = "task_failed"
	DeliveryTaskCancelled   DeliveryType = "task_cancelled"
	DeliveryTaskInterrupted DeliveryType = "task_interrupted"
)

// DeliveryStatus tracks outbox lifecycle.
type DeliveryStatus string

const (
	DeliverySending DeliveryStatus = "sending"
	DeliveryAcked   DeliveryStatus = "acked"
)

// StableDeliveryID is platform-owned: task_id:delivery_type.
// The Worker never receives or creates this ID.
func StableDeliveryID(taskID string, deliveryType DeliveryType) string {
	return taskID + ":" + string(deliveryType)
}

// DeliveryFile 是任务完成时绑定到 task_complete outbox 的文件快照
// (审查 R5-I3): 内容在成功事务内捕获(串行槽释放前), 异步 delivery 直接
// 发送快照, 不再重新解析 workspace 路径——下一条串行任务可能覆盖/删除
// 同名输出。
type DeliveryFile struct {
	Marker    string // [FILE:...] 标记原文(去括号)
	FileName  string // 用户可见文件名
	RelPath   string // workspace 内相对路径(消息媒体审计用)
	Digest    string // sha256:hex
	SizeBytes int64
	// SpoolPath 是 delivery spool 卷内相对路径(2026-08-13 审查 B4/T5):
	// 成功事务时文件流式复制到 GA_DELIVERY_SPOOL_DIR(Platform rw / Poller
	// ro 同卷), DB 不再存字节——发送时直接读 spool。空 = 存量行(content
	// 快照兼容, 30d 保留期内自然过期)。
	SpoolPath string
	// Content 是内容快照(发送时写入 Platform 私有临时文件)。spool 引用化
	// 后新写入行 Content 为空, 仅存量行/测试使用。
	Content []byte
}

// Delivery is a durable terminal delivery outbox row.
type Delivery struct {
	DeliveryID    string
	TaskID        string
	DeliveryType  DeliveryType
	Status        DeliveryStatus
	PayloadRef    string
	PayloadDigest string
	ErrorCode     string
	ErrorMessage  string
	AttemptCount  int
	// AttemptToken 是本次 claim 的 fencing token(审查 F2): claim 时生成,
	// Ack/Retry/DeadLetter 必须携带, 防止旧 attempt 覆盖新 attempt。
	AttemptToken string
	// RequeuedAt 是 admin 死信重投时间(2026-08-14 复审 P1): 非空时重试
	// 窗口锚点取 GREATEST(tasks.terminal_at, requeued_at)——管理重投开启
	// 新窗口, 否则事故后数小时重投的行会在下一个 tick 被原样打回死信。
	RequeuedAt *time.Time
}

// DeliveryAdminRow 是 admin 死信管理视图行(2026-08-14 审查 E2):
// 08-14 事故恢复曾靠手动 SQL, 管理员需要可审计的查询视图。
type DeliveryAdminRow struct {
	DeliveryID   string
	TaskID       string
	DeliveryType DeliveryType
	Status       DeliveryStatus
	ErrorCode    string
	ErrorMessage string
	AttemptCount int
	CreatedAt    time.Time
	TerminalAt   *time.Time
	RequeuedAt   *time.Time
}
