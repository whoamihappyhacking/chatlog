package util

import (
	"fmt"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

func IsNormalString(b []byte) bool {
	str := string(b)

	// 检查是否为有效的 UTF-8
	if !utf8.ValidString(str) {
		return false
	}

	// 检查是否全部为可打印字符
	for _, r := range str {
		if !unicode.IsPrint(r) {
			return false
		}
	}

	return true
}

func MustAnyToInt(v interface{}) int {
	str := fmt.Sprintf("%v", v)
	if i, err := strconv.Atoi(str); err == nil {
		return i
	}
	return 0
}

func IsNumeric(s string) bool {
	for _, r := range s {
		if !unicode.IsDigit(r) {
			return false
		}
	}
	return len(s) > 0
}

func SplitInt64ToTwoInt32(input int64) (int64, int64) {
	return input & 0xFFFFFFFF, input >> 32
}

func Str2List(str string, sep string) []string {
	list := make([]string, 0)

	if str == "" {
		return list
	}

	listMap := make(map[string]bool)
	for _, elem := range strings.Split(str, sep) {
		elem = strings.TrimSpace(elem)
		if len(elem) == 0 {
			continue
		}
		if _, ok := listMap[elem]; ok {
			continue
		}
		listMap[elem] = true
		list = append(list, elem)
	}

	return list
}

// BuildFTSQuery 将用户输入转换为 SQLite FTS5 可以安全解析的查询表达式。
// - 如果输入包含 AND/OR/NEAR/()/"/* 等高级语法，则直接返回原样，允许高级用户自行控制；
// - 否则将空白分隔的关键词转换为 "term" AND "term2" 的形式，避免意外的模糊匹配；
// - 同时会对双引号做转义处理，避免 MATCH 解析失败。
func BuildFTSQuery(input string) string {
	s := strings.TrimSpace(input)
	if s == "" {
		return ""
	}

	upper := strings.ToUpper(s)
	advanced := strings.ContainsAny(s, "\"'*()") ||
		strings.Contains(upper, " AND ") ||
		strings.Contains(upper, " OR ") ||
		strings.Contains(upper, " NEAR ") ||
		strings.HasPrefix(upper, "NOT ")
	if advanced {
		return s
	}

	tokens := strings.Fields(s)
	if len(tokens) == 0 {
		return ""
	}

	parts := make([]string, 0, len(tokens))
	for _, token := range tokens {
		t := strings.TrimSpace(token)
		if t == "" {
			continue
		}
		t = strings.ReplaceAll(t, "\"", "\"\"")
		parts = append(parts, fmt.Sprintf("\"%s\"", t))
	}

	if len(parts) == 0 {
		return ""
	}

	return strings.Join(parts, " AND ")
}
