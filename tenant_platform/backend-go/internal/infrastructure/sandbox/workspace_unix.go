//go:build !windows

package sandbox

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

// ensureWorkspaceDirsBeneath 幂等创建 dirs(含祖先)并设置属主/属组/模式。
// 全部操作基于 openat(O_NOFOLLOW)/mkdirat/unlinkat/fchown/fchmod:
//   - 任何组件是 symlink 时删除链接本身(不触碰目标)后重建为目录;
//   - fchown/fchmod 作用于已打开的目标目录 fd, 不存在“检查→按路径操作”
//     窗口, 符号链接替换无法把 root 的 chown/chmod 引向工作区外路径。
//
// 非 root 环境(单元测试)跳过属主修复, 与旧行为一致。
func ensureWorkspaceDirsBeneath(root string, dirs []string, uid, shareGID int) error {
	rootFD, err := unix.Open(root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("open workspace root %s: %w", root, err)
	}
	defer unix.Close(rootFD)

	for _, d := range dirs {
		rel, err := filepath.Rel(root, d)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
			return fmt.Errorf("workspace dir %s escapes root %s", d, root)
		}
		parts := strings.Split(filepath.Clean(rel), string(filepath.Separator))
		fd := rootFD
		for i, part := range parts {
			if part == "" || part == "." {
				continue
			}
			final := i == len(parts)-1
			if err := unix.Mkdirat(fd, part, workspaceDirsMode); err != nil && !errors.Is(err, unix.EEXIST) {
				return fmt.Errorf("mkdirat %s: %w", part, err)
			}
			child, openErr := unix.Openat(fd, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
			if openErr != nil {
				if !errors.Is(openErr, unix.ELOOP) {
					return fmt.Errorf("openat %s: %w", part, openErr)
				}
				// symlink 组件: 只 unlink 链接本身, 重建为目录。
				if err := unix.Unlinkat(fd, part, 0); err != nil {
					return fmt.Errorf("remove symlink %s: %w", part, err)
				}
				if err := unix.Mkdirat(fd, part, workspaceDirsMode); err != nil {
					return fmt.Errorf("mkdirat %s: %w", part, err)
				}
				child, openErr = unix.Openat(fd, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
				if openErr != nil {
					return fmt.Errorf("openat %s: %w", part, openErr)
				}
			}
			if !final {
				if fd != rootFD {
					unix.Close(fd)
				}
				fd = child
				continue
			}
			if os.Geteuid() == 0 {
				if err := unix.Fchown(child, uid, shareGID); err != nil {
					unix.Close(child)
					return fmt.Errorf("fchown %s: %w", d, err)
				}
				// fchown 会清除 setgid, 需在之后重新设置。
				if err := unix.Fchmod(child, uint32(workspaceDirsMode)); err != nil {
					unix.Close(child)
					return fmt.Errorf("fchmod %s: %w", d, err)
				}
			}
			unix.Close(child)
		}
	}
	return nil
}
