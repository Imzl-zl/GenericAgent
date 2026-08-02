//go:build windows

package application

import (
	"fmt"
	"io"
	"os"
)

// snapshotDeliverable 是 Windows 开发环境等价实现: 用 Lstat 拒绝符号链接,
// fstat 校验普通文件与大小上限, 复制到 Platform 私有快照目录。
// 生产部署在 Linux(见 deliverable_snapshot_unix.go 的 O_NOFOLLOW 实现)。
func snapshotDeliverable(absPath, snapshotDir string, maxBytes int64) (string, error) {
	if absPath == "" {
		return "", fmt.Errorf("deliverable path is empty")
	}
	info, err := os.Lstat(absPath)
	if err != nil {
		return "", fmt.Errorf("stat deliverable %q: %w", absPath, err)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("deliverable %q is not a regular file (mode %s)", absPath, info.Mode())
	}
	if maxBytes > 0 && info.Size() > maxBytes {
		return "", fmt.Errorf("deliverable %q exceeds size limit %d (got %d)", absPath, maxBytes, info.Size())
	}
	src, err := os.Open(absPath)
	if err != nil {
		return "", fmt.Errorf("open deliverable %q: %w", absPath, err)
	}
	defer src.Close()
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
