package domain

import (
	"errors"
	"time"
)

// ErrDuplicateMediaAsset indicates the (message_id, storage_path) pair already
// exists. Returned by MessageStore.InsertMediaAsset when the partial UNIQUE
// index rejects a duplicate INSERT. Callers treat this as idempotent success.
var ErrDuplicateMediaAsset = errors.New("duplicate media asset")

// MediaAsset is the metadata row for one inbound or outbound media file.
// Binary content lives on the file system; this row is the control-plane
// index. storage_path is relative to media_dir so the same row works when
// the media_dir mount point changes (local -> NFS -> S3 mount).
type MediaAsset struct {
	ID           int64
	UserID       int64
	BotID        int64
	MessageID   int64 // 0 if not linked to a message row
	FileName     string
	StoragePath  string // relative path under media_dir
	ContentType  string // MIME: image/jpeg, video/mp4, application/pdf, ...
	SizeBytes    int64
	SHA256       string // optional, empty in P0
	Direction    MessageDirection
	CreatedAt    time.Time
}
