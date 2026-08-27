package main

import (
	"fmt"
	"zhiwei/migrations"

	"github.com/golang-migrate/migrate/v4/source/iofs"
)

func main() {
	src, err := iofs.New(migrations.FS, ".")
	if err != nil {
		fmt.Printf("iofs.New err: %v\n", err)
		return
	}
	fmt.Println("=== iofs source 遍历（First/Next）===")
	v, err := src.First()
	fmt.Printf("First: ver=%d err=%v\n", v, err)
	for {
		nv, err := src.Next(v)
		fmt.Printf("  ver=%d Next->ver=%d err=%v\n", v, nv, err)
		if err != nil {
			break
		}
		v = nv
	}
	fmt.Println("=== done ===")
}
