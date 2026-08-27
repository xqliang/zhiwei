package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"zhiwei/internal/ids"
	"zhiwei/internal/profile"
	"zhiwei/internal/provider"
	"zhiwei/internal/repo"
	"zhiwei/internal/repotest"
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
	db, err := repo.NewDB(repotest.DSN(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := ids.InitForTest(); err != nil {
		t.Fatal(err)
	}
	svc := &profile.Service{
		DB:       db,
		Sessions: &repo.SessionRepo{DB: db}, Transcripts: &repo.TranscriptRepo{DB: db},
		Memories: &repo.MemoryRepo{DB: db}, Speakers: &repo.SpeakerRepo{DB: db},
		Persons: &repo.PersonRepo{DB: db}, Attributes: &repo.PersonAttributeRepo{DB: db},
		Relationships: &repo.PersonRelationshipRepo{DB: db}, ChangeLogs: &repo.PersonChangeLogRepo{DB: db},
		Events:  &repo.PersonEventRepo{DB: db},
		Metrics: &repo.PersonMetricRepo{DB: db}, Cycles: &repo.PersonCycleRepo{DB: db},
		Activities: &repo.PersonActivityRepo{DB: db},
		Pets:       &repo.PersonPetRepo{DB: db},
		LLM:        &profileTestLLM{}, Model: "test", Prompt: "sys", Window: 10,
		Gate: profile.GateConfig{AutoConf: 0.75},
	}
	if err := repo.EnsurePersonBootstrap(context.Background(), svc.Persons, svc.Speakers); err != nil {
		t.Fatal(err)
	}
	r := newAuthedRouter() // 注入登录用户 1（handler 现要求 auth.UserID，否则 401）
	RegisterPerson(r, &PersonHandler{
		Persons: svc.Persons, Speakers: svc.Speakers, Attributes: svc.Attributes,
		Relationships: svc.Relationships, ChangeLogs: svc.ChangeLogs,
		Events: svc.Events, Metrics: svc.Metrics, Cycles: svc.Cycles,
		Activities: svc.Activities, Pets: svc.Pets, Service: svc,
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
	if p, _ := svc.Persons.Get(ctx, 1, created.ID); p.Status != "dismissed" {
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
	got, _ := svc.Persons.Get(ctx, 1, p.ID)
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
	got, _ = svc.Persons.Get(ctx, 1, p.ID)
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

// TestPersonDetailSpeakerName 详情接口的声纹富化 + PATCH 换绑/解绑（详情页「关联声纹」
// 下拉的后端链路）：绑定时 speaker_name=声纹名；解绑后 speaker_name 为空。
func TestPersonDetailSpeakerName(t *testing.T) {
	h, svc := setupPersonAPI(t)
	ctx := context.Background()

	// setup 已跑 EnsurePersonBootstrap，此后新建的声纹不会被自动建档占用
	sp := &repo.Speaker{Name: "测试声纹王五", Source: "enrolled"}
	if err := svc.Speakers.Create(ctx, sp); err != nil {
		t.Fatal(err)
	}
	rec := doReq(t, h, "POST", "/api/persons",
		map[string]any{"display_name": "王五", "speaker_id": sp.ID.String()})
	if rec.Code != 200 {
		t.Fatalf("新建失败: %d %s", rec.Code, rec.Body.String())
	}
	var p repo.Person
	_ = json.Unmarshal(rec.Body.Bytes(), &p)

	// 详情应带声纹名（徽标直接展示，前端不用二次查表）
	rec = doReq(t, h, "GET", "/api/persons/"+p.ID.String(), nil)
	if rec.Code != 200 {
		t.Fatalf("详情失败: %d %s", rec.Code, rec.Body.String())
	}
	var d struct {
		SpeakerName string `json:"speaker_name"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &d)
	// 绑定不变式：建档绑定后声纹名同步为人物名（原名「测试声纹王五」→「王五」），
	// 故详情 speaker_name 即人物名。
	if d.SpeakerName != "王五" {
		t.Fatalf("speaker_name 应为同步后的声纹名（=人物名「王五」），得: %q", d.SpeakerName)
	}

	// PATCH 解绑（speaker_id 传空串）→ 详情 speaker_name 为空、person.speaker_id 为 NULL
	if rec := doReq(t, h, "PATCH", "/api/persons/"+p.ID.String(),
		map[string]any{"speaker_id": ""}); rec.Code != 200 {
		t.Fatalf("解绑失败: %d %s", rec.Code, rec.Body.String())
	}
	got, _ := svc.Persons.Get(ctx, 1, p.ID)
	if got.SpeakerID != nil {
		t.Fatalf("解绑后 speaker_id 应为 NULL，得: %v", got.SpeakerID)
	}
	rec = doReq(t, h, "GET", "/api/persons/"+p.ID.String(), nil)
	d.SpeakerName = "" // omitempty 省略的字段不会覆盖旧值，先清再 Unmarshal
	_ = json.Unmarshal(rec.Body.Bytes(), &d)
	if d.SpeakerName != "" {
		t.Fatalf("解绑后 speaker_name 应为空，得: %q", d.SpeakerName)
	}

	// PATCH 换绑回原声纹 → 详情恢复声纹名
	if rec := doReq(t, h, "PATCH", "/api/persons/"+p.ID.String(),
		map[string]any{"speaker_id": sp.ID.String()}); rec.Code != 200 {
		t.Fatalf("换绑失败: %d %s", rec.Code, rec.Body.String())
	}
	rec = doReq(t, h, "GET", "/api/persons/"+p.ID.String(), nil)
	d.SpeakerName = ""
	_ = json.Unmarshal(rec.Body.Bytes(), &d)
	if d.SpeakerName != "王五" {
		t.Fatalf("换绑后 speaker_name 应恢复（声纹名解绑期间保留为人物名「王五」），得: %q", d.SpeakerName)
	}
}

// TestPersonSpeakerNameSync 绑定不变式（speaker.name 跟随 person.display_name）：
// 建档绑定 / 人物改名 / 解绑 / 换绑 四个场景下声纹名的表现——绑定与改名同步，
// 解绑不回改（保留历史名），换绑把新声纹名同步为人物名。
func TestPersonSpeakerNameSync(t *testing.T) {
	h, svc := setupPersonAPI(t)
	ctx := context.Background()

	sp := &repo.Speaker{Name: "自动登记说话人x7", Source: "auto"}
	if err := svc.Speakers.Create(ctx, sp); err != nil {
		t.Fatal(err)
	}
	sp2 := &repo.Speaker{Name: "另一个声纹y9", Source: "enrolled"}
	if err := svc.Speakers.Create(ctx, sp2); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = svc.Speakers.Delete(context.Background(), sp.ID)
		_ = svc.Speakers.Delete(context.Background(), sp2.ID)
	})

	// ① 建档绑定 → 声纹名同步为人物名
	rec := doReq(t, h, "POST", "/api/persons",
		map[string]any{"display_name": "王五", "speaker_id": sp.ID.String()})
	if rec.Code != 200 {
		t.Fatalf("新建失败: %d %s", rec.Code, rec.Body.String())
	}
	var p repo.Person
	_ = json.Unmarshal(rec.Body.Bytes(), &p)
	t.Cleanup(func() { _ = svc.Persons.SetStatus(context.Background(), p.ID, "dismissed") })
	if got, _ := svc.Speakers.Get(ctx, sp.ID); got.Name != "王五" {
		t.Fatalf("建档绑定后声纹名应为人物名「王五」，得: %q", got.Name)
	}

	// ② 人物改名 → 已绑声纹名跟随
	if rec := doReq(t, h, "PATCH", "/api/persons/"+p.ID.String(),
		map[string]any{"display_name": "王五改"}); rec.Code != 200 {
		t.Fatalf("改名失败: %d %s", rec.Code, rec.Body.String())
	}
	if got, _ := svc.Speakers.Get(ctx, sp.ID); got.Name != "王五改" {
		t.Fatalf("人物改名后声纹名应跟随「王五改」，得: %q", got.Name)
	}

	// ③ 解绑 → 声纹名保留（不回改历史名）
	if rec := doReq(t, h, "PATCH", "/api/persons/"+p.ID.String(),
		map[string]any{"speaker_id": ""}); rec.Code != 200 {
		t.Fatalf("解绑失败: %d %s", rec.Code, rec.Body.String())
	}
	if got, _ := svc.Speakers.Get(ctx, sp.ID); got.Name != "王五改" {
		t.Fatalf("解绑后声纹名应保留「王五改」，得: %q", got.Name)
	}

	// ④ 换绑到新声纹 → 新声纹名同步为人物名（旧声纹不动）
	if rec := doReq(t, h, "PATCH", "/api/persons/"+p.ID.String(),
		map[string]any{"speaker_id": sp2.ID.String()}); rec.Code != 200 {
		t.Fatalf("换绑失败: %d %s", rec.Code, rec.Body.String())
	}
	if got, _ := svc.Speakers.Get(ctx, sp2.ID); got.Name != "王五改" {
		t.Fatalf("换绑后新声纹名应为人物名「王五改」，得: %q", got.Name)
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
	// P2a①：未给 importance 的「旅行」事件 → 事件类型默认 0.5（不再是旧代偿的 confidence/固定 1.0）
	if ev.Importance < 0.49 || ev.Importance > 0.51 {
		t.Fatalf("旅行事件未给 importance 应走类型默认 0.5，实得 %v", ev.Importance)
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

	// P2a①：body 带 importance=0.9 → 落库 0.9（显式值优先于类型默认）
	rec = doReq(t, h, "POST", "/api/persons/"+owner.ID.String()+"/events",
		map[string]any{"event_type": "旅行", "title": "API 测试-重要旅行", "importance": 0.9})
	if rec.Code != 200 {
		t.Fatalf("加事件(带 importance)失败: %d %s", rec.Code, rec.Body.String())
	}
	var evImp repo.PersonEvent
	_ = json.Unmarshal(rec.Body.Bytes(), &evImp)
	if evImp.Importance < 0.89 || evImp.Importance > 0.91 {
		t.Fatalf("body importance=0.9 应落库 0.9，实得 %v", evImp.Importance)
	}
}

// TestPersonEventMultiRelatedAPI 覆盖 P2a② API 层同行人物：related_person_ids 数组落多元素、
// 旧单字段 related_person_id 向后兼容、数组含非法 id → 400。跨包非自隔离：cleanup 删掉 owner 的
// person_event/event 审计与两名被引用人物（含其 person 审计）。
func TestPersonEventMultiRelatedAPI(t *testing.T) {
	h, svc := setupPersonAPI(t)
	ctx := context.Background()
	owner, _ := svc.Persons.GetOwner(ctx, 1)

	a, err := svc.ManualCreatePerson(ctx, 1, "API多人同行甲", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	b, err := svc.ManualCreatePerson(ctx, 1, "API多人同行乙", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cctx := context.Background()
		_, _ = svc.DB.ExecContext(cctx, "DELETE FROM person_event WHERE person_id = ?", owner.ID.Int64())
		_, _ = svc.DB.ExecContext(cctx, "DELETE FROM person_change_log WHERE person_id = ? AND entity_kind = 'event'", owner.ID.Int64())
		for _, id := range []ids.ID{a.ID, b.ID} {
			_, _ = svc.DB.ExecContext(cctx, "DELETE FROM person_change_log WHERE person_id = ?", id.Int64())
			_, _ = svc.DB.ExecContext(cctx, "DELETE FROM person WHERE id = ?", id.Int64())
		}
	})

	// ① related_person_ids 数组两人 → 落两元素（顺序同入参）
	rec := doReq(t, h, "POST", "/api/persons/"+owner.ID.String()+"/events",
		map[string]any{"event_type": "旅行", "title": "API多人-数组两人",
			"related_person_ids": []string{a.ID.String(), b.ID.String()}})
	if rec.Code != 200 {
		t.Fatalf("加事件(数组)失败: %d %s", rec.Code, rec.Body.String())
	}
	var ev repo.PersonEvent
	_ = json.Unmarshal(rec.Body.Bytes(), &ev)
	if len(ev.RelatedPersonIDs) != 2 || ev.RelatedPersonIDs[0] != a.ID || ev.RelatedPersonIDs[1] != b.ID {
		t.Fatalf("related_person_ids 两人应落 [甲,乙]，实得 %v", ev.RelatedPersonIDs)
	}

	// ② 旧单字段 related_person_id 向后兼容 → 落 1 元素
	rec = doReq(t, h, "POST", "/api/persons/"+owner.ID.String()+"/events",
		map[string]any{"event_type": "会议", "title": "API多人-旧单字段", "related_person_id": a.ID.String()})
	if rec.Code != 200 {
		t.Fatalf("加事件(单字段)失败: %d %s", rec.Code, rec.Body.String())
	}
	var ev2 repo.PersonEvent
	_ = json.Unmarshal(rec.Body.Bytes(), &ev2)
	if len(ev2.RelatedPersonIDs) != 1 || ev2.RelatedPersonIDs[0] != a.ID {
		t.Fatalf("旧单字段应落 1 元素（甲），实得 %v", ev2.RelatedPersonIDs)
	}

	// ③ 数组含非法 id → 400
	if rec := doReq(t, h, "POST", "/api/persons/"+owner.ID.String()+"/events",
		map[string]any{"event_type": "旅行", "title": "API多人-非法id",
			"related_person_ids": []string{"not-an-id"}}); rec.Code != 400 {
		t.Fatalf("数组含非法 id 应 400: %d %s", rec.Code, rec.Body.String())
	}
}

// TestPersonCycleAPI 覆盖周期/日程 API 全链路：手动加周期（anchor+period 算 next_predicted_at）、
// cycle_type 枚举校验、列表带 note 免责文案（spec §9）、确认队列含 cycle 条目并 HTTP 确认、
// 删除转 dismissed。跨包非自隔离：t.Cleanup 删掉 owner 的 person_cycle 行 + entity_kind='cycle'
// 审计行，防污染 profile 包同库断言。
func TestPersonCycleAPI(t *testing.T) {
	h, svc := setupPersonAPI(t)
	ctx := context.Background()
	owner, _ := svc.Persons.GetOwner(ctx, 1)
	t.Cleanup(func() {
		_, _ = svc.DB.ExecContext(context.Background(), "DELETE FROM person_cycle WHERE person_id = ?", owner.ID.Int64())
		_, _ = svc.DB.ExecContext(context.Background(), "DELETE FROM person_change_log WHERE person_id = ? AND entity_kind = 'cycle'", owner.ID.Int64())
	})

	// 手动加周期（medication + anchor 2026-08-01 + period 30 → next_predicted_at = 08-31）
	rec := doReq(t, h, "POST", "/api/persons/"+owner.ID.String()+"/cycles",
		map[string]any{"cycle_type": "medication", "label": "API 测试-降压药",
			"anchor_date": "2026-08-01", "period_days": 30, "dosage": "5mg", "frequency": "每日一次"})
	if rec.Code != 200 {
		t.Fatalf("加周期失败: %d %s", rec.Code, rec.Body.String())
	}
	var c repo.PersonCycle
	_ = json.Unmarshal(rec.Body.Bytes(), &c)
	if c.Status != "active" || c.Source != "manual" {
		t.Fatalf("手动周期错误: %+v", c)
	}
	// anchor 2026-08-01 + period 30 天 = 2026-08-31（估算非医疗建议）
	if c.NextPredictedAt == nil || c.NextPredictedAt.Format("2006-01-02") != "2026-08-31" {
		t.Fatalf("next_predicted_at 应为 2026-08-31: %v", c.NextPredictedAt)
	}

	// 非法 cycle_type → 400
	if rec := doReq(t, h, "POST", "/api/persons/"+owner.ID.String()+"/cycles",
		map[string]any{"cycle_type": "健身"}); rec.Code != 400 {
		t.Fatalf("非法 cycle_type 应 400: %d", rec.Code)
	}

	// 列表带 note 免责文案（spec §9）
	rec = doReq(t, h, "GET", "/api/persons/"+owner.ID.String()+"/cycles", nil)
	if rec.Code != 200 {
		t.Fatalf("列表失败: %d", rec.Code)
	}
	var listR struct {
		Cycles []repo.PersonCycle `json:"cycles"`
		Note   string             `json:"note"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &listR)
	if listR.Note == "" {
		t.Fatal("cycles 响应应含 note 免责文案")
	}
	if len(listR.Cycles) < 1 {
		t.Fatalf("列表应含 1 条: %d", len(listR.Cycles))
	}

	// 造一条 pending（低置信、不同 type/label 避免撞上面 active 的冲突路径）→ 队列含 cycle → 确认
	if _, err := svc.ApplyFacts(ctx, ids.New(), 1, []profile.Fact{
		{Plane: "cycle", Subject: profile.Subject{Kind: "self"}, CycleType: "followup",
			CycleLabel: "API 测试-复诊", Confidence: 0.5, EpistemicType: "observed"},
	}); err != nil {
		t.Fatal(err)
	}
	rec = doReq(t, h, "GET", "/api/profile/pending", nil)
	var pend struct {
		Items []map[string]any `json:"items"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &pend)
	var cItemID string
	for _, it := range pend.Items {
		if it["kind"] == "cycle" && it["cycle_type"] == "followup" {
			cItemID, _ = it["id"].(string)
		}
	}
	if cItemID == "" {
		t.Fatalf("队列缺 cycle 条目: %+v", pend.Items)
	}
	// 详情 PendingCount 应含该 pending cycle（此刻 owner 仅此一条 pending）
	rec = doReq(t, h, "GET", "/api/persons/"+owner.ID.String(), nil)
	var detail struct {
		PendingCount int `json:"pending_count"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &detail)
	if detail.PendingCount < 1 {
		t.Fatalf("详情 PendingCount 应含 pending cycle: %d", detail.PendingCount)
	}
	if rec := doReq(t, h, "POST", "/api/profile/pending/cycle/"+cItemID+"/confirm", nil); rec.Code != 200 {
		t.Fatalf("周期确认失败: %d %s", rec.Code, rec.Body.String())
	}
	cid2, _ := ids.ParseID(cItemID)
	if got, _ := svc.Cycles.Get(ctx, cid2); got == nil || got.Status != "active" {
		t.Fatalf("确认后应 active: %+v", got)
	}

	// 删除不存在的周期 → 404
	if rec := doReq(t, h, "DELETE", "/api/persons/"+owner.ID.String()+"/cycles/"+ids.New().String(), nil); rec.Code != 404 {
		t.Fatalf("删除不存在周期应 404: %d %s", rec.Code, rec.Body.String())
	}
	// 删除周期 → dismissed
	if rec := doReq(t, h, "DELETE", "/api/persons/"+owner.ID.String()+"/cycles/"+c.ID.String(), nil); rec.Code != 200 {
		t.Fatalf("删除失败: %d", rec.Code)
	}
	if d, _ := svc.Cycles.Get(ctx, c.ID); d == nil || d.Status != "dismissed" {
		t.Fatalf("删除后应 dismissed: %+v", d)
	}

	// ListCycles 默认滤历史版本：c 已 dismissed，默认列表（无 status）不应含它
	// （json.Unmarshal 会先把 listR.Cycles 长度重置为 0 再 append，可安全复用）。
	rec = doReq(t, h, "GET", "/api/persons/"+owner.ID.String()+"/cycles", nil)
	_ = json.Unmarshal(rec.Body.Bytes(), &listR)
	for _, cy := range listR.Cycles {
		if cy.ID == c.ID {
			t.Fatalf("默认列表不应含 dismissed 周期 c(id=%s): %+v", c.ID, listR.Cycles)
		}
	}
	// ?status=dismissed 显式过滤时才含（对齐 ListMetrics/ListEvents 的 status 语义）
	rec = doReq(t, h, "GET", "/api/persons/"+owner.ID.String()+"/cycles?status=dismissed", nil)
	var dismissedR struct {
		Cycles []repo.PersonCycle `json:"cycles"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &dismissedR)
	foundDismissed := false
	for _, cy := range dismissedR.Cycles {
		if cy.ID == c.ID {
			foundDismissed = true
		}
	}
	if !foundDismissed {
		t.Fatalf("?status=dismissed 应含刚删除的周期 c(id=%s): %+v", c.ID, dismissedR.Cycles)
	}
}

// TestPersonActivityAPI 覆盖生活轨迹 API 全链路：手动加活动（含可空 tool/location/commute_mode/
// duration_min）、activity 空值校验、时间线升序（乱序录入→GET 断言升序）、时间窗半开区间、确认
// 队列含 activity 条目（带 occurred_at）+ 详情 pending 计数 + HTTP 确认、删除转 dismissed（软删，
// 全状态 GET 仍返回该行——前端靠 status!==dismissed 客户端过滤）。跨包非自隔离：t.Cleanup 删掉
// owner 的 person_activity 行 + entity_kind='activity' 审计行，防污染 profile 包同库断言。
func TestPersonActivityAPI(t *testing.T) {
	h, svc := setupPersonAPI(t)
	ctx := context.Background()
	owner, _ := svc.Persons.GetOwner(ctx, 1)
	t.Cleanup(func() {
		_, _ = svc.DB.ExecContext(context.Background(), "DELETE FROM person_activity WHERE person_id = ?", owner.ID.Int64())
		_, _ = svc.DB.ExecContext(context.Background(), "DELETE FROM person_change_log WHERE person_id = ? AND entity_kind = 'activity'", owner.ID.Int64())
	})

	base := "/api/persons/" + owner.ID.String() + "/activities"

	// 手动加活动（通勤·地铁·40分钟）——含可空 commute_mode/duration_min。POST 返回裸行（对齐 AddMetric/AddCycle）。
	rec := doReq(t, h, "POST", base,
		map[string]any{"activity": "通勤", "commute_mode": "地铁", "started_at": "2026-08-20", "duration_min": 40})
	if rec.Code != 200 {
		t.Fatalf("加活动失败: %d %s", rec.Code, rec.Body.String())
	}
	var a1 repo.PersonActivity
	_ = json.Unmarshal(rec.Body.Bytes(), &a1)
	if a1.Status != "active" || a1.Source != "manual" {
		t.Fatalf("手动活动错误: %+v", a1)
	}
	if a1.CommuteMode == nil || *a1.CommuteMode != "地铁" {
		t.Fatalf("commute_mode 应为 地铁: %v", a1.CommuteMode)
	}
	if a1.DurationMin == nil || *a1.DurationMin != 40 {
		t.Fatalf("duration_min 应为 40: %v", a1.DurationMin)
	}

	// activity 空 → 400（handler 层校验，非 Service 500）
	if rec := doReq(t, h, "POST", base, map[string]any{"activity": "  "}); rec.Code != 400 {
		t.Fatalf("空 activity 应 400: %d %s", rec.Code, rec.Body.String())
	}

	// 乱序再加两条不同日期活动（08-25、08-18）→ 后续 GET 断言按 started_at 升序
	if rec := doReq(t, h, "POST", base, map[string]any{"activity": "打球", "started_at": "2026-08-25"}); rec.Code != 200 {
		t.Fatalf("加活动2失败: %d %s", rec.Code, rec.Body.String())
	}
	if rec := doReq(t, h, "POST", base,
		map[string]any{"activity": "写代码", "tool": "电脑", "location": "公司", "started_at": "2026-08-18"}); rec.Code != 200 {
		t.Fatalf("加活动3失败: %d %s", rec.Code, rec.Body.String())
	}

	// GET 全部 → 3 条且按 started_at 升序（08-18 写代码 / 08-20 通勤 / 08-25 打球）
	rec = doReq(t, h, "GET", base, nil)
	if rec.Code != 200 {
		t.Fatalf("列表失败: %d %s", rec.Code, rec.Body.String())
	}
	var listR struct {
		Activities []repo.PersonActivity `json:"activities"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &listR)
	if len(listR.Activities) != 3 {
		t.Fatalf("应 3 条: %d", len(listR.Activities))
	}
	for i := 1; i < len(listR.Activities); i++ {
		if listR.Activities[i].StartedAt.Before(listR.Activities[i-1].StartedAt) {
			t.Fatalf("活动应按 started_at 升序: %+v", listR.Activities)
		}
	}
	if listR.Activities[0].Activity != "写代码" || listR.Activities[2].Activity != "打球" {
		t.Fatalf("升序首尾错误: %s ... %s", listR.Activities[0].Activity, listR.Activities[2].Activity)
	}

	// 时间窗查询：半开区间 [2026-08-20, 2026-08-21) 命中 08-20 通勤 → 1 条
	rec = doReq(t, h, "GET", base+"?from=2026-08-20&to=2026-08-21", nil)
	if rec.Code != 200 {
		t.Fatalf("时间窗查询失败: %d %s", rec.Code, rec.Body.String())
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &listR)
	if len(listR.Activities) != 1 || listR.Activities[0].Activity != "通勤" {
		t.Fatalf("时间窗应命中 1 条通勤: %+v", listR.Activities)
	}
	// from 烂串 → 400
	if rec := doReq(t, h, "GET", base+"?from=abc", nil); rec.Code != 400 {
		t.Fatalf("from 烂串应 400: %d", rec.Code)
	}

	// 造一条 pending（低置信 0.5<0.75）→ 队列含 activity 条目（带 occurred_at）→ 详情计数 → HTTP 确认 → active
	if _, err := svc.ApplyFacts(ctx, ids.New(), 1, []profile.Fact{
		{Plane: "activity", Subject: profile.Subject{Kind: "self"}, ActivityText: "游泳",
			StartedAt: "2026-08-22", Confidence: 0.5, EpistemicType: "observed"},
	}); err != nil {
		t.Fatal(err)
	}
	rec = doReq(t, h, "GET", "/api/profile/pending", nil)
	var pend struct {
		Items []map[string]any `json:"items"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &pend)
	var aItemID, aOccurredAt string
	for _, it := range pend.Items {
		if it["kind"] == "activity" && it["value"] == "游泳" {
			aItemID, _ = it["id"].(string)
			aOccurredAt, _ = it["occurred_at"].(string)
		}
	}
	if aItemID == "" {
		t.Fatalf("队列缺 activity 条目: %+v", pend.Items)
	}
	// 队列条目补 started_at（occurred_at 字段）——前端时间线依赖（对齐 metric 队列条目）
	if aOccurredAt == "" {
		t.Fatalf("activity 队列条目应含 occurred_at: %+v", pend.Items)
	}
	// 详情 PendingCount 应含该 pending activity（此刻 owner 仅此一条 pending）
	rec = doReq(t, h, "GET", "/api/persons/"+owner.ID.String(), nil)
	var detail struct {
		PendingCount int `json:"pending_count"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &detail)
	if detail.PendingCount < 1 {
		t.Fatalf("详情 PendingCount 应含 pending activity: %d", detail.PendingCount)
	}
	// 名册角标（ListWithPending SQL 求和）应同样含该 pending activity——roster/详情角标须一致
	// （person.go 注释不变量）。直接覆盖 ListWithPending 的 person_activity 子查询。
	rec = doReq(t, h, "GET", "/api/persons", nil)
	var roster struct {
		Persons []repo.PersonWithPending `json:"persons"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &roster)
	ownerBadge := -1
	for _, p := range roster.Persons {
		if p.ID == owner.ID {
			ownerBadge = p.PendingCount
		}
	}
	if ownerBadge < 1 {
		t.Fatalf("名册角标应含 pending activity: %d", ownerBadge)
	}
	if rec := doReq(t, h, "POST", "/api/profile/pending/activity/"+aItemID+"/confirm", nil); rec.Code != 200 {
		t.Fatalf("活动确认失败: %d %s", rec.Code, rec.Body.String())
	}
	aid2, _ := ids.ParseID(aItemID)
	if got, _ := svc.Activities.Get(ctx, aid2); got == nil || got.Status != "active" {
		t.Fatalf("确认后应 active: %+v", got)
	}

	// 删除不存在的活动（合法 id 但库中无此行）→ 404
	if rec := doReq(t, h, "DELETE", base+"/"+ids.New().String(), nil); rec.Code != 404 {
		t.Fatalf("删除不存在活动应 404: %d %s", rec.Code, rec.Body.String())
	}
	// 删除活动 → dismissed（软删）
	if rec := doReq(t, h, "DELETE", base+"/"+a1.ID.String(), nil); rec.Code != 200 {
		t.Fatalf("删除失败: %d", rec.Code)
	}
	if d, _ := svc.Activities.Get(ctx, a1.ID); d == nil || d.Status != "dismissed" {
		t.Fatalf("删除后应 dismissed: %+v", d)
	}
	// 软删后「全状态」GET 仍返回该行（前端靠 status!==dismissed 客户端过滤，非后端剔除）
	rec = doReq(t, h, "GET", base, nil)
	_ = json.Unmarshal(rec.Body.Bytes(), &listR)
	foundDismissed := false
	for _, a := range listR.Activities {
		if a.ID == a1.ID {
			if a.Status != "dismissed" {
				t.Fatalf("软删行状态应 dismissed: %+v", a)
			}
			foundDismissed = true
		}
	}
	if !foundDismissed {
		t.Fatalf("全状态 GET 应仍含软删行(id=%s): %+v", a1.ID, listR.Activities)
	}
}

// TestAddAttributeF4Status 锁死 F4 校验错误的 HTTP 状态码映射：脏值（值域不合法）→ 400
// （对齐 metric 枚举校验的 400 口径），合法但需归一的值 → 200 且返回值已规范化。
// AddAttribute 与 PatchAttribute 两条手动写入路径都覆盖。
func TestAddAttributeF4Status(t *testing.T) {
	h, svc := setupPersonAPI(t)
	ctx := context.Background()

	t.Cleanup(func() {
		cctx := context.Background()
		if o, err := svc.Persons.GetOwner(cctx, 1); err == nil && o != nil {
			pk := o.ID.Int64()
			_, _ = svc.DB.ExecContext(cctx, `DELETE FROM person_attribute WHERE person_id = ? AND attr_key IN ('gender','birthday')`, pk)
			_, _ = svc.DB.ExecContext(cctx, `DELETE FROM person_change_log WHERE person_id = ? AND entity_kind='attribute' AND attr_key IN ('gender','birthday')`, pk)
		}
	})

	owner, _ := svc.Persons.GetOwner(ctx, 1)
	base := "/api/persons/" + owner.ID.String() + "/attributes"

	// 脏枚举 → 400（而非 500）
	if rec := doReq(t, h, "POST", base, map[string]any{"attr_key": "gender", "value": "男性"}); rec.Code != 400 {
		t.Fatalf("脏枚举应 400: %d %s", rec.Code, rec.Body.String())
	}
	// 脏日期 → 400
	if rec := doReq(t, h, "POST", base, map[string]any{"attr_key": "birthday", "value": "八月三号"}); rec.Code != 400 {
		t.Fatalf("脏日期应 400: %d %s", rec.Code, rec.Body.String())
	}

	// 合法但需重排的日期 → 200，返回值已规范化
	rec := doReq(t, h, "POST", base, map[string]any{"attr_key": "birthday", "value": "2026/08/03"})
	if rec.Code != 200 {
		t.Fatalf("合法日期应 200: %d %s", rec.Code, rec.Body.String())
	}
	var attr repo.PersonAttribute
	_ = json.Unmarshal(rec.Body.Bytes(), &attr)
	if attr.ValueText != "2026-08-03" {
		t.Fatalf("日期应规范化为 2026-08-03: %+v", attr)
	}

	// PatchAttribute 路径同样把脏值映射为 400（改成非目录枚举值）
	if rec := doReq(t, h, "PATCH", base+"/"+attr.ID.String(),
		map[string]any{"attr_key": "birthday", "value": "不是日期"}); rec.Code != 400 {
		t.Fatalf("PATCH 脏值应 400: %d %s", rec.Code, rec.Body.String())
	}
}

// tMetricPoint / tMetricGroup / metricListResp 解析 GET /metrics 与详情内嵌 metrics 的
// 分组结构（测试局部形状，只取断言所需字段）。
type tMetricPoint struct {
	ValueNum  *float64 `json:"value_num"`
	ValueText string   `json:"value_text"`
	Status    string   `json:"status"`
}

type tMetricGroup struct {
	Key     string         `json:"key"`
	Label   string         `json:"label"`
	Unit    string         `json:"unit"`
	Numeric bool           `json:"numeric"`
	Points  []tMetricPoint `json:"points"`
}

type metricListResp struct {
	Metrics      []tMetricGroup `json:"metrics"`
	PendingCount int            `json:"pending_count"`
}

// findMetricGroup 在分组结果里按 key 找一组（未命中返回 nil），避免依赖 metric_key 的字典序。
func findMetricGroup(r metricListResp, key string) *tMetricGroup {
	for i := range r.Metrics {
		if r.Metrics[i].Key == key {
			return &r.Metrics[i]
		}
	}
	return nil
}

// TestPersonMetricAPI 覆盖 metric 平面（画像第 5 平面）API 全链路：手动加数值测点（weight，
// value_num+unit）与类别测点（diet，value_text）、metric_key 枚举校验、列表分组 + ?metric_key
// 过滤、详情内嵌 metrics 分组（Numeric/points）+ pending 计数、删除转 dismissed、确认队列含
// metric 条目并 HTTP 确认为 active。跨包非自隔离：t.Cleanup 删掉 owner 的 person_metric 行 +
// entity_kind='metric' 审计行，防污染 profile 包同库断言（模式同 TestPersonEventAPI）。
func TestPersonMetricAPI(t *testing.T) {
	h, svc := setupPersonAPI(t)
	ctx := context.Background()
	owner, _ := svc.Persons.GetOwner(ctx, 1)
	base := "/api/persons/" + owner.ID.String()
	t.Cleanup(func() {
		_, _ = svc.DB.ExecContext(context.Background(), "DELETE FROM person_metric WHERE person_id = ?", owner.ID.Int64())
		_, _ = svc.DB.ExecContext(context.Background(), "DELETE FROM person_change_log WHERE person_id = ? AND entity_kind = 'metric'", owner.ID.Int64())
	})

	// 手动加数值测点：weight=70kg（measured_at 纯日期串）
	rec := doReq(t, h, "POST", base+"/metrics",
		map[string]any{"metric_key": "weight", "value_num": 70, "unit": "kg", "measured_at": "2026-08-01"})
	if rec.Code != 200 {
		t.Fatalf("加数值测点失败: %d %s", rec.Code, rec.Body.String())
	}
	var m repo.PersonMetric
	_ = json.Unmarshal(rec.Body.Bytes(), &m)
	if m.Status != "active" || m.Source != "manual" || m.ValueNum == nil || *m.ValueNum != 70 {
		t.Fatalf("手动数值测点错误: %+v", m)
	}

	// 非法 metric_key → 400（目录外键，handler 层校验）
	if rec := doReq(t, h, "POST", base+"/metrics",
		map[string]any{"metric_key": "bogus", "value_num": 1}); rec.Code != 400 {
		t.Fatalf("非法 metric_key 应 400: %d %s", rec.Code, rec.Body.String())
	}
	// measured_at 缺省 → time.Now()（不应 500，Service 不会撞 measured_at 零值）
	rec = doReq(t, h, "POST", base+"/metrics",
		map[string]any{"metric_key": "weight", "value_num": 71})
	if rec.Code != 200 {
		t.Fatalf("measured_at 缺省应 200: %d %s", rec.Code, rec.Body.String())
	}

	// 手动加类别测点：diet=火锅（value_text，Numeric=false）
	rec = doReq(t, h, "POST", base+"/metrics",
		map[string]any{"metric_key": "diet", "value_text": "火锅"})
	if rec.Code != 200 {
		t.Fatalf("加类别测点失败: %d %s", rec.Code, rec.Body.String())
	}

	// GET /metrics 列表：应含 weight 组（Numeric=true，2 个点）与 diet 组（Numeric=false）
	rec = doReq(t, h, "GET", base+"/metrics", nil)
	if rec.Code != 200 {
		t.Fatalf("指标列表失败: %d %s", rec.Code, rec.Body.String())
	}
	var listR metricListResp
	_ = json.Unmarshal(rec.Body.Bytes(), &listR)
	wg := findMetricGroup(listR, "weight")
	if wg == nil || !wg.Numeric || wg.Unit != "kg" || len(wg.Points) != 2 {
		t.Fatalf("weight 组错误: %+v", wg)
	}
	if wg.Points[0].ValueNum == nil || *wg.Points[0].ValueNum != 70 {
		t.Fatalf("weight 首点应 70: %+v", wg.Points[0])
	}
	dg := findMetricGroup(listR, "diet")
	if dg == nil || dg.Numeric || len(dg.Points) != 1 || dg.Points[0].ValueText != "火锅" {
		t.Fatalf("diet 组错误: %+v", dg)
	}

	// ?metric_key=weight 过滤：只剩 weight 组
	rec = doReq(t, h, "GET", base+"/metrics?metric_key=weight", nil)
	var filtered metricListResp
	_ = json.Unmarshal(rec.Body.Bytes(), &filtered)
	if len(filtered.Metrics) != 1 || filtered.Metrics[0].Key != "weight" {
		t.Fatalf("metric_key 过滤应只剩 weight: %+v", filtered.Metrics)
	}

	// 详情内嵌 metrics（同分组结构）：weight 组 Numeric=true、diet 组 point.ValueText=火锅
	rec = doReq(t, h, "GET", base, nil)
	var detail metricListResp
	_ = json.Unmarshal(rec.Body.Bytes(), &detail)
	if g := findMetricGroup(detail, "weight"); g == nil || !g.Numeric {
		t.Fatalf("详情缺 weight 组或 Numeric 错误: %+v", g)
	}
	if g := findMetricGroup(detail, "diet"); g == nil || g.Numeric || len(g.Points) != 1 || g.Points[0].ValueText != "火锅" {
		t.Fatalf("详情 diet 组错误: %+v", g)
	}

	// 删除数值测点 → dismissed（详情不再含 weight 组，diet 仍在）
	if rec := doReq(t, h, "DELETE", base+"/metrics/"+m.ID.String(), nil); rec.Code != 200 {
		t.Fatalf("删除测点失败: %d %s", rec.Code, rec.Body.String())
	}
	if d, _ := svc.Metrics.Get(ctx, m.ID); d == nil || d.Status != "dismissed" {
		t.Fatalf("删除后应 dismissed: %+v", d)
	}
	// 删不存在的测点（合法 id、库中无行）→ 404
	if rec := doReq(t, h, "DELETE", base+"/metrics/"+ids.New().String(), nil); rec.Code != 404 {
		t.Fatalf("删不存在测点应 404: %d %s", rec.Code, rec.Body.String())
	}

	// 造一条 pending 测点（直接走 repo，status=pending），验证进确认队列 + HTTP 确认为 active
	valence := -0.5
	pm := &repo.PersonMetric{
		PersonID: owner.ID, MetricKey: "emotion", ValueNum: &valence,
		MeasuredAt: time.Now(), Confidence: 0.5, EpistemicType: "observed",
		Source: "extract", Status: "pending",
	}
	if err := svc.Metrics.Create(ctx, pm); err != nil {
		t.Fatal(err)
	}
	// 详情 pending 计数应含该测点，且 emotion 组的点 status=pending
	rec = doReq(t, h, "GET", base, nil)
	var detail2 metricListResp
	_ = json.Unmarshal(rec.Body.Bytes(), &detail2)
	if detail2.PendingCount < 1 {
		t.Fatalf("详情 pending 计数应含 metric: %d", detail2.PendingCount)
	}
	if g := findMetricGroup(detail2, "emotion"); g == nil || len(g.Points) != 1 || g.Points[0].Status != "pending" {
		t.Fatalf("详情 emotion 组应含 1 个 pending 点: %+v", g)
	}

	// 确认队列含 metric 条目（kind=metric）
	rec = doReq(t, h, "GET", "/api/profile/pending", nil)
	if rec.Code != 200 {
		t.Fatalf("队列失败: %d %s", rec.Code, rec.Body.String())
	}
	var pend struct {
		Items []map[string]any `json:"items"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &pend)
	var mItemID string
	for _, it := range pend.Items {
		if it["kind"] == "metric" && it["metric_key"] == "emotion" {
			mItemID, _ = it["id"].(string)
		}
	}
	if mItemID == "" {
		t.Fatalf("队列缺 metric 条目: %+v", pend.Items)
	}

	// HTTP 确认 → active
	if rec := doReq(t, h, "POST", "/api/profile/pending/metric/"+mItemID+"/confirm", nil); rec.Code != 200 {
		t.Fatalf("metric 确认失败: %d %s", rec.Code, rec.Body.String())
	}
	if d, _ := svc.Metrics.Get(ctx, pm.ID); d == nil || d.Status != "active" {
		t.Fatalf("确认后应 active: %+v", d)
	}
}

// TestPetAPIFlow 覆盖宠物 API：POST（birthday 必填 400 / 正常 200 / 同名 409）、GET 列表、
// 详情含 pets、PATCH 整只替换、DELETE、?status=dismissed 查回。
func TestPetAPIFlow(t *testing.T) {
	h, svc := setupPersonAPI(t)
	ctx := context.Background()
	oid, _ := svc.Persons.GetOwner(ctx, 1)

	t.Cleanup(func() {
		cctx := context.Background()
		if o, err := svc.Persons.GetOwner(cctx, 1); err == nil && o != nil {
			_, _ = svc.DB.ExecContext(cctx, `DELETE FROM person_pet WHERE person_id = ?`, o.ID.Int64())
			_, _ = svc.DB.ExecContext(cctx, `DELETE FROM person_change_log WHERE person_id = ? AND entity_kind = 'pet'`, o.ID.Int64())
		}
	})

	// birthday 缺失 → 400（handler 校验）。
	rec := doReq(t, h, "POST", "/api/persons/"+oid.ID.String()+"/pets",
		map[string]any{"name": "小花", "species": "猫"})
	if rec.Code != 400 {
		t.Fatalf("birthday 缺失应 400: %d %s", rec.Code, rec.Body.String())
	}
	// 正常新增。
	rec = doReq(t, h, "POST", "/api/persons/"+oid.ID.String()+"/pets",
		map[string]any{"name": "小花", "nickname": "花花", "species": "猫", "breed": "布偶",
			"gender": "母", "age_text": "3岁", "birthday": "2023-04-01", "likes": "不吃鱼"})
	if rec.Code != 200 {
		t.Fatalf("新增宠物失败: %d %s", rec.Code, rec.Body.String())
	}
	var pet struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &pet)
	if pet.Status != "active" {
		t.Fatalf("手动新增应 active: %+v", pet)
	}
	// 同名再增 → 409。
	rec = doReq(t, h, "POST", "/api/persons/"+oid.ID.String()+"/pets",
		map[string]any{"name": "小花", "species": "猫", "birthday": "2023-04-01"})
	if rec.Code != 409 {
		t.Fatalf("同名新增应 409: %d %s", rec.Code, rec.Body.String())
	}

	// GET 列表。
	rec = doReq(t, h, "GET", "/api/persons/"+oid.ID.String()+"/pets", nil)
	if rec.Code != 200 {
		t.Fatalf("列表失败: %d %s", rec.Code, rec.Body.String())
	}
	var listR struct {
		Pets []repo.PersonPet `json:"pets"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &listR)
	if len(listR.Pets) != 1 || listR.Pets[0].Name != "小花" {
		t.Fatalf("列表应 1 只小花: %+v", listR.Pets)
	}

	// 详情含 pets。
	rec = doReq(t, h, "GET", "/api/persons/"+oid.ID.String(), nil)
	if rec.Code != 200 {
		t.Fatalf("详情失败: %d", rec.Code)
	}
	var det struct {
		Pets []repo.PersonPet `json:"pets"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &det)
	if len(det.Pets) != 1 {
		t.Fatalf("详情应含 1 只宠物: %+v", det.Pets)
	}

	// PATCH 整只替换（改 likes）。
	rec = doReq(t, h, "PATCH", "/api/persons/"+oid.ID.String()+"/pets/"+pet.ID,
		map[string]any{"name": "小花", "species": "猫", "breed": "布偶",
			"birthday": "2023-04-01", "likes": "爱吃鱼罐头"})
	if rec.Code != 200 {
		t.Fatalf("编辑失败: %d %s", rec.Code, rec.Body.String())
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &pet) // PATCH 返回新行（新 ID），后续 DELETE 用它
	rec = doReq(t, h, "GET", "/api/persons/"+oid.ID.String()+"/pets", nil)
	_ = json.Unmarshal(rec.Body.Bytes(), &listR)
	if len(listR.Pets) != 1 || listR.Pets[0].Likes == nil || *listR.Pets[0].Likes != "爱吃鱼罐头" {
		t.Fatalf("编辑后列表应只剩 1 只 active 且 likes 更新: %+v", listR.Pets)
	}

	// DELETE → dismissed（默认列表不再返回）。
	rec = doReq(t, h, "DELETE", "/api/persons/"+oid.ID.String()+"/pets/"+pet.ID, nil)
	if rec.Code != 200 {
		t.Fatalf("删除失败: %d %s", rec.Code, rec.Body.String())
	}
	rec = doReq(t, h, "GET", "/api/persons/"+oid.ID.String()+"/pets", nil)
	_ = json.Unmarshal(rec.Body.Bytes(), &listR)
	if len(listR.Pets) != 0 {
		t.Fatalf("删除后默认列表应为空: %+v", listR.Pets)
	}
	// ?status=dismissed 可查回（对齐 ListCycles）。
	rec = doReq(t, h, "GET", "/api/persons/"+oid.ID.String()+"/pets?status=dismissed", nil)
	_ = json.Unmarshal(rec.Body.Bytes(), &listR)
	if len(listR.Pets) != 1 {
		t.Fatalf("dismissed 过滤应查回 1 行: %+v", listR.Pets)
	}
}
