package domain

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestTruncateUTF8(t *testing.T) {
	// 中文 3 字节/字符: 截断在非边界会切出半个字符。
	long := strings.Repeat("中文文本", 100) // 400 字符 = 1200 字节
	for _, limit := range []int{1, 2, 3, 4, 5, 100, 1001, 1199, 1200} {
		got := TruncateUTF8(long, limit)
		if !utf8.ValidString(got) {
			t.Fatalf("limit=%d: result is not valid UTF-8: %q", limit, got)
		}
		if len(got) > limit {
			t.Fatalf("limit=%d: result %d bytes exceeds limit", limit, len(got))
		}
		if limit >= len(long) && got != long {
			t.Fatalf("limit=%d: full string should pass through unchanged", limit)
		}
	}
}

func TestTruncateUTF8ShortInput(t *testing.T) {
	s := "短"
	if got := TruncateUTF8(s, 10); got != s {
		t.Fatalf("short input changed: %q", got)
	}
	// ASCII 边界行为: 单字节字符按字节截断即可。
	if got := TruncateUTF8("abcdef", 3); got != "abc" {
		t.Fatalf("ascii truncate = %q, want abc", got)
	}
}

func TestTruncateUTF8ZeroLimit(t *testing.T) {
	if got := TruncateUTF8("abc", 0); got != "abc" {
		t.Fatalf("limit 0 should pass through, got %q", got)
	}
	if got := TruncateUTF8("abc", -1); got != "abc" {
		t.Fatalf("negative limit should pass through, got %q", got)
	}
}
