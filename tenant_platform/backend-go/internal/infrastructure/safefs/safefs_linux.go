//go:build linux

package safefs

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

// openSrcBeneath 打开 root 下 rel 作为复制源(round9 审查: 源侧 TOCTOU
// 加固)。使用 openat2 + RESOLVE_BENEATH|RESOLVE_NO_SYMLINKS: 内核在单次
// 不可分割的路径解析中同时保证"不逃逸 root"与"任何组件都不是符号链接"。
// 与"先 EvalSymlinks 校验、后用原路径重新打开"的两步式检查不同, openat2
// 不存在检查-使用窗口——Poller 在检查后把父目录替换为指向 /proc 或其他
// 卷的 symlink 时, 这里要么解析失败, 要么按旧 inode 继续(不跟随链接)。
func openSrcBeneath(root, rel string, flags int) (int, error) {
	rel, err := CleanRel(root, rel)
	if err != nil {
		return -1, err
	}
	rootFD, err := unix.Open(root, unix.O_PATH|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, fmt.Errorf("open source root %s: %w", root, err)
	}
	defer unix.Close(rootFD)
	how := &unix.OpenHow{
		Flags:   uint64(flags | unix.O_CLOEXEC | unix.O_NOFOLLOW),
		Resolve: unix.RESOLVE_BENEATH | unix.RESOLVE_NO_SYMLINKS,
	}
	fd, err := unix.Openat2(rootFD, strings.TrimPrefix(filepath.ToSlash(rel), "./"), how)
	if err != nil {
		return -1, &os.PathError{Op: "openat2", Path: filepath.Join(root, filepath.FromSlash(rel)), Err: err}
	}
	return fd, nil
}
