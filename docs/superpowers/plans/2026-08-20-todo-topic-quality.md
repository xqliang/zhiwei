# 代办/Topic 质量改进 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: superpowers:subagent-driven-development。Steps use `- [ ]`。逐任务串行（不并行 implementer）。

**Goal:** 代办按名去重(A) + topic 动态合并(B) + 代办时间戳显示(C)。

**Architecture:** 见 `docs/superpowers/specs/2026-08-20-todo-topic-quality-design.md`。A 用 `NormalizeTitle`（repo 纯逻辑）+ commit 落库去重 + `DedupSuggested` 存量清理；C 纯前端；B 用 prompt v3 强化复用 + 前端相似度提示 + LLM consolidate/merge（手动确认）。

**Tech Stack:** Go+chi+sqlx+MySQL、Vue 3 CDN。测试：`make test`（纯逻辑）/ `make test-integration`（DB）。集成单测快路径：`make init-testdb && TEST_MYSQL_DSN="zhiwei:zhiwei@tcp(127.0.0.1:3307)/zhiwei_test?parseTime=true&charset=utf8mb4&multiStatements=true" go test -p 1 -run TestX ./internal/<pkg> -v`。`node --check web/app.js` 验前端语法。

**DAG:** T1(C)∥T2(A.1)∥T5(B.1) 起步 → T3(A.2, 依赖T2) → T4(A.3, 依赖T2) → T6(B.2) → T7(B.3后端) → T8(B.3前端, 依赖T7)。

---

## Task 1 (C): 代办生成/处理时间显示（纯前端）

**Files:** Modify `web/index.html`（三组待办卡各加一行）

- [ ] **Step 1: 改模板** — 待确认/进行中/已完成三组 todo 卡，在标题 `.kv` 行之后加：
```html
      <div class="muted">生成 {{ fmtTime(td.created_at) }}<template v-if="td.status!=='suggested'"> · 处理 {{ fmtTime(td.updated_at) }}</template></div>
```
> 三处待办卡（`v-for="td in suggestedTodos"` / `activeTodos` / `doneTodos`）结构略异：待确认/进行中在 `.kv` 后、topic 徽标前插入；已完成（无 topic 操作）在 `.kv` 后插入。`fmtTime` 已存在；`td.created_at`/`td.updated_at` API 已返回。
- [ ] **Step 2: 跑 hash-web（app.js 未改，但 index.html 改了，src 仍指向旧 hash——无需 bump）** — 跳过（app.js 内容未变，hash 不变）。
- [ ] **Step 3: 验证** — `node --check web/app.js`（OK，未改）；`curl -s http://localhost:8080/ | grep -c '生成 {{'` 应为 3。
- [ ] **Step 4: 提交** — `git add web/index.html && git commit -m "feat(web): 待办卡显示生成/处理时间"`

---

## Task 2 (A.1): NormalizeTitle（纯逻辑）

**Files:** Create `internal/repo/normalize.go`、`internal/repo/normalize_test.go`

- [ ] **Step 1: 写失败测试** — `normalize_test.go`：
```go
package repo

import "testing"

func TestNormalizeTitle(t *testing.T) {
	cases := map[string]string{
		"给 Tom 发邮件": "给tom发邮件",
		"给Tom发邮件":   "给tom发邮件",
		"给 tom 发邮件": "给tom发邮件",
		"Abc! 123":  "abc123",
		"  ":       "",
		"":         "",
	}
	for in, want := range cases {
		if got := NormalizeTitle(in); got != want {
			t.Fatalf("NormalizeTitle(%q)=%q, want %q", in, got, want)
		}
	}
}
```
- [ ] **Step 2: 跑确认失败** — `go test ./internal/repo -run TestNormalizeTitle -v` → FAIL `undefined: NormalizeTitle`。
- [ ] **Step 3: 实现** — `normalize.go`：
```go
package repo

import (
	"strings"
	"unicode"
)

// NormalizeTitle 归一化待办标题用于按名去重：trim + 小写 + 仅保留字母/数字
// （去标点空格），使 "给 Tom 发邮件"/"给Tom发邮件"/"给 tom 发邮件" 归一为同值。
func NormalizeTitle(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(s)) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}
```
- [ ] **Step 4: 跑通过** — `go test ./internal/repo -run TestNormalizeTitle -v` → PASS。
- [ ] **Step 5: 提交** — `git add internal/repo/normalize.go internal/repo/normalize_test.go && git commit -m "feat: NormalizeTitle 归一化标题用于代办按名去重"`

---

## Task 3 (A.2): 落库去重（ListOpenTitles + commit 跳过重复）

**Files:** Modify `internal/repo/todo.go`、`internal/pipeline/stage_extract.go`、`internal/pipeline/stage_extract_test.go`

- [ ] **Step 1: 写失败测试** — `stage_extract_test.go` 加 `TestStageExtractDedupTodoByTitle`：预置一条已存在 open todo（status=confirmed，title「给 Tom 发邮件」），fake LLM 再产出「给Tom发邮件」候选（is_todo）→ 跑 extract → 断言**未新增 todo**（仍 1 条）、memory 照常落库（2 条候选的 memory 都在）。
```go
func TestStageExtractDedupTodoByTitle(t *testing.T) {
	llm := &fakeExtractLLM{} // 产出「给 Tom 发邮件」(todo) + 「学习 Rust」
	d := newExtractDeps(t, llm)
	sid, _ := setupExtractFixture(t, &d)
	ctx := context.Background()
	handler := BuildStages(d)["extract"]
	j := &repo.Job{SessionID: sid, Stage: "extract", Status: "running"}
	_ = handler(ctx, j, sid) // 第一跑：产出 todo「给 Tom 发邮件」(confirmed)
	// 把该 todo 改成 confirmed（闸门 confidence 0.9>=0.85 已是 confirmed；确认它是 open）
	// 第二跑同 fake LLM：标题归一后与已有 todo 撞 → 不应新增
	_ = handler(ctx, j, sid)
	todos, _ := d.Todos.ListBySession(ctx, sid)
	// 该 session 的 todo 数：第一次 1 条；第二次重跑（幂等清理重建）——若去重生效，重建时跳过 dup 仍是 1
	if len(todos) != 1 { t.Fatalf("去重后 todo=%d, want 1", len(todos)) }
}
```
> 注意：commit 重跑会先删本 session todo 再重建；去重要求重建时识别到「已有 open todo 同名则不插」。但重跑删的是**本 session** todo——第一跑的 todo 属本 session，会被删，则去重比对对象没了。**改测试设计**：预置一条**不同 session** 的 open todo「给 Tom 发邮件」(confirmed)，再跑新 session 的 extract（fake LLM 产出「给Tom发邮件」）→ 断言新 session 不新增 todo（命中已有不同-session open todo）。用独立 session/fixture。

- [ ] **Step 2: 跑确认失败** — 集成测试跑红（todo 被新增）。
- [ ] **Step 3: repo 加 ListOpenTitles** — `todo.go`：
```go
// ListOpenTitles 返回未关闭（suggested+confirmed）todo 的标题（落库去重比对用）。
func (r *TodoRepo) ListOpenTitles(ctx context.Context, userID int64) ([]string, error) {
	var titles []string
	err := r.DB.SelectContext(ctx,
		`SELECT title FROM todo WHERE user_id = ? AND status IN ('suggested','confirmed')`, userID)
	return titles, err
}
```
- [ ] **Step 4: commit 加去重** — `stage_extract.go` `commitExtract`，在「5. todo」段前取 open 标题集，循环里跳过命中：
```go
	// 落库去重：新 suggested todo 若归一化标题命中已有未关闭 todo（或批内已加），跳过
	openTitles, err := d.Todos.ListOpenTitles(ctx, userID)
	if err != nil { return fmt.Errorf("读 open todo 标题: %w", err) }
	dupSet := map[string]bool{}
	for _, ti := range openTitles { dupSet[NormalizeTitle(ti)] = true }
```
然后在 todo 循环 `for i, c := range gated { if !c.IsTodo... continue` 后加：
```go
		nk := NormalizeTitle(c.Title)
		if dupSet[nk] {
			continue // 命中已有未关闭 todo，跳过（memory/关联仍由上文处理）
		}
		dupSet[nk] = true // 批内去重
```
> `todoTopicRows` 的构建在 todos 插入后按 `todos` 同序——跳过的候选不进 todos，其 todo_topic 段也跳过（plans 与 todos 同步追加，只对未跳过候选 append）。务必让 `plans`/`todos`/`todoIdx` 三者同序（跳过的候选三处都跳）。
- [ ] **Step 5: 跑通过** — `make init-testdb && TEST_MYSQL_DSN=... go test -p 1 -run 'TestStageExtractDedupTodoByTitle|TestStageExtractCommit' ./internal/pipeline -v` → PASS。
- [ ] **Step 6: 提交** — `git add internal/repo/todo.go internal/pipeline/stage_extract.go internal/pipeline/stage_extract_test.go && git commit -m "feat: 代办落库按归一化标题去重(跳过命中已有未关闭 todo)"`

---

## Task 4 (A.3): 存量清理 DedupSuggested + cmd/dedup-todos

**Files:** Modify `internal/repo/todo.go`、Create `cmd/dedup-todos/main.go`、Test `internal/repo/todo_test.go`

- [ ] **Step 1: 写失败测试** — `todo_test.go` 加 `TestTodoDedupSuggested`：插 3 条 suggested（「给Tom」「给 Tom」「学习Rust」，同 user 不同 session 或同 session 不同 id），调 `DedupSuggested(ctx, 1)` → 断言返回 1（dismissed 1 条 dup），「给Tom」与「给 Tom」保留最旧一条、另一条 dismissed，「学习Rust」不动。
- [ ] **Step 2: 跑确认失败** → `undefined: DedupSuggested`。
- [ ] **Step 3: 实现 DedupSuggested** — `todo.go`：
```go
// DedupSuggested 折叠 suggested todo 的归一化标题重复：每组保留 created_at 最旧一条，
// 其余置 dismissed。单事务。返回 dismissed 数。extract 存量清理用。
func (r *TodoRepo) DedupSuggested(ctx context.Context, userID int64) (int, error) {
	type row struct {
		ID        ids.ID    `db:"id"`
		Title     string    `db:"title"`
		CreatedAt time.Time `db:"created_at"`
	}
	var rows []row
	if err := r.DB.SelectContext(ctx,
		`SELECT id, title, created_at FROM todo WHERE user_id = ? AND status = 'suggested' ORDER BY created_at`, userID); err != nil {
		return 0, err
	}
	keep := map[string]bool{} // norm -> oldest id
	var dismiss []ids.ID
	for _, x := range rows {
		k := NormalizeTitle(x.Title)
		if k == "" { continue }
		if !keep[k] {
			keep[k] = true // 第一条（最旧，因 ORDER BY created_at）保留
			continue
		}
		dismiss = append(dismiss, x.ID)
	}
	if len(dismiss) == 0 { return 0, nil }
	tx, err := r.DB.BeginTxx(ctx, nil)
	if err != nil { return 0, err }
	defer func() { _ = tx.Rollback() }()
	for _, id := range dismiss {
		if _, err := tx.ExecContext(ctx, `UPDATE todo SET status='dismissed' WHERE id=?`, id.Int64()); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil { return 0, err }
	return len(dismiss), nil
}
```
- [ ] **Step 4: 实现 cmd/dedup-todos/main.go**：
```go
package main

import (
	"fmt"
	"log"

	"zhiwei/internal/config"
	"zhiwei/internal/repo"
)

func main() {
	cfg, err := config.Load()
	if err != nil { log.Fatal(err) }
	db, err := repo.NewDB(cfg.MySQLDSN)
	if err != nil { log.Fatal(err) }
	n, err := (&repo.TodoRepo{DB: db}).DedupSuggested(ctx(), 1) // 用 context.Background()
	if err != nil { log.Fatal(err) }
	fmt.Printf("已折叠 %d 条重复 suggested todo\n", n)
}
```
> `ctx()` 用 `context.Background()`（import context）。`config.Load`/`MySQLDSN` 字段以 `internal/config/config.go` 实际为准（读源文件确认）。
- [ ] **Step 5: 跑通过** — `make init-testdb && TEST_MYSQL_DSN=... go test -p 1 -run TestTodoDedupSuggested ./internal/repo -v` → PASS；`go build ./...` → 净。
- [ ] **Step 6: 跑存量清理（dev 库）** — `set -a; source .env; set +a; go run ./cmd/dedup-todos` → 打印清理数。
- [ ] **Step 7: 提交** — `git add internal/repo/todo.go internal/repo/todo_test.go cmd/dedup-todos/main.go && git commit -m "feat: DedupSuggested 存量折叠 + cmd/dedup-todos 一次性清理"`

---

## Task 5 (B.1): prompt v3 强化复用

**Files:** Create `prompts/extraction_v3.md`、Modify `cmd/zhiwei-server/main.go`

- [ ] **Step 1: 写 v3** — 复制 `prompts/extraction_v2.md` 为 `extraction_v3.md`，把第 8 条强化为：
```
8. topic 归属用 topics 数组：优先复用「已有主题列表」中的 topic_id——只要候选主题与某个已有 topic **语义相近**就复用其 id，不要造近重复名（如已有「SDPC俱乐部活动」就别再造「…准备」「…活动准备」）；只有确实无相近已有 topic 才给 suggested_name；一条候选可归入多个主题（0~N 项）；确实无关则 topics 为空数组。
```
- [ ] **Step 2: main.go 改路径** — `const promptPath = "prompts/extraction_v3.md"`。
- [ ] **Step 3: 验证** — `go build ./...`；`make init-testdb && TEST_MYSQL_DSN=... go test -p 1 -run TestStageExtract ./internal/pipeline -v`（fake LLM 不受 prompt 内容影响，应仍绿）。
- [ ] **Step 4: 提交** — `git add prompts/extraction_v3.md cmd/zhiwei-server/main.go && git commit -m "feat: 抽取 prompt v3 强化已有 topic 语义复用避免碎片化"`

---

## Task 6 (B.2): 前端「疑似可合并」提示

**Files:** Modify `web/app.js`、`web/index.html`

- [ ] **Step 1: app.js 加相似度启发** — 在 Topics 段加：
```js
    function normTitle(s) { return (s || '').toLowerCase().replace(/[^\p{L}\p{N}]/gu, ''); }
    // 疑似可合并：与列表中任一其他 topic 归一化后「互为包含」或 Levenshtein 比>0.85
    function suspectOf(t, all) {
      const a = normTitle(t.name);
      if (!a) return null;
      for (const o of all) {
        if (o.id === t.id) continue;
        const b = normTitle(o.name);
        if (!b) continue;
        if (a.includes(b) || b.includes(a)) return o.name;
        if (a.length > 3 && b.length > 3 && similarRatio(a, b) > 0.85) return o.name;
      }
      return null;
    }
    function similarRatio(a, b) {
      const m = a.length, n = b.length;
      const dp = Array.from({length: m+1}, (_, i) => [i, ...Array(n).fill(0)]);
      for (let j = 0; j <= n; j++) dp[0][j] = j;
      for (let i = 1; i <= m; i++) for (let j = 1; j <= n; j++)
        dp[i][j] = a[i-1] === b[j-1] ? dp[i-1][j-1] : 1 + Math.min(dp[i-1][j], dp[i][j-1], dp[i-1][j-1]);
      return 1 - dp[m][n] / Math.max(m, n);
    }
```
> 在 return 暴露 `suspectOf`（模板对每个 topic 调 `suspectOf(t, topics)` 拿疑似对象名）。`topics` 是 `ref`，模板里直接用 `topics`（数组）传参。
- [ ] **Step 2: index.html topic 卡加徽标** — topic 列表卡片（:182 附近 `<div class="card" v-for="t in topics">`）的名称行后加：
```html
            <span v-if="suspectOf(t, topics)" class="badge" style="background:#fef3c7;color:#92400e">疑似可合并: {{ suspectOf(t, topics) }}</span>
```
- [ ] **Step 3: 验证** — `node --check web/app.js` → OK；`make hash-web`（app.js 改了→新 hash）；`curl -s http://localhost:8080/ | grep -c suspectOf` ≥1。
- [ ] **Step 4: 提交** — `git add web/app.js web/index.html && git commit -m "feat(web): topic 列表疑似可合并提示(客户端相似度启发)"`

---

## Task 7 (B.3 后端): consolidate / merge API

**Files:** Create `prompts/topic_consolidate_v1.md`、Modify `internal/api/topic.go`、`internal/repo/topic.go`、`cmd/zhiwei-server/main.go`、Test `internal/api/topic_test.go`

- [ ] **Step 1: 写 merge 失败测试** — `topic_test.go` 加 `TestTopicMerge`：预置 2 topic（A、B）各带 memory_topic + todo_topic 关联 → `POST /api/topics/merge {groups:[{canonical_name:"A", member_ids:[A,B]}]}` → 断言 A 关联聚合（A 原+B 原，去重）、B 置 dismissed、B 关联已删。
- [ ] **Step 2: 跑确认失败** → 路由未注册。
- [ ] **Step 3: 写 consolidate prompt** — `prompts/topic_consolidate_v1.md`：
```markdown
# 知微 topic 合并提议（版本：topic_consolidate_v1）
你是记忆主题整理器。输入是该用户的全部 active/suggested 主题列表。你的任务：找出语义相近、应合并为一的主题组，给出规范名（优先用某成员现名或更简洁的提炼名），输出合并组。
## 规则
1. 只合并**确实语义相近**的（如「SDPC俱乐部划船活动准备」与「SDPC俱乐部划船活动」）；不要合并不同主题。
2. 规范名简短准确（如「SDPC俱乐部活动」）。
3. 不需要合并的不要列出。
## 输出格式
只输出 JSON，无围栏。
{"groups":[{"canonical_name":"SDPC俱乐部活动","member_ids":["<tid1>","<tid2>"]}]}
无合并则 {"groups":[]}。
```
- [ ] **Step 4: repo 加 MergeGroups** — `topic.go`：
```go
// MergeGroups 把多组 topic 合并：每组以 canonical_name 复用已有同名 active/suggested topic
// 或新建（active, ai），把各 member 的 memory_topic/todo_topic 关联 INSERT IGNORE 迁到
// canonical、删 member 关联、member 置 dismissed。单事务。
func (r *TopicRepo) MergeGroups(ctx context.Context, groups []MergeGroup) error {
	tx, err := r.DB.BeginTxx(ctx, nil)
	if err != nil { return err }
	defer func() { _ = tx.Rollback() }()
	for _, g := range groups {
		if len(g.MemberIDs) == 0 { continue }
		// 找/建 canonical
		var cid ids.ID
		if ex, err := r.FindActiveByNameExt(ctx, tx, 1, g.CanonicalName); err != nil {
			return err
		} else if ex != nil {
			cid = ex.ID
		} else {
			tp := &Topic{Name: g.CanonicalName, Status: "active", CreatedBy: "ai"}
			if err := r.CreateExt(ctx, tx, tp); err != nil { return err }
			cid = tp.ID
		}
		for _, mid := range g.MemberIDs {
			if mid == cid { continue }
			// 迁 memory_topic
			if _, err := tx.ExecContext(ctx,
				`INSERT IGNORE INTO memory_topic (memory_id, topic_id, source)
				 SELECT memory_id, ?, source FROM memory_topic WHERE topic_id = ?`, cid.Int64(), mid.Int64()); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `DELETE FROM memory_topic WHERE topic_id = ?`, mid.Int64()); err != nil {
				return err
			}
			// 迁 todo_topic
			if _, err := tx.ExecContext(ctx,
				`INSERT IGNORE INTO todo_topic (todo_id, topic_id, source)
				 SELECT todo_id, ?, source FROM todo_topic WHERE topic_id = ?`, cid.Int64(), mid.Int64()); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `DELETE FROM todo_topic WHERE topic_id = ?`, mid.Int64()); err != nil {
				return err
			}
			// member 置 dismissed
			if _, err := tx.ExecContext(ctx, `UPDATE topic SET status='dismissed' WHERE id = ?`, mid.Int64()); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

type MergeGroup struct {
	CanonicalName string   `json:"canonical_name"`
	MemberIDs     []ids.ID `json:"member_ids"`
}
```
- [ ] **Step 5: api 加 consolidate/merge handler** — `topic.go`：`TopicHandler` 加 `LLM provider.LLMProvider`、`LLMModel`、`ConsolidatePrompt string` 字段；`RegisterTopic` 加 `r.Post("/api/topics/consolidate", h.Consolidate)`、`r.Post("/api/topics/merge", h.Merge)`。
  - `Consolidate`：取 active+suggested topics → 组 LLM 输入 → 调 LLM → 解析 groups（member_ids 字符串→ids.ID）→ 返回 `{"groups":[...]}`。解析容错沿用 ParseCandidates 思路（剥围栏、截 {}）。
  - `Merge`：解码 body `{"groups":[{"canonical_name","member_ids":[string]}]}` → 转 `[]repo.MergeGroup` → `h.Topics.MergeGroups` → 返回 `{"merged": true}`。
- [ ] **Step 6: main.go 接线** — 读 `prompts/topic_consolidate_v1.md` → `api.RegisterTopic(r, &api.TopicHandler{Topics: topics, Memories: memories, Todos: todos, LLM: llm, LLMModel: cfg.LLMFastModel, ConsolidatePrompt: <bytes>})`。
- [ ] **Step 7: 跑通过** — `make init-testdb && TEST_MYSQL_DSN=... go test -p 1 -run 'TestTopicMerge' ./internal/api -v` → PASS；`go build ./...`。
- [ ] **Step 8: 提交** — `git add prompts/topic_consolidate_v1.md internal/api/topic.go internal/repo/topic.go cmd/zhiwei-server/main.go internal/api/topic_test.go && git commit -m "feat: topic 智能合并(consolidate LLM提议 + merge 关联迁移)"`

---

## Task 8 (B.3 前端): 智能合并按钮 + 组编辑/确认 UI

**Files:** Modify `web/app.js`、`web/index.html`

- [ ] **Step 1: app.js 加 consolidate/merge 方法** — Topics 段：
```js
    const mergeDraft = ref(null); // {groups:[{canonical_name, member_ids, memberNames:[]}]
    async function startConsolidate() {
      try {
        const d = await api('POST', '/api/topics/consolidate', {});
        mergeDraft.value = (d.groups || []).map(g => ({ canonical_name: g.canonical_name, member_ids: g.member_ids, memberNames: (g.member_ids||[]).map(id => (topics.value.find(t=>t.id===id)||{}).name || id) }));
        if (!mergeDraft.value.length) toast.value = '暂无需要合并的主题';
      } catch (e) { showError(e); }
    }
    function toggleMergeMember(g, id) {
      const i = g.member_ids.indexOf(id);
      if (i >= 0) g.member_ids.splice(i, 1); else g.member_ids.push(id);
    }
    async function applyMerge() {
      const groups = (mergeDraft.value||[]).filter(g => g.member_ids.length >= 2)
        .map(g => ({ canonical_name: g.canonical_name.trim(), member_ids: g.member_ids }));
      if (!groups.length) { mergeDraft.value = null; return; }
      try {
        await api('POST', '/api/topics/merge', { groups });
        mergeDraft.value = null;
        await loadTopics();
      } catch (e) { showError(e); }
    }
```
> return 暴露 `mergeDraft, startConsolidate, toggleMergeMember, applyMerge`。
- [ ] **Step 2: index.html Topics 列表加「智能合并」按钮 + 草稿面板** — 列表视图（`v-if="!topicDetail"`）顶部「＋ 新建」旁加 `<button class="mini" @click="startConsolidate">智能合并</button>`；下方加草稿面板：
```html
      <div class="card" v-if="mergeDraft">
        <div class="todo-group-title">合并提议（编辑后确认）</div>
        <div v-if="!mergeDraft.length" class="muted">暂无可合并组</div>
        <div v-for="(g, gi) in mergeDraft" :key="gi" style="margin-bottom:10px">
          <input class="txt" v-model="g.canonical_name" style="margin-bottom:4px">
          <div v-for="name in g.memberNames" :key="name" class="muted">
            <label><input type="checkbox" :checked="g.member_ids.includes(g.member_ids[gi])" @change="toggleMergeMember(g, g.member_ids[gi])"> {{ name }}</label>
          </div>
        </div>
        <button class="primary" @click="applyMerge">确认合并</button>
        <button class="mini" @click="mergeDraft = null">取消</button>
      </div>
```
> 上面的 `:checked`/`toggleMergeMember` 索引逻辑写得不够准（`g.member_ids[gi]` 不对）——实现时改为：对每个候选 member id（从 topics 列表里 g.memberNames 对应的 id）渲染 checkbox，`:checked="g.member_ids.includes(id)"` `@change="toggleMergeMember(g, id)"`。需先把 memberNames→id 映射对齐（在 startConsolidate 里同时存 member 对象 {id,name}）。
- [ ] **Step 3: 验证** — `node --check web/app.js`；`make hash-web`；`curl -s http://localhost:8080/ | grep -c 'startConsolidate'` ≥1。
- [ ] **Step 4: 手动验收** — 刷新 → Topics 页点「智能合并」→ 看提议组 → 编辑/确认 → merge → 列表刷新（SDPC 两 topic 合并为一）。
- [ ] **Step 5: 提交** — `git add web/app.js web/index.html && git commit -m "feat(web): topic 智能合并按钮 + 合并组编辑/确认 UI"`

---

## 自检
- **spec 覆盖**：A(A.1-A.4)↔T2-T4；C↔T1；B.1↔T5、B.2↔T6、B.3↔T7-T8。无遗漏。
- **占位**：T8 Step2 的 checkbox 索引逻辑已注明实现时修正（用 {id,name} 对齐）。
- **类型一致**：`NormalizeTitle`（T2）在 T3/T4 引用一致；`MergeGroup`（T7）api↔repo 一致；`ListOpenTitles`/`DedupSuggested`（T3/T4）签名一致。
