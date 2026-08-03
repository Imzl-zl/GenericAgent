package application

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path/filepath"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/infrastructure/safefs"
)

// maxDeliverableFiles 与 maxTotalDeliverableBytes 是任务级交付聚合上限
// (审查 R5-I5): 除单文件 8MiB 上限外, 大量小文件不得累计数十 GB 内存/事务
// 压力。超限 fail-closed(任务失败并提示), 不做静默截断。
const (
	maxDeliverableFiles        = 32
	maxTotalDeliverableBytes   = 64 << 20 // 64 MiB
)

// captureTaskDeliverableFiles 在任务成功事务提交前捕获 [FILE:...] 标记文件
// 的内容快照(审查 R5-I3): 文件在任务完成时刻读取(Worker 仍持有串行槽, 同
// Runner 尚无下一条任务), 内容、digest、大小与成功事务原子持久化; 异步
// delivery 直接发送快照, 不再重新解析 workspace 路径——否则下一条串行任务
// 可能覆盖/删除同名输出, 交付错误内容或 dead-letter。
//
// 安全边界与 delivery 原路径一致: marker 必须解析到 workspace 沙箱内
// (resolveUnderRoot + EvalSymlinks), RecordOutbound 强制 outputs/ 前缀,
// safefs.ReadFileBeneathLimited 以单次 openat 校验普通文件并限长读取。
// SessionFiles 未接线(loopback 无共享卷)时返回 nil, 不阻断成功。
func captureTaskDeliverableFiles(ctx context.Context, sf SessionFiles, sessionKey, body string) ([]domain.DeliveryFile, error) {
	if sf == nil {
		return nil, nil
	}
	visible := userVisibleTaskResult(body)
	markers := extractFileMarkers(visible)
	if len(markers) == 0 {
		return nil, nil
	}
	root := sf.SandboxRoot(sessionKey)
	files := make([]domain.DeliveryFile, 0, len(markers))
	var totalBytes int64
	for _, marker := range markers {
		if len(files) >= maxDeliverableFiles {
			return nil, fmt.Errorf("deliverable file count exceeds limit %d", maxDeliverableFiles)
		}
		_, relPath, err := sf.ResolveMarker(sessionKey, marker)
		if err != nil {
			return nil, fmt.Errorf("resolve file marker %q: %w", marker, err)
		}
		// manifest 登记(方向 outbound)在任务完成时进行——此时文件仍是本
		// 任务生成的内容, 与快照一致; 之后文件被覆盖也不影响已登记条目。
		ref, err := sf.RecordOutbound(sessionKey, marker)
		if err != nil {
			return nil, fmt.Errorf("record outbound %q: %w", marker, err)
		}
		content, err := safefs.ReadFileBeneathLimited(root, filepath.FromSlash(relPath), defaultMaxDeliverableBytes)
		if err != nil {
			return nil, fmt.Errorf("read deliverable %q: %w", marker, err)
		}
		totalBytes += int64(len(content))
		if totalBytes > maxTotalDeliverableBytes {
			return nil, fmt.Errorf("deliverable total bytes exceed limit %d", maxTotalDeliverableBytes)
		}
		sum := sha256.Sum256(content)
		files = append(files, domain.DeliveryFile{
			Marker:    marker,
			FileName:  ref.OriginalName,
			RelPath:   relPath,
			Digest:    "sha256:" + hex.EncodeToString(sum[:]),
			SizeBytes: int64(len(content)),
			Content:   content,
		})
	}
	return files, nil
}
