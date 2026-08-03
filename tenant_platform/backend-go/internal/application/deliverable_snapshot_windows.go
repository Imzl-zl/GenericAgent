//go:build windows

package application

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/infrastructure/safefs"
)

// snapshotDeliverable 是 Windows 开发环境等价实现: 用 safefs(逐组件拒绝
// 符号链接)打开, fstat 校验普通文件与大小上限, 复制到 Platform 私有快照目录。
// 生产部署在 Linux(见 deliverable_snapshot_unix.go)。
func snapshotDeliverable(absPath, root, rel, snapshotDir string, maxBytes int64) (string, error) {
	if absPath == "" {
		return "", fmt.Errorf("deliverable path is empty")
	}
	f, err := safefs.OpenBeneath(root, rel, os.O_RDONLY, 0)
	if err != nil {
		return "", fmt.Errorf("open deliverable %q: %w", absPath, err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return "", fmt.Errorf("stat deliverable %q: %w", absPath, err)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("deliverable %q is not a regular file (mode %s)", absPath, info.Mode())
	}
	if maxBytes > 0 && info.Size() > maxBytes {
		return "", fmt.Errorf("deliverable %q exceeds size limit %d (got %d)", absPath, maxBytes, info.Size())
	}
	if err := os.MkdirAll(snapshotDir, 0o700); err != nil {
		return "", fmt.Errorf("create deliverable snapshot dir: %w", err)
	}
	dstName := filepath.Join(snapshotDir, snapshotFileName(absPath))
	dst, err := os.OpenFile(dstName, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", fmt.Errorf("create deliverable snapshot: %w", err)
	}
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(dstName)
		}
	}()
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
