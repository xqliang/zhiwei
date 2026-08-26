package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"zhiwei/internal/ids"
	"zhiwei/internal/repo"
)

// TestPersonAPIUserIsolation 锁定 person/画像 REST 的多租户隔离（阶段 2A）：
//   - 读隔离：user1 的 GET /api/persons 不含 user2 的 person；GET 详情越权 → 404；
//     GET /api/profile/pending 不含 user2 的 pending。
//   - 写隔离（子表 IDOR）：user1 对 user2 的属性行 PatchAttribute/DeleteAttribute、
//     对 user2 的 pending ConfirmPending → 全部 404，且 user2 的行不受影响。
//
// setupPersonAPI 的路由经 newAuthedRouter 注入 user1；user2 的数据直接走 Service/repo
// 造（UserID=2）。共享库非自隔离：t.Cleanup 精确删除本用例造的 user2 行与 user1 person。
func TestPersonAPIUserIsolation(t *testing.T) {
	h, svc := setupPersonAPI(t) // 路由注入登录用户 1
	ctx := context.Background()
	const u2 = int64(2)

	// user2 侧：owner + 一个 person + 一条 active 属性 + 一条 pending 属性（均 UserID=2）。
	if err := repo.EnsureOwnerForUser(ctx, svc.Persons, u2); err != nil {
		t.Fatal(err)
	}
	p2, err := svc.ManualCreatePerson(ctx, u2, "ISO_u2_张三", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	a2, err := svc.ManualAddAttribute(ctx, u2, p2.ID, "city", "U2城市")
	if err != nil {
		t.Fatal(err)
	}
	pend2 := &repo.PersonAttribute{
		UserID: u2, PersonID: p2.ID, AttrKey: "personality", ValueText: "U2内向",
		ValueType: "text", Confidence: 0.5, EpistemicType: "observed", Source: "extract", Status: "pending",
	}
	if err := svc.Attributes.Create(ctx, pend2); err != nil {
		t.Fatal(err)
	}

	// user1 侧：经 HTTP 建一个 person（注入 uid=1 → UserID=1）。
	rec := doReq(t, h, "POST", "/api/persons", map[string]any{"display_name": "ISO_u1_李四"})
	if rec.Code != 200 {
		t.Fatalf("user1 建 person 失败: %d %s", rec.Code, rec.Body.String())
	}
	var p1 repo.Person
	_ = json.Unmarshal(rec.Body.Bytes(), &p1)

	t.Cleanup(func() {
		cctx := context.Background()
		_, _ = svc.DB.ExecContext(cctx, `DELETE FROM person_attribute WHERE person_id = ?`, p2.ID.Int64())
		_, _ = svc.DB.ExecContext(cctx, `DELETE FROM person_change_log WHERE person_id IN (?, ?)`, p2.ID.Int64(), p1.ID.Int64())
		_, _ = svc.DB.ExecContext(cctx, `DELETE FROM person WHERE user_id = 2`)
		_, _ = svc.DB.ExecContext(cctx, `DELETE FROM person WHERE id = ?`, p1.ID.Int64())
	})

	// ---- 读隔离 ----
	// GET /api/persons：含 user1 的 p1，不含 user2 的 p2。
	rec = doReq(t, h, "GET", "/api/persons", nil)
	if rec.Code != 200 {
		t.Fatalf("名册失败: %d %s", rec.Code, rec.Body.String())
	}
	var listR struct {
		Persons []repo.PersonWithPending `json:"persons"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &listR)
	have := map[ids.ID]bool{}
	for _, p := range listR.Persons {
		have[p.ID] = true
	}
	if !have[p1.ID] {
		t.Fatalf("user1 名册应含自己的 person %d", p1.ID)
	}
	if have[p2.ID] {
		t.Fatalf("越权：user1 名册不应含 user2 的 person %d", p2.ID)
	}

	// GET 详情 user2 的 person → 404。
	if rec := doReq(t, h, "GET", "/api/persons/"+p2.ID.String(), nil); rec.Code != http.StatusNotFound {
		t.Fatalf("user1 读 user2 person 详情应 404, got %d %s", rec.Code, rec.Body.String())
	}

	// GET /api/profile/pending：不含 user2 的 pending 属性。
	rec = doReq(t, h, "GET", "/api/profile/pending", nil)
	if rec.Code != 200 {
		t.Fatalf("确认队列失败: %d %s", rec.Code, rec.Body.String())
	}
	var pend struct {
		Items []map[string]any `json:"items"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &pend)
	for _, it := range pend.Items {
		if id, _ := it["id"].(string); id == pend2.ID.String() {
			t.Fatalf("越权：user1 确认队列不应含 user2 的 pending %s", pend2.ID)
		}
	}

	// ---- 写隔离（子表 IDOR）：全部 404，且 user2 行不受影响 ----
	base2 := "/api/persons/" + p2.ID.String()
	// PatchAttribute user2 的属性行 → 404。
	if rec := doReq(t, h, "PATCH", base2+"/attributes/"+a2.ID.String(),
		map[string]any{"attr_key": "city", "value": "篡改值"}); rec.Code != http.StatusNotFound {
		t.Fatalf("user1 改 user2 属性应 404, got %d %s", rec.Code, rec.Body.String())
	}
	// DeleteAttribute user2 的属性行 → 404。
	if rec := doReq(t, h, "DELETE", base2+"/attributes/"+a2.ID.String(), nil); rec.Code != http.StatusNotFound {
		t.Fatalf("user1 删 user2 属性应 404, got %d %s", rec.Code, rec.Body.String())
	}
	// user2 的 active 属性未被越权修改/删除。
	if got, _ := svc.Attributes.Get(ctx, a2.ID); got == nil || got.Status != "active" || got.ValueText != "U2城市" {
		t.Fatalf("user2 属性不应被越权改动: %+v", got)
	}

	// ConfirmPending user2 的 pending → 404。
	if rec := doReq(t, h, "POST", "/api/profile/pending/attribute/"+pend2.ID.String()+"/confirm", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("user1 确认 user2 pending 应 404, got %d %s", rec.Code, rec.Body.String())
	}
	// user2 的 pending 仍为 pending（未被越权确认为 active）。
	if got, _ := svc.Attributes.Get(ctx, pend2.ID); got == nil || got.Status != "pending" {
		t.Fatalf("user2 pending 不应被越权确认: %+v", got)
	}
}
