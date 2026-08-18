// Package repo 是 MySQL DAO 层。业务包只 import 本包，
// 不直接持有 *sqlx.DB，保证未来可替换存储实现。
package repo

import (
	"github.com/jmoiron/sqlx"
	_ "github.com/go-sql-driver/mysql"
)

// NewDB 建立连接池并 ping 验证。连接数按单机个人规模设置。
func NewDB(dsn string) (*sqlx.DB, error) {
	db, err := sqlx.Connect("mysql", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(10)
	db.SetMaxIdleConns(5)
	return db, nil
}
