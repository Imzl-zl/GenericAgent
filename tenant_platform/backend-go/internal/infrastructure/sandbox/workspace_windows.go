//go:build windows

package sandbox

import (
	"fmt"
	"os"
)

// ensureWorkspaceDirsBeneath 是 unix dirfd 实现的非 unix 降级版。
// Windows 上无 openat/fchown 语义且 Geteuid() != 0(属主修复跳过), 这里
// 保留路径式实现: 逐组件 Lstat 拒绝/移除 symlink 后再 MkdirAll, chown 前
// 二次 Lstat 复检(尽力而为; 生产部署目标是 Linux, 此路径仅用于开发编译)。
func ensureWorkspaceDirsBeneath(root string, dirs []string, uid, shareGID int) error {
	for _, d := range dirs {
		if err := ensureDirPathNoSymlink(root, d); err != nil {
			return err
		}
		if err := os.MkdirAll(d, workspaceDirsMode); err != nil {
			return fmt.Errorf("create workspace dir %s: %w", d, err)
		}
		if os.Geteuid() == 0 {
			info, err := os.Lstat(d)
			if err != nil {
				return fmt.Errorf("lstat workspace dir %s: %w", d, err)
			}
			if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
				return fmt.Errorf("workspace dir %s replaced by non-directory", d)
			}
			if err := os.Chown(d, uid, shareGID); err != nil {
				return fmt.Errorf("chown workspace dir %s: %w", d, err)
			}
			if err := os.Chmod(d, workspaceDirsMode); err != nil {
				return fmt.Errorf("chmod workspace dir %s: %w", d, err)
			}
		}
	}
	return nil
}
