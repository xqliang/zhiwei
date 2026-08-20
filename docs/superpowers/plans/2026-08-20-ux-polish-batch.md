# UX 打磨批次 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: 用 superpowers:subagent-driven-development 逐任务实现。Steps 用 `- [ ]`。前端任务共享 `web/app.js`/`web/index.html`，**必须串行**（不并行 implementer）。

**Goal:** F1 手动合并主题 + F2 记忆 inplace 编辑 + F3 待办编辑/删除 + F4 topic 删除 + F5 时间线卡片增强(ASR+计数)+删除时间线。

**Architecture:** 见 `docs/superpowers/specs/2026-08-20-ux-polish-batch-design.md`。共享模式：inplace 编辑（点→输入→PATCH→reload，取消还原）、2 步行内删除确认（按钮→`确认删除?`+`取消`→再点→DELETE→reload，不用 `confirm()`）、删除=硬删除（单事务级联，区别于既有 `忽略`/dismiss）。F2/F1 纯前端；F3/F4 后端加 DELETE+扩展 PATCH + 前端；F5 后端 ListSessions 富化 + DELETE session 级联 + 前端。

**Tech Stack:** Go+chi+sqlx+MySQL、Vue 3 CDN。后端 TDD：`make init-testdb && TEST_MYSQL_DSN="zhiwei:zhiwei@tcp(127.0.0.1:3307)/zhiwei_test?parseTime=true&charset=utf8mb4&multiStatements=true" go test -p 1 -run <Test> ./internal/api -v`。前端：`node --check web/app.js` + `make hash-web`（`web/app.*.js` gitignored，**不提交**，只提交 `web/app.js`+`web/index.html`）。

**关键约定（Vue 模板）：** 模板里 ref 自动解包——写 `detail.session.id`/`topicDetail.topic.id`，**不要**写 `.value`（`.value` 只在 `app.js` 的 JS 代码里用）。

**DAG:** T1(F2 前端)→T2(F1 前端)→T3(F3 后端+前端)→T4(F4 后端+前端)→T5(F5 后端)→T6(F5 前端)。T2 的 topic 卡改是 T4 的 topic 卡改的「前置态」——T4 锚定 T2 产出的标记；T1/T6 共改 timeline return 行、T2/T4 共改 topics return 行，后改者锚定前改者产出。

---

## Task 1 (F2): 记忆内容 inplace 编辑（纯前端）

**Files:** Modify `web/app.js`、`web/index.html`。复用 `PATCH /api/memories/{id} {title, content}`（已存在，改 content→version+1）。

- [ ] **Step 1: app.js 加 memory inplace 编辑** — 在 `dismissMemory` 函数后（约 `web/app.js:93`）插入：

```js
    // ---------- 记忆 inplace 编辑（复用 PATCH /api/memories/{id}） ----------
    // editingMem = {id, title, content}；点 title/content 进入编辑、保存 PATCH、取消还原。
    const editingMem = ref(null);
    function startEditMemory(m) { editingMem.value = { id: m.id, title: m.title, content: m.content }; }
    function cancelEditMemory() { editingMem.value = null; }
    async function saveEditMemory(reload) {
      const e = editingMem.value;
      if (!e || !e.title.trim()) return; // 空 title 不发
      try {
        await api('PATCH', '/api/memories/' + e.id, { title: e.title.trim(), content: e.content });
        editingMem.value = null;
        if (reload) await reload();
      } catch (e2) { showError(e2); }
    }
```

- [ ] **Step 2: index.html 时间线 memory 卡可编辑** — 把 `web/index.html:112-120` 的 memory 卡 `.kv` + content 行替换为：

旧：
```html
            <div class="kv">
              <div>
                <span class="type-tag" :style="{background: typeMeta(m.type).color}">{{ typeMeta(m.type).label }}</span>
                <b>{{ m.title }}</b>
              </div>
              <button class="mini" @click="dismissMemory(m)" title="忽略此记忆">✕</button>
            </div>
            <div style="margin:6px 0">{{ m.content }}</div>
```
新：
```html
            <div class="kv">
              <div>
                <span class="type-tag" :style="{background: typeMeta(m.type).color}">{{ typeMeta(m.type).label }}</span>
                <b v-if="!editingMem || editingMem.id !== m.id" @click="startEditMemory(m)" style="cursor:text">{{ m.title }}</b>
                <input v-else class="txt" v-model="editingMem.title" style="display:inline-block;width:auto">
              </div>
              <button class="mini" @click="dismissMemory(m)" title="忽略此记忆">✕</button>
            </div>
            <div v-if="editingMem && editingMem.id === m.id">
              <textarea class="txt" v-model="editingMem.content" rows="3" style="margin:6px 0"></textarea>
              <div style="display:flex; gap:8px; margin-bottom:6px">
                <button class="primary" style="padding:4px 12px" @click="saveEditMemory(() => reloadSession(detail.session.id))">保存</button>
                <button class="mini" @click="cancelEditMemory()">取消</button>
              </div>
            </div>
            <div v-else style="margin:6px 0">{{ m.content }}</div>
```

> 模板里 `detail.session.id`（ref 自动解包，不带 `.value`）。展开态下 `detail` 必有 `session`。

- [ ] **Step 3: index.html topic 详情 memory 卡可编辑** — 把 `web/index.html:235-239` 的 topic 详情 memory 卡 title+content 行替换为：

旧：
```html
        <div>
          <span class="type-tag" :style="{background: typeMeta(m.type).color}">{{ typeMeta(m.type).label }}</span>
          <b>{{ m.title }}</b>
        </div>
        <div style="margin:6px 0">{{ m.content }}</div>
```
新：
```html
        <div>
          <span class="type-tag" :style="{background: typeMeta(m.type).color}">{{ typeMeta(m.type).label }}</span>
          <b v-if="!editingMem || editingMem.id !== m.id" @click="startEditMemory(m)" style="cursor:text">{{ m.title }}</b>
          <input v-else class="txt" v-model="editingMem.title" style="display:inline-block;width:auto">
        </div>
        <div v-if="editingMem && editingMem.id === m.id">
          <textarea class="txt" v-model="editingMem.content" rows="3" style="margin:6px 0"></textarea>
          <div style="display:flex; gap:8px; margin-bottom:6px">
            <button class="primary" style="padding:4px 12px" @click="saveEditMemory(() => openTopic(topicDetail.topic.id))">保存</button>
            <button class="mini" @click="cancelEditMemory()">取消</button>
          </div>
        </div>
        <div v-else style="margin:6px 0">{{ m.content }}</div>
```

- [ ] **Step 4: app.js return 暴露** — return 块的 timeline 行（`web/app.js:358`）末尾追加 `editingMem, startEditMemory, cancelEditMemory, saveEditMemory,`：

旧：`sessions, detail, expandedId, loadSessions, toggleSession, reloadSession, audioUrl, dismissMemory, retryJob,`
新：`sessions, detail, expandedId, loadSessions, toggleSession, reloadSession, audioUrl, dismissMemory, retryJob, editingMem, startEditMemory, cancelEditMemory, saveEditMemory,`

- [ ] **Step 5: 验证** — `node --check web/app.js`；`make hash-web`。

- [ ] **Step 6: 提交** — `git add web/app.js web/index.html && git commit -m "feat(web): 记忆内容 inplace 编辑(title+content, 复用 PATCH /api/memories)"`

---

## Task 2 (F1): 手动合并主题（纯前端）

**Files:** Modify `web/app.js`、`web/index.html`。复用 `POST /api/topics/merge {groups:[{canonical_name, member_ids}]}`。

- [ ] **Step 1: app.js 加手动合并** — 在 `applyMerge` 函数后（约 `web/app.js:289`）插入：

```js
    // ---------- 手动合并 topic（选多个→输新名→复用 /api/topics/merge） ----------
    // manualMergeMode=选择模式；manualSelected=勾选 id；manualConfirming=已点开始合并→输名。
    const manualMergeMode = ref(false);
    const manualSelected = ref([]);
    const manualMergeName = ref('');
    const manualConfirming = ref(false);
    function startManualMerge() {
      manualMergeMode.value = true; manualSelected.value = []; manualConfirming.value = false; manualMergeName.value = '';
    }
    function cancelManualMerge() {
      manualMergeMode.value = false; manualSelected.value = []; manualConfirming.value = false; manualMergeName.value = '';
    }
    function toggleManualSelect(t) {
      const i = manualSelected.value.indexOf(t.id);
      if (i >= 0) manualSelected.value.splice(i, 1); else manualSelected.value.push(t.id);
    }
    async function applyManualMerge() {
      const ids = manualSelected.value.slice();
      if (ids.length < 2) { toast.value = '至少选 2 个主题'; return; }
      const name = manualMergeName.value.trim();
      if (!name) { toast.value = '请输入规范名'; return; }
      try {
        await api('POST', '/api/topics/merge', { groups: [{ canonical_name: name, member_ids: ids }] });
        cancelManualMerge();
        await loadTopics();
      } catch (e) { showError(e); }
    }
```

- [ ] **Step 2: index.html Topics 列表加「手动合并」按钮** — `web/index.html:175` 智能合并按钮后加：

旧：`<button class="mini" @click="startConsolidate">智能合并</button>`
新：
```html
          <button class="mini" @click="startConsolidate">智能合并</button>
          <button class="mini" @click="startManualMerge">手动合并</button>
```

- [ ] **Step 3: index.html topic 卡改选择模式** — 把 `web/index.html:199-212` 的 topic 卡整块替换为：

旧：
```html
      <div class="card" v-for="t in topics" :key="t.id">
        <div class="kv">
          <div style="cursor:pointer" @click="openTopic(t.id)">
            <b>{{ t.name }}</b>
            <span v-if="t.status==='suggested'" class="badge suggested">待确认</span>
            <span v-if="suspectOf(t, topics)" class="badge" style="background:#fef3c7;color:#92400e">疑似可合并: {{ suspectOf(t, topics) }}</span>
            <div class="muted">{{ t.memory_count }} 条记忆 · {{ t.open_todo_count }} 个进行中待办</div>
          </div>
          <div style="display:flex; gap:6px">
            <button class="mini" v-if="t.status==='suggested'" @click="confirmTopic(t)">确认</button>
            <button class="mini" @click="dismissTopic(t)">忽略</button>
          </div>
        </div>
      </div>
```
新：
```html
      <div class="card" v-for="t in topics" :key="t.id">
        <div class="kv">
          <div style="display:flex; align-items:center; gap:8px">
            <input v-if="manualMergeMode" type="checkbox" :checked="manualSelected.includes(t.id)" @change="toggleManualSelect(t)">
            <div style="cursor:pointer" @click="!manualMergeMode && openTopic(t.id)">
              <b>{{ t.name }}</b>
              <span v-if="t.status==='suggested'" class="badge suggested">待确认</span>
              <span v-if="suspectOf(t, topics)" class="badge" style="background:#fef3c7;color:#92400e">疑似可合并: {{ suspectOf(t, topics) }}</span>
              <div class="muted">{{ t.memory_count }} 条记忆 · {{ t.open_todo_count }} 个进行中待办</div>
            </div>
          </div>
          <div v-if="!manualMergeMode" style="display:flex; gap:6px">
            <button class="mini" v-if="t.status==='suggested'" @click="confirmTopic(t)">确认</button>
            <button class="mini" @click="dismissTopic(t)">忽略</button>
          </div>
        </div>
      </div>
```

> 选择模式下操作区（确认/忽略）隐藏，只显示勾选框；非选择模式才显示操作区（T4 将在此区加删除）。

- [ ] **Step 4: index.html 加手动合并底部条** — 在 topic 列表 `v-for` 之后、列表视图 `</template>`（`web/index.html:213`）之前插入：

```html
      <div class="card" v-if="manualMergeMode" style="position:sticky; bottom:0">
        <div class="kv">
          <div class="muted">已选 {{ manualSelected.length }} 个主题</div>
          <div style="display:flex; gap:8px">
            <button v-if="!manualConfirming" class="primary" style="padding:6px 14px" :disabled="manualSelected.length < 2" @click="manualConfirming = true; manualMergeName = (topics.find(t => t.id === manualSelected[0]) || {}).name || ''">开始合并</button>
            <template v-else>
              <input class="txt" v-model="manualMergeName" placeholder="规范名（必填）" style="width:auto">
              <button class="primary" style="padding:6px 14px" @click="applyManualMerge">确认合并</button>
            </template>
            <button class="mini" @click="cancelManualMerge">取消</button>
          </div>
        </div>
      </div>
```

- [ ] **Step 5: app.js return 暴露** — return 块的 topics 行（`web/app.js:361`）当前是：
`loadTopics, openTopic, closeTopicDetail, confirmTopic, dismissTopic, startRename, commitRename, createTopic, suspectOf, mergeDraft, startConsolidate, toggleMergeMember, applyMerge,`
末尾追加 `manualMergeMode, manualSelected, manualMergeName, manualConfirming, startManualMerge, cancelManualMerge, toggleManualSelect, applyManualMerge,`

- [ ] **Step 6: 验证** — `node --check web/app.js`；`make hash-web`。

- [ ] **Step 7: 提交** — `git add web/app.js web/index.html && git commit -m "feat(web): topic 手动合并(选多个+输新名, 复用 /api/topics/merge)"`

---

## Task 3 (F3): 待办编辑 + 删除（后端 + 前端）

**Files:** Modify `internal/repo/todo.go`、`internal/api/todo.go`、`internal/api/todo_test.go`、`web/app.js`、`web/index.html`

### 后端

- [ ] **Step 1: 写失败测试** — `internal/api/todo_test.go` 末尾追加（`setupTodoAPI(t)` 返回 `(http.Handler, *repo.TodoRepo, *repo.Todo)`，三值；td 已含 ID/Status/Title）：

```go
// TestTodoEditTitle 验证 PATCH title（无 status）改名成功、状态不变。
func TestTodoEditTitle(t *testing.T) {
	r, tr, td := setupTodoAPI(t)
	ctx := context.Background()
	newTitle := "改名后的待办-" + td.ID.String()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPatch, "/api/todos/"+td.ID.String(),
		strings.NewReader(`{"title":"`+newTitle+`"}`))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("edit title: %d %s", rec.Code, rec.Body.String())
	}
	got, _ := tr.Get(ctx, td.ID)
	if got.Title != newTitle {
		t.Fatalf("title=%s, want %s", got.Title, newTitle)
	}
	if got.Status != td.Status { // title 改动不应碰状态
		t.Fatalf("status changed: %s -> %s", td.Status, got.Status)
	}
}

// TestTodoDelete 验证 DELETE 硬删 todo（不存在也不报错→204），重复删除幂等。
func TestTodoDelete(t *testing.T) {
	r, tr, td := setupTodoAPI(t)
	ctx := context.Background()
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/api/todos/"+td.ID.String(), nil))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete: %d", rec.Code)
	}
	if _, err := tr.Get(ctx, td.ID); err == nil {
		t.Fatal("todo 仍存在")
	}
	// 重复删除幂等（204）
	rec2 := httptest.NewRecorder()
	r.ServeHTTP(rec2, httptest.NewRequest(http.MethodDelete, "/api/todos/"+td.ID.String(), nil))
	if rec2.Code != http.StatusNoContent {
		t.Fatalf("idempotent delete: %d", rec2.Code)
	}
}
```

> `todo_test.go` 已 import `context/net/http/net/http/httptest/strings/testing`，无需新增。

- [ ] **Step 2: 跑确认失败** — `make init-testdb && TEST_MYSQL_DSN="zhiwei:zhiwei@tcp(127.0.0.1:3307)/zhiwei_test?parseTime=true&charset=utf8mb4&multiStatements=true" go test -p 1 -run 'TestTodoEditTitle|TestTodoDelete' ./internal/api -v` → FAIL（Patch 不接受 title；无 DELETE 路由）。

- [ ] **Step 3: repo 加 UpdateTitle + Delete** — `internal/repo/todo.go` 在 `UpdateStatus` 后（约 `:94`）追加：

```go
// UpdateTitle 改待办标题（用户手改）。不做状态校验；状态流转走 UpdateStatus。
// 「不存在」返回 nil（UPDATE 0 行，与 UpdateStatus 同语义）。
func (r *TodoRepo) UpdateTitle(ctx context.Context, id ids.ID, title string) error {
	_, err := r.DB.ExecContext(ctx, `UPDATE todo SET title = ? WHERE id = ?`, title, id.Int64())
	return err
}

// Delete 硬删除待办 + 其 todo_topic 关联（单事务级联）。区别于 dismiss（软删，
// 保留行+状态 dismissed）。不存在也不报错（DELETE 返回 0 行）。区别于
// DeleteBySessionExt（按 session 批删派生 todo）。
func (r *TodoRepo) Delete(ctx context.Context, id ids.ID) error {
	tx, err := r.DB.BeginTxx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `DELETE FROM todo_topic WHERE todo_id = ?`, id.Int64()); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM todo WHERE id = ?`, id.Int64()); err != nil {
		return err
	}
	return tx.Commit()
}
```

> `BeginTxx`/defer Rollback/Commit 模式与本文件 `DedupSuggested` 一致。

- [ ] **Step 4: api 扩展 Patch（接受 title）+ 加 Delete + 路由** — `internal/api/todo.go`：

(a) import 加 `"strings"`（当前只有 `encoding/json/net/http/chi/ids/repo`）。改 import 块为：
```go
import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"zhiwei/internal/ids"
	"zhiwei/internal/repo"
)
```

(b) `RegisterTodo`（`:21-26`）加 DELETE 路由：
旧：
```go
	r.Patch("/api/todos/{id}", h.Patch)
	r.Post("/api/todos/{id}/topics", h.AddTopic)
```
新：
```go
	r.Patch("/api/todos/{id}", h.Patch)
	r.Delete("/api/todos/{id}", h.Delete)
	r.Post("/api/todos/{id}/topics", h.AddTopic)
```

(c) 整个 `Patch`（`:57-85`）替换为（接受 `{title?, status?}`，任一非空即合法；title 永远写、status 需 CanTransition；两者同 body 时先 title 后 status，CanTransition 用原始状态判断）：
```go
// Patch 变更待办：title（改名）和/或 status（状态机流转）。至少一个非空（400）。
// 校验顺序：解码（400）→ title/status 非空（400）→ status 枚举（400）→ 存在性（404）
// → 状态流转合法性（409）。title 不做 CanTransition，与状态独立。
func (h *TodoHandler) Patch(w http.ResponseWriter, r *http.Request) {
	id, err := ids.ParseID(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	var req struct {
		Title  string `json:"title"`
		Status string `json:"status"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "请求体非法", http.StatusBadRequest)
		return
	}
	title := strings.TrimSpace(req.Title)
	if title == "" && req.Status == "" {
		http.Error(w, "title 或 status 至少一个非空", http.StatusBadRequest)
		return
	}
	if req.Status != "" && !validTodoStatus(req.Status) {
		http.Error(w, "status 取值非法", http.StatusBadRequest)
		return
	}
	td, err := h.Todos.Get(r.Context(), id)
	if err != nil {
		http.Error(w, "todo 不存在", http.StatusNotFound)
		return
	}
	if title != "" {
		if err := h.Todos.UpdateTitle(r.Context(), id, title); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		td.Title = title
	}
	if req.Status != "" {
		// CanTransition 用原始 td.Status（title 改动不影响状态），保证 status-only 的语义不变
		if !repo.CanTransition(td.Status, req.Status) {
			http.Error(w, "不允许的状态流转: "+td.Status+" → "+req.Status, http.StatusConflict)
			return
		}
		if err := h.Todos.UpdateStatus(r.Context(), id, req.Status); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		td.Status = req.Status
	}
	writeJSON(w, map[string]any{"todo": td})
}
```

(d) 在 `RemoveTopic` 后（文件末尾，`:149` 后）追加 `Delete` handler：
```go
// Delete 硬删除待办 + 关联（单事务级联，2 步确认由前端）。幂等：不存在也 204。
func (h *TodoHandler) Delete(w http.ResponseWriter, r *http.Request) {
	id, err := ids.ParseID(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	if err := h.Todos.Delete(r.Context(), id); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
```

- [ ] **Step 5: 跑通过（含回归）** — `... go test -p 1 -run 'TestTodo' ./internal/api -v` → 全 PASS（既有 `TestTodoPatchTransitions` 发 `{status:...}` 仍合法：title 空+status 非空→走 status 路径，语义不变）。

### 前端

- [ ] **Step 6: app.js 加 todo inplace 编辑 + 删除** — 在 `setTodoStatus` 后（约 `web/app.js:330`）插入：

```js
    // ---------- 待办 inplace 编辑 + 删除 ----------
    const editingTodo = ref(null);
    function startEditTodo(t) { editingTodo.value = { id: t.id, title: t.title }; }
    function cancelEditTodo() { editingTodo.value = null; }
    async function saveEditTodo(reload) {
      const e = editingTodo.value;
      if (!e || !e.title.trim()) return;
      try { await api('PATCH', '/api/todos/' + e.id, { title: e.title.trim() }); editingTodo.value = null; if (reload) await reload(); }
      catch (e2) { showError(e2); }
    }
    // 2 步行内删除确认：deletingTodoId 存正待确认删除的 todo id
    const deletingTodoId = ref(null);
    function askDeleteTodo(t) { deletingTodoId.value = t.id; }
    function cancelDeleteTodo() { deletingTodoId.value = null; }
    async function confirmDeleteTodo(t, reload) {
      try { await api('DELETE', '/api/todos/' + t.id); deletingTodoId.value = null; if (reload) await reload(); }
      catch (e) { showError(e); }
    }
```

- [ ] **Step 7: app.js return 暴露** — return 块的 todos 行（`web/app.js:363`）当前 `loadTodos, setTodoStatus, jumpToSession,` → 末尾追加 `editingTodo, startEditTodo, cancelEditTodo, saveEditTodo, deletingTodoId, askDeleteTodo, cancelDeleteTodo, confirmDeleteTodo,`

- [ ] **Step 8: index.html 待确认卡可编辑+删除** — `web/index.html:266-272` 的待确认卡 `.kv` 替换为：

旧：
```html
      <div class="kv">
        <b>☑️ {{ td.title }}</b>
        <div style="display:flex; gap:6px">
          <button class="mini" @click="setTodoStatus(td, 'confirmed')">加入</button>
          <button class="mini" @click="setTodoStatus(td, 'dismissed')">忽略</button>
        </div>
      </div>
```
新：
```html
      <div class="kv">
        <b v-if="!editingTodo || editingTodo.id !== td.id" @click="startEditTodo(td)" style="cursor:text">☑️ {{ td.title }}</b>
        <input v-else class="txt" v-model="editingTodo.title" style="display:inline-block;width:auto">
        <div style="display:flex; gap:6px">
          <button v-if="editingTodo && editingTodo.id === td.id" class="primary" style="padding:4px 10px" @click="saveEditTodo(loadTodos)">保存</button>
          <button v-if="editingTodo && editingTodo.id === td.id" class="mini" @click="cancelEditTodo()">取消</button>
          <button v-if="!editingTodo || editingTodo.id !== td.id" class="mini" @click="setTodoStatus(td, 'confirmed')">加入</button>
          <button v-if="!editingTodo || editingTodo.id !== td.id" class="mini" @click="setTodoStatus(td, 'dismissed')">忽略</button>
          <button v-if="(!editingTodo || editingTodo.id !== td.id) && deletingTodoId !== td.id" class="mini" @click="askDeleteTodo(td)">删除</button>
          <button v-if="deletingTodoId === td.id" class="mini" style="background:#fee2e2;color:#991b1b" @click="confirmDeleteTodo(td, loadTodos)">确认删除?</button>
          <button v-if="deletingTodoId === td.id" class="mini" @click="cancelDeleteTodo()">取消</button>
        </div>
      </div>
```

- [ ] **Step 9: index.html 进行中卡可编辑+删除** — `web/index.html:289-295` 的进行中卡 `.kv` 替换为（按钮文案 完成/忽略）：

旧：
```html
      <div class="kv">
        <b>☑️ {{ td.title }}</b>
        <div style="display:flex; gap:6px">
          <button class="mini" @click="setTodoStatus(td, 'done')">完成</button>
          <button class="mini" @click="setTodoStatus(td, 'dismissed')">忽略</button>
        </div>
      </div>
```
新：
```html
      <div class="kv">
        <b v-if="!editingTodo || editingTodo.id !== td.id" @click="startEditTodo(td)" style="cursor:text">☑️ {{ td.title }}</b>
        <input v-else class="txt" v-model="editingTodo.title" style="display:inline-block;width:auto">
        <div style="display:flex; gap:6px">
          <button v-if="editingTodo && editingTodo.id === td.id" class="primary" style="padding:4px 10px" @click="saveEditTodo(loadTodos)">保存</button>
          <button v-if="editingTodo && editingTodo.id === td.id" class="mini" @click="cancelEditTodo()">取消</button>
          <button v-if="!editingTodo || editingTodo.id !== td.id" class="mini" @click="setTodoStatus(td, 'done')">完成</button>
          <button v-if="!editingTodo || editingTodo.id !== td.id" class="mini" @click="setTodoStatus(td, 'dismissed')">忽略</button>
          <button v-if="(!editingTodo || editingTodo.id !== td.id) && deletingTodoId !== td.id" class="mini" @click="askDeleteTodo(td)">删除</button>
          <button v-if="deletingTodoId === td.id" class="mini" style="background:#fee2e2;color:#991b1b" @click="confirmDeleteTodo(td, loadTodos)">确认删除?</button>
          <button v-if="deletingTodoId === td.id" class="mini" @click="cancelDeleteTodo()">取消</button>
        </div>
      </div>
```

- [ ] **Step 10: index.html 已完成卡加删除** — `web/index.html:313-315` 的已完成卡 `.kv` 替换为（已完成做删除即可，不做编辑——删除线编辑语义怪）：

旧：
```html
        <div class="kv">
          <div>✅ <s>{{ td.title }}</s></div>
        </div>
```
新：
```html
        <div class="kv">
          <div>✅ <s>{{ td.title }}</s></div>
          <div style="display:flex; gap:6px">
            <button v-if="deletingTodoId !== td.id" class="mini" @click="askDeleteTodo(td)">删除</button>
            <button v-if="deletingTodoId === td.id" class="mini" style="background:#fee2e2;color:#991b1b" @click="confirmDeleteTodo(td, loadTodos)">确认删除?</button>
            <button v-if="deletingTodoId === td.id" class="mini" @click="cancelDeleteTodo()">取消</button>
          </div>
        </div>
```

- [ ] **Step 11: index.html 时间线 todo 卡可编辑+删除** — `web/index.html:138-147` 的时间线 todo 卡替换为：

旧：
```html
          <div class="card" v-for="td in detail.todos" :key="td.id" style="margin:6px 0">
            <div class="kv">
              <div>☑️ {{ td.title }}</div>
              <span class="badge" :class="td.status">
                {{ td.status === 'suggested' ? '待确认' : td.status === 'confirmed' ? '已确认' : td.status === 'done' ? '已完成' : '已忽略' }}
              </span>
            </div>
            <div class="muted" v-if="td.due_at">截止 {{ fmtDue(td.due_at) }} · 到「待办」页处理</div>
          </div>
```
新：
```html
          <div class="card" v-for="td in detail.todos" :key="td.id" style="margin:6px 0">
            <div class="kv">
              <div v-if="!editingTodo || editingTodo.id !== td.id" @click="startEditTodo(td)" style="cursor:text">☑️ {{ td.title }}</div>
              <input v-else class="txt" v-model="editingTodo.title" style="display:inline-block;width:auto">
              <div style="display:flex; gap:6px">
                <button v-if="editingTodo && editingTodo.id === td.id" class="primary" style="padding:4px 10px" @click="saveEditTodo(() => reloadSession(detail.session.id))">保存</button>
                <button v-if="editingTodo && editingTodo.id === td.id" class="mini" @click="cancelEditTodo()">取消</button>
                <span v-if="!editingTodo || editingTodo.id !== td.id" class="badge" :class="td.status">
                  {{ td.status === 'suggested' ? '待确认' : td.status === 'confirmed' ? '已确认' : td.status === 'done' ? '已完成' : '已忽略' }}
                </span>
                <button v-if="(!editingTodo || editingTodo.id !== td.id) && deletingTodoId !== td.id" class="mini" @click="askDeleteTodo(td)">删除</button>
                <button v-if="deletingTodoId === td.id" class="mini" style="background:#fee2e2;color:#991b1b" @click="confirmDeleteTodo(td, () => reloadSession(detail.session.id))">确认删除?</button>
                <button v-if="deletingTodoId === td.id" class="mini" @click="cancelDeleteTodo()">取消</button>
              </div>
            </div>
            <div class="muted" v-if="td.due_at">截止 {{ fmtDue(td.due_at) }} · 到「待办」页处理</div>
          </div>
```

- [ ] **Step 12: 验证** — `node --check web/app.js`；`make hash-web`；`go build ./...`。

- [ ] **Step 13: 提交** — `git add internal/repo/todo.go internal/api/todo.go internal/api/todo_test.go web/app.js web/index.html && git commit -m "feat: 待办 title 编辑(扩展 PATCH) + 硬删除(DELETE 级联 todo_topic)"`

---

## Task 4 (F4): topic 删除（后端 + 前端）

**Files:** Modify `internal/repo/topic.go`、`internal/api/topic.go`、`internal/api/topic_test.go`、`web/app.js`、`web/index.html`

### 后端

- [ ] **Step 1: 写失败测试** — `internal/api/topic_test.go` 末尾追加（`setupMergeFixtures(t)` 返回 `(*repo.TopicRepo, *repo.MemoryRepo, *repo.TodoRepo, *repo.Topic, *repo.Topic)`=tr,mr,tdr,a,b；a/b 各挂 1 memory+1 todo 关联；`tr.DB` 是 `*sqlx.DB`）：

```go
// TestTopicDelete 验证 DELETE 硬删 topic + 其 memory_topic/todo_topic 关联（单事务级联），
// member B 完好（关联不误删）。区别于 dismiss（PATCH dismissed 软删）。重复删除幂等。
func TestTopicDelete(t *testing.T) {
	tr, mr, tdr, a, b := setupMergeFixtures(t)
	r := chi.NewRouter()
	RegisterTopic(r, &TopicHandler{Topics: tr, Memories: mr, Todos: tdr}) // Delete 不调 LLM
	ctx := context.Background()

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/api/topics/"+a.ID.String(), nil))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body.String())
	}
	if _, err := tr.Get(ctx, a.ID); err == nil {
		t.Fatal("topic a 仍存在")
	}
	// a 的关联已级联删
	var n int
	if err := tr.DB.GetContext(ctx, &n, `SELECT COUNT(*) FROM memory_topic WHERE topic_id = ?`, a.ID.Int64()); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("a memory_topic 残留 %d", n)
	}
	if err := tr.DB.GetContext(ctx, &n, `SELECT COUNT(*) FROM todo_topic WHERE topic_id = ?`, a.ID.Int64()); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("a todo_topic 残留 %d", n)
	}
	// b 完好（未被误删/误改 dismissed）
	gotB, err := tr.Get(ctx, b.ID)
	if err != nil || gotB.Status == "dismissed" {
		t.Fatalf("b 被误删/误改: err=%v status=%s", err, gotB.Status)
	}
	// 重复删除幂等（204）
	rec2 := httptest.NewRecorder()
	r.ServeHTTP(rec2, httptest.NewRequest(http.MethodDelete, "/api/topics/"+a.ID.String(), nil))
	if rec2.Code != http.StatusNoContent {
		t.Fatalf("idempotent delete: %d", rec2.Code)
	}
}
```

> `topic_test.go` 已 import `context/chi/repo`，无需新增。

- [ ] **Step 2: 跑确认失败** — `... go test -p 1 -run 'TestTopicDelete' ./internal/api -v` → FAIL（无 DELETE 路由）。

- [ ] **Step 3: repo 加 Delete** — `internal/repo/topic.go` 在 `MergeGroups` 后（约 `:188`）追加：

```go
// Delete 硬删除 topic + 其 memory_topic/todo_topic 关联（单事务级联）。区别于 dismiss
// （PATCH dismissed 软删，保留行）。不存在也不报错。与 MergeGroups 互补：MergeGroups
// 迁移关联+保留 member 行（审计），Delete 彻底清 topic+关联。
func (r *TopicRepo) Delete(ctx context.Context, id ids.ID) error {
	tx, err := r.DB.BeginTxx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `DELETE FROM memory_topic WHERE topic_id = ?`, id.Int64()); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM todo_topic WHERE topic_id = ?`, id.Int64()); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM topic WHERE id = ?`, id.Int64()); err != nil {
		return err
	}
	return tx.Commit()
}
```

- [ ] **Step 4: api 加 Delete + 路由** — `internal/api/topic.go`：

(a) `RegisterTopic`（`:33-40`）加 DELETE：
旧：
```go
	r.Get("/api/topics/{id}", h.Get)
	r.Patch("/api/topics/{id}", h.Patch)
}
```
新：
```go
	r.Get("/api/topics/{id}", h.Get)
	r.Patch("/api/topics/{id}", h.Patch)
	r.Delete("/api/topics/{id}", h.Delete)
}
```

(b) 文件末尾（`Merge` 后）追加 `Delete` handler：
```go
// Delete 硬删除 topic + 关联（单事务级联，2 步确认由前端）。幂等：不存在也 204。
func (h *TopicHandler) Delete(w http.ResponseWriter, r *http.Request) {
	id, err := ids.ParseID(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	if err := h.Topics.Delete(r.Context(), id); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
```

- [ ] **Step 5: 跑通过（含回归）** — `... go test -p 1 -run 'TestTopic' ./internal/api -v` → 全 PASS（`TestTopicMerge`/`TestTopicDetailAndPatch` 不受影响）。

### 前端

- [ ] **Step 6: app.js 加 topic 删除** — 在 `dismissTopic` 后（约 `web/app.js:188`）插入：

```js
    // ---------- topic 删除（硬删，2 步确认） ----------
    const deletingTopicId = ref(null);
    function askDeleteTopic(t) { deletingTopicId.value = t.id; }
    function cancelDeleteTopic() { deletingTopicId.value = null; }
    async function confirmDeleteTopic(t) {
      try {
        await api('DELETE', '/api/topics/' + t.id);
        deletingTopicId.value = null;
        await loadTopics();
        if (topicDetail.value && topicDetail.value.topic.id === t.id) closeTopicDetail();
      } catch (e) { showError(e); }
    }
```

> `topicDetail.value`（JS 里用 `.value`）。删除当前详情打开的 topic 时一并关详情。

- [ ] **Step 7: app.js return 暴露** — return 块 topics 行（`web/app.js:361`，T2 已在该行追加手动合并函数）末尾再追加 `deletingTopicId, askDeleteTopic, cancelDeleteTopic, confirmDeleteTopic,`。

- [ ] **Step 8: index.html topic 卡加删除按钮** — T2 产出的 topic 卡操作区（`v-if="!manualMergeMode"`）当前是：

```html
          <div v-if="!manualMergeMode" style="display:flex; gap:6px">
            <button class="mini" v-if="t.status==='suggested'" @click="confirmTopic(t)">确认</button>
            <button class="mini" @click="dismissTopic(t)">忽略</button>
          </div>
```
替换为（加 2 步删除确认；与 T3 todo 删除按钮同构）：
```html
          <div v-if="!manualMergeMode" style="display:flex; gap:6px">
            <button class="mini" v-if="t.status==='suggested'" @click="confirmTopic(t)">确认</button>
            <button class="mini" @click="dismissTopic(t)">忽略</button>
            <button v-if="deletingTopicId !== t.id" class="mini" @click="askDeleteTopic(t)">删除</button>
            <button v-if="deletingTopicId === t.id" class="mini" style="background:#fee2e2;color:#991b1b" @click="confirmDeleteTopic(t)">确认删除?</button>
            <button v-if="deletingTopicId === t.id" class="mini" @click="cancelDeleteTopic()">取消</button>
          </div>
```

- [ ] **Step 9: 验证** — `node --check web/app.js`；`make hash-web`；`go build ./...`。

- [ ] **Step 10: 提交** — `git add internal/repo/topic.go internal/api/topic.go internal/api/topic_test.go web/app.js web/index.html && git commit -m "feat: topic 硬删除(DELETE 级联 memory_topic/todo_topic) + 前端 2 步确认"`

---

## Task 5 (F5 后端): session 删除 + ListSessions 富化

**Files:** Modify `internal/repo/session.go`、`internal/api/query.go`、`internal/api/query_test.go`

- [ ] **Step 1: 写失败测试** — `internal/api/query_test.go` 末尾追加。先加共享 fixture 助手（构造 1 session+1 段转写+1 active memory+1 confirmed todo），再写两个测试：

```go
// buildEnrichedSession 构造 1 session + 1 段转写 + 1 active memory + 1 confirmed todo，
// 供 ListSessions 富化与 DeleteSession 级联测试共用。返回 router+session id+SessionRepo
// （含 DB 句柄供断言）。
func buildEnrichedSession(t *testing.T) (http.Handler, ids.ID, *repo.SessionRepo) {
	_ = ids.Init(1)
	db, err := repo.NewDB(repo.TestDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	sessions := &repo.SessionRepo{DB: db}
	jobs := &repo.JobRepo{DB: db}
	transcripts := &repo.TranscriptRepo{DB: db}
	memories := &repo.MemoryRepo{DB: db}
	todos := &repo.TodoRepo{DB: db}

	sid := ids.New()
	if err := sessions.Create(ctx, &repo.AudioSession{
		ID: sid, Source: "web_upload", Filename: "enriched.wav",
		StoragePath: "/tmp/enriched.wav", Status: "completed",
	}); err != nil {
		t.Fatal(err)
	}
	tr := &repo.Transcript{SessionID: sid, Language: "zh-CN"}
	if err := transcripts.Create(ctx, tr); err != nil {
		t.Fatal(err)
	}
	conf := 0.95
	if err := transcripts.InsertSegments(ctx, []repo.TranscriptSegment{
		{TranscriptID: tr.ID, SequenceNo: 1, SpeakerLabel: "1",
			Text: "明天记得发邮件确认设计稿", StartMS: 0, EndMS: 1000, Confidence: &conf},
	}); err != nil {
		t.Fatal(err)
	}
	eventAt := time.Now()
	_ = memories.InsertExt(ctx, db, []*repo.Memory{{
		Type: "event", Title: "富化用例发邮件", Content: "明天记得给 Tom 发邮件",
		EpistemicType: "observed", Confidence: 0.9, SessionID: sid,
		EventAt: &eventAt, Status: "active",
	}})
	memRows, _ := memories.ListBySession(ctx, sid)
	_ = todos.InsertExt(ctx, db, []*repo.Todo{{
		Title: "富化用例给 Tom 发邮件", SourceMemoryID: &memRows[0].ID, Status: "confirmed", Confidence: 0.9,
	}})

	r := chi.NewRouter()
	RegisterQuery(r, &QueryHandler{
		Sessions: sessions, Jobs: jobs, Transcripts: transcripts, Memories: memories, Todos: todos,
	})
	return r, sid, sessions
}

// TestListSessionsEnriched 验证 ListSessions 富化字段：asr_preview 含转写文本、
// memory_count/todo_count 各 1。按 session id 精确定位，避免脏库其他行干扰。
func TestListSessionsEnriched(t *testing.T) {
	r, sid, _ := buildEnrichedSession(t)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/sessions", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("list: %d %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Sessions []struct {
			ID          string `json:"id"`
			AsrPreview  string `json:"asr_preview"`
			MemoryCount int    `json:"memory_count"`
			TodoCount   int    `json:"todo_count"`
		} `json:"sessions"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("json: %v %s", err, rec.Body.String())
	}
	found := false
	for _, s := range resp.Sessions {
		if s.ID == sid.String() {
			found = true
			if !strings.Contains(s.AsrPreview, "明天记得发邮件") {
				t.Fatalf("asr_preview=%s", s.AsrPreview)
			}
			if s.MemoryCount != 1 {
				t.Fatalf("memory_count=%d, want 1", s.MemoryCount)
			}
			if s.TodoCount != 1 {
				t.Fatalf("todo_count=%d, want 1", s.TodoCount)
			}
		}
	}
	if !found {
		t.Fatalf("session %s missing: %s", sid, rec.Body.String())
	}
}

// TestDeleteSession 验证 DELETE session 级联：audio_session/memory/transcript/todo 均删。
func TestDeleteSession(t *testing.T) {
	r, sid, sr := buildEnrichedSession(t)
	ctx := context.Background()
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/api/sessions/"+sid.String(), nil))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body.String())
	}
	// 级联断言：四类行均 0（todo 经 source_memory_id 子查询，memory 删后子查询空→0）
	checks := []struct{ name, sql string }{
		{"audio_session", `SELECT COUNT(*) FROM audio_session WHERE id = ?`},
		{"memory", `SELECT COUNT(*) FROM memory WHERE session_id = ?`},
		{"transcript", `SELECT COUNT(*) FROM transcript WHERE session_id = ?`},
		{"todo", `SELECT COUNT(*) FROM todo WHERE source_memory_id IN (SELECT id FROM memory WHERE session_id = ?)`},
	}
	for _, c := range checks {
		var n int
		if err := sr.DB.GetContext(ctx, &n, c.sql, sid.Int64()); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if n != 0 {
			t.Fatalf("%s 残留 %d", c.name, n)
		}
	}
}
```

> `query_test.go` 已 import `context/encoding/json/net/http/net/http/httptest/strings/testing/time/chi/ids/repo`，无需新增。`sr.DB.GetContext` 调 `*sqlx.DB` 方法，无需 import sqlx（不命名该类型）。

- [ ] **Step 2: 跑确认失败** — `... go test -p 1 -run 'TestListSessionsEnriched|TestDeleteSession' ./internal/api -v` → FAIL（无 asr_preview/counts；无 DELETE 路由）。

- [ ] **Step 3: repo 加 SessionRepo.Delete** — `internal/repo/session.go` 在 `SetJobID` 后（约 `:59`）追加：

```go
// Delete 硬删除 session + 全部派生数据（单事务级联）。音频文件由 handler 库外删（best-effort）。
// 顺序：关联子表先于主表（子查询依赖主表行仍存在）；各步 target 表 ≠ 子查询 source 表，
// 无 MySQL「不能在子查询里更新目标表」之限。jobID 非空则一并删 job。
func (r *SessionRepo) Delete(ctx context.Context, id ids.ID, jobID *ids.ID) error {
	tx, err := r.DB.BeginTxx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	steps := []string{
		`DELETE FROM memory_topic WHERE memory_id IN (SELECT id FROM memory WHERE session_id = ?)`,
		`DELETE FROM todo_topic WHERE todo_id IN (SELECT id FROM todo WHERE source_memory_id IN (SELECT id FROM memory WHERE session_id = ?))`,
		`DELETE FROM todo WHERE source_memory_id IN (SELECT id FROM memory WHERE session_id = ?)`,
		`DELETE FROM memory WHERE session_id = ?`,
		`DELETE FROM transcript_segment WHERE transcript_id IN (SELECT id FROM transcript WHERE session_id = ?)`,
		`DELETE FROM transcript WHERE session_id = ?`,
		`DELETE FROM audio_session WHERE id = ?`,
	}
	for _, q := range steps {
		if _, err := tx.ExecContext(ctx, q, id.Int64()); err != nil {
			return err
		}
	}
	if jobID != nil {
		if _, err := tx.ExecContext(ctx, `DELETE FROM job WHERE id = ?`, jobID.Int64()); err != nil {
			return err
		}
	}
	return tx.Commit()
}
```

- [ ] **Step 4: api ListSessions 富化** — `internal/api/query.go` 整个 `ListSessions`（`:31-54`）替换为（单 SQL 带 3 相关子查询避免 N+1；asr_full 截 120 runes 得 asr_preview；保留 job 状态附加）：

```go
// ListSessions 列出会话，每行富化 asr_preview（转写前 120 字）+ memory_count +
// todo_count（单 SQL 相关子查询，避免 N+1），并附最新 job 状态（处理进度）。
// asr_full 不外泄（json:"-"），仅截断后以 asr_preview 输出。
func (h *QueryHandler) ListSessions(w http.ResponseWriter, r *http.Request) {
	limit := intQuery(r, "limit", 50)
	type row struct {
		repo.AudioSession
		JobStatus   string `json:"job_status,omitempty"`
		JobStage    string `json:"job_stage,omitempty"`
		MemoryCount int    `db:"memory_count" json:"memory_count"`
		TodoCount   int    `db:"todo_count" json:"todo_count"`
		AsrFull     string `db:"asr_full" json:"-"` // GROUP_CONCAT 全文，截断后给 AsrPreview
		AsrPreview  string `db:"-" json:"asr_preview"`
	}
	var rows []row
	err := h.Sessions.DB.SelectContext(r.Context(), &rows, `
SELECT s.*,
  (SELECT COUNT(*) FROM memory WHERE session_id = s.id AND status = 'active') AS memory_count,
  (SELECT COUNT(*) FROM todo WHERE source_memory_id IN (SELECT id FROM memory WHERE session_id = s.id) AND status != 'dismissed') AS todo_count,
  (SELECT IFNULL(GROUP_CONCAT(seg.text ORDER BY seg.start_ms SEPARATOR ''), '')
     FROM transcript_segment seg JOIN transcript tr ON tr.id = seg.transcript_id
     WHERE tr.session_id = s.id) AS asr_full
FROM audio_session s ORDER BY s.id DESC LIMIT ?`, limit)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	out := make([]row, len(rows))
	for i, s := range rows {
		out[i] = s
		// asr_full 截 120 runes（够卡片预览；GROUP_CONCAT 默认上限 1024 够取前 120）
		if rs := []rune(s.AsrFull); len(rs) > 120 {
			out[i].AsrPreview = string(rs[:120]) + "…"
		} else {
			out[i].AsrPreview = s.AsrFull
		}
		if s.JobID != nil {
			if j, err := h.Jobs.Get(r.Context(), *s.JobID); err == nil {
				out[i].JobStatus, out[i].JobStage = j.Status, j.Stage
			}
		}
	}
	writeJSON(w, map[string]any{"sessions": out})
}
```

> `h.Sessions.DB.SelectContext`：SessionRepo.DB 是 `*sqlx.DB`（导出字段）。`SELECT s.*` + 子查询列映射到嵌入 AudioSession + row 自有字段，与 `TopicRepo.ListWithCounts`（`SELECT t.*, (子查询) AS memory_count`）同模式（已验证可行）。`rs` 局部变量避免遮蔽 `r *http.Request`。

- [ ] **Step 5: api DeleteSession + 路由** — `internal/api/query.go`：

(a) import 加 `"os"`（当前 `encoding/json/net/http/strconv/chi/ids/repo`）：
```go
import (
	"encoding/json"
	"net/http"
	"os"
	"strconv"

	"github.com/go-chi/chi/v5"

	"zhiwei/internal/ids"
	"zhiwei/internal/repo"
)
```

(b) `RegisterQuery`（`:24-29`）加 DELETE：
旧：
```go
	r.Get("/api/sessions/{id}", h.GetSession)
	r.Get("/api/sessions/{id}/audio", h.ServeAudio)
```
新：
```go
	r.Get("/api/sessions/{id}", h.GetSession)
	r.Delete("/api/sessions/{id}", h.DeleteSession)
	r.Get("/api/sessions/{id}/audio", h.ServeAudio)
```

(c) 文件末尾（`intOffset` 后、`writeJSON` 前）追加 `DeleteSession`：
```go
// DeleteSession 硬删除 session + 派生数据（级联单事务）+ 音频文件 best-effort。
// 2 步确认由前端；后端：Get 不存在→404，删成功→204。音频文件库外删，失败仅 log 不阻断
// （DB 已删，文件残留可接受；区别于 DB 事务的强一致）。StoragePath 是 json:"-" 不外泄，
// 此处仅服务端读用于删文件。
func (h *QueryHandler) DeleteSession(w http.ResponseWriter, r *http.Request) {
	sid, err := ids.ParseID(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	s, err := h.Sessions.Get(r.Context(), sid)
	if err != nil {
		http.Error(w, "session 不存在", http.StatusNotFound)
		return
	}
	if err := h.Sessions.Delete(r.Context(), sid, s.JobID); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if s.StoragePath != "" {
		_ = os.Remove(s.StoragePath) // best-effort：失败不阻断（DB 已删）
	}
	w.WriteHeader(http.StatusNoContent)
}
```

- [ ] **Step 6: 跑通过（含回归）** — `... go test -p 1 -run 'TestSessionsAndDetail|TestListSessionsEnriched|TestDeleteSession|TestServeAudio' ./internal/api -v` → 全 PASS（`TestSessionsAndDetail` 的列表断言仍绿：富化只增字段不破坏 `sessions[]` 结构）。

- [ ] **Step 7: 提交** — `git add internal/repo/session.go internal/api/query.go internal/api/query_test.go && git commit -m "feat: session 删除(级联单事务+音频文件 best-effort) + ListSessions 富化(asr_preview+memory/todo count)"`

---

## Task 6 (F5 前端): 时间线卡片增强 + 删除时间线

**Files:** Modify `web/app.js`、`web/index.html`

- [ ] **Step 1: app.js 加 session 删除** — 在 `reloadSession` 后（约 `web/app.js:85`）插入：

```js
    // ---------- session 删除（硬删级联，2 步确认） ----------
    const deletingSessionId = ref(null);
    function askDeleteSession(s) { deletingSessionId.value = s.id; }
    function cancelDeleteSession() { deletingSessionId.value = null; }
    async function confirmDeleteSession(s) {
      try { await api('DELETE', '/api/sessions/' + s.id); deletingSessionId.value = null; await loadSessions(); }
      catch (e) { showError(e); }
    }
```

- [ ] **Step 2: app.js return 暴露** — return 块 timeline 行（`web/app.js:358`，T1 已在该行追加 editingMem 等）末尾再追加 `deletingSessionId, askDeleteSession, cancelDeleteSession, confirmDeleteSession,`。

- [ ] **Step 3: index.html session 卡富化 + 删除** — `web/index.html:81-89` 的折叠 session 卡替换为（加 asr_preview 行 + 计数行 + 删除按钮区，删除按钮 `@click.stop` 阻止冒泡触发展开）：

旧：
```html
      <div class="card clickable" style="cursor:pointer" @click="toggleSession(s.id)">
        <div class="kv">
          <div>
            <b>{{ s.filename }}</b>
            <div class="muted">{{ fmtTime(s.created_at) }} · {{ s.source === 'web_record' ? '录音' : '上传' }}</div>
          </div>
          <span class="badge" :class="s.job_status || s.status">{{ statusText(s.job_status, s.job_stage) }}</span>
        </div>
      </div>
```
新：
```html
      <div class="card clickable" style="cursor:pointer" @click="toggleSession(s.id)">
        <div class="kv">
          <div>
            <b>{{ s.filename }}</b>
            <div v-if="s.asr_preview" class="muted">{{ s.asr_preview }}</div>
            <div class="muted">{{ fmtTime(s.created_at) }} · {{ s.source === 'web_record' ? '录音' : '上传' }} · {{ s.memory_count }} 条记忆 · {{ s.todo_count }} 个待办</div>
          </div>
          <div style="display:flex; align-items:center; gap:6px" @click.stop>
            <span class="badge" :class="s.job_status || s.status">{{ statusText(s.job_status, s.job_stage) }}</span>
            <button v-if="deletingSessionId !== s.id" class="mini" @click="askDeleteSession(s)">删除</button>
            <button v-if="deletingSessionId === s.id" class="mini" style="background:#fee2e2;color:#991b1b" @click="confirmDeleteSession(s)">确认删除?</button>
            <button v-if="deletingSessionId === s.id" class="mini" @click="cancelDeleteSession()">取消</button>
          </div>
        </div>
      </div>
```

> `s.asr_preview`/`s.memory_count`/`s.todo_count` 来自 T5 富化的 ListSessions 响应。`@click.stop` 包住右侧按钮区，防点删除误触发展开。

- [ ] **Step 4: 验证** — `node --check web/app.js`；`make hash-web`；`go build ./...`。

- [ ] **Step 5: 提交** — `git add web/app.js web/index.html && git commit -m "feat(web): 时间线卡片显示 ASR 预览+记忆/待办计数 + 删除时间线(2 步确认)"`

---

## 自检

- **spec 覆盖**：F2↔T1；F1↔T2；F3↔T3（后端 Step1-5 + 前端 Step6-13）；F4↔T4（后端 Step1-5 + 前端 Step6-10）；F5 后端↔T5、前端↔T6。spec 每节均有任务落点，无遗漏。
- **占位扫描**：每步含实际 Go/JS/HTML 代码与命令；测试 fixture 签名已对齐源文件（`setupTodoAPI`→3 值；`setupMergeFixtures`→5 值无 router；`setupQueryAPI`→传 repo 参数，故新增 `buildEnrichedSession` 助手）。无「TBD/以实际为准」。
- **类型/签名一致**：
  - `TodoRepo.UpdateTitle(ctx, id, title)`/`Delete(ctx, id)`（T3 repo）↔ `TodoHandler.Patch` 调 `h.Todos.UpdateTitle`、`Delete` 调 `h.Todos.Delete`（T3 api）一致。
  - `TopicRepo.Delete(ctx, id)`（T4 repo）↔ `TopicHandler.Delete` 调 `h.Topics.Delete`（T4 api）一致。
  - `SessionRepo.Delete(ctx, id, jobID *ids.ID)`（T5 repo）↔ `DeleteSession` 调 `h.Sessions.Delete(ctx, sid, s.JobID)`（`AudioSession.JobID` 是 `*ids.ID`）（T5 api）一致。
  - `ListSessions` 富化字段 `asr_preview`/`memory_count`/`todo_count`（T5 后端 JSON tag）↔ 前端 `s.asr_preview`/`s.memory_count`/`s.todo_count`（T6）一致。
  - 前端 inplace（`editingMem`/`editingTodo`）+ 2 步删除（`deletingTodoId`/`deletingTopicId`/`deletingSessionId`/`manualConfirming`）模式同构跨任务。
  - return 块：T1/T6 共改 timeline 行、T2/T4 共改 topics 行、T3 改 todos 行——任务串行，后改者锚定前改者产出（T4 Step8 锚 T2 产出的操作区；T6 Step2 锚 T1 产出的 timeline 行）。
- **取舍落地**：①删除=硬删除（todo/topic/session 级联，T3/T4/T5）；②2 步行内确认（无 `confirm()`，T3/T4/T6）；③asr 截 120（T5）；④F1 canonical=新名输入默认第一个（T2）；⑤F2 复用 PATCH /api/memories、F3 扩展 PATCH /api/todos（T1/T3）；⑥session 级联单事务+文件 best-effort（T5）。
- **回归保护**：T3 Patch 重写保 `TestTodoPatchTransitions`（status-only body 仍走 status 路径，CanTransition 用原始状态）；T5 ListSessions 富化保 `TestSessionsAndDetail`（`sessions[]` 结构不变，只增字段）；T4 保 `TestTopicMerge`/`TestTopicDetailAndPatch`（新增 DELETE 不影响 PATCH/merge）。
- **已知/约束**：`web/app.*.js` gitignored 不提交（各前端任务只 `git add web/app.js web/index.html`）；前端无单测（node --check + hash-web + `go build`）；todo/topic `忽略`(dismiss) 与新 `删除`(硬删) 并列共存；MySQL 子查询删除无 target=source 自引用（T5 注释已说明）。
