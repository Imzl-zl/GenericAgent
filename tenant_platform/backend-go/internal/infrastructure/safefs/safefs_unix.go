//go:build unix

package safefs

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

// OpenBeneath 打开 root 下 rel 路径, 逐级 openat + O_NOFOLLOW: 任何组件
// (含最终组件)是符号链接都返回错误, 读写无法逃逸 root。
// flags 语义同 os.OpenFile; perm 仅在创建时生效。
func OpenBeneath(root, rel string, flags int, perm os.FileMode) (*os.File, error) {
	rel, err := CleanRel(root, rel)
	if err != nil {
		return nil, err
	}
	rootFD, err := unix.Open(root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("open root %s: %w", root, err)
	}
	defer unix.Close(rootFD)

	dirFD := rootFD
	closeDir := func() {
		if dirFD != rootFD {
			unix.Close(dirFD)
		}
	}
	parts := strings.Split(rel, string(filepath.Separator))
	for i, part := range parts {
		if part == "" || part == "." {
			continue
		}
		last := i == len(parts)-1
		if last {
			fd, openErr := unix.Openat(dirFD, part, flags|unix.O_NOFOLLOW|unix.O_CLOEXEC, uint32(perm))
			closeDir()
			if openErr != nil {
				return nil, fmt.Errorf("open %s under %s: %w", rel, root, openErr)
			}
			return os.NewFile(uintptr(fd), filepath.Join(root, filepath.FromSlash(rel))), nil
		}
		fd, openErr := unix.Openat(dirFD, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if openErr != nil {
			closeDir()
			return nil, fmt.Errorf("open dir %s under %s: %w", part, root, openErr)
		}
		closeDir()
		dirFD = fd
	}
	closeDir()
	return nil, fmt.Errorf("invalid relative path %s", rel)
}

// ReadFileBeneath 读取 root 下 rel 的普通文件(不跟随符号链接)。
func ReadFileBeneath(root, rel string) ([]byte, error) {
	return ReadFileBeneathLimited(root, rel, 0)
}

// ErrFileTooLarge 表示读取期间文件超过 maxBytes(审查 R5-I5): fstat 后文件
// 被继续增长时, 旧实现 LimitReader(maxBytes) 会静默截断——现在读取上限为
// maxBytes+1, 读到超限字节即拒绝, 不返回不完整内容。
var ErrFileTooLarge = errors.New("file exceeded read limit")

// ReadFileBeneathLimited 读取 root 下 rel 的普通文件, 但先按 size 校验上限
// (maxBytes > 0 时), 拒绝超限文件而不读入内存(审查 I8: 不可信 staging 文件
// 不得先 ReadAll 后查限, 防止恶意超大文件耗尽 Platform 内存)。
// 读取上限为 maxBytes+1: fstat 与读取之间文件被增长时, 读到 maxBytes+1
// 字节即报 ErrFileTooLarge, 不做静默截断(审查 R5-I5)。
func ReadFileBeneathLimited(root, rel string, maxBytes int64) ([]byte, error) {
	f, err := OpenBeneath(root, rel, unix.O_RDONLY, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", rel)
	}
	if maxBytes > 0 && info.Size() > maxBytes {
		return nil, fmt.Errorf("%s exceeds size limit %d (got %d)", rel, maxBytes, info.Size())
	}
	if maxBytes > 0 {
		buf, err := io.ReadAll(io.LimitReader(f, maxBytes+1))
		if err != nil {
			return nil, err
		}
		if int64(len(buf)) > maxBytes {
			return nil, fmt.Errorf("%w: %s grew beyond %d bytes during read", ErrFileTooLarge, rel, maxBytes)
		}
		return buf, nil
	}
	return io.ReadAll(f)
}

// MkdirAllBeneath 在 root 下逐级创建目录(每级都拒绝符号链接), 返回是否新建。
func MkdirAllBeneath(root, rel string, perm os.FileMode) error {
	rel, err := CleanRel(root, rel)
	if err != nil {
		return err
	}
	rootFD, err := unix.Open(root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("open root %s: %w", root, err)
	}
	defer unix.Close(rootFD)

	dirFD := rootFD
	closeDir := func() {
		if dirFD != rootFD {
			unix.Close(dirFD)
		}
	}
	parts := strings.Split(rel, string(filepath.Separator))
	for _, part := range parts {
		if part == "" || part == "." {
			continue
		}
		fd, openErr := unix.Openat(dirFD, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if openErr == nil {
			closeDir()
			dirFD = fd
			continue
		}
		if !isNotExist(openErr) {
			closeDir()
			return fmt.Errorf("open dir %s under %s: %w", part, root, openErr)
		}
		// 不存在: 用 O_EXCL 创建(不跟随链接), 再打开。
		if err := unix.Mkdirat(dirFD, part, uint32(perm)); err != nil {
			closeDir()
			return fmt.Errorf("mkdir %s under %s: %w", part, root, err)
		}
		fd, openErr = unix.Openat(dirFD, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if openErr != nil {
			closeDir()
			return fmt.Errorf("reopen dir %s under %s: %w", part, root, openErr)
		}
		closeDir()
		dirFD = fd
	}
	closeDir()
	return nil
}

// RemoveBeneath 删除 root 下 rel(中间组件不跟随符号链接)。
func RemoveBeneath(root, rel string) error {
	rel, err := CleanRel(root, rel)
	if err != nil {
		return err
	}
	dirRel, base := filepath.Split(rel)
	dirRel = strings.TrimSuffix(dirRel, string(filepath.Separator))
	if dirRel == "" {
		dirRel = "."
	}
	dirFD, err := openDirBeneath(root, dirRel)
	if err != nil {
		return err
	}
	defer unix.Close(dirFD)
	if err := unix.Unlinkat(dirFD, base, 0); err != nil {
		return fmt.Errorf("remove %s under %s: %w", rel, root, err)
	}
	return nil
}

// AtomicWriteBeneath 在 root 下 rel 的父目录内创建临时文件(不跟随链接),
// 写入 + fsync 后原子重命名到目标; 任何组件是符号链接都会失败。
func AtomicWriteBeneath(root, rel string, data []byte, perm os.FileMode) error {
	rel, err := CleanRel(root, rel)
	if err != nil {
		return err
	}
	dirRel, base := filepath.Split(rel)
	dirRel = strings.TrimSuffix(dirRel, string(filepath.Separator))
	if dirRel == "" {
		dirRel = "."
	}
	dirFD, err := openDirBeneath(root, dirRel)
	if err != nil {
		return err
	}
	defer unix.Close(dirFD)

	tmpName := fmt.Sprintf(".%s.tmp%d", base, os.Getpid())
	fd, err := unix.Openat(dirFD, tmpName, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, uint32(perm))
	if err != nil {
		return fmt.Errorf("create temp for %s: %w", rel, err)
	}
	cleanup := func() { _ = unix.Unlinkat(dirFD, tmpName, 0) }
	if _, err := writeAll(fd, data); err != nil {
		unix.Close(fd)
		cleanup()
		return fmt.Errorf("write %s: %w", rel, err)
	}
	if err := unix.Fsync(fd); err != nil {
		unix.Close(fd)
		cleanup()
		return fmt.Errorf("sync %s: %w", rel, err)
	}
	if err := unix.Close(fd); err != nil {
		cleanup()
		return fmt.Errorf("close %s: %w", rel, err)
	}
	if err := unix.Renameat(dirFD, tmpName, dirFD, base); err != nil {
		cleanup()
		return fmt.Errorf("rename %s: %w", rel, err)
	}
	// rename 后 fsync 父目录, 保证崩溃后目录项持久(审查 I8)。
	if err := unix.Fsync(dirFD); err != nil {
		return fmt.Errorf("fsync parent dir for %s: %w", rel, err)
	}
	return nil
}

// CopyFileBeneath 把 src 复制为 root 下 rel(目标侧不跟随符号链接)。
func CopyFileBeneath(root, rel, src string, perm os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("open source %s: %w", src, err)
	}
	defer in.Close()
	out, err := OpenBeneath(root, rel, unix.O_WRONLY|unix.O_CREAT|unix.O_TRUNC, perm)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return fmt.Errorf("copy to %s: %w", rel, err)
	}
	if err := out.Sync(); err != nil {
		out.Close()
		return fmt.Errorf("sync %s: %w", rel, err)
	}
	return out.Close()
}

func openDirBeneath(root, rel string) (int, error) {
	// O_NOFOLLOW: root 本身也不得是符号链接(审查: 防御性加固)。
	rootFD, err := unix.Open(root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, fmt.Errorf("open root %s: %w", root, err)
	}
	if rel == "." {
		return rootFD, nil
	}
	dirFD := rootFD
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		if part == "" || part == "." {
			continue
		}
		fd, openErr := unix.Openat(dirFD, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if dirFD != rootFD {
			unix.Close(dirFD)
		}
		if openErr != nil {
			unix.Close(rootFD)
			return -1, fmt.Errorf("open dir %s under %s: %w", part, root, openErr)
		}
		dirFD = fd
	}
	// 审查: 循环结束后 rootFD 仍未关闭(仅当 rel != "." 时到达此处, 此时
	// dirFD 是最后一级子目录, 与 rootFD 不同), 必须显式关闭避免 fd 泄漏。
	unix.Close(rootFD)
	return dirFD, nil
}

func writeAll(fd int, data []byte) (int, error) {
	total := 0
	for len(data) > 0 {
		n, err := unix.Write(fd, data)
		if err != nil {
			return total, err
		}
		total += n
		data = data[n:]
	}
	return total, nil
}

func isNotExist(err error) bool {
	return err == unix.ENOENT
}
