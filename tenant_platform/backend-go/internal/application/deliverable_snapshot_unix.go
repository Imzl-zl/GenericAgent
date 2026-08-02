//go:build !windows

package application

import (
	"fmt"
	"io"
	"os"

	"golang.org/x/sys/unix"
)

// snapshotDeliverable 打开已解析的输出文件(O_NOFOLLOW + fstat 校验普通文件
// 与大小上限), 复制到 Platform 私有不可变快照目录后返回快照路径。
// transport 发送的是 Platform 独占的快照, Runner 无法在校验后替换原文件,
// 消除"校验路径 → 按路径重开"之间的 TOCTOU(方案 §4/§6)。
func snapshotDeliverable(absPath, snapshotDir string, maxBytes int64) (string, error) {
	if absPath == "" {
		return "", fmt.Errorf("deliverable path is empty")
	}
	fd, err := unix.Open(absPath, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return "", fmt.Errorf("open deliverable %q: %w", absPath, err)
	}
	defer unix.Close(fd)
	info, err := unix.Fstat(fd)
	if err != nil {
		return "", fmt.Errorf("fstat deliverable %q: %w", absPath, err)
	}
	if info.Mode&unix.S_IFMT != unix.S_IFREG {
		return "", fmt.Errorf("deliverable %q is not a regular file (mode %o)", absPath, info.Mode)
	}
	if maxBytes > 0 && info.Size > maxBytes {
		return "", fmt.Errorf("deliverable %q exceeds size limit %d (got %d)", absPath, maxBytes, info.Size)
	}
	if err := os.MkdirAll(snapshotDir, 0o700); err != nil {
		return "", fmt.Errorf("create deliverable snapshot dir: %w", err)
	}
	dst, err := os.CreateTemp(snapshotDir, "deliverable-*.bin")
	if err != nil {
		return "", fmt.Errorf("create deliverable snapshot: %w", err)
	}
	dstName := dst.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(dstName)
		}
	}()
	src := os.NewFile(uintptr(fd), absPath)
	if _, err := io.Copy(dst, src); err != nil {
		_ = dst.Close()
		return "", fmt.Errorf("copy deliverable snapshot: %w", err)
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
