// Package entity 提供 ASR 实体纠错的基础件：发音归一化、候选召回、知识库种子刷新。
// 设计见 docs/superpowers/specs/2026-08-29-asr-entity-correction-design.md。
package entity

import (
	"strings"

	"github.com/hbollon/go-edlib"
	"github.com/mozillazg/go-pinyin"
)

// pinyinArgs 无声调拼音参数（包级复用，避免逐次构造）。
var pinyinArgs = pinyin.NewArgs()

func init() { pinyinArgs.Style = pinyin.Normal }

// NormalizePinyin 把任意串转「发音键」：中文字符逐字转无声调拼音（音节空格分隔），
// 连续 ASCII 字母/数字归并为一个小写词（混合名「Tom猫」→「tom mao」），其余（标点、
// 空白、其它符号）丢弃。该键用于召回阶段的 CJK 发音相似度比对。
//
// 局限（可接受）：多音字按 go-pinyin 默认读音归一（如「重庆」→zhong qing），因 KB 实体与
// 查询子串两侧对称归一，自匹配不受影响；仅当 ASR 错字恰好是另一读音的同音字时损失召回。
func NormalizePinyin(s string) string {
	var parts []string
	var latin strings.Builder
	flush := func() {
		if latin.Len() > 0 {
			parts = append(parts, latin.String())
			latin.Reset()
		}
	}
	for _, r := range s {
		if r < 128 {
			lr := strings.ToLower(string(r))
			if (lr >= "a" && lr <= "z") || (lr >= "0" && lr <= "9") {
				latin.WriteString(lr)
				continue
			}
			flush() // 标点/空白：切断拉丁词
			continue
		}
		flush()
		if py := pinyin.SinglePinyin(r, pinyinArgs); len(py) > 0 {
			parts = append(parts, py[0])
		}
		// 非汉字的其它 Unicode 字符（生僻符号等）忽略。
	}
	flush()
	return strings.Join(parts, " ")
}

// NormalizeLatin 拉丁归一化：仅保留小写字母数字（丢弃标点/下划线/连字符）。
// 存 entity_kb.metaphone 列，用于拉丁名/内部代号的匹配（替代 Double Metaphone：
// 无成熟维护的 Go 实现，拉丁串本身短，直接 Jaro-Winkler 相似度足够）。
func NormalizeLatin(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= 128 {
			continue
		}
		lr := strings.ToLower(string(r))
		if (lr >= "a" && lr <= "z") || (lr >= "0" && lr <= "9") {
			b.WriteString(lr)
		}
	}
	return b.String()
}

// Similarity 发音相似度：edlib Jaro-Winkler（对短串的公共前缀/单字符差异敏感，
// 契合「同音错字」场景），返回 [0,1]。相等=1，任一为空=0；算法错误降级 0（保守）。
func Similarity(a, b string) float64 {
	if a == "" || b == "" {
		return 0
	}
	if a == b {
		return 1
	}
	s, err := edlib.StringsSimilarity(a, b, edlib.JaroWinkler)
	if err != nil {
		return 0
	}
	return float64(s)
}
