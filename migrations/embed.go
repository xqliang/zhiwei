// Package migrations 把 SQL 迁移文件嵌入二进制，供测试库懒建时进程内执行
// （生产仍走 make migrate-up 的 CLI 路径，两路同源同一份文件）。
package migrations

import "embed"

// FS 内嵌本目录下全部 up/down 迁移脚本。golang-migrate 的 iofs source
// 直接读它，与 CLI 读磁盘 migrations/ 得到的是同一批文件、同一套版本号。
//
//go:embed *.up.sql *.down.sql
var FS embed.FS
