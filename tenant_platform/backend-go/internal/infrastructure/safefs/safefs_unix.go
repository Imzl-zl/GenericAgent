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
				// Round8(发现): 裸 Errno 无法被 errors.Is(err, os.ErrNotExist)
				// 识别——Linux 上 os.IsNotExist 判定失效(manifest 缺失被当错误)。
				// 包成 *os.PathError 保持标准库语义。
				return nil, &os.PathError{Op: "open", Path: filepath.Join(root, filepath.FromSlash(rel)), Err: openErr}
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
		// round13 审查(CI): 幂等语义内置——目标不存在视为删除成功。
		// 修复前这里包装 ENOENT 返回, 调用方的 os.IsNotExist 对
		// fmt.Errorf(%w) 包装错误返回 false(不遍历 Unwrap 链), 破坏
		// 幂等契约; Windows 版直接返回 *PathError 恰好不受影响。
		if errors.Is(err, unix.ENOENT) {
			return nil
		}
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
// Round8 审查: 源侧同样 fail-closed——O_NOFOLLOW 拒绝符号链接源, fstat
// 校验普通文件(设备/FIFO/socket 拒绝), maxBytes>0 时复制中限长截断
// (防 Poller 被攻破后读取 Platform 容器任意文件/特殊文件)。
// round9 审查: 目标侧失败清理不再用 os.Remove(out.Name())——复制期间
// 目标父目录被替换为 symlink 时, 路径式删除会沿新链接删除其他目录的
// 同名文件; 改为保留父目录 dirfd 并用 unlinkat 清理。
func CopyFileBeneath(root, rel, src string, perm os.FileMode, maxBytes int64) error {
	rel, err := CleanRel(root, rel)
	if err != nil {
		return err
	}
	in, err := unix.Open(src, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("open source %s: %w", src, err)
	}
	defer unix.Close(in)
	var st unix.Stat_t
	if err := unix.Fstat(in, &st); err != nil {
		return fmt.Errorf("stat source %s: %w", src, err)
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG {
		return fmt.Errorf("source %s is not a regular file", src)
	}

	out, dirFD, cleanup, err := createBeneath(root, rel, perm)
	if err != nil {
		return err
	}
	defer unix.Close(dirFD)
	copied, copyErr := copyBounded(out, in, maxBytes)
	if copyErr != nil {
		out.Close()
		cleanup()
		return copyErr
	}
	if maxBytes > 0 && copied > maxBytes {
		out.Close()
		cleanup()
		return fmt.Errorf("%w: source %s exceeded %d bytes during copy", ErrFileTooLarge, src, maxBytes)
	}
	if err := out.Sync(); err != nil {
		out.Close()
		return fmt.Errorf("sync %s: %w", rel, err)
	}
	return out.Close()
}

// CopyFileFromBeneath 把 srcRoot 下 srcRel 复制为 dstRoot 下 dstRel。
// round9 审查(媒体路径 TOCTOU): 源侧用 openat2 RESOLVE_BENEATH|
// RESOLVE_NO_SYMLINKS 在单次内核路径解析中完成"不逃逸根 + 无符号链接"
// 校验(openSrcBeneath), 与调用方 EvalSymlinks 预检之间不存在检查-使用
// 窗口; 目标侧同 CopyFileBeneath(dirfd + unlinkat 清理)。
func CopyFileFromBeneath(srcRoot, srcRel, dstRoot, dstRel string, perm os.FileMode, maxBytes int64) error {
	in, err := openSrcBeneath(srcRoot, srcRel, unix.O_RDONLY|unix.O_NONBLOCK)
	if err != nil {
		return fmt.Errorf("open source %s under %s: %w", srcRel, srcRoot, err)
	}
	defer unix.Close(in)
	var st unix.Stat_t
	if err := unix.Fstat(in, &st); err != nil {
		return fmt.Errorf("stat source %s: %w", srcRel, err)
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG {
		return fmt.Errorf("source %s is not a regular file", srcRel)
	}

	out, dirFD, cleanup, err := createBeneath(dstRoot, dstRel, perm)
	if err != nil {
		return err
	}
	defer unix.Close(dirFD)
	copied, copyErr := copyBounded(out, in, maxBytes)
	if copyErr != nil {
		out.Close()
		cleanup()
		return copyErr
	}
	if maxBytes > 0 && copied > maxBytes {
		out.Close()
		cleanup()
		return fmt.Errorf("%w: source %s exceeded %d bytes during copy", ErrFileTooLarge, srcRel, maxBytes)
	}
	if err := out.Sync(); err != nil {
		out.Close()
		return fmt.Errorf("sync %s: %w", dstRel, err)
	}
	return out.Close()
}

// createBeneath 在 root 下 rel 创建目标文件: 父目录 dirfd + openat
// O_NOFOLLOW(中间组件与最终组件均不跟随符号链接)。返回文件、父目录 fd
// 与 unlinkat 清理函数——失败路径必须用清理函数删除目标, 禁止按路径删除。
func createBeneath(root, rel string, perm os.FileMode) (*os.File, int, func(), error) {
	dirRel, base := filepath.Split(rel)
	dirRel = strings.TrimSuffix(dirRel, string(filepath.Separator))
	if dirRel == "" {
		dirRel = "."
	}
	dirFD, err := openDirBeneath(root, dirRel)
	if err != nil {
		return nil, -1, nil, err
	}
	fd, err := unix.Openat(dirFD, base, unix.O_WRONLY|unix.O_CREAT|unix.O_TRUNC|unix.O_NOFOLLOW|unix.O_CLOEXEC, uint32(perm))
	if err != nil {
		unix.Close(dirFD)
		return nil, -1, nil, fmt.Errorf("open target %s under %s: %w", rel, root, err)
	}
	out := os.NewFile(uintptr(fd), filepath.Join(root, filepath.FromSlash(rel)))
	cleanup := func() { _ = unix.Unlinkat(dirFD, base, 0) }
	return out, dirFD, cleanup, nil
}

// copyBounded 从 fd 复制到 dst 并返回字节数;maxBytes>0 时超限立即返回
// ErrFileTooLarge(不读入内存, 不阻塞特殊文件)。
func copyBounded(dst *os.File, fd int, maxBytes int64) (int64, error) {
	buf := make([]byte, 64*1024)
	var total int64
	for {
		n, err := unix.Read(fd, buf)
		if n > 0 {
			if maxBytes > 0 && total+int64(n) > maxBytes {
				return total, ErrFileTooLarge
			}
			if _, werr := dst.Write(buf[:n]); werr != nil {
				return total, werr
			}
			total += int64(n)
		}
		if err == unix.EINTR || err == unix.EAGAIN {
			continue
		}
		if err != nil {
			return total, err
		}
		if n == 0 {
			return total, nil
		}
	}
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
