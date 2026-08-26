package auth

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/jmoiron/sqlx"

	"zhiwei/internal/ids"
)

// User 对应 app_user 表。password_hash 为空表示未设密码（不可登录）。
type User struct {
	ID           ids.ID    `db:"id"`
	Username     string    `db:"username"`
	PasswordHash string    `db:"password_hash"`
	DisplayName  string    `db:"display_name"`
	CreatedAt    time.Time `db:"created_at"`
	UpdatedAt    time.Time `db:"updated_at"`
}

// Store 是 auth 子系统自持的持久化层，直接使用 sqlx，不经 internal/repo。
type Store struct{ DB *sqlx.DB }

// 说明（时区安全）：go-sql-driver 在未设 loc 参数时以 UTC 序列化 time.Time，
// 因此写入 user_session.expires_at 的是 UTC 墙钟。为使过期比较与之严格一致、
// 且不依赖 MySQL 会话/全局 time_zone 设置，这里统一用 UTC_TIMESTAMP(3) 而非 NOW()
//（NOW() 取会话 time_zone，若服务器非 UTC 会与存储值产生偏移，造成会话提前/延后失效）。
// 详见收尾报告「偏离」一节。

// GetUserByUsername 按用户名取用户；不存在返回 (nil, nil)。
func (s *Store) GetUserByUsername(ctx context.Context, username string) (*User, error) {
	var u User
	err := s.DB.GetContext(ctx, &u,
		`SELECT id, username, password_hash, display_name, created_at, updated_at
		   FROM app_user WHERE username = ?`, username)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &u, nil
}

// GetUser 按 id 取用户；不存在返回 (nil, nil)。
func (s *Store) GetUser(ctx context.Context, id ids.ID) (*User, error) {
	var u User
	err := s.DB.GetContext(ctx, &u,
		`SELECT id, username, password_hash, display_name, created_at, updated_at
		   FROM app_user WHERE id = ?`, id.Int64())
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &u, nil
}

// CreateUser 建一个用户（username 唯一）：雪花生成 id 并返回。
// 用户名冲突返回可辨中文错误——正常路径先 GetUserByUsername 查重命中即返回；
// 并发下若查重与 INSERT 之间被抢插，DB 的 UNIQUE 约束会报 1062，这里同样归一成
// 同一可辨错误（并发安全兜底，绝不会写入重复用户名）。passwordHash 空即建号但禁登。
func (s *Store) CreateUser(ctx context.Context, username, passwordHash, displayName string) (ids.ID, error) {
	existing, err := s.GetUserByUsername(ctx, username)
	if err != nil {
		return 0, err
	}
	if existing != nil {
		return 0, fmt.Errorf("用户名已存在: %s", username)
	}

	id := ids.New()
	_, err = s.DB.ExecContext(ctx,
		`INSERT INTO app_user (id, username, password_hash, display_name) VALUES (?, ?, ?, ?)`,
		id.Int64(), username, passwordHash, displayName)
	if err != nil {
		// MySQL error 1062 = 唯一键冲突（uk_user_username）：归一成可辨错误。
		var myErr *mysql.MySQLError
		if errors.As(err, &myErr) && myErr.Number == 1062 {
			return 0, fmt.Errorf("用户名已存在: %s", username)
		}
		return 0, err
	}
	return id, nil
}

// SetPasswordHash 更新指定用户的口令哈希。传入空串即「清除密码」（禁登）。
func (s *Store) SetPasswordHash(ctx context.Context, id ids.ID, hash string) error {
	_, err := s.DB.ExecContext(ctx,
		`UPDATE app_user SET password_hash = ? WHERE id = ?`, hash, id.Int64())
	return err
}

// CreateSession 写入一条会话。expiresAt 由调用方计算（通常 time.Now().Add(ttl)）。
func (s *Store) CreateSession(ctx context.Context, token string, userID ids.ID, expiresAt time.Time) error {
	_, err := s.DB.ExecContext(ctx,
		`INSERT INTO user_session (token, user_id, expires_at) VALUES (?, ?, ?)`,
		token, userID.Int64(), expiresAt)
	return err
}

// SessionUserID 用 token 查未过期会话对应的 userID。
// 无此 token 或已过期 → (0, false, nil)；仅底层错误才返回 err。
func (s *Store) SessionUserID(ctx context.Context, token string) (ids.ID, bool, error) {
	var uid int64
	err := s.DB.GetContext(ctx, &uid,
		`SELECT user_id FROM user_session WHERE token = ? AND expires_at > UTC_TIMESTAMP(3)`, token)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	return ids.ID(uid), true, nil
}

// DeleteSession 删除指定 token 的会话（登出）。token 不存在不算错误。
func (s *Store) DeleteSession(ctx context.Context, token string) error {
	_, err := s.DB.ExecContext(ctx, `DELETE FROM user_session WHERE token = ?`, token)
	return err
}

// DeleteExpiredSessions 清理所有已过期会话，返回删除行数（供定期清理任务调用）。
func (s *Store) DeleteExpiredSessions(ctx context.Context) (int64, error) {
	res, err := s.DB.ExecContext(ctx,
		`DELETE FROM user_session WHERE expires_at <= UTC_TIMESTAMP(3)`)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
