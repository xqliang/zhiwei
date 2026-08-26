// Package auth 实现 cookie + 服务端 session 的鉴权子系统：口令哈希、随机 token、
// 用户/会话持久化、HTTP 中间件与登录/登出/查询处理器。
//
// 设计要点：
//   - 口令用 bcrypt（自带盐、可调 cost），永不明文落库；空 hash 表示「未设密码」，不可登录。
//   - session token 用 crypto/rand 生成的 32 字节随机数（64 位 hex），仅存服务端表，
//     cookie 只携带该不透明 token；服务端查表定 userID，天然可撤销（删表行即登出）。
//   - 本包自持持久化（直用 sqlx），不依赖 internal/repo，便于独立演进与测试。
package auth

import (
	"crypto/rand"
	"encoding/hex"

	"golang.org/x/crypto/bcrypt"
)

// HashPassword 用 bcrypt 默认 cost 生成口令哈希（含随机盐）。
// 注意：bcrypt 只取口令前 72 字节，超长口令在新版库中会直接返回错误。
func HashPassword(pw string) (string, error) {
	b, err := bcrypt.GenerateFromPassword([]byte(pw), bcrypt.DefaultCost)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// CheckPassword 校验明文口令是否匹配哈希。
// 空 hash（未设密码的用户）一律返回 false —— 未设密码不可登录，避免「空口令登录」。
func CheckPassword(hash, pw string) bool {
	if hash == "" {
		return false
	}
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(pw)) == nil
}

// NewToken 生成一个 session token：crypto/rand 读 32 字节 → 64 位小写 hex。
// 必须用加密安全随机源（crypto/rand），禁用 math/rand，否则 token 可被预测。
func NewToken() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}
