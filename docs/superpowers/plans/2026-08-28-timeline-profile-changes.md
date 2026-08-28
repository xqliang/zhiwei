# 转写详情 timeline 展示 profile 平面变更 — 实现计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 在转写详情页右栏「提取的记忆」下方新增「涉及的画像变更」区块，按平面分组展示这条录音触发的 change_log 变更事件流。

**Architecture:** 后端新增 `PersonChangeLogRepo.ListBySession`，在 `GET /api/sessions/{id}` 的 detail 响应里加 `profile_changes` 字段（与现有 `memories`/`todos` 完全一致的内联模式，前端零额外请求）；前端在右栏底部按 `entity_kind` 分组渲染变更卡片，复用现有 `card sunken`/`todo-group-title`/`muted` 样式与 `typeMeta` 映射模式。

**Tech Stack:** Go（sqlx）、Vue 3（web/app.js + web/index.html，CDP 构建产物 `web/app.*.js` 勿手改）。

**Spec:** `docs/superpowers/specs/2026-08-28-timeline-profile-changes-design.md`

---

## 文件结构

| 文件 | 责任 | 动作 |
|---|---|---|
| `internal/repo/person_change_log.go` | 加 `ListBySession` 查询 | 修改 |
| `internal/repo/person_change_log_test.go` | `ListBySession` 单测 | 修改 |
| `internal/api/query.go` | `QueryHandler` 加 `ChangeLogs` 字段 + detail 响应加 `profile_changes` | 修改 |
| `cmd/zhiwei-server/main.go` | 装配 `ChangeLogs: personLogs` 到 QueryHandler | 修改 |
| `web/app.js` | 加 `PROFILE_PLANE_META` + 分组/动作归一/摘要格式化函数 + `goProfilePending` | 修改 |
| `web/index.html` | 右栏底部加「涉及的画像变更」区块 | 修改 |

---

## Task 1: 后端 `ListBySession` + 测试

**Files:**
- Modify: `internal/repo/person_change_log.go`（在 `ListByPerson` 方法后追加）
- Test: `internal/repo/person_change_log_test.go`（追加测试函数）

- [ ] **Step 1: 写失败测试**

在 `internal/repo/person_change_log_test.go` 末尾（`fp` 辅助函数之后）追加：

```go
func TestPersonChangeLogListBySession(t *testing.T) {
	db, err := NewDB(repotest.DSN(t))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	persons := &PersonRepo{DB: db}
	logs := &PersonChangeLogRepo{DB: db}

	p := &Person{DisplayName: "session审计测试人物"}
	if err := persons.Create(ctx, p); err != nil {
		t.Fatal(err)
	}
	sessA := ids.New()
	sessB := ids.New()

	// sessA 两条（attribute + pet）
	if err := logs.Create(ctx, &PersonChangeLog{
		PersonID: p.ID, EntityKind: "attribute", AttrKey: strp("occupation"),
		ChangeType: "create", ChangedBy: "llm", NewValue: strp(`"工程师"`), SessionID: &sessA, Confidence: fp(0.9),
	}); err != nil {
		t.Fatal(err)
	}
	if err := logs.Create(ctx, &PersonChangeLog{
		PersonID: p.ID, EntityKind: "pet",
		ChangeType: "create", ChangedBy: "llm", NewValue: strp(`"泡泡（猫）"`), SessionID: &sessA,
	}); err != nil {
		t.Fatal(err)
	}
	// sessB 一条
	if err := logs.Create(ctx, &PersonChangeLog{
		PersonID: p.ID, EntityKind: "pet",
		ChangeType: "create", ChangedBy: "llm", NewValue: strp(`"豆豆（狗）"`), SessionID: &sessB,
	}); err != nil {
		t.Fatal(err)
	}
	// 一条无 session（手动改值，不应被 ListBySession 命中）
	if err := logs.Create(ctx, &PersonChangeLog{
		PersonID: p.ID, EntityKind: "pet",
		ChangeType: "update", ChangedBy: "user", NewValue: strp(`"手动改"`),
	}); err != nil {
		t.Fatal(err)
	}

	// ListBySession(sessA)：2 条，按 id 正序
	rowsA, err := logs.ListBySession(ctx, sessA)
	if err != nil {
		t.Fatal(err)
	}
	if len(rowsA) != 2 {
		t.Fatalf("sessA 应 2 条: %d", len(rowsA))
	}
	if rowsA[0].EntityKind != "attribute" || rowsA[1].EntityKind != "pet" {
		t.Fatalf("sessA 应按 id 正序: %+v", rowsA)
	}
	// ListBySession(sessB)：1 条
	rowsB, err := logs.ListBySession(ctx, sessB)
	if err != nil {
		t.Fatal(err)
	}
	if len(rowsB) != 1 || rowsB[0].NewValue == nil || *rowsB[0].NewValue != `"豆豆（狗）"` {
		t.Fatalf("sessB 应 1 条豆豆: %+v", rowsB)
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `TEST_MYSQL_DSN="root:root@tcp(127.0.0.1:3307)/zhiwei_test?parseTime=true&charset=utf8mb4&multiStatements=true" go test ./internal/repo/ -run TestPersonChangeLogListBySession -v`
Expected: FAIL — `logs.ListBySession undefined`（方法不存在）。

- [ ] **Step 3: 实现 `ListBySession`**

在 `internal/repo/person_change_log.go` 的 `ListByPerson` 方法之后追加：

```go
// ListBySession 返回某次录音（session）触发的全平面画像变更审计，按 id（≈时间）正序。
// 供转写详情页 timeline 展示「这条录音涉及的 profile 平面变更」。只命中带 session_id 的
// LLM 抽取行；手动改值（session_id 为空）不命中，符合「该录音触发」语义（对齐 ListByPerson 的 ORDER BY id）。
func (r *PersonChangeLogRepo) ListBySession(ctx context.Context, sessionID ids.ID) ([]PersonChangeLog, error) {
	const q = `SELECT * FROM person_change_log WHERE session_id = ? ORDER BY id`
	var list []PersonChangeLog
	err := r.DB.SelectContext(ctx, &list, q, sessionID.Int64())
	return list, err
}
```

- [ ] **Step 4: 跑测试确认通过**

Run: `TEST_MYSQL_DSN="root:root@tcp(127.0.0.1:3307)/zhiwei_test?parseTime=true&charset=utf8mb4&multiStatements=true" go test ./internal/repo/ -run TestPersonChangeLogListBySession -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/repo/person_change_log.go internal/repo/person_change_log_test.go
git commit -m "feat(repo): PersonChangeLog.ListBySession 按录音查平面变更

供转写详情 timeline 展示该录音触发的 profile 平面变更；按 id 正序，
只命中带 session_id 的 LLM 行。"
```

---

## Task 2: QueryHandler 装配 + detail 响应加字段

**Files:**
- Modify: `internal/api/query.go`（QueryHandler struct 加字段 + detail handler 加响应）
- Modify: `cmd/zhiwei-server/main.go`（装配 ChangeLogs）

- [ ] **Step 1: QueryHandler 加字段**

`internal/api/query.go` 的 `QueryHandler` struct 里，在 `Todos` 字段后（约 27 行）加：

```go
	Todos       *repo.TodoRepo    // Sprint 2：详情附带 todo 卡片
	ChangeLogs  *repo.PersonChangeLogRepo // 详情附带该录音触发的 profile 平面变更（entity_kind 覆盖 8 平面）
```

- [ ] **Step 2: detail handler 加响应字段**

`internal/api/query.go` 的 detail handler 里，在 `if h.Todos != nil { ... }` 块之后（约 495 行）加：

```go
	if h.ChangeLogs != nil {
		if changes, err := h.ChangeLogs.ListBySession(r.Context(), sid); err == nil {
			resp["profile_changes"] = changes
		}
	}
```

- [ ] **Step 3: main.go 装配**

`cmd/zhiwei-server/main.go` 的 `QueryHandler{...}` 装配（约 258-264 行），在 `Todos: todos,` 后加 `ChangeLogs: personLogs,`：

```go
	api.RegisterQuery(r, &api.QueryHandler{
		Sessions: sessions, Jobs: jobs, Transcripts: transcripts,
		Memories: memories, Todos: todos, Speakers: speakers, ChangeLogs: personLogs,
		...
	})
```

（`personLogs` 变量已在本文件存在，供 profile Service 使用。）

- [ ] **Step 4: 编译验证**

Run: `go build ./... && go vet ./internal/api/ ./cmd/...`
Expected: 通过，无错误。

- [ ] **Step 5: Commit**

```bash
git add internal/api/query.go cmd/zhiwei-server/main.go
git commit -m "feat(api): 会话详情附带 profile_changes（该录音触发的平面变更）

QueryHandler 装配 PersonChangeLogRepo，detail 响应加 profile_changes
（ListBySession），前端零额外请求，对齐现有 memories/todos 内联模式。"
```

---

## Task 3: 前端渲染「涉及的画像变更」

**Files:**
- Modify: `web/app.js`（模块级常量 + setup 内函数 + return 暴露）
- Modify: `web/index.html`（右栏底部区块）

- [ ] **Step 1: app.js 加模块级平面元信息常量**

在 `web/app.js` 的 `TYPE_META` 常量（约 6-13 行）之后加：

```js
// profile 平面元信息（变更事件流的平面标签；对齐 TYPE_META，平面→图标/中文名/颜色）。
// entity_kind 覆盖 8 平面：person/attribute/relationship/event/metric/cycle/activity/pet。
const PROFILE_PLANE_META = {
  person:       { label: '人物', icon: '👤', color: '#6b7280' },
  attribute:    { label: '属性', icon: '🏷️', color: '#6366f1' },
  relationship: { label: '关系', icon: '🔗', color: '#7c3aed' },
  event:        { label: '大事记', icon: '📌', color: '#d97706' },
  metric:       { label: '指标', icon: '📈', color: '#059669' },
  cycle:        { label: '周期', icon: '💊', color: '#dc2626' },
  activity:     { label: '轨迹', icon: '🏃', color: '#0284c7' },
  pet:          { label: '宠物', icon: '🐱', color: '#0891b2' },
};
```

- [ ] **Step 2: app.js 加处理函数（setup 内）**

在 `setup()` 内（靠近 `typeMeta` 函数，约 143 行附近）加以下函数，并确保在 `setup()` 的 `return { ... }` 中暴露它们（对齐 3089 行 `typeMeta` 的暴露方式）：

```js
    // ---------- profile 平面变更（转写详情 timeline）----------
    // profilePlaneMeta 平面→元信息（兜底未知 kind）
    function profilePlaneMeta(kind) { return PROFILE_PLANE_META[kind] || { label: kind, icon: '•', color: '#6b7280' }; }
    // profileChangeAction 变更动作归一：change_type+note → {label, color}。
    // 规则见 specs「变更动作归一规则」：新增(note空)/更新(含「合并更新」)/佐证(reaffirm)/待确认(含 conflict/待人工确认)。
    function profileChangeAction(log) {
      const note = log.note || '';
      if (log.change_type === 'reaffirm' || note.includes('佐证')) return { label: '佐证', color: '#059669' };
      if (note.includes('合并更新')) return { label: '更新', color: '#d97706' };
      if (note.includes('conflict') || note.includes('待人工确认')) return { label: '待确认', color: '#dc2626' };
      return { label: '新增', color: '#6366f1' };
    }
    // fmtChangeSummary 实体摘要：new_value 是 JSON 字符串（带引号），JSON.parse 去引号；失败原样显示。
    function fmtChangeSummary(log) {
      if (!log.new_value) return profilePlaneMeta(log.entity_kind).label + '变更';
      try { return JSON.parse(log.new_value); } catch (e) { return log.new_value; }
    }
    // profileChangeGroups 按 entity_kind 分组（返回 {kind: [logs]}，组内已按后端 id 正序）。
    const profileChangeGroups = computed(() => {
      const g = {};
      for (const log of (detail.value && detail.value.profile_changes) || []) {
        (g[log.entity_kind] = g[log.entity_kind] || []).push(log);
      }
      return g;
    });
    // goProfilePending 跳转 profile 待确认总览页（复用现有入口；若前端为 hash 路由则 location.hash = '#/profile'）。
    function goProfilePending() { if (location.hash !== '#/profile') location.hash = '#/profile'; }
```

`return { ... }` 中加入：`profilePlaneMeta, profileChangeAction, fmtChangeSummary, profileChangeGroups, goProfilePending,`（以及已有的 `detail`、`fmtTime`）。

> 注意：若前端 profile 入口不是 hash 路由，执行时将 `goProfilePending` 对齐为现有 profile 页的实际导航方式（先确认 `web/index.html` 里 profile 相关 tab/链接怎么跳）。

- [ ] **Step 3: index.html 加区块**

在 `web/index.html` 右栏 `detail-aside` 内，todo 卡片 `</template>` 之后（约 747 行）、`</div><!-- /detail-aside -->`（748 行）之前，插入：

```html
          <!-- 涉及的画像变更（该录音触发的 profile 平面变更，按平面分组；无变更不渲染） -->
          <div v-if="detail && detail.profile_changes && detail.profile_changes.length" style="margin-top:14px">
            <div class="todo-group-title">涉及的画像变更</div>
            <div v-for="(logs, kind) in profileChangeGroups" :key="'pcg'+kind" style="margin-bottom:10px">
              <div class="muted" style="margin:2px 0 4px; font-size:var(--fs-xs)">
                {{ profilePlaneMeta(kind).icon }} {{ profilePlaneMeta(kind).label }}（{{ logs.length }}）
              </div>
              <div class="card sunken" v-for="log in logs" :key="log.id" style="margin-bottom:6px">
                <div style="display:flex; align-items:center; gap:6px">
                  <div style="min-width:0; flex:1; font-size:var(--fs-sm)">{{ fmtChangeSummary(log) }}</div>
                  <span :style="{background:profileChangeAction(log).color, color:'#fff', padding:'1px 7px', borderRadius:'4px', fontSize:'10px', flexShrink:0}">{{ profileChangeAction(log).label }}</span>
                </div>
                <div class="muted" style="font-size:var(--fs-xs); margin-top:3px">
                  {{ log.changed_by === 'llm' ? '🤖 LLM 抽取' : '✋ 手动' }}<template v-if="log.confidence != null"> · 置信 {{ log.confidence.toFixed(2) }}</template> · {{ fmtTime(log.created_at) }}
                  <button v-if="profileChangeAction(log).label === '待确认'" class="btn mini" @click="goProfilePending" style="margin-left:6px">去确认</button>
                </div>
              </div>
            </div>
          </div>

```

- [ ] **Step 4: 构建前端 + 静态检查**

Run（若有 hash-web 目标）：`make hash-web` 或项目既有前端构建命令；否则确认 `web/app.js` 无语法错误（`node --check web/app.js`）。
Expected: 构建产物更新（`web/app.*.js`），`web/app.js` 语法通过。

> 若项目无自动 hash 流程，执行时确认 `web/index.html` 引用的是 `web/app.js` 还是 hash 产物，并同步。

- [ ] **Step 5: Commit**

```bash
git add web/app.js web/index.html
git commit -m "feat(web): 转写详情右栏展示「涉及的画像变更」

todo 卡片下方按平面分组展示该录音触发的 change_log 变更（新增/更新/佐证/
待确认 + LLM/手动 + 置信 + 时间），待确认可跳 profile pending。复用现有卡片样式。"
```

---

## 验证（整体）

- [ ] 后端全绿：`go build ./... && go vet ./... && TEST_MYSQL_DSN="root:root@tcp(127.0.0.1:3307)/zhiwei_test?parseTime=true&charset=utf8mb4&multiStatements=true" go test ./internal/repo/ ./internal/api/ ./internal/profile/`
- [ ] dev 起服务（`make dev` 或 `go run cmd/zhiwei-server`），打开泡泡那条录音（session `2093242790510071808`）的详情页，确认右栏出现「涉及的画像变更」区块：宠物平面 2 条（新增 + 合并更新），摘要显示「泡泡（猫·美短起司）」。
- [ ] 打开一条纯闲聊录音（无 profile 变更），确认区块不渲染。
- [ ] 幂等回归：本计划未改 extract/profile 落库逻辑，`go test ./internal/profile/` 全绿即可。
