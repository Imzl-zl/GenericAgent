package domain

import "unicode/utf8"

// TruncateUTF8 按字节上限截断字符串且不切断多字节 UTF-8 字符(审查 F2):
// 返回的字节长度 <= limit, 截断点在合法字符边界。错误/结果文本多为中文,
// 裸字节切片会切出半个字符产生乱码写入 DB 或发送 IM。
func TruncateUTF8(s string, limit int) string {
	if limit <= 0 || len(s) <= limit {
		return s
	}
	cut := s[:limit]
	// 逐字节回退到最后一个完整 rune 边界(最多回退 3 字节)。
	for len(cut) > 0 && !utf8.ValidString(cut) {
		cut = cut[:len(cut)-1]
	}
	return cut
}
