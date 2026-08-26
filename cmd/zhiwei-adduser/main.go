// zhiwei-adduser 是新增用户的一次性 CLI：在 app_user 建一条记录，并为该用户
// 引导 owner「我」（画像系统本人节点），使新用户开箱即有自己的人物档案根节点。
//
// 用法: zhiwei-adduser -u <username> -p <password> [-n <displayName>]
//
//	set -a; . ./.env; set +a; go run ./cmd/zhiwei-adduser -u alice -p secret -n 爱丽丝
//
// DSN 取自 config.Load()（ZW_MYSQL_DSN）；口令用 bcrypt 哈希后落库，绝不明文存储。
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"

	"zhiwei/internal/auth"
	"zhiwei/internal/config"
	"zhiwei/internal/ids"
	"zhiwei/internal/repo"
)

func main() {
	username := flag.String("u", "", "用户名（必填，全局唯一）")
	password := flag.String("p", "", "登录口令（必填，bcrypt 哈希后落库）")
	displayName := flag.String("n", "", "显示名（可选，缺省取用户名）")
	flag.Parse()

	// u、p 必填，缺则打印用法并以 2 退出（与 flag 解析失败的约定退出码一致）。
	if *username == "" || *password == "" {
		fmt.Fprintln(os.Stderr, "用法: zhiwei-adduser -u <username> -p <password> [-n <displayName>]")
		os.Exit(2)
	}

	cfg, err := config.Load()
	if err != nil {
		log.Fatal(err)
	}
	// 雪花必须先 Init，否则 ids.New() 会 panic（CreateUser / EnsureOwnerForUser 均依赖）。
	if err := ids.Init(1); err != nil {
		log.Fatal(err)
	}
	db, err := repo.NewDB(cfg.MySQLDSN)
	if err != nil {
		log.Fatal(err)
	}

	name := *displayName
	if name == "" {
		name = *username
	}
	hash, err := auth.HashPassword(*password)
	if err != nil {
		log.Fatal(err)
	}

	ctx := context.Background()
	store := &auth.Store{DB: db}
	uid, err := store.CreateUser(ctx, *username, hash, name)
	if err != nil {
		log.Fatal(err)
	}

	// 为新用户引导 owner「我」（幂等；新用户此前无任何 person 行，故必然创建）。
	persons := &repo.PersonRepo{DB: db}
	if err := repo.EnsureOwnerForUser(ctx, persons, uid.Int64()); err != nil {
		log.Fatal(err)
	}

	fmt.Printf("已创建用户 %s (id=%s)\n", *username, uid)
}
