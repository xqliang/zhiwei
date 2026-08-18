// Package api 提供 HTTP 路由装配。MVP 单用户免登录，无认证中间件。
package api

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"
)

// NewRouter 装配全部路由。各业务 handler 通过参数注入，见后续 Task。
func NewRouter() http.Handler {
	r := chi.NewRouter()
	r.Get("/api/health", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	})
	return r
}
