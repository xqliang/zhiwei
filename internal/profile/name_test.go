package profile

import "testing"

func TestNormalizePersonName(t *testing.T) {
	cases := []struct {
		in   string
		want string // "" = 拒绝（不建人物）
	}{
		// 家庭/集体后缀剥离（口语粘连主场景）
		{"老保一家", "老保"},
		{"张三一家人", "张三"},
		{"他们全家", ""},   // 剥离后是代词 → 拒绝
		{"一对夫妻", ""},   // 纯集合名词 → 拒绝
		{"  ", ""},         // 空
		{"", ""},           // 空
		// 代词/非人名拒绝
		{"他们", ""},
		{"大家", ""},
		{"有人", ""},
		// 过长拒绝（>8 rune）
		{"一二三四五六七八九", ""},
		// 正常名字原样保留
		{"老保", "老保"},
		{"Allen", "Allen"},
		{"欧阳修文", "欧阳修文"},         // 4 字正常名
		{"玛利亚·迪亚兹", "玛利亚·迪亚兹"}, // 含间隔号 7 rune
		{"张老师", "张老师"},             // 单字称谓是正常称呼，不剥
		{"李医生", "李医生"},
	}
	for _, c := range cases {
		if got := NormalizePersonName(c.in); got != c.want {
			t.Errorf("NormalizePersonName(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
