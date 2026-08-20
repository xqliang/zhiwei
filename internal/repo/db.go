// Package repo 是 MySQL DAO 层。业务包只 import 本包，
// 不直接持有 *sqlx.DB，保证未来可替换存储实现。
package repo

import (
	"context"
	"database/sql"

	_ "github.com/go-sql-driver/mysql"
	"github.com/jmoiron/sqlx"
)

// NewDB 建立连接池并 ping 验证。连接数按单机个人规模设置。
// 启用 unsafe 模式：sqlx 扫描时忽略结果集中无对应 struct 字段的列
// （memory/todo 表仍保留 legacy topic_id 列，T6 才删；本任务已删 Go 字段，
// 需 unsafe 让 SELECT m.* 不报 missing destination）。
// 注意 sqlx v1.4 的 Unsafe() 返回新对象，必须用其返回值。
func NewDB(dsn string) (*sqlx.DB, error) {
	db, err := sqlx.Connect("mysql", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(10)
	db.SetMaxIdleConns(5)
	return db.Unsafe(), nil
}

// ExecerContext 是 *sqlx.DB 与 *sqlx.Tx 共同满足的执行接口。
// 事务内的写方法（当前消费者：TopicRepo.CreateExt；后续 memory/todo DAO
// 的事务写入也走此接口）以此为参数，事务外调用传 r.DB 即可，同一实现两用。
type ExecerContext interface {
	ExecContext(ctx context.Context, query string, args ...interface{}) (sql.Result, error)
	NamedExecContext(ctx context.Context, query string, arg interface{}) (sql.Result, error)
}

// 编译期断言：*sqlx.DB 与 *sqlx.Tx 均满足 ExecerContext。
var _ ExecerContext = (*sqlx.DB)(nil)
var _ ExecerContext = (*sqlx.Tx)(nil)

// QueryRowxContext 是 *sqlx.DB 与 *sqlx.Tx 共同满足的单行查询接口。
// 事务内读操作（如 FindActiveByNameExt）以此为参数，事务外调用传 r.DB。
type QueryRowxContext interface {
	QueryRowxContext(ctx context.Context, query string, args ...interface{}) *sqlx.Row
}

var _ QueryRowxContext = (*sqlx.DB)(nil)
var _ QueryRowxContext = (*sqlx.Tx)(nil)

// QueryerContext 是 *sqlx.DB 与 *sqlx.Tx 共同满足的多行查询接口
//（SelectContext）。事务内需要返回多行的读操作（如重跑前快照手动关联）
// 以此为参数，事务外调用传 r.DB。
type QueryerContext interface {
	SelectContext(ctx context.Context, dest interface{}, query string, args ...interface{}) error
}

// 编译期断言：*sqlx.DB 与 *sqlx.Tx 均满足 QueryerContext。
var _ QueryerContext = (*sqlx.DB)(nil)
var _ QueryerContext = (*sqlx.Tx)(nil)
