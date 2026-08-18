// Package api 提供 HTTP 路由装配。MVP 单用户免登录，无认证中间件。
package api

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"
)

// NewRouter 装配基础路由；业务 handler 由 main 调 RegisterXxx 注入。
func NewRouter() *chi.Mux {
	r := chi.NewRouter()
	r.Get("/api/health", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	})
	// 静态页（web/ 目录，Task 15 填充后生效）
	fileServer := http.FileServer(http.Dir("./web"))
	r.Get("/", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, "./web/index.html")
	})
	r.Handle("/app/*", http.StripPrefix("/app/", fileServer))
	return r
}
