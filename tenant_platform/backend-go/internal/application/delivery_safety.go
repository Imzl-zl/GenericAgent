package application

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// 交付安全(方案 §4/§6):
// - Platform 只交付 outputs/ 下的最终普通文件;
// - 拒绝符号链接、设备、管道、目录、越界路径与超大文件;
// - 用 Lstat 而非 Stat, 不跟随链接, 也不读取链接目标。

// defaultMaxDeliverableBytes 是单文件交付大小上限(与既有单文件限制一致)。
const defaultMaxDeliverableBytes = 8 << 20 // 8 MiB

// validateDeliverable 校验并返回可交付的普通文件路径(默认大小上限)。
func validateDeliverable(outputsRoot, marker string) (string, error) {
	return validateDeliverableLimited(outputsRoot, marker, defaultMaxDeliverableBytes)
}

// validateDeliverableLimited 校验 outputsRoot 下 marker 指向的普通文件。
// 返回 resolved 绝对路径; 任何非普通文件或越界都返回错误。
func validateDeliverableLimited(outputsRoot, marker string, maxBytes int64) (string, error) {
	if strings.TrimSpace(marker) == "" {
		return "", fmt.Errorf("deliverable marker is empty")
	}
	base, err := filepath.Abs(outputsRoot)
	if err != nil {
		return "", fmt.Errorf("resolve outputs root: %w", err)
	}
	var candidate string
	if filepath.IsAbs(marker) {
		candidate = marker
	} else {
		candidate = filepath.Join(base, filepath.FromSlash(marker))
	}
	resolved, err := filepath.Abs(candidate)
	if err != nil {
		return "", fmt.Errorf("resolve candidate path: %w", err)
	}
	rel, err := filepath.Rel(base, resolved)
	if err != nil {
		return "", fmt.Errorf("resolve relative path: %w", err)
	}
	rel = filepath.Clean(rel)
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", fmt.Errorf("deliverable escapes outputs root: %s", marker)
	}

	// Lstat: 不跟随符号链接; 链接本身即拒绝(不读取链接目标)。
	info, err := os.Lstat(resolved)
	if err != nil {
		return "", fmt.Errorf("stat deliverable %q: %w", marker, err)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("deliverable %q is not a regular file (mode %s)", marker, info.Mode())
	}
	if maxBytes > 0 && info.Size() > maxBytes {
		return "", fmt.Errorf("deliverable %q exceeds size limit %d", marker, maxBytes)
	}

	// 中间路径组件也可能是指向根外的符号链接(Lstat 只检查最后组件):
	// EvalSymlinks 解析全部组件后必须仍落在 outputs 根内(方案 §4 不跟随链接)。
	evaluated, err := filepath.EvalSymlinks(resolved)
	if err != nil {
		return "", fmt.Errorf("resolve deliverable symlinks %q: %w", marker, err)
	}
	evalRel, err := filepath.Rel(base, evaluated)
	if err != nil {
		return "", fmt.Errorf("resolve evaluated path: %w", err)
	}
	evalRel = filepath.Clean(evalRel)
	if evalRel == ".." || strings.HasPrefix(evalRel, ".."+string(filepath.Separator)) || filepath.IsAbs(evalRel) {
		return "", fmt.Errorf("deliverable %q resolves outside outputs root via symlink", marker)
	}
	return resolved, nil
}
