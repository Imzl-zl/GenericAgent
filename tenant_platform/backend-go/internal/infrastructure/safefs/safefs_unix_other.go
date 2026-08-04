//go:build unix && !linux

package safefs

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

// openSrcBeneath 打开 root 下 rel 作为复制源。非 Linux unix(darwin 等)
// 没有 openat2: 回退为逐级 openat + O_NOFOLLOW(与 OpenBeneath 相同语义,
// 中间组件与最终组件都拒绝符号链接)。生产部署是 Linux(方案 §13), 该回退
// 只保证跨平台构建一致。
func openSrcBeneath(root, rel string, flags int) (int, error) {
	rel, err := CleanRel(root, rel)
	if err != nil {
		return -1, err
	}
	rootFD, err := unix.Open(root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, fmt.Errorf("open source root %s: %w", root, err)
	}
	defer unix.Close(rootFD)
	dirFD := rootFD
	parts := strings.Split(rel, string(filepath.Separator))
	for i, part := range parts {
		if part == "" || part == "." {
			continue
		}
		last := i == len(parts)-1
		if last {
			fd, openErr := unix.Openat(dirFD, part, flags|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
			if dirFD != rootFD {
				unix.Close(dirFD)
			}
			if openErr != nil {
				return -1, &os.PathError{Op: "open", Path: filepath.Join(root, filepath.FromSlash(rel)), Err: openErr}
			}
			return fd, nil
		}
		fd, openErr := unix.Openat(dirFD, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if openErr != nil {
			if dirFD != rootFD {
				unix.Close(dirFD)
			}
			return -1, fmt.Errorf("open dir %s under %s: %w", part, root, openErr)
		}
		if dirFD != rootFD {
			unix.Close(dirFD)
		}
		dirFD = fd
	}
	if dirFD != rootFD {
		unix.Close(dirFD)
	}
	return -1, fmt.Errorf("invalid relative path %s", rel)
}
