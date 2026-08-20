// Package main: 一次性存量清理脚本——折叠该用户所有 suggested todo 的归一化标题重复。
// 用法：set -a; . ./.env; set +a; go run ./cmd/dedup-todos
//
// 与 extract 落库去重（commitExtract 内 ListOpenTitlesExt）互补：那个防新增重复，
// 本脚本清存量重复。幂等——dismissed 可重新确认恢复，多次跑只会折叠每次新增的重复。
package main

import (
	"context"
	"fmt"
	"log"

	"zhiwei/internal/config"
	"zhiwei/internal/repo"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		log.Fatal(err)
	}
	db, err := repo.NewDB(cfg.MySQLDSN)
	if err != nil {
		log.Fatal(err)
	}
	n, err := (&repo.TodoRepo{DB: db}).DedupSuggested(context.Background(), 1)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("已折叠 %d 条重复 suggested todo\n", n)
}
