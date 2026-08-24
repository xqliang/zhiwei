package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"

	"zhiwei/internal/ids"
	"zhiwei/internal/profile"
	"zhiwei/internal/provider"
	"zhiwei/internal/repo"
)

// profileTestLLM 是 api 包测试用的 fake LLM（回填端点测试时注入预置响应）。
type profileTestLLM struct{ resps []string }

func (f *profileTestLLM) Chat(_ context.Context, _ provider.ChatRequest) (provider.ChatResponse, error) {
	if len(f.resps) == 0 {
		return provider.ChatResponse{}, nil
	}
	r := f.resps[0]
	f.resps = f.resps[1:]
	return provider.ChatResponse{Content: r, TotalTokens: 10}, nil
}

var _ provider.LLMProvider = (*profileTestLLM)(nil)

func setupPersonAPI(t *testing.T) (http.Handler, *profile.Service) {
	t.Helper()
	db, err := repo.NewDB(repo.TestDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := ids.Init(1); err != nil {
		t.Fatal(err)
	}
	svc := &profile.Service{
		DB:       db,
		Sessions: &repo.SessionRepo{DB: db}, Transcripts: &repo.TranscriptRepo{DB: db},
		Memories: &repo.MemoryRepo{DB: db}, Speakers: &repo.SpeakerRepo{DB: db},
		Persons: &repo.PersonRepo{DB: db}, Attributes: &repo.PersonAttributeRepo{DB: db},
		Relationships: &repo.PersonRelationshipRepo{DB: db}, ChangeLogs: &repo.PersonChangeLogRepo{DB: db},
		LLM: &profileTestLLM{}, Model: "test", Prompt: "sys", Window: 10,
		Gate: profile.GateConfig{AutoConf: 0.75},
	}
	if err := repo.EnsurePersonBootstrap(context.Background(), svc.Persons, svc.Speakers); err != nil {
		t.Fatal(err)
	}
	r := chi.NewRouter()
	RegisterPerson(r, &PersonHandler{
		Persons: svc.Persons, Attributes: svc.Attributes,
		Relationships: svc.Relationships, ChangeLogs: svc.ChangeLogs, Service: svc,
	})
	return r, svc
}

func doReq(t *testing.T, h http.Handler, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var rd *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	} else {
		rd = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, rd)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestPersonAPIFlow(t *testing.T) {
	h, svc := setupPersonAPI(t)
	ctx := context.Background()

	// 名册：至少有 owner「我」
	rec := doReq(t, h, "GET", "/api/persons", nil)
	if rec.Code != 200 {
		t.Fatalf("名册 500: %s", rec.Body.String())
	}
	var listR struct {
		Persons []repo.PersonWithPending `json:"persons"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &listR)
	if len(listR.Persons) == 0 || !listR.Persons[0].IsOwner {
		t.Fatalf("名册应含 owner: %+v", listR.Persons)
	}

	// 新建人物
	rec = doReq(t, h, "POST", "/api/persons", map[string]any{"display_name": "张三"})
	if rec.Code != 200 {
		t.Fatalf("新建人物失败: %d %s", rec.Code, rec.Body.String())
	}
	var created repo.Person
	_ = json.Unmarshal(rec.Body.Bytes(), &created)
	if created.ID == 0 || created.DisplayName != "张三" {
		t.Fatalf("新建返回错误: %+v", created)
	}
	// 空名 → 400
	if rec := doReq(t, h, "POST", "/api/persons", map[string]any{"display_name": " "}); rec.Code != 400 {
		t.Fatalf("空名应 400: %d", rec.Code)
	}

	// 手动加属性（owner）
	owner, _ := svc.Persons.GetOwner(ctx, 1)
	rec = doReq(t, h, "POST", "/api/persons/"+owner.ID.String()+"/attributes",
		map[string]any{"attr_key": "city", "value": "北京"})
	if rec.Code != 200 {
		t.Fatalf("加属性失败: %d %s", rec.Code, rec.Body.String())
	}
	var attr repo.PersonAttribute
	_ = json.Unmarshal(rec.Body.Bytes(), &attr)
	if attr.Status != "active" || attr.Source != "manual" {
		t.Fatalf("手动属性错误: %+v", attr)
	}

	// 详情：分组结构 + 属性在「基本」组
	rec = doReq(t, h, "GET", "/api/persons/"+owner.ID.String(), nil)
	if rec.Code != 200 {
		t.Fatalf("详情失败: %d", rec.Code)
	}
	var detail struct {
		Person *repo.Person     `json:"person"`
		Groups []map[string]any `json:"groups"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &detail)
	if detail.Person == nil || detail.Person.ID != owner.ID {
		t.Fatalf("详情 person 错误: %+v", detail.Person)
	}
	found := false
	for _, g := range detail.Groups {
		if g["group"] == "基本" {
			found = true
		}
	}
	if !found {
		t.Fatalf("详情缺基本组: %+v", detail.Groups)
	}

	// 改属性值（PATCH = supersede + 新行）
	rec = doReq(t, h, "PATCH", "/api/persons/"+owner.ID.String()+"/attributes/"+attr.ID.String(),
		map[string]any{"attr_key": "city", "value": "上海"})
	if rec.Code != 200 {
		t.Fatalf("改属性失败: %d %s", rec.Code, rec.Body.String())
	}
	var attr2 repo.PersonAttribute
	_ = json.Unmarshal(rec.Body.Bytes(), &attr2)
	if attr2.ValueText != "上海" || attr2.SupersedesID == nil || *attr2.SupersedesID != attr.ID {
		t.Fatalf("改值应 supersede: %+v", attr2)
	}

	// 历史：应有 create + update 记录
	rec = doReq(t, h, "GET", "/api/persons/"+owner.ID.String()+"/history?entity_kind=attribute", nil)
	if rec.Code != 200 {
		t.Fatalf("历史失败: %d", rec.Code)
	}
	var hist struct {
		History []repo.PersonChangeLog `json:"history"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &hist)
	if len(hist.History) < 2 {
		t.Fatalf("历史至少 2 条: %d", len(hist.History))
	}

	// 关系
	rec = doReq(t, h, "POST", "/api/persons/"+owner.ID.String()+"/relationships",
		map[string]any{"relation_type": "朋友", "related_person_id": created.ID.String(), "label": "老张"})
	if rec.Code != 200 {
		t.Fatalf("加关系失败: %d %s", rec.Code, rec.Body.String())
	}

	// 确认队列：给 owner 塞一条 pending（直接走 Service），然后列队 + 确认
	_, err := svc.ApplyFacts(ctx, ids.New(), 1, []profile.Fact{
		{Plane: "attribute", Subject: profile.Subject{Kind: "self"}, AttrKey: "personality",
			Value: "沉稳", Confidence: 0.5, EpistemicType: "observed"},
	})
	if err != nil {
		t.Fatal(err)
	}
	rec = doReq(t, h, "GET", "/api/profile/pending", nil)
	if rec.Code != 200 {
		t.Fatalf("队列失败: %d %s", rec.Code, rec.Body.String())
	}
	var pend struct {
		Items []map[string]any `json:"items"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &pend)
	if len(pend.Items) == 0 {
		t.Fatal("队列应有 pending")
	}
	item := pend.Items[len(pend.Items)-1]
	itemID, _ := item["id"].(string)
	rec = doReq(t, h, "POST", "/api/profile/pending/attribute/"+itemID+"/confirm", nil)
	if rec.Code != 200 {
		t.Fatalf("确认失败: %d %s", rec.Code, rec.Body.String())
	}

	// 回填：POST /api/profile/extract 带 session_id（session 无转写 → 0 facts 也算成功）
	sess := &repo.AudioSession{ID: ids.New(), Source: "web_upload", Filename: "t.wav", StoragePath: "/tmp/x", Status: "completed"}
	if err := svc.Sessions.Create(ctx, sess); err != nil {
		t.Fatal(err)
	}
	rec = doReq(t, h, "POST", "/api/profile/extract", map[string]any{"session_id": sess.ID.String()})
	if rec.Code != 200 {
		t.Fatalf("回填失败: %d %s", rec.Code, rec.Body.String())
	}
	// 非法 session_id → 400
	if rec := doReq(t, h, "POST", "/api/profile/extract", map[string]any{"session_id": "abc"}); rec.Code != 400 {
		t.Fatalf("非法 id 应 400: %d", rec.Code)
	}

	// 人物归档（DELETE = dismissed）
	if rec := doReq(t, h, "DELETE", "/api/persons/"+created.ID.String(), nil); rec.Code != 200 {
		t.Fatalf("归档失败: %d", rec.Code)
	}
	if p, _ := svc.Persons.Get(ctx, created.ID); p.Status != "dismissed" {
		t.Fatalf("归档后应 dismissed: %+v", p)
	}
}
