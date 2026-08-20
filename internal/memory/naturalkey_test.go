package memory

import (
	"strings"
	"testing"

	"zhiwei/internal/ids"
)

func TestNaturalKey(t *testing.T) {
	a, b := ids.ID(3), ids.ID(1)
	// 顺序无关：排序后一致
	k1 := NaturalKey([]ids.ID{a, b}, "买菜")
	k2 := NaturalKey([]ids.ID{b, a}, "买菜")
	if k1 != k2 {
		t.Fatalf("排序不稳定: %q vs %q", k1, k2)
	}
	// 分隔符隔离 segment 与 title，防歧义
	if !strings.Contains(k1, "\x1f") {
		t.Fatalf("缺分隔符: %q", k1)
	}
	// 空 segment：退化为 title（键仍稳定可比较）
	if NaturalKey(nil, "X") != "\x1fX" {
		t.Fatalf("空段退化失败: %q", NaturalKey(nil, "X"))
	}
	// 不同 title → 不同键
	if NaturalKey([]ids.ID{a}, "A") == NaturalKey([]ids.ID{a}, "B") {
		t.Fatalf("title 未入键")
	}
	// 不同 segment → 不同键
	if NaturalKey([]ids.ID{a}, "A") == NaturalKey([]ids.ID{b}, "A") {
		t.Fatalf("segment 未入键")
	}
}
