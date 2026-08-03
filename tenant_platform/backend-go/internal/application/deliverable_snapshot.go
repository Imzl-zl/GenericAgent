package application

import (
	"crypto/rand"
	"encoding/hex"
	"path/filepath"
)

// snapshotFileName 生成 <rand>-<sanitized 原文件名> 的快照名, 保留用户可见
// 文件名与扩展名(transport 以路径 basename 作为用户可见文件名), 随机前缀
// 防止同目录并发冲突。
func snapshotFileName(absPath string) string {
	base := sanitizeFileName(filepath.Base(absPath))
	if base == "" {
		base = "file"
	}
	var raw [4]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "deliverable-" + base
	}
	return "deliverable-" + hex.EncodeToString(raw[:]) + "-" + base
}
