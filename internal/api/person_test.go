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
		Events: &repo.PersonEventRepo{DB: db},
		LLM:    &profileTestLLM{}, Model: "test", Prompt: "sys", Window: 10,
		Gate: profile.GateConfig{AutoConf: 0.75},
	}
	if err := repo.EnsurePersonBootstrap(context.Background(), svc.Persons, svc.Speakers); err != nil {
		t.Fatal(err)
	}
	r := chi.NewRouter()
	RegisterPerson(r, &PersonHandler{
		Persons: svc.Persons, Attributes: svc.Attributes,
		Relationships: svc.Relationships, ChangeLogs: svc.ChangeLogs,
		Events: svc.Events, Service: svc,
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

	// 本用例往共享 owner（user_id=1）写了不少行：手动 city（active）、personality=沉稳
	// （pending→HTTP 确认为 active）、朋友关系及各自审计。api 包按字典序最先跑，与后续
	// profile 包共用同一 zhiwei_test 库；若不清理，profile 包 TestApplyFactsGatePaths 的
	// 「owner 无 active personality」断言会撞见这条残留的 active 沉稳而失败。收尾删掉 owner
	// 的 city/personality 属性 + 关系 + 审计，恢复干净基线（模式参照 profile/extract_session_test.go）。
	// 提前用 t.Cleanup 注册，保证任一断言 t.Fatal 提前退出时也会清理。
	t.Cleanup(func() {
		cctx := context.Background()
		if o, err := svc.Persons.GetOwner(cctx, 1); err == nil && o != nil {
			oid := o.ID.Int64()
			_, _ = svc.DB.ExecContext(cctx, `DELETE FROM person_attribute WHERE person_id = ? AND attr_key IN ('city','personality')`, oid)
			_, _ = svc.DB.ExecContext(cctx, `DELETE FROM person_relationship WHERE person_id = ?`, oid)
			_, _ = svc.DB.ExecContext(cctx, `DELETE FROM person_change_log WHERE person_id = ?`, oid)
		}
	})

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

	// PATCH attribute 带与目标行不一致的 attr_key → 400（锁死「静默改到别的 key」回归）
	if rec := doReq(t, h, "PATCH", "/api/persons/"+owner.ID.String()+"/attributes/"+attr2.ID.String(),
		map[string]any{"attr_key": "occupation", "value": "工程师"}); rec.Code != 400 {
		t.Fatalf("attr_key 不一致应 400: %d %s", rec.Code, rec.Body.String())
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

// TestPersonPatchPreservesSummary 锁死部分更新语义：PATCH 只改名（不传 summary）
// 必须保留现有备注；传空串则显式清空。
func TestPersonPatchPreservesSummary(t *testing.T) {
	h, svc := setupPersonAPI(t)
	ctx := context.Background()

	// 建带 summary 的人物
	sum := "重要客户，谨慎跟进"
	rec := doReq(t, h, "POST", "/api/persons", map[string]any{"display_name": "李四", "summary": sum})
	if rec.Code != 200 {
		t.Fatalf("新建失败: %d %s", rec.Code, rec.Body.String())
	}
	var p repo.Person
	_ = json.Unmarshal(rec.Body.Bytes(), &p)
	if p.Summary == nil || *p.Summary != sum {
		t.Fatalf("新建应带 summary: %+v", p)
	}

	// PATCH 只改名、不传 summary → summary 应保留（回归 #1：静默清空）
	if rec := doReq(t, h, "PATCH", "/api/persons/"+p.ID.String(),
		map[string]any{"display_name": "李四改"}); rec.Code != 200 {
		t.Fatalf("改名失败: %d %s", rec.Code, rec.Body.String())
	}
	got, _ := svc.Persons.Get(ctx, p.ID)
	if got.DisplayName != "李四改" {
		t.Fatalf("改名未生效: %+v", got)
	}
	if got.Summary == nil || *got.Summary != sum {
		t.Fatalf("不传 summary 应保留现值，却变成: %v", got.Summary)
	}

	// PATCH 传空串 → 显式清空为 NULL
	empty := ""
	if rec := doReq(t, h, "PATCH", "/api/persons/"+p.ID.String(),
		map[string]any{"summary": empty}); rec.Code != 200 {
		t.Fatalf("清空 summary 失败: %d %s", rec.Code, rec.Body.String())
	}
	got, _ = svc.Persons.Get(ctx, p.ID)
	if got.Summary != nil {
		t.Fatalf("空串应清空 summary，却为: %q", *got.Summary)
	}
}

// TestPersonCreateSpeakerConflict 声纹换绑冲突：同一声纹已绑定人物后，
// 再建人物绑同一声纹 → 409。
func TestPersonCreateSpeakerConflict(t *testing.T) {
	h, svc := setupPersonAPI(t)
	ctx := context.Background()

	// setup 已跑 EnsurePersonBootstrap，此后新建的声纹不会被自动建档占用
	sp := &repo.Speaker{Name: "声纹A", Source: "enrolled"}
	if err := svc.Speakers.Create(ctx, sp); err != nil {
		t.Fatal(err)
	}
	// 首次绑定成功
	if rec := doReq(t, h, "POST", "/api/persons",
		map[string]any{"display_name": "赵六", "speaker_id": sp.ID.String()}); rec.Code != 200 {
		t.Fatalf("首次绑定失败: %d %s", rec.Code, rec.Body.String())
	}
	// 再建人物绑同一声纹 → 409
	if rec := doReq(t, h, "POST", "/api/persons",
		map[string]any{"display_name": "冒名者", "speaker_id": sp.ID.String()}); rec.Code != 409 {
		t.Fatalf("重复绑定应 409: %d %s", rec.Code, rec.Body.String())
	}
}

// TestPendingDismissHTTP 冒烟 dismiss 端点：造一条 pending 属性，走 HTTP dismiss → dismissed。
// 单列（不改 TestPersonAPIFlow 里的 confirm 路径覆盖）。跨运行幂等：dismissed 非 active，
// 下次 ApplyFacts 仍走 create_pending。
func TestPendingDismissHTTP(t *testing.T) {
	h, svc := setupPersonAPI(t)
	ctx := context.Background()

	// 低置信 observed → create_pending（owner 无 occupation active 值）
	if _, err := svc.ApplyFacts(ctx, ids.New(), 1, []profile.Fact{
		{Plane: "attribute", Subject: profile.Subject{Kind: "self"}, AttrKey: "occupation",
			Value: "自由职业", Confidence: 0.4, EpistemicType: "observed"},
	}); err != nil {
		t.Fatal(err)
	}
	rec := doReq(t, h, "GET", "/api/profile/pending", nil)
	if rec.Code != 200 {
		t.Fatalf("队列失败: %d %s", rec.Code, rec.Body.String())
	}
	var pend struct {
		Items []map[string]any `json:"items"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &pend)
	var itemID string
	for _, it := range pend.Items {
		if it["kind"] == "attribute" && it["attr_key"] == "occupation" {
			itemID, _ = it["id"].(string)
		}
	}
	if itemID == "" {
		t.Fatalf("未找到 occupation pending: %+v", pend.Items)
	}
	// 非法 kind → 400
	if rec := doReq(t, h, "POST", "/api/profile/pending/bogus/"+itemID+"/dismiss", nil); rec.Code != 400 {
		t.Fatalf("非法 kind 应 400: %d %s", rec.Code, rec.Body.String())
	}
	// dismiss 走 HTTP
	if rec := doReq(t, h, "POST", "/api/profile/pending/attribute/"+itemID+"/dismiss", nil); rec.Code != 200 {
		t.Fatalf("dismiss 失败: %d %s", rec.Code, rec.Body.String())
	}
	aid, _ := ids.ParseID(itemID)
	a, _ := svc.Attributes.Get(ctx, aid)
	if a == nil || a.Status != "dismissed" {
		t.Fatalf("dismiss 后应 dismissed: %+v", a)
	}
}

// TestPersonEventAPI 覆盖大事记 API 全链路：手动加事件（RFC3339 时区截断回归）、
// event_type 枚举校验、列表 + status 过滤、详情内嵌 events + pending 计数、确认队列
// 含 event 条目并 HTTP 确认、删除转 dismissed。跨包非自隔离：t.Cleanup 删掉 owner 的
// person_event 行 + entity_kind='event' 审计行，防污染 profile 包同库断言。
func TestPersonEventAPI(t *testing.T) {
	h, svc := setupPersonAPI(t)
	ctx := context.Background()
	owner, _ := svc.Persons.GetOwner(ctx, 1)
	t.Cleanup(func() {
		_, _ = svc.DB.ExecContext(context.Background(), "DELETE FROM person_event WHERE person_id = ?", owner.ID.Int64())
		_, _ = svc.DB.ExecContext(context.Background(), "DELETE FROM person_change_log WHERE person_id = ? AND entity_kind = 'event'", owner.ID.Int64())
	})

	// 手动加事件（RFC3339 带时区 → 日期截断）
	rec := doReq(t, h, "POST", "/api/persons/"+owner.ID.String()+"/events",
		map[string]any{"event_type": "旅行", "title": "API 测试-云南行", "occurred_at": "2026-07-20T05:00:00+08:00",
			"end_at": "2026-07-27", "location": "云南", "description": "自驾"})
	if rec.Code != 200 {
		t.Fatalf("加事件失败: %d %s", rec.Code, rec.Body.String())
	}
	var ev repo.PersonEvent
	_ = json.Unmarshal(rec.Body.Bytes(), &ev)
	if ev.Status != "active" || ev.Source != "manual" || ev.OccurredAt == nil {
		t.Fatalf("手动事件错误: %+v", ev)
	}
	// M1 回归：+08:00 05:00 → 存库日期 07-20
	if ev.OccurredAt.Format("2006-01-02") != "2026-07-20" {
		t.Fatalf("occurred_at 日期错误: %v", ev.OccurredAt)
	}

	// 非法 event_type → 400
	if rec := doReq(t, h, "POST", "/api/persons/"+owner.ID.String()+"/events",
		map[string]any{"event_type": "神秘", "title": "x"}); rec.Code != 400 {
		t.Fatalf("非法类型应 400: %d", rec.Code)
	}
	// 空 title → 400（handler 层校验，非 Service 500）
	if rec := doReq(t, h, "POST", "/api/persons/"+owner.ID.String()+"/events",
		map[string]any{"event_type": "旅行", "title": "  "}); rec.Code != 400 {
		t.Fatalf("空 title 应 400: %d %s", rec.Code, rec.Body.String())
	}

	// events 列表（含 status 过滤）
	rec = doReq(t, h, "GET", "/api/persons/"+owner.ID.String()+"/events", nil)
	if rec.Code != 200 {
		t.Fatalf("列表失败: %d", rec.Code)
	}
	var listR struct {
		Events []repo.PersonEvent `json:"events"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &listR)
	if len(listR.Events) != 1 {
		t.Fatalf("应 1 条: %d", len(listR.Events))
	}
	// ?status=pending 过滤：此刻仅 1 条 active、0 条 pending → 应过滤为空
	rec = doReq(t, h, "GET", "/api/persons/"+owner.ID.String()+"/events?status=pending", nil)
	if rec.Code != 200 {
		t.Fatal("status 过滤失败")
	}
	var pendingR struct {
		Events []repo.PersonEvent `json:"events"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &pendingR)
	if len(pendingR.Events) != 0 {
		t.Fatalf("status=pending 此刻应 0 条: %d", len(pendingR.Events))
	}

	// 详情含 events + pending 计数（先造一条 pending：低置信）
	_, err := svc.ApplyFacts(ctx, ids.New(), 1, []profile.Fact{
		{Plane: "event", Subject: profile.Subject{Kind: "self"}, EventType: "里程碑", EventTitle: "API 测试-升职",
			OccurredAt: "2026-01-15", Confidence: 0.5, EpistemicType: "observed"},
	})
	if err != nil {
		t.Fatal(err)
	}
	rec = doReq(t, h, "GET", "/api/persons/"+owner.ID.String(), nil)
	var detail struct {
		Events       []repo.PersonEvent `json:"events"`
		PendingCount int                `json:"pending_count"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &detail)
	if len(detail.Events) != 2 {
		t.Fatalf("详情应含 2 条事件: %d", len(detail.Events))
	}
	if detail.PendingCount < 1 {
		t.Fatalf("pending 计数应含事件: %d", detail.PendingCount)
	}

	// 队列含 event 条目并 HTTP 确认
	rec = doReq(t, h, "GET", "/api/profile/pending", nil)
	var pend struct {
		Items []map[string]any `json:"items"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &pend)
	var evItemID string
	for _, it := range pend.Items {
		if it["kind"] == "event" && it["value"] == "API 测试-升职" {
			evItemID, _ = it["id"].(string)
		}
	}
	if evItemID == "" {
		t.Fatal("队列缺 event 条目")
	}
	rec = doReq(t, h, "POST", "/api/profile/pending/event/"+evItemID+"/confirm", nil)
	if rec.Code != 200 {
		t.Fatalf("事件确认失败: %d %s", rec.Code, rec.Body.String())
	}

	// 删除不存在的事件（合法 id 但库中无此行）→ 404
	if rec := doReq(t, h, "DELETE", "/api/persons/"+owner.ID.String()+"/events/"+ids.New().String(), nil); rec.Code != 404 {
		t.Fatalf("删除不存在事件应 404: %d %s", rec.Code, rec.Body.String())
	}

	// 删除事件
	rec = doReq(t, h, "DELETE", "/api/persons/"+owner.ID.String()+"/events/"+ev.ID.String(), nil)
	if rec.Code != 200 {
		t.Fatalf("删除失败: %d", rec.Code)
	}
	if d, _ := svc.Events.Get(ctx, ev.ID); d.Status != "dismissed" {
		t.Fatalf("删除后应 dismissed: %+v", d)
	}
}
