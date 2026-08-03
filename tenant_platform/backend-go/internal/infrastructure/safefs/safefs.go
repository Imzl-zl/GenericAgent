// Package safefs 提供“不跟随符号链接”的 root 受限文件访问原语。
//
// 共享工作区卷中, Runner(不可信)可写 memory/temp/state 等目录, Platform
// 在宿主命名空间读写这些路径时, 中间目录可能被替换为指向其他工作区或
// 宿主任意位置的符号链接(TOCTOU/跨租户越界)。safefs 用逐级 openat +
// O_NOFOLLOW(Unix) 或逐组件 Lstat 校验(Windows)保证任何组件(含最终组件)
// 都不是符号链接, 读写无法逃逸 root。
package safefs

import (
	"fmt"
	"path/filepath"
	"strings"
)

// maxDepth 限制路径组件数, 防止超深路径耗尽 fd。
const maxDepth = 64

// CleanRel 校验 rel 是 root 内的相对路径: 非绝对、无 .. 逃逸、组件数受限。
func CleanRel(root, rel string) (string, error) {
	if root == "" {
		return "", fmt.Errorf("root is required")
	}
	rel = filepath.Clean(filepath.FromSlash(rel))
	if rel == "." || rel == "" {
		return "", fmt.Errorf("relative path is required")
	}
	if filepath.IsAbs(rel) || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path escapes root: %s", rel)
	}
	parts := strings.Split(rel, string(filepath.Separator))
	if len(parts) > maxDepth {
		return "", fmt.Errorf("path too deep: %s", rel)
	}
	return rel, nil
}
