package domain

// DeliveryType is a platform-owned terminal delivery classification.
type DeliveryType string

const (
	DeliveryTaskComplete    DeliveryType = "task_complete"
	DeliveryTaskFailed      DeliveryType = "task_failed"
	DeliveryTaskCancelled   DeliveryType = "task_cancelled"
	DeliveryTaskInterrupted DeliveryType = "task_interrupted"
)

// DeliveryStatus tracks outbox lifecycle.
type DeliveryStatus string

const (
	DeliveryPending    DeliveryStatus = "pending"
	DeliverySending    DeliveryStatus = "sending"
	DeliveryAcked      DeliveryStatus = "acked"
	DeliveryDeadLetter DeliveryStatus = "dead_letter"
)

// StableDeliveryID is platform-owned: task_id:delivery_type.
// The Worker never receives or creates this ID.
func StableDeliveryID(taskID string, deliveryType DeliveryType) string {
	return taskID + ":" + string(deliveryType)
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
}
