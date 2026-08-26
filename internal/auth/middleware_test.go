package auth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"zhiwei/internal/ids"
)

// TestUserIDCtx 验证 ctx 注入/读取往返，空 ctx 返回 ok=false。
func TestUserIDCtx(t *testing.T) {
	if _, ok := UserID(context.Background()); ok {
		t.Fatal("空 ctx 不应有 UserID")
	}
	ctx := WithUserID(context.Background(), 42)
	id, ok := UserID(ctx)
	if !ok || id != 42 {
		t.Fatalf("往返失败 id=%d ok=%v", id, ok)
	}
}

// TestMiddleware 验证：无 cookie→401；无效 token→401；有效 token→放行且注入 uid。
func TestMiddleware(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	var seenUID ids.ID
	var seenOK bool
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenUID, seenOK = UserID(r.Context())
		w.WriteHeader(http.StatusOK)
	})
	h := Middleware(s)(next)

	// 1) 无 cookie → 401，next 不执行。
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/x", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("无 cookie 应 401，实际 %d", rec.Code)
	}
	if seenOK {
		t.Fatal("无 cookie 不应放行到 next")
	}
	assertUnauthorizedBody(t, rec)

	// 2) 无效 token → 401。
	rec = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.AddCookie(&http.Cookie{Name: CookieName, Value: "deadbeef-not-a-real-token"})
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("无效 token 应 401，实际 %d", rec.Code)
	}
	if seenOK {
		t.Fatal("无效 token 不应放行到 next")
	}

	// 3) 有效 token → 放行 + 注入 uid=1。
	tok := newToken(t)
	if err := s.CreateSession(ctx, tok, 1, time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/x", nil)
	req.AddCookie(&http.Cookie{Name: CookieName, Value: tok})
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("有效 token 应放行 200，实际 %d", rec.Code)
	}
	if !seenOK || seenUID != 1 {
		t.Fatalf("next 应收到注入的 uid=1，got uid=%d ok=%v", seenUID, seenOK)
	}
}

// TestLoginLogoutMe 覆盖登录/查询/登出全链路。
func TestLoginLogoutMe(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	const ttl = time.Hour

	hash, err := HashPassword("pw")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	if err := s.SetPasswordHash(ctx, 1, hash); err != nil {
		t.Fatalf("SetPasswordHash: %v", err)
	}

	login := LoginHandler(s, ttl, false)

	// 错误口令 → 401，且不下发 cookie。
	rec := httptest.NewRecorder()
	login(rec, jsonReq(http.MethodPost, "/login", `{"username":"owner","password":"WRONG"}`))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("错口令应 401，实际 %d", rec.Code)
	}
	if sessionCookie(rec) != "" {
		t.Fatal("登录失败不应下发 session cookie")
	}

	// 不存在的用户 → 同样 401（防枚举）。
	rec = httptest.NewRecorder()
	login(rec, jsonReq(http.MethodPost, "/login", `{"username":"ghost","password":"pw"}`))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("未知用户应 401，实际 %d", rec.Code)
	}

	// 正确口令 → 200 + Set-Cookie + user 体。
	rec = httptest.NewRecorder()
	login(rec, jsonReq(http.MethodPost, "/login", `{"username":"owner","password":"pw"}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("正确口令应 200，实际 %d body=%s", rec.Code, rec.Body.String())
	}
	tok := sessionCookie(rec)
	if tok == "" {
		t.Fatal("登录成功应下发 session cookie")
	}
	if got := userIDFromBody(t, rec); got != "1" {
		t.Fatalf("登录返回 user.id 应为 \"1\"，实际 %q", got)
	}

	// 带 cookie 调 Me → 200。
	me := MeHandler(s)
	rec = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/me", nil)
	req.AddCookie(&http.Cookie{Name: CookieName, Value: tok})
	me(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("有效会话 Me 应 200，实际 %d", rec.Code)
	}
	if got := userIDFromBody(t, rec); got != "1" {
		t.Fatalf("Me 返回 user.id 应为 \"1\"，实际 %q", got)
	}

	// 无 cookie 调 Me → 401。
	rec = httptest.NewRecorder()
	me(rec, httptest.NewRequest(http.MethodGet, "/me", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("无会话 Me 应 401，实际 %d", rec.Code)
	}

	// Logout → 200 且清 cookie（MaxAge<0）。
	logout := LogoutHandler(s, false)
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/logout", nil)
	req.AddCookie(&http.Cookie{Name: CookieName, Value: tok})
	logout(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("Logout 应 200，实际 %d", rec.Code)
	}
	if !cookieCleared(rec) {
		t.Fatal("Logout 应下发过期(MaxAge<0)的 session cookie")
	}

	// Logout 后原 token 失效：Me → 401。
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/me", nil)
	req.AddCookie(&http.Cookie{Name: CookieName, Value: tok})
	me(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("登出后 Me 应 401，实际 %d", rec.Code)
	}
}

// ---- 测试辅助 ----

func jsonReq(method, target, body string) *http.Request {
	r := httptest.NewRequest(method, target, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	return r
}

// sessionCookie 从响应里取 zw_session 的值（未设置或已过期则返回空串）。
func sessionCookie(rec *httptest.ResponseRecorder) string {
	for _, c := range rec.Result().Cookies() {
		if c.Name == CookieName && c.MaxAge >= 0 && c.Value != "" {
			return c.Value
		}
	}
	return ""
}

// cookieCleared 判断响应是否下发了「清除」语义的 session cookie（MaxAge<0）。
func cookieCleared(rec *httptest.ResponseRecorder) bool {
	for _, c := range rec.Result().Cookies() {
		if c.Name == CookieName && c.MaxAge < 0 {
			return true
		}
	}
	return false
}

func userIDFromBody(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var body struct {
		User struct {
			ID string `json:"id"`
		} `json:"user"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("解析响应体失败: %v (body=%s)", err, rec.Body.String())
	}
	return body.User.ID
}

func assertUnauthorizedBody(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	var body struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("401 应为 JSON 体: %v", err)
	}
	if body.Error != "unauthorized" {
		t.Fatalf("401 body.error 应为 unauthorized，实际 %q", body.Error)
	}
}
