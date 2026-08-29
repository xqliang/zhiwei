package entity

import "testing"

// TestNormalizePinyin 拼音归一化：中文逐字转无声调拼音空格分隔；连续 ASCII 字母/数字
// 归并为一个词（大小写不敏感）；标点/空白丢弃；空串返回空串。
func TestNormalizePinyin(t *testing.T) {
	cases := []struct{ in, want string }{
		{"张梦瑜", "zhang meng yu"},
		{"阿黄", "a huang"},
		{"Tom猫", "tom mao"},
		{"Alpha-2项目", "alpha 2 xiang mu"},
		{"张三，你好！", "zhang san ni hao"},
		{"  ", ""},
		{"", ""},
	}
	for _, c := range cases {
		if got := NormalizePinyin(c.in); got != c.want {
			t.Errorf("NormalizePinyin(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestNormalizeLatin 拉丁归一化：仅保留小写字母数字，其余丢弃。
func TestNormalizeLatin(t *testing.T) {
	if got := NormalizeLatin("Sky-Net_v2"); got != "skynetv2" {
		t.Errorf("NormalizeLatin = %q", got)
	}
	if got := NormalizeLatin("…"); got != "" {
		t.Errorf("纯标点应归一化为空串, got %q", got)
	}
}

// TestSimilarity 相似度：相等=1；空串=0；同音错字相近；完全不相关低。
func TestSimilarity(t *testing.T) {
	if got := Similarity("zhang meng yu", "zhang meng yu"); got != 1 {
		t.Errorf("相等应=1, got %v", got)
	}
	if got := Similarity("", "abc"); got != 0 {
		t.Errorf("空串应=0, got %v", got)
	}
	// 典型 ASR 同音错：张梦瑜(zhang meng yu) vs 长梦鱼(chang meng yu)——首音节不同、中尾相同。
	got := Similarity("chang meng yu", "zhang meng yu")
	t.Logf("实测 chang meng yu vs zhang meng yu = %v", got)
	if got < 0.7 {
		t.Errorf("同音错字应≥0.7, got %v", got)
	}
	// 完全不相关：Jaro-Winkler 对空格分隔的短拼音串评分偏高，实测约 0.57，
	// 故这里以召回阶段（Task 6）的 minSim 默认阈值 0.6 为界——不相关串必须落在
	// 召回门槛以下，才不会被误召回（同音错字 0.95 则远高于该门槛）。
	if got := Similarity("zhang meng yu", "kao ya rou"); got >= 0.6 {
		t.Errorf("不相关应低于召回阈值 0.6, got %v", got)
	}
}
