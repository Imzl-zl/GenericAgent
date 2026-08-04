//go:build !windows

package application

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/infrastructure/safefs"
)

// snapshotDeliverable 打开已解析的输出文件(root 受限 + 逐级 O_NOFOLLOW +
// fstat 校验普通文件与大小上限), 复制到 Platform 私有不可变快照目录后返回
// 快照路径。快照文件名保留原文件名(transport 以路径 basename 作为用户可见
// 文件名, 方案 §6)。transport 发送的是 Platform 独占的快照, Runner 无法在
// 校验后替换原文件, 消除"校验路径 → 按路径重开"之间的 TOCTOU。
func snapshotDeliverable(absPath, root, rel, snapshotDir string, maxBytes int64) (string, error) {
	if absPath == "" {
		return "", fmt.Errorf("deliverable path is empty")
	}
	// O_NONBLOCK: Runner 可在 Lstat 之后把交付文件替换成 FIFO/设备文件,
	// 阻塞式打开会让本 delivery worker 永久卡在 openat(发送 context 无法
	// 中断 syscall), 四个并发 delivery worker 可被全部占满。非阻塞打开后
	// 由下方 fstat 的 IsRegular 校验拒绝非普通文件, 对普通文件无副作用。
	f, err := safefs.OpenBeneath(root, rel, syscall.O_NONBLOCK, 0)
	if err != nil {
		return "", fmt.Errorf("open deliverable %q: %w", absPath, err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return "", fmt.Errorf("fstat deliverable %q: %w", absPath, err)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("deliverable %q is not a regular file (mode %o)", absPath, info.Mode())
	}
	if maxBytes > 0 && info.Size() > maxBytes {
		return "", fmt.Errorf("deliverable %q exceeds size limit %d (got %d)", absPath, maxBytes, info.Size())
	}
	// 快照目录/文件必须允许部署共享组读取(round10 审查 B6): Compose 中
	// Platform(10001)写 delivery_spool 共享卷, Bot Poller(10002, 组 10003)
	// 只读挂载并直接读取——0700/0600 会让 Poller 必然 EACCES。卷根由
	// platform.Dockerfile 预置为 10001:10003 2770(setgid 继承组), 此处用
	// 0770/0640 即可让共享组成员可读。
	if err := os.MkdirAll(snapshotDir, 0o2770); err != nil {
		return "", fmt.Errorf("create deliverable snapshot dir: %w", err)
	}
	dstName := filepath.Join(snapshotDir, snapshotFileName(absPath))
	dst, err := os.OpenFile(dstName, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o640)
	if err != nil {
		return "", fmt.Errorf("create deliverable snapshot: %w", err)
	}
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(dstName)
		}
	}()
	// 复制阶段再次限制大小: fstat 之后文件可能继续增长(写穿文件)。
	reader := io.Reader(f)
	var limited *io.LimitedReader
	if maxBytes > 0 {
		limited = &io.LimitedReader{R: f, N: maxBytes + 1}
		reader = limited
	}
	written, err := io.Copy(dst, reader)
	if err != nil {
		_ = dst.Close()
		return "", fmt.Errorf("copy deliverable snapshot: %w", err)
	}
	if limited != nil && written > maxBytes {
		_ = dst.Close()
		return "", fmt.Errorf("deliverable %q grew past size limit %d while copying", absPath, maxBytes)
	}
	if err := dst.Sync(); err != nil {
		_ = dst.Close()
		return "", fmt.Errorf("sync deliverable snapshot: %w", err)
	}
	if err := dst.Close(); err != nil {
		return "", fmt.Errorf("close deliverable snapshot: %w", err)
	}
	cleanup = false
	return dstName, nil
}
