package repo

import "testing"

func TestNormalizeTitle(t *testing.T) {
	cases := map[string]string{
		"给 Tom 发邮件": "给tom发邮件",
		"给Tom发邮件":   "给tom发邮件",
		"给 tom 发邮件": "给tom发邮件",
		"Abc! 123":  "abc123",
		"  ":       "",
		"":         "",
		"SDPC俱乐部划船活动准备": "sdpc俱乐部划船活动准备",
	}
	for in, want := range cases {
		if got := NormalizeTitle(in); got != want {
			t.Fatalf("NormalizeTitle(%q)=%q, want %q", in, got, want)
		}
	}
}
