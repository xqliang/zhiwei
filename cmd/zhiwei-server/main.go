// zhiwei-server 是知微云端 MVP 的唯一入口：HTTP API + 异步 pipeline worker。
package main

import (
	"log"
	"net/http"

	"zhiwei/internal/api"
)

func main() {
	log.Println("zhiwei-server listening on :8080")
	log.Fatal(http.ListenAndServe(":8080", api.NewRouter()))
}
