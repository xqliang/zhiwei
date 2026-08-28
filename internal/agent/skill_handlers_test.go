package agent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"zhiwei/internal/repo"
)

func skillHandler(t *testing.T) (*AgentHandler, string) {
	t.Helper()
	db, err := repo.NewDB(orchDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = db.Exec("DELETE FROM agent_skill WHERE name LIKE 'test-%'") })
	root := t.TempDir()
	_ = os.MkdirAll(filepath.Join(root, "enabled"), 0o755)
	_ = os.MkdirAll(filepath.Join(root, "disabled"), 0o755)
	inst := NewSkillInstaller("http://codeload.invalid", "http://search.invalid", root)
	return &AgentHandler{Skills: &repo.AgentSkillRepo{DB: db}, SkillInst: inst}, root
}

func doSkill(h *AgentHandler, method, path, body string) *httptest.ResponseRecorder {
	r := chi.NewRouter()
	r.Use(injectUser(1))
	RegisterAgent(r, h)
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

func TestSkillHandlersLifecycle(t *testing.T) {
	h, root := skillHandler(t)

	// 直接造一条已安装技能（磁盘 + DB），走启禁/删除路径（安装路径由 Task 3 测过）
	s := &repo.AgentSkill{Name: "test-demo", DisplayName: "test-demo", Description: "d",
		Content: "---\nname: test-demo\ndescription: d\n---\nx", Enabled: true, Source: "a/b/test-demo"}
	if err := h.Skills.Create(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	skillDir := filepath.Join(root, "enabled", "test-demo")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(s.Content), 0o644); err != nil {
		t.Fatal(err)
	}

	// 列表
	rec := doSkill(h, "GET", "/api/agent/skills", "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "test-demo") {
		t.Fatalf("GET code=%d body=%s", rec.Code, rec.Body.String())
	}

	// 详情含 content
	rec = doSkill(h, "GET", "/api/agent/skills/"+s.ID.String(), "")
	if !strings.Contains(rec.Body.String(), "name: test-demo") {
		t.Errorf("详情应含 content: %s", rec.Body.String())
	}

	// 禁用：目录应移到 disabled/
	rec = doSkill(h, "PATCH", "/api/agent/skills/"+s.ID.String(), `{"enabled":false}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("PATCH code=%d body=%s", rec.Code, rec.Body.String())
	}
	if _, err := os.Stat(filepath.Join(root, "disabled", "test-demo", "SKILL.md")); err != nil {
		t.Errorf("禁用后应在 disabled/: %v", err)
	}
	if _, err := os.Stat(skillDir); err == nil {
		t.Error("禁用后 enabled/ 下不应存在")
	}

	// 重新启用：移回 enabled/
	rec = doSkill(h, "PATCH", "/api/agent/skills/"+s.ID.String(), `{"enabled":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("PATCH enable code=%d body=%s", rec.Code, rec.Body.String())
	}
	if _, err := os.Stat(filepath.Join(root, "enabled", "test-demo", "SKILL.md")); err != nil {
		t.Errorf("启用后应在 enabled/: %v", err)
	}

	// 删除：磁盘 + DB 全清
	rec = doSkill(h, "DELETE", "/api/agent/skills/"+s.ID.String(), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("DELETE code=%d body=%s", rec.Code, rec.Body.String())
	}
	if _, err := os.Stat(filepath.Join(root, "enabled", "test-demo")); err == nil {
		t.Error("删除后磁盘应清")
	}
	list, _ := h.Skills.List(context.Background())
	for _, x := range list {
		if x.Name == "test-demo" {
			t.Error("删除后 DB 应清")
		}
	}

	// 安装端点：source 非法 → 400
	rec = doSkill(h, "POST", "/api/agent/skills/install", `{"source":"bad-format"}`)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("非法 source 应 400, got %d", rec.Code)
	}

	// 搜索端点不可达 → 502
	rec = doSkill(h, "GET", "/api/agent/skills/search?q=x", "")
	if rec.Code != http.StatusBadGateway {
		t.Errorf("搜索失败应 502, got %d", rec.Code)
	}

	// 未装配 → 503
	rec2 := httptest.NewRecorder()
	h2 := &AgentHandler{}
	r2 := chi.NewRouter()
	r2.Use(injectUser(1))
	RegisterAgent(r2, h2)
	r2.ServeHTTP(rec2, httptest.NewRequest("GET", "/api/agent/skills", nil))
	if rec2.Code != http.StatusServiceUnavailable {
		t.Errorf("未装配应 503, got %d", rec2.Code)
	}
}
