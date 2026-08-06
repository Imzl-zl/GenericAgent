package domain

// DeliveryType is a platform-owned terminal delivery classification.
type DeliveryType string

const (
	DeliveryTaskStarted     DeliveryType = "task_started"     // Initial "processing..." notification
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
	Content   []byte // 内容快照(发送时写入 Platform 私有临时文件)
}

// Delivery is a durable terminal delivery outbox row.
type Delivery struct {
	DeliveryID     string
	TaskID         string
	DeliveryType   DeliveryType
	Status         DeliveryStatus
	PayloadRef     string
	PayloadDigest  string
	ErrorCode      string
	ErrorMessage   string
	AttemptCount   int
	// AttemptToken 是本次 claim 的 fencing token(审查 F2): claim 时生成,
	// Ack/Retry/DeadLetter 必须携带, 防止旧 attempt 覆盖新 attempt。
	AttemptToken string
}
