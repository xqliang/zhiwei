package auth

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"golang.org/x/crypto/bcrypt"

	"zhiwei/internal/ids"
)

// CookieName 是承载 session token 的 cookie 名。
const CookieName = "zw_session"

// ctxKey 是本包私有的 context key 类型，避免与其他包的 key 冲突。
type ctxKey struct{}

var userIDKey ctxKey

// WithUserID 把已鉴权的 userID 注入 context。
func WithUserID(ctx context.Context, id ids.ID) context.Context {
	return context.WithValue(ctx, userIDKey, id)
}

// UserID 从 context 读取已鉴权的 userID；未注入返回 ok=false。
func UserID(ctx context.Context) (ids.ID, bool) {
	id, ok := ctx.Value(userIDKey).(ids.ID)
	return id, ok
}

// SetSessionCookie 下发 session cookie。
// HttpOnly 防 JS 读取（XSS 窃取）；SameSite=Lax 抵御大部分 CSRF；
// Secure 由部署形态决定（HTTPS 传 true）；Path=/ 全站生效。
func SetSessionCookie(w http.ResponseWriter, token string, ttl time.Duration, secure bool) {
	http.SetCookie(w, &http.Cookie{
		Name:     CookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   secure,
		MaxAge:   int(ttl.Seconds()),
	})
}

// ClearSessionCookie 下发一个立即过期的同名 cookie 以清除登录态（登出）。
func ClearSessionCookie(w http.ResponseWriter, secure bool) {
	http.SetCookie(w, &http.Cookie{
		Name:     CookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   secure,
		MaxAge:   -1,
	})
}

// writeJSON 统一 JSON 响应。
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func errorJSON(w http.ResponseWriter, status int, code string) {
	writeJSON(w, status, map[string]string{"error": code})
}

// userDTO 是对外暴露的用户字段（绝不含 password_hash）。
// ID 经 ids.ID 的 MarshalJSON 序列化为字符串，规避前端精度丢失。
type userDTO struct {
	ID          ids.ID `json:"id"`
	Username    string `json:"username"`
	DisplayName string `json:"display_name"`
}

func toDTO(u *User) userDTO {
	return userDTO{ID: u.ID, Username: u.Username, DisplayName: u.DisplayName}
}

// Middleware 校验请求携带的 session cookie：命中未过期会话则把 userID 注入 ctx 后放行；
// 否则返回 401，且绝不调用 next（安全不变量：无有效会话决不放行）。
// 底层 DB 故障返回 500（同样不放行），以免把基础设施错误伪装成鉴权失败。
func Middleware(store *Store) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			c, err := r.Cookie(CookieName)
			if err != nil || c.Value == "" {
				errorJSON(w, http.StatusUnauthorized, "unauthorized")
				return
			}
			uid, ok, err := store.SessionUserID(r.Context(), c.Value)
			if err != nil {
				errorJSON(w, http.StatusInternalServerError, "internal")
				return
			}
			if !ok {
				errorJSON(w, http.StatusUnauthorized, "unauthorized")
				return
			}
			next.ServeHTTP(w, r.WithContext(WithUserID(r.Context(), uid)))
		})
	}
}

// loginRequest 是登录请求体。
type loginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// placeholderHash 是一个固定且永不匹配的合法 bcrypt 值，仅用于登录失败路径的
// 计时对齐：无论「用户不存在」「未设密码」还是「密码错误」，都执行一次等价的
// bcrypt 比较，抹平响应时延差，防止基于计时的用户名枚举（对应 spec 的「防枚举」）。
var placeholderHash = mustHash("zw-login-timing-placeholder")

func mustHash(pw string) string {
	h, err := bcrypt.GenerateFromPassword([]byte(pw), bcrypt.DefaultCost)
	if err != nil {
		panic(err) // 固定常量口令，正常永不触发
	}
	return string(h)
}

// LoginHandler 处理登录：校验用户名口令 → 生成 token → 建会话 → 下发 cookie。
// 失败一律返回 401 且不区分「用户名错」与「密码错」（防枚举）。
func LoginHandler(store *Store, ttl time.Duration, secure bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req loginRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			errorJSON(w, http.StatusBadRequest, "bad_request")
			return
		}

		u, err := store.GetUserByUsername(r.Context(), req.Username)
		if err != nil {
			errorJSON(w, http.StatusInternalServerError, "internal")
			return
		}

		// 计时对齐：始终执行恰好一次 bcrypt 比较。
		// 用户不存在或未设密码时比对占位哈希（必然失败），使各失败分支时延一致。
		hash := placeholderHash
		if u != nil && u.PasswordHash != "" {
			hash = u.PasswordHash
		}
		matched := bcrypt.CompareHashAndPassword([]byte(hash), []byte(req.Password)) == nil

		if u == nil || u.PasswordHash == "" || !matched {
			errorJSON(w, http.StatusUnauthorized, "unauthorized")
			return
		}

		token, err := NewToken()
		if err != nil {
			errorJSON(w, http.StatusInternalServerError, "internal")
			return
		}
		if err := store.CreateSession(r.Context(), token, u.ID, time.Now().Add(ttl)); err != nil {
			errorJSON(w, http.StatusInternalServerError, "internal")
			return
		}
		SetSessionCookie(w, token, ttl, secure)
		writeJSON(w, http.StatusOK, map[string]any{"user": toDTO(u)})
	}
}

// LogoutHandler 处理登出：删服务端会话 + 清 cookie。幂等——无 cookie 也返回 200。
func LogoutHandler(store *Store, secure bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if c, err := r.Cookie(CookieName); err == nil && c.Value != "" {
			// 删除失败不阻断登出：cookie 仍会被清除，且过期会话终会被清理任务回收。
			_ = store.DeleteSession(r.Context(), c.Value)
		}
		ClearSessionCookie(w, secure)
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	}
}

// MeHandler 返回当前会话对应的用户。无/无效会话 → 401。
func MeHandler(store *Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie(CookieName)
		if err != nil || c.Value == "" {
			errorJSON(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		uid, ok, err := store.SessionUserID(r.Context(), c.Value)
		if err != nil {
			errorJSON(w, http.StatusInternalServerError, "internal")
			return
		}
		if !ok {
			errorJSON(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		u, err := store.GetUser(r.Context(), uid)
		if err != nil {
			errorJSON(w, http.StatusInternalServerError, "internal")
			return
		}
		if u == nil { // 会话指向的用户已被删除
			errorJSON(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"user": toDTO(u)})
	}
}
