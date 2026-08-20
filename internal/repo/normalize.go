package repo

import (
	"strings"
	"unicode"
)

// NormalizeTitle 归一化待办标题用于按名去重：trim + 小写 + 仅保留字母/数字
// （去标点空格），使 "给 Tom 发邮件"/"给Tom发邮件"/"给 tom 发邮件" 归一为同值。
// 用于代办落库去重（commitExtract）与存量折叠（DedupSuggested）。
func NormalizeTitle(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(s)) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}
