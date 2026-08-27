// zhiwei-resetpw 重置或清空某用户登录口令的一次性 CLI。
//
// 用法:
//
//	# 清空口令（password_hash 置空 → 该用户不可登录，直到重新设置或 owner 由 ZW_OWNER_PASSWORD 引导）
//	set -a; . ./.env; set +a; go run ./cmd/zhiwei-resetpw -u owner
//
//	# 重置为新口令（bcrypt 哈希后落库，绝不明文存储）
//	set -a; . ./.env; set +a; go run ./cmd/zhiwei-resetpw -u owner -p 新口令
//
// 场景：忘记密码需重置；或想清空 owner 口令后配 ZW_OWNER_PASSWORD 重启走引导。
// DSN 取自 config.Load()（ZW_MYSQL_DSN）。仅改 password_hash，不动其它字段/数据。
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"

	"zhiwei/internal/auth"
	"zhiwei/internal/config"
	"zhiwei/internal/repo"
)

func main() {
	username := flag.String("u", "", "用户名（必填）")
	// -p 缺省即「清空口令」（password_hash 置空、禁登）；给值则重置为该口令。
	password := flag.String("p", "", "新口令（可选；缺省=清空口令，禁登）")
	flag.Parse()

	if *username == "" {
		fmt.Fprintln(os.Stderr, "用法: zhiwei-resetpw -u <username> [-p <新口令>]（省略 -p 即清空口令）")
		os.Exit(2)
	}

	cfg, err := config.Load()
	if err != nil {
		log.Fatal(err)
	}
	db, err := repo.NewDB(cfg.MySQLDSN)
	if err != nil {
		log.Fatal(err)
	}

	ctx := context.Background()
	store := &auth.Store{DB: db}
	u, err := store.GetUserByUsername(ctx, *username)
	if err != nil {
		log.Fatal(err)
	}
	if u == nil {
		log.Fatalf("用户不存在: %s", *username)
	}

	// 口令为空 → 清空（SetPasswordHash 空串即禁登）；非空 → bcrypt 哈希后设置。
	hash := ""
	if *password != "" {
		hash, err = auth.HashPassword(*password)
		if err != nil {
			log.Fatal(err)
		}
	}
	if err := store.SetPasswordHash(ctx, u.ID, hash); err != nil {
		log.Fatal(err)
	}

	if *password == "" {
		fmt.Printf("已清空用户 %s (id=%s) 的口令——当前不可登录，直到重新设置（或 owner 配 ZW_OWNER_PASSWORD 重启引导）\n", *username, u.ID)
	} else {
		fmt.Printf("已重置用户 %s (id=%s) 的口令\n", *username, u.ID)
	}
}
