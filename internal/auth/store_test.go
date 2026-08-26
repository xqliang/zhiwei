package auth

import (
	"context"
	"os"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"
	"github.com/jmoiron/sqlx"
)

// testStore 返回接测试库的 Store；未设 TEST_MYSQL_DSN 时跳过。
// 每个用例独占连接与 t.Cleanup：清空 user_session、删除非 owner(id=1) 的用户、
// 复位 owner 口令为空，保证用例间互不污染（本项目测试库非自隔离）。
func testStore(t *testing.T) *Store {
	t.Helper()
	dsn := os.Getenv("TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("TEST_MYSQL_DSN 未设置，跳过集成测试")
	}
	db, err := sqlx.Connect("mysql", dsn)
	if err != nil {
		t.Fatalf("连接测试库失败: %v", err)
	}
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM user_session`)
		_, _ = db.Exec(`DELETE FROM app_user WHERE id <> 1`)
		_, _ = db.Exec(`UPDATE app_user SET password_hash = '' WHERE id = 1`)
		_ = db.Close()
	})
	return &Store{DB: db}
}

// newToken 是测试内取合法 64-hex token 的便捷封装。
func newToken(t *testing.T) string {
	t.Helper()
	tok, err := NewToken()
	if err != nil {
		t.Fatalf("NewToken: %v", err)
	}
	return tok
}

// TestGetUser 验证按 id / username 取用户，以及缺失返回 (nil,nil)。
func TestGetUser(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	u, err := s.GetUser(ctx, 1)
	if err != nil {
		t.Fatalf("GetUser: %v", err)
	}
	if u == nil || u.ID != 1 || u.Username != "owner" {
		t.Fatalf("owner 用户不符: %+v", u)
	}

	byName, err := s.GetUserByUsername(ctx, "owner")
	if err != nil {
		t.Fatalf("GetUserByUsername: %v", err)
	}
	if byName == nil || byName.ID != 1 {
		t.Fatalf("按用户名取 owner 失败: %+v", byName)
	}

	// 不存在 → (nil,nil)，不报错。
	missing, err := s.GetUserByUsername(ctx, "nobody-xyz")
	if err != nil {
		t.Fatalf("缺失用户不应报错: %v", err)
	}
	if missing != nil {
		t.Fatalf("缺失用户应为 nil，实际 %+v", missing)
	}

	missingByID, err := s.GetUser(ctx, 999999)
	if err != nil {
		t.Fatalf("缺失 id 不应报错: %v", err)
	}
	if missingByID != nil {
		t.Fatalf("缺失 id 应为 nil，实际 %+v", missingByID)
	}
}

// TestCreateUser 验证建号后可按用户名取回、id 非零、字段落库正确，且重名返回可辨错误。
// 新建用户由 testStore 的 Cleanup 统一清理（DELETE FROM app_user WHERE id <> 1）。
func TestCreateUser(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	hash, err := HashPassword("pw")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	uid, err := s.CreateUser(ctx, "alice-2c", hash, "爱丽丝")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if uid == 0 {
		t.Fatal("CreateUser 返回的 id 不应为零")
	}

	got, err := s.GetUserByUsername(ctx, "alice-2c")
	if err != nil {
		t.Fatalf("GetUserByUsername: %v", err)
	}
	if got == nil {
		t.Fatal("建号后按用户名应命中，实际为 nil")
	}
	if got.ID != uid || got.DisplayName != "爱丽丝" || got.PasswordHash != hash {
		t.Fatalf("建号后取回字段不符: %+v (want id=%d)", got, uid)
	}

	// 重名 → 返回错误（不静默成功）。
	if _, err := s.CreateUser(ctx, "alice-2c", hash, "另一个爱丽丝"); err == nil {
		t.Fatal("重名用户应返回错误")
	}
}

// TestSetPasswordHash 验证写入后可读回。
func TestSetPasswordHash(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	hash, err := HashPassword("pw")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	if err := s.SetPasswordHash(ctx, 1, hash); err != nil {
		t.Fatalf("SetPasswordHash: %v", err)
	}
	u, err := s.GetUser(ctx, 1)
	if err != nil {
		t.Fatalf("GetUser: %v", err)
	}
	if u.PasswordHash != hash {
		t.Fatalf("口令哈希未写入，got %q want %q", u.PasswordHash, hash)
	}
}

// TestSessionLifecycle 验证建 session → 命中 → 删除后不命中。
func TestSessionLifecycle(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	tok := newToken(t)

	if err := s.CreateSession(ctx, tok, 1, time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	uid, ok, err := s.SessionUserID(ctx, tok)
	if err != nil {
		t.Fatalf("SessionUserID: %v", err)
	}
	if !ok || uid != 1 {
		t.Fatalf("有效 session 应命中 uid=1，got uid=%d ok=%v", uid, ok)
	}

	if err := s.DeleteSession(ctx, tok); err != nil {
		t.Fatalf("DeleteSession: %v", err)
	}
	_, ok, err = s.SessionUserID(ctx, tok)
	if err != nil {
		t.Fatalf("SessionUserID after delete: %v", err)
	}
	if ok {
		t.Fatal("删除后不应命中")
	}
}

// TestSessionExpired 验证已过期的 session 不命中。
func TestSessionExpired(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	tok := newToken(t)

	if err := s.CreateSession(ctx, tok, 1, time.Now().Add(-time.Hour)); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	_, ok, err := s.SessionUserID(ctx, tok)
	if err != nil {
		t.Fatalf("SessionUserID: %v", err)
	}
	if ok {
		t.Fatal("已过期 session 不应命中")
	}
}

// TestDeleteExpiredSessions 验证只清理过期行、保留有效行、返回删除数。
func TestDeleteExpiredSessions(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	expired := newToken(t)
	valid := newToken(t)

	if err := s.CreateSession(ctx, expired, 1, time.Now().Add(-time.Hour)); err != nil {
		t.Fatalf("CreateSession expired: %v", err)
	}
	if err := s.CreateSession(ctx, valid, 1, time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("CreateSession valid: %v", err)
	}

	n, err := s.DeleteExpiredSessions(ctx)
	if err != nil {
		t.Fatalf("DeleteExpiredSessions: %v", err)
	}
	if n < 1 {
		t.Fatalf("应至少删除 1 条过期 session，实际 %d", n)
	}

	// 有效 session 仍在。
	_, ok, err := s.SessionUserID(ctx, valid)
	if err != nil {
		t.Fatalf("SessionUserID valid: %v", err)
	}
	if !ok {
		t.Fatal("有效 session 不应被清理")
	}
}
