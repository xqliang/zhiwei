# 声纹删/合并 → 关联人物级联处理 实现计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 说话人被删/合并时，其关联人物未编辑过则静默 dismiss，编辑过则返回提示由用户确认，owner 永级联删除。

**Architecture:** 新增 `internal/api/speaker_cascade.go`（`personEditedByUser` 判定 + `cascadePersonOnSpeakerRemoval` 处理），挂到 `SpeakerHandler.Delete` 和 `Merge`。新增 `ChangeLogs` 依赖字段。复用 `ManualSetPersonStatus` dismiss 语义，无新迁移。

**Tech Stack:** Go + chi + sqlx；现有 person_profile service。

**规格：** `docs/superpowers/specs/2026-08-31-speaker-cascade-person-design.md`

---

### Task 1: repo 层 — personEditedByUser 判定（纯函数，可测）

**Files:**
- Modify: `internal/api/speaker_cascade.go`（新建）
- Test: `internal/api/speaker_cascade_test.go`

- [ ] **Step 1: 写失败测试**

`internal/api/speaker_cascade_test.go`：
```go
package api

import (
	"context"
	"testing"

	"zhiwei/internal/ids"
	"zhiwei/internal/repo"
	"zhiwei/internal/repotest"
)

func newCascadeTestDB(t *testing.T) *repo.PersonChangeLogRepo {
	t.Helper()
	db, err := repo.NewDB(repotest.DSN(t))
	if err != nil {
		t.Fatal(err)
	}
	return &repo.PersonChangeLogRepo{DB: db}
}

// personEditedByUser: 有 changed_by='user' 且非 create → true。
func TestPersonEditedByUser(t *testing.T) {
	repo := newCascadeTestDB(t)
	ctx := t.Context()
	pid := ids.New()

	// 场景1: 只有 user create → false
	repo.Create(ctx, &repo.PersonChangeLog{PersonID: pid, EntityKind: "person", ChangeType: "create", ChangedBy: "user"})
	if personEditedByUser(ctx, repo, pid) {
		t.Error("仅 user create 应为 false")
	}

	// 场景2: 有 user update → true
	repo.Create(ctx, &repo.PersonChangeLog{PersonID: pid, EntityKind: "attribute", ChangeType: "update", ChangedBy: "user"})
	if !personEditedByUser(ctx, repo, pid) {
		t.Error("有 user update 应为 true")
	}
}

// personEditedByUser: 只有 llm 记录 → false。
func TestPersonEditedByUserOnlyLLM(t *testing.T) {
	repo := newCascadeTestDB(t)
	ctx := t.Context()
	pid := ids.New()
	repo.Create(ctx, &repo.PersonChangeLog{PersonID: pid, EntityKind: "person", ChangeType: "create", ChangedBy: "llm"})
	repo.Create(ctx, &repo.PersonChangeLog{PersonID: pid, EntityKind: "attribute", ChangeType: "update", ChangedBy: "llm"})
	if personEditedByUser(ctx, repo, pid) {
		t.Error("仅 llm 应为 false")
	}
}

// personEditedByUser: 查询失败（不存在的表/DB错）→ true（保守）。
func TestPersonEditedByUserQueryError(t *testing.T) {
	db, _ := repo.NewDB(repotest.DSN(t))
	// 用一个空 DB 连接模拟查询失败
	db.Close()
	repo := &repo.PersonChangeLogRepo{DB: db}
	if !personEditedByUser(ctx, repo, ids.New()) {
		t.Error("查询失败应为 true（保守）")
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/api/ -run TestPersonEditedByUser -v -count=1`
Expected: FAIL — `personEditedByUser` 未定义。

- [ ] **Step 3: 实现 personEditedByUser**

`internal/api/speaker_cascade.go`：
```go
package api

import (
	"context"
	"log"

	"zhiwei/internal/ids"
	"zhiwei/internal/repo"
)

// personEditedByUser 判断人物是否被手工编辑过（宽口径：含属性/事件/关系的 user 变更）。
// 依据 person_change_log.changed_by='user' 且非仅 create。
// 查询失败时保守返回 true（不静默删）。
func personEditedByUser(ctx context.Context, repo *repo.PersonChangeLogRepo, personID ids.ID) bool {
	if repo == nil {
		return false // 无审计 repo，视为未编辑（静默删）
	}
	logs, err := repo.ListByPerson(ctx, personID, "", "")
	if err != nil {
		return true // 查询失败保守视为已编辑
	}
	for _, l := range logs {
		if l.ChangedBy == "user" && l.ChangeType != "create" {
			return true
		}
	}
	return false
}

// personCascadePrompt 需用户确认的人物摘要。
type personCascadePrompt struct {
	PersonID string `json:"person_id"`
	Name     string `json:"name"`
	Reason   string `json:"reason"`
}

// cascadePersonOnSpeakerRemoval 处理声纹被删/合并移除后其关联人物的去向。
// 无关联人物 / owner 人物：跳过。
// 未编辑过：autoDismiss=true 时静默 dismiss。
// 编辑过：不删，返回提示列表。
func cascadePersonOnSpeakerRemoval(ctx context.Context, h *SpeakerHandler, uid int64, speakerID ids.ID, autoDismiss bool) []personCascadePrompt {
	if h.Persons == nil {
		return nil
	}
	p, err := h.Persons.GetBySpeaker(ctx, speakerID)
	if err != nil || p == nil || p.IsOwner {
		return nil // 无关联 / owner：跳过
	}
	if personEditedByUser(ctx, h.ChangeLogs, p.ID) {
		return []personCascadePrompt{{PersonID: p.ID.String(), Name: p.DisplayName, Reason: "该人物被手工编辑过"}}
	}
	if autoDismiss && h.Service != nil {
		if err := h.Service.ManualSetPersonStatus(ctx, uid, p.ID, "dismissed"); err != nil {
			log.Printf("[speaker-cascade] dismiss 人物失败 person=%s: %v", p.ID, err)
		}
	}
	return nil
}

var _ = context.Background
```

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/api/ -run TestPersonEditedByUser -v -count=1`
Expected: PASS。

- [ ] **Step 5: 提交**

```bash
git add internal/api/speaker_cascade.go internal/api/speaker_cascade_test.go
git commit -m "feat(api): speaker_cascade 人物编辑判定 + 级联处理逻辑"
```

---

### Task 2: handler 层 — Delete/Merge 挂载 + ChangeLogs 字段

**Files:**
- Modify: `internal/api/speaker.go`（Delete 和 Merge 末尾）
- Modify: `cmd/zhiwei-server/main.go`（SpeakerHandler 加 ChangeLogs）

- [ ] **Step 1: 写失败测试**

`internal/api/speaker_test.go` 追加：
```go
func TestDeleteSpeakerCascadeUnedited(t *testing.T) {
	db, _ := repo.NewDB(repotest.DSN(t))
	ctx := t.Context()
	ids.InitForTest()
	speakers := &repo.SpeakerRepo{DB: db}
	persons := &repo.PersonRepo{DB: db}
	changeLogs := &repo.PersonChangeLogRepo{DB: db}
	sp := &repo.Speaker{Name: "sp1", Source: "auto"}
	speakers.Create(ctx, sp)
	p := &repo.Person{UserID: 1, DisplayName: "sp1", SpeakerID: &sp.ID, Source: "auto"}
	persons.Create(ctx, p)

	h := &SpeakerHandler{Speakers: speakers, Persons: persons, ChangeLogs: changeLogs, Service: profileSvc}
	r := chi.NewRouter()
	RegisterSpeaker(r, h)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/api/speakers/"+sp.ID.String(), nil))
	if rec.Code != 204 {
		t.Fatalf("code=%d", rec.Code)
	}
	got, _ := persons.Get(ctx, 1, p.ID)
	if got == nil || got.Status != "dismissed" {
		t.Errorf("未编辑人物应被 dismiss, got %v", got)
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/api/ -run TestDeleteSpeakerCascadeUnedited -v -count=1`
Expected: FAIL — `ChangeLogs` 字段不存在，或人物未被 dismiss。

- [ ] **Step 3: SpeakerHandler 加 ChangeLogs 字段 + 挂载 Delete/Merge**

`internal/api/speaker.go`：

(a) `SpeakerHandler` struct 加字段：
```go
ChangeLogs *repo.PersonChangeLogRepo // 人物编辑判定（nil=降级：视为未编辑，静默删）
```

(b) `Delete` 函数末尾（`w.WriteHeader(http.StatusNoContent)` 之前）加：
```go
// 级联处理关联人物
if prompts := cascadePersonOnSpeakerRemoval(r.Context(), h, 1, id, true); len(prompts) > 0 {
	// 编辑过的人物：不删，返回提示
	writeJSON(w, map[string]any{"cascade_prompts": prompts})
	return
}
w.WriteHeader(http.StatusNoContent)
```

(c) `Merge` 函数末尾（`writeJSON(w, map[string]any{"ok": true, ...})` 之前）加：
```go
// 级联处理每个源声纹的关联人物
var cascadePrompts []personCascadePrompt
for _, sid := range srcIDs {
	if prompts := cascadePersonOnSpeakerRemoval(r.Context(), h, 1, sid, true); len(prompts) > 0 {
		cascadePrompts = append(cascadePrompts, prompts...)
	}
}
writeJSON(w, map[string]any{"ok": true, "merged_segments": merged, "removed_speakers": len(srcIDs), "cascade_prompts": cascadePrompts})
```

(d) `cmd/zhiwei-server/main.go` SpeakerHandler 字面量加 `ChangeLogs: personLogs,`。

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/api/ -run TestDeleteSpeakerCascadeUnedited -v -count=1`
Expected: PASS。

- [ ] **Step 5: 提交**

```bash
git add internal/api/speaker.go internal/api/speaker_test.go cmd/zhiwei-server/main.go
git commit -m "feat(api): Delete/Merge 挂载人物级联处理 + ChangeLogs 依赖"
```

---

### Task 3: 前端 — 删除/合并响应提示

**Files:**
- Modify: `web/app.js`（处理 cascade_prompts）
- Modify: `web/index.html`（如需要）

- [ ] **Step 1: app.js 处理 cascade_prompts**

删除声纹响应处理：
```js
// 删除声纹响应：若有 cascade_prompts，提示用户确认删除关联人物
const res = await api('DELETE', '/api/speakers/' + id);
if (res && res.cascade_prompts && res.cascade_prompts.length > 0) {
  const names = res.cascade_prompts.map(p => p.name).join('、');
  if (confirm('声纹已删除。其关联人物「' + names + '」曾被手工编辑，是否一并删除？')) {
    for (const p of res.cascade_prompts) {
      await api('PATCH', '/api/persons/' + p.person_id, { status: 'dismissed' });
    }
    notify('关联人物已删除');
  }
}
```

合并声纹响应处理类似。

- [ ] **Step 2: 提交**

```bash
git add web/app.js
git commit -m "feat(web): 删除/合并声纹响应处理 cascade_prompts 提示"
```

---

## Self-Review 结果

**Spec 覆盖：**
- §3 级联逻辑（`personEditedByUser` + `cascadePersonOnSpeakerRemoval`）→ Task 1
- §3 挂载点（Delete/Merge）→ Task 2
- §3 SpeakerHandler 新字段（ChangeLogs）→ Task 2
- §6 前端提示 → Task 3
- §7 测试（repo/handler/merge）→ Task 1/2

**关键正确性点：**
- `personEditedByUser` 查询失败保守返回 true（不静默删）
- owner 人物硬保护（`p.IsOwner` 跳过）
- 未编辑人物静默 dismiss（可恢复软删）
- 编辑过人物返回 `cascade_prompts` 由前端提示
- 无新迁移

**类型一致性：**
- `personCascadePrompt` 结构体在 Task 1 定义，Task 2/3 使用
- `cascadePersonOnSpeakerRemoval` 签名一致
- `ChangeLogs` 字段在 Task 2 定义
