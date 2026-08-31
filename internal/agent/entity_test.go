package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"zhiwei/internal/repo"
	"zhiwei/internal/repotest"
)

// entityHandler 造一个装配了 EntityKB/EntitySettings 的 handler，并清理该测试用户的实体数据。
// 用固定测试 uid（与其它测试的 uid=1 隔离），避免共享库时相互踩数据。
func entityHandler(t *testing.T, uid int64) *AgentHandler {
	t.Helper()
	db, err := repo.NewDB(repotest.DSN(t))
	if err != nil {
		t.Fatal(err)
	}
	clean := func() {
		_, _ = db.Exec("DELETE FROM entity_kb WHERE user_id = ?", uid)
		_, _ = db.Exec("DELETE FROM entity_settings WHERE user_id = ?", uid)
	}
	clean()
	t.Cleanup(clean)
	return &AgentHandler{
		EntityKB:       &repo.EntityKBRepo{DB: db},
		EntitySettings: &repo.EntitySettingsRepo{DB: db},
	}
}

// doEntity 用注入了 uid 的路由发一次请求。
func doEntity(h *AgentHandler, uid int64, method, path, body string) *httptest.ResponseRecorder {
	r := chi.NewRouter()
	r.Use(injectUser(uid))
	RegisterAgent(r, h)
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

func TestEntitySettingsAPI(t *testing.T) {
	const uid = int64(7001)
	h := entityHandler(t, uid)

	// GET 默认值：无行时返回默认配置（enabled + 0.8 + 全量 6 kinds + counts map）。
	rec := doEntity(h, uid, "GET", "/api/agent/entity-settings", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET code=%d body=%s", rec.Code, rec.Body.String())
	}
	var got struct {
		CorrectionEnabled   bool           `json:"correction_enabled"`
		ConfidenceThreshold float64        `json:"confidence_threshold"`
		AutoSources         []string       `json:"auto_sources"`
		CountsByKind        map[string]int `json:"counts_by_kind"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !got.CorrectionEnabled {
		t.Errorf("默认应启用 correction, got=%v", got.CorrectionEnabled)
	}
	if got.ConfidenceThreshold != 0.8 {
		t.Errorf("默认阈值应 0.8, got=%v", got.ConfidenceThreshold)
	}
	if len(got.AutoSources) != 6 {
		t.Errorf("默认 auto_sources 应 6 个, got=%v", got.AutoSources)
	}
	if got.CountsByKind == nil {
		t.Errorf("counts_by_kind 应为 map（可空但非 nil）: %s", rec.Body.String())
	}

	// PUT 保存新值。
	rec = doEntity(h, uid, "PUT", "/api/agent/entity-settings",
		`{"correction_enabled":false,"confidence_threshold":0.9,"auto_sources":["person","pet"]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT code=%d body=%s", rec.Code, rec.Body.String())
	}

	// GET 读回：反映保存值。
	rec = doEntity(h, uid, "GET", "/api/agent/entity-settings", "")
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if got.CorrectionEnabled {
		t.Errorf("保存后应禁用, got=%v", got.CorrectionEnabled)
	}
	if got.ConfidenceThreshold != 0.9 {
		t.Errorf("保存后阈值应 0.9, got=%v", got.ConfidenceThreshold)
	}
	if len(got.AutoSources) != 2 {
		t.Errorf("保存后 auto_sources 应 2 个, got=%v", got.AutoSources)
	}

	// PUT 越界阈值 → 400。
	rec = doEntity(h, uid, "PUT", "/api/agent/entity-settings", `{"confidence_threshold":1.5}`)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("阈值越界应 400, got=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestEntityCRUDAPI(t *testing.T) {
	const uid = int64(7002)
	h := entityHandler(t, uid)

	// POST 手动新增：服务端算拼音。
	rec := doEntity(h, uid, "POST", "/api/agent/entities",
		`{"canonical":"天枢","kind":"custom","note":"内部代号"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST code=%d body=%s", rec.Code, rec.Body.String())
	}
	var e repo.Entity
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
		t.Fatalf("decode entity: %v", err)
	}
	if e.ID == 0 {
		t.Error("新增实体应有 ID")
	}
	if e.Source != repo.EntitySourceManual {
		t.Errorf("Source 应为 manual, got=%q", e.Source)
	}
	if e.Pinyin == nil {
		t.Fatal("服务端应算 pinyin, got nil")
	}
	if *e.Pinyin != "tian shu" {
		t.Errorf("拼音应为 'tian shu', got=%q", *e.Pinyin)
	}
	id := e.ID.String()

	// POST 空 canonical → 400。
	rec = doEntity(h, uid, "POST", "/api/agent/entities", `{"canonical":"  ","kind":"custom"}`)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("空 canonical 应 400, got=%d", rec.Code)
	}
	// POST 非法 kind → 400。
	rec = doEntity(h, uid, "POST", "/api/agent/entities", `{"canonical":"x","kind":"bogus"}`)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("非法 kind 应 400, got=%d body=%s", rec.Code, rec.Body.String())
	}

	// GET 列表含它。
	rec = doEntity(h, uid, "GET", "/api/agent/entities", "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "天枢") {
		t.Fatalf("列表应含天枢: code=%d body=%s", rec.Code, rec.Body.String())
	}

	// PATCH 改名 + 备注：拼音应重算为 tian xuan。
	rec = doEntity(h, uid, "PATCH", "/api/agent/entities/"+id, `{"canonical":"天璇","note":"改名"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("PATCH code=%d body=%s", rec.Code, rec.Body.String())
	}
	var patched repo.Entity
	_ = json.Unmarshal(rec.Body.Bytes(), &patched)
	if patched.Canonical != "天璇" {
		t.Errorf("改名后 canonical 应为天璇, got=%q", patched.Canonical)
	}
	if patched.Pinyin == nil || *patched.Pinyin != "tian xuan" {
		t.Errorf("改名后拼音应重算为 'tian xuan', got=%v", patched.Pinyin)
	}

	// PATCH 一个 auto 实体的 canonical → 400（auto 不可改名）。
	if err := h.EntityKB.ReplaceAuto(context.Background(), uid, repo.EntityKindPerson,
		[]repo.Entity{{Canonical: "张三"}}); err != nil {
		t.Fatal(err)
	}
	autoList, err := h.EntityKB.List(context.Background(), uid, repo.EntityKindPerson)
	if err != nil || len(autoList) == 0 {
		t.Fatalf("应有 auto 实体: err=%v len=%d", err, len(autoList))
	}
	autoID := autoList[0].ID.String()
	rec = doEntity(h, uid, "PATCH", "/api/agent/entities/"+autoID, `{"canonical":"李四"}`)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("auto 改名应 400, got=%d body=%s", rec.Code, rec.Body.String())
	}

	// PATCH enabled=false → 200，实体禁用。
	rec = doEntity(h, uid, "PATCH", "/api/agent/entities/"+id, `{"enabled":false}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("PATCH enabled code=%d body=%s", rec.Code, rec.Body.String())
	}
	var disabled repo.Entity
	_ = json.Unmarshal(rec.Body.Bytes(), &disabled)
	if disabled.Enabled {
		t.Errorf("禁用后 enabled 应为 false, got=%v", disabled.Enabled)
	}

	// DELETE → 204；列表不再含；再 DELETE → 404。
	rec = doEntity(h, uid, "DELETE", "/api/agent/entities/"+id, "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("DELETE 应 204, got=%d body=%s", rec.Code, rec.Body.String())
	}
	rec = doEntity(h, uid, "GET", "/api/agent/entities", "")
	if strings.Contains(rec.Body.String(), "天璇") {
		t.Errorf("删除后列表不应含天璇: %s", rec.Body.String())
	}
	rec = doEntity(h, uid, "DELETE", "/api/agent/entities/"+id, "")
	if rec.Code != http.StatusNotFound {
		t.Errorf("重复删除应 404, got=%d", rec.Code)
	}
}

func TestEntityAPINilDeps(t *testing.T) {
	h := &AgentHandler{} // 未装配 EntityKB/EntitySettings
	for _, tc := range []struct {
		method, path, body string
	}{
		{"GET", "/api/agent/entity-settings", ""},
		{"PUT", "/api/agent/entity-settings", `{"confidence_threshold":0.5}`},
		{"POST", "/api/agent/entities", `{"canonical":"x","kind":"custom"}`},
	} {
		rec := doEntity(h, 1, tc.method, tc.path, tc.body)
		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("%s %s 未装配应 503, got=%d", tc.method, tc.path, rec.Code)
		}
	}
}
