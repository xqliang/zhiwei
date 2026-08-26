package auth

import (
	"encoding/hex"
	"testing"
)

// TestHashCheckPassword 验证密码哈希往返 + 错密码/空 hash 均为 false。
func TestHashCheckPassword(t *testing.T) {
	const pw = "s3cr3t-口令"
	hash, err := HashPassword(pw)
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	if hash == "" {
		t.Fatal("哈希不应为空")
	}
	if hash == pw {
		t.Fatal("哈希不应等于明文")
	}
	if !CheckPassword(hash, pw) {
		t.Fatal("正确口令应校验通过")
	}
	if CheckPassword(hash, "wrong") {
		t.Fatal("错误口令必须失败")
	}
	// 空 hash（未设密码的用户）任何口令都不可登录。
	if CheckPassword("", pw) {
		t.Fatal("空 hash 必须失败")
	}
	if CheckPassword("", "") {
		t.Fatal("空 hash + 空口令也必须失败")
	}
}

// TestNewToken 验证 token 为 64 位 hex、可解码、且两次不相等（随机性）。
func TestNewToken(t *testing.T) {
	a, err := NewToken()
	if err != nil {
		t.Fatalf("NewToken: %v", err)
	}
	if len(a) != 64 {
		t.Fatalf("token 长度应为 64，实际 %d", len(a))
	}
	if _, err := hex.DecodeString(a); err != nil {
		t.Fatalf("token 应为合法 hex: %v", err)
	}
	b, err := NewToken()
	if err != nil {
		t.Fatalf("NewToken: %v", err)
	}
	if a == b {
		t.Fatal("两次生成的 token 不应相同")
	}
}
