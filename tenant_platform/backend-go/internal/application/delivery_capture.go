package application

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/infrastructure/safefs"
)

// maxDeliverableFiles 与 maxTotalDeliverableBytes 是任务级交付聚合上限
// (审查 R5-I5): 除单文件上限外, 大量小文件不得累计无界磁盘/事务压力。
// 超限 fail-closed(任务失败并提示), 不做静默截断。
const (
	maxDeliverableFiles      = 32
	maxTotalDeliverableBytes = 256 << 20 // 256 MiB(spool 引用化后无内存压力, 审查 B4/T5)
)

// 单文件交付上限按媒体类型分化(2026-08-13 审查 B4/T5): spool 引用化后
// 不再受 DB bytea 约束——image ≤20MiB、video ≤100MiB(对齐入站
// maxInboundMediaBytes, Phase C 视频)、其余 ≤8MiB(原 defaultMaxDeliverableBytes)。
var maxDeliverableBytesByType = map[string]int64{
	"image": 20 << 20,
	"video": 100 << 20,
	"file":  8 << 20,
}

func deliverableMaxBytes(relPath string) int64 {
	if v, ok := maxDeliverableBytesByType[mediaTypeForPath(relPath)]; ok {
		return v
	}
	return defaultMaxDeliverableBytes
}

// sha256FileStream 流式计算文件 sha256(避免整读进内存, spool 文件可达
// 100MiB; spool 是 Platform 刚复制的普通文件, 无符号链接风险)。
func sha256FileStream(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// captureTaskDeliverableFiles 在任务成功事务提交前捕获 [FILE:...] 标记文件
// (审查 R5-I3 + 2026-08-13 审查 B4/T5 spool 引用化): 文件在任务完成时刻
// 流式复制到 delivery spool 卷(Worker 仍持有串行槽, 同 Runner 尚无下一条
// 任务), spool 相对路径、digest、大小与成功事务原子持久化; 异步 delivery
// 直接发送 spool 文件, 不再重新解析 workspace 路径——否则下一条串行任务
// 可能覆盖/删除同名输出, 交付错误内容或 dead-letter。
//
// 安全边界与 delivery 原路径一致: marker 必须解析到 workspace 沙箱内
// (resolveUnderRoot + EvalSymlinks), RecordOutbound 强制 outputs/ 前缀,
// safefs.CopyFileFromBeneath 以 openat2 单次解析完成"不逃逸根 + 无符号
// 链接"校验并限长流式复制(内存峰值 = 复制缓冲, 非文件大小)。
// SessionFiles 未接线(loopback 无共享卷)时返回 nil, 不阻断成功。
// spoolDir 为空(测试/loopback)时回退内存快照(旧语义)。
func captureTaskDeliverableFiles(ctx context.Context, sf SessionFiles, sessionKey, spoolDir, body string) ([]domain.DeliveryFile, error) {
	if sf == nil {
		return nil, nil
	}
	visible := userVisibleTaskResult(body)
	markers := extractFileMarkers(visible)
	if len(markers) == 0 {
		return nil, nil
	}
	root, err := sf.SandboxRoot(sessionKey)
	if err != nil {
		return nil, err
	}
	files := make([]domain.DeliveryFile, 0, len(markers))
	var totalBytes int64
	// spool 引用: 预先创建 <spool>/capture/<dirKey> 目标目录(createBeneath
	// 要求父目录存在, openat O_DIRECTORY)。dirKey 由 sessionKey 哈希派生。
	var spoolTaskDir string
	if spoolDir != "" {
		spoolTaskDir = filepath.Join("capture", deliveryFileKey(sessionKey))
		if err := safefs.MkdirAllBeneath(spoolDir, filepath.FromSlash(spoolTaskDir), 0o750); err != nil {
			return nil, fmt.Errorf("create spool capture dir: %w", err)
		}
	}
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
		maxBytes := deliverableMaxBytes(relPath)
		if spoolDir != "" {
			// spool 引用: 流式复制到 <spool>/capture/<taskKey>/<marker-hash>_<name>。
			// 目录 key 用 sessionKey hash + marker hash 派生(防同名覆盖, 与
			// delivery 快照文件名同款), 避免把不可信 relPath 直接进路径。
			spoolRel := filepath.Join(spoolTaskDir,
				fmt.Sprintf("%s_%s", deliveryFileMarkerKey(marker), deliverableSnapshotBase(relPath)))
			if err := safefs.CopyFileFromBeneath(root, filepath.FromSlash(relPath), spoolDir, filepath.FromSlash(spoolRel), 0o640, maxBytes); err != nil {
				return nil, fmt.Errorf("copy deliverable %q to spool: %w", marker, err)
			}
			spoolAbs := filepath.Join(spoolDir, filepath.FromSlash(spoolRel))
			info, err := os.Lstat(spoolAbs)
			if err != nil {
				return nil, fmt.Errorf("stat spool deliverable %q: %w", marker, err)
			}
			if !info.Mode().IsRegular() {
				return nil, fmt.Errorf("spool deliverable %q is not a regular file", marker)
			}
			if info.Size() > maxBytes {
				return nil, fmt.Errorf("deliverable %q exceeds size limit %d", marker, maxBytes)
			}
			if totalBytes+info.Size() > maxTotalDeliverableBytes {
				return nil, fmt.Errorf("deliverable total bytes exceed limit %d", maxTotalDeliverableBytes)
			}
			totalBytes += info.Size()
			sum, err := sha256FileStream(spoolAbs)
			if err != nil {
				return nil, fmt.Errorf("digest spool deliverable %q: %w", marker, err)
			}
			files = append(files, domain.DeliveryFile{
				Marker:    marker,
				FileName:  sanitizeDeliverableDisplayName(ref.OriginalName, relPath),
				RelPath:   relPath,
				Digest:    "sha256:" + sum,
				SizeBytes: info.Size(),
				SpoolPath: filepath.ToSlash(spoolRel),
			})
			continue
		}
		// 无 spool 目录(测试/loopback): 回退内存快照(旧语义)。
		content, err := safefs.ReadFileBeneathLimited(root, filepath.FromSlash(relPath), maxBytes)
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
			FileName:  sanitizeDeliverableDisplayName(ref.OriginalName, relPath),
			RelPath:   relPath,
			Digest:    "sha256:" + hex.EncodeToString(sum[:]),
			SizeBytes: int64(len(content)),
			Content:   content,
		})
	}
	return files, nil
}
