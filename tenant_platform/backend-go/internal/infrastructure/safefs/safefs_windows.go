//go:build !unix

package safefs

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// ErrFileTooLarge 表示读取期间文件超过 maxBytes(审查 R5-I5): 与 Unix 实现
// 同语义, fstat 后文件继续增长时读取上限为 maxBytes+1, 超限即拒绝。
var ErrFileTooLarge = errors.New("file exceeded read limit")

// OpenBeneath Windows 开发回退: 逐组件 Lstat 拒绝符号链接后打开。
// 生产部署是 Linux(方案 §13), 该实现只保证开发环境行为一致。
func OpenBeneath(root, rel string, flags int, perm os.FileMode) (*os.File, error) {
	rel, err := CleanRel(root, rel)
	if err != nil {
		return nil, err
	}
	abs := filepath.Join(root, filepath.FromSlash(rel))
	if err := rejectSymlinkComponents(root, abs); err != nil {
		return nil, err
	}
	return os.OpenFile(abs, flags, perm)
}

func ReadFileBeneath(root, rel string) ([]byte, error) {
	return ReadFileBeneathLimited(root, rel, 0)
}

// ReadFileBeneathLimited Windows 开发回退: 与 Unix 分支同语义(先校验 size
// 上限 + maxBytes+1 读取检测, 审查 R5-I5: fstat 后文件增长不得静默截断)。
func ReadFileBeneathLimited(root, rel string, maxBytes int64) ([]byte, error) {
	f, err := OpenBeneath(root, rel, os.O_RDONLY, 0)
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

func MkdirAllBeneath(root, rel string, perm os.FileMode) error {
	rel, err := CleanRel(root, rel)
	if err != nil {
		return err
	}
	abs := filepath.Join(root, filepath.FromSlash(rel))
	if err := rejectSymlinkComponents(root, filepath.Dir(abs)); err != nil {
		return err
	}
	return os.MkdirAll(abs, perm)
}

func AtomicWriteBeneath(root, rel string, data []byte, perm os.FileMode) error {
	rel, err := CleanRel(root, rel)
	if err != nil {
		return err
	}
	abs := filepath.Join(root, filepath.FromSlash(rel))
	if err := rejectSymlinkComponents(root, filepath.Dir(abs)); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(abs), "."+filepath.Base(abs)+"-*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, abs)
}

func CopyFileBeneath(root, rel, src string, perm os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := OpenBeneath(root, rel, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Sync(); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// RemoveBeneath 删除 root 下 rel(中间组件不跟随符号链接)。
func RemoveBeneath(root, rel string) error {
	rel, err := CleanRel(root, rel)
	if err != nil {
		return err
	}
	abs := filepath.Join(root, filepath.FromSlash(rel))
	if err := rejectSymlinkComponents(root, filepath.Dir(abs)); err != nil {
		return err
	}
	return os.Remove(abs)
}

// rejectSymlinkComponents 检查 abs 在 root 内的每个中间组件(不含最终组件)
// 都不是符号链接。Windows 的最终组件由 OpenFile 的 FILE_FLAG_OPEN_REPARSE
// 语义或 Lstat 调用方自行保证。
func rejectSymlinkComponents(root, abs string) error {
	rel, err := filepath.Rel(root, abs)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("path escapes root: %s", abs)
	}
	cur := root
	parts := strings.Split(rel, string(filepath.Separator))
	for i, part := range parts {
		if part == "" || part == "." {
			continue
		}
		cur = filepath.Join(cur, part)
		if i == len(parts)-1 {
			continue // 最终组件交给调用方
		}
		info, err := os.Lstat(cur)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("symlink component in path: %s", cur)
		}
	}
	return nil
}
