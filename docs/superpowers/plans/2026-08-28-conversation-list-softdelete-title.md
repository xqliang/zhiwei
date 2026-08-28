# 会话列表页：软删除 + 标题编辑 + 自动生成标题 实现计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 为「问知微」对话列表页加软删除（=archived）、手动编辑标题（title_source=manual）、模型自动生成简短标题（第 2 轮后异步、auto、manual 优先、失败静默）。

**Architecture:** 数据层给 `agent_conversation` 加 `title_source` 列 + 新增 repo 方法（UpdateTitle/Archive/TitleState/CountByConversation）；API 层加 PATCH/DELETE/title-generate 三端点；自动生成标题经 orchestrator 可选回调 `OnTurnComplete` 挂载，真正逻辑在 `agent/title.go`（generateTitle），由 main.go 注入现有 `llm`/`agentModel`；前端列表项加行内编辑/删除按钮。全程沿用 user_id 行级 IDOR 防护。

**Tech Stack:** Go + chi + sqlx + MySQL（migrate）；Vue3 CDN（无构建，`web/app.js`+`index.html`，改完 `make hash-web`）；火山 Ark LLM（doubao-seed-1-6）。

**测试约定（务必遵守）：**
- repo 集成测试用 `repotest.DSN(t)`（`internal/repotest`，按包隔离库 `zhiwei_test_repo`，已内嵌迁移）；未设 `TEST_MYSQL_DSN` 会 skip。
- agent 包测试用 `orchDSN(t)`（`internal/agent/orchestrator_test.go:14`，读 `TEST_MYSQL_DSN`，未设 skip）。
- handler 测试用 `injectUser(uid)` 中间件模拟 authGate 注入用户（`internal/agent/handlers_test.go:19`）。
- `ids.ID` 是 `int64`：`.Int64()` / `.String()` / `ids.New()` / `ids.ParseID(s)`（`internal/ids/ids.go`）。
- 迁移号：main 最新 `000019`，本特性用 **`000020`**。⚠️ 合并回 main 前核对 main 最新号重编号（撞号坑，见 memory `[[zhiwei-db-per-feature-convention]]`）。
- 每任务末尾提交；go 改动后 `go build ./...` + `go vet ./...`。

---

### Task 1: 迁移 `000020_conversation_title`

**Files:**
- Create: `migrations/000020_conversation_title.up.sql`
- Create: `migrations/000020_conversation_title.down.sql`

- [ ] **Step 1: 写 up 迁移**

`migrations/000020_conversation_title.up.sql`：
```sql
-- 区分标题来源：''(未设/占位) | 'manual'(用户手动改) | 'auto'(模型生成)。
-- 用于「自动生成标题」判定：manual 永不覆盖；空/auto 可生成。
-- 合并回 main 前核对 main 最新迁移号，必要时重编号（并行分支撞号坑）。
ALTER TABLE agent_conversation
  ADD COLUMN title_source VARCHAR(16) NOT NULL DEFAULT '' AFTER title;
```

- [ ] **Step 2: 写 down 迁移**

`migrations/000020_conversation_title.down.sql`：
```sql
ALTER TABLE agent_conversation DROP COLUMN title_source;
```

- [ ] **Step 3: 对本地测试库应用迁移并验证列存在**

Run（本地 MySQL，库 `zhiwei_test_repo` 或你的隔离库；`$DSN` 换成实际）：
```bash
go run ./cmd/migrate -dsn "$TEST_MYSQL_DSN" up
```
Expected：无报错。然后验证列：
```bash
mysql -e "SHOW COLUMNS FROM agent_conversation LIKE 'title_source'" zhiwei_test_repo
```
Expected：输出一行 `title_source | varchar(16) | NO | | ''`。

- [ ] **Step 4: 提交**

```bash
git add migrations/000020_conversation_title.up.sql migrations/000020_conversation_title.down.sql
git commit -m "feat(agent): 迁移 000020 加 title_source 列区分标题来源

manual=用户手动改(永不覆盖)、auto=模型生成、空=未设/占位。
供自动生成标题判定用。"
```

---

### Task 2: repo — AgentConversation 加 TitleSource + UpdateTitle + TitleState

**Files:**
- Modify: `internal/repo/agent_conversation.go`
- Test: `internal/repo/agent_conversation_test.go`

- [ ] **Step 1: 写失败测试 — UpdateTitle 与 TitleState**

在 `internal/repo/agent_conversation_test.go` 追加：
```go
func TestAgentConversationUpdateTitle(t *testing.T) {
	db, err := NewDB(repotest.DSN(t))
	if err != nil {
		t.Fatal(err)
	}
	r := &AgentConversationRepo{DB: db}
	ctx := t.Context()

	c := &AgentConversation{Title: "原标题"}
	if err := r.Create(ctx, c); err != nil {
		t.Fatal(err)
	}

	// 手动改标题 → source=manual
	if err := r.UpdateTitle(ctx, 1, c.ID, "用户改的标题", "manual"); err != nil {
		t.Fatalf("UpdateTitle: %v", err)
	}
	title, source, err := r.TitleState(ctx, 1, c.ID)
	if err != nil {
		t.Fatalf("TitleState: %v", err)
	}
	if title != "用户改的标题" || source != "manual" {
		t.Errorf("got title=%q source=%q, want 用户改的标题/manual", title, source)
	}

	// 越权：user_id=2 改不到 user_id=1 的会话 → ErrNoRows
	if err := r.UpdateTitle(ctx, 2, c.ID, "越权改", "manual"); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("越权 UpdateTitle 应 ErrNoRows, got %v", err)
	}
	// 越权读也拿不到
	if _, _, err := r.TitleState(ctx, 2, c.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("越权 TitleState 应 ErrNoRows, got %v", err)
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/repo/ -run TestAgentConversationUpdateTitle -v -count=1`
Expected: FAIL — `r.UpdateTitle` / `r.TitleState` 未定义（编译错）。

- [ ] **Step 3: 实现结构体字段 + 两个方法**

`internal/repo/agent_conversation.go`：
- 结构体加字段（`:16` 后）：
```go
TitleSource string `db:"title_source" json:"title_source"` // ''|manual|auto
```
- 加 import `"database/sql"`。
- 在 `SetDSHSession` 后加：
```go
// UpdateTitle 改标题并标记来源（manual|auto）。行级 user_id 过滤（IDOR）：越权/不存在 → 0 行 → ErrNoRows。
func (r *AgentConversationRepo) UpdateTitle(ctx context.Context, userID int64, id ids.ID, title, source string) error {
	res, err := r.DB.ExecContext(ctx,
		`UPDATE agent_conversation SET title=?, title_source=? WHERE id=? AND user_id=?`,
		title, source, id.Int64(), userID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// TitleState 取标题与来源（自动生成判定用）。行级 user_id 过滤：越权/不存在 → ErrNoRows。
func (r *AgentConversationRepo) TitleState(ctx context.Context, userID int64, id ids.ID) (title, source string, err error) {
	err = r.DB.QueryRowContext(ctx,
		`SELECT title, title_source FROM agent_conversation WHERE id=? AND user_id=?`,
		id.Int64(), userID).Scan(&title, &source)
	return title, source, err
}
```

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/repo/ -run TestAgentConversationUpdateTitle -v -count=1`
Expected: PASS。

- [ ] **Step 5: 提交**

```bash
git add internal/repo/agent_conversation.go internal/repo/agent_conversation_test.go
git commit -m "feat(repo): AgentConversation 加 title_source + UpdateTitle/TitleState"
```

---

### Task 3: repo — Archive 软删除 + List 保持只查 active

**Files:**
- Modify: `internal/repo/agent_conversation.go`
- Test: `internal/repo/agent_conversation_test.go`

- [ ] **Step 1: 写失败测试 — Archive 幂等软删 + 越权**

在 `internal/repo/agent_conversation_test.go` 追加：
```go
func TestAgentConversationArchive(t *testing.T) {
	db, err := NewDB(repotest.DSN(t))
	if err != nil {
		t.Fatal(err)
	}
	r := &AgentConversationRepo{DB: db}
	ctx := t.Context()

	c := &repo.AgentConversation{Title: "待归档"}
	if err := r.Create(ctx, c); err != nil {
		t.Fatal(err)
	}

	// 归档：成功
	if err := r.Archive(ctx, 1, c.ID); err != nil {
		t.Fatalf("Archive: %v", err)
	}
	// List（只查 active）不再含它
	list, err := r.List(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	for _, x := range list {
		if x.ID == c.ID {
			t.Error("已归档会话不应出现在 List(active) 中")
		}
	}
	// 但 Get 仍能读到（status=archived）
	got, err := r.Get(ctx, 1, c.ID)
	if err != nil {
		t.Fatalf("Get 归档会话: %v", err)
	}
	if got.Status != "archived" {
		t.Errorf("status 应为 archived, got %q", got.Status)
	}

	// 幂等：再次 Archive 不报错（已是 archived，0 行但返回 nil）
	if err := r.Archive(ctx, 1, c.ID); err != nil {
		t.Errorf("重复 Archive 应幂等无错, got %v", err)
	}

	// 越权：user_id=2 归档 user_id=1 的会话 → 0 行，不报错（幂等语义，非 ErrNoRows）
	if err := r.Archive(ctx, 2, c.ID); err != nil {
		t.Errorf("越权 Archive 应幂等无错, got %v", err)
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/repo/ -run TestAgentConversationArchive -v -count=1`
Expected: FAIL — `r.Archive` 未定义。

- [ ] **Step 3: 实现 Archive**

在 `internal/repo/agent_conversation.go` 的 `UpdateTitle` 后加：
```go
// Archive 软删除：status→archived。幂等（已是 archived 则 0 行、返回 nil，不报错）。
// 行级 user_id 过滤：越权行 n=0 同样返回 nil（软删语义无「不存在即错」）。
func (r *AgentConversationRepo) Archive(ctx context.Context, userID int64, id ids.ID) error {
	_, err := r.DB.ExecContext(ctx,
		`UPDATE agent_conversation SET status='archived' WHERE id=? AND user_id=? AND status='active'`,
		id.Int64(), userID)
	return err
}
```

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/repo/ -run TestAgentConversationArchive -v -count=1`
Expected: PASS。

- [ ] **Step 5: 提交**

```bash
git add internal/repo/agent_conversation.go internal/repo/agent_conversation_test.go
git commit -m "feat(repo): AgentConversation.Archive 幂等软删除(=archived)"
```

---

### Task 4: repo — CountByConversation 统计用户消息数

**Files:**
- Modify: `internal/repo/agent_message.go`
- Test: `internal/repo/agent_message_test.go`

- [ ] **Step 1: 写失败测试 — CountByConversation**

在 `internal/repo/agent_message_test.go` 追加（先确认该文件 import；补 `"zhiwei/internal/ids"` 若缺）：
```go
func TestCountByConversation(t *testing.T) {
	db, err := NewDB(repotest.DSN(t))
	if err != nil {
		t.Fatal(err)
	}
	convRepo := &AgentConversationRepo{DB: db}
	msgRepo := &AgentMessageRepo{DB: db}
	ctx := t.Context()

	c := &AgentConversation{Title: "计数用"}
	if err := convRepo.Create(ctx, c); err != nil {
		t.Fatal(err)
	}
	cid := &c.ID

	// 2 条 user + 1 条 assistant
	for _, m := range []*AgentMessage{
		{ConversationID: cid, Role: "user", Content: "问1"},
		{ConversationID: cid, Role: "assistant", Content: "答1"},
		{ConversationID: cid, Role: "user", Content: "问2"},
	} {
		if err := msgRepo.Append(ctx, m); err != nil {
			t.Fatal(err)
		}
	}

	n, err := msgRepo.CountByConversation(ctx, 1, c.ID)
	if err != nil {
		t.Fatalf("CountByConversation: %v", err)
	}
	if n != 2 {
		t.Errorf("user 消息数应为 2, got %d", n)
	}

	// 越权：user_id=2 看不到 user_id=1 的消息 → 0
	n2, err := msgRepo.CountByConversation(ctx, 2, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if n2 != 0 {
		t.Errorf("越权计数应为 0, got %d", n2)
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/repo/ -run TestCountByConversation -v -count=1`
Expected: FAIL — `CountByConversation` 未定义。

- [ ] **Step 3: 实现 CountByConversation**

在 `internal/repo/agent_message.go` 的 `ListByConversation` 后加：
```go
// CountByConversation 统计某会话的 user 消息数（判定是否到第 2 轮）。行级 user_id 过滤。
func (r *AgentMessageRepo) CountByConversation(ctx context.Context, userID int64, convID ids.ID) (int, error) {
	var n int
	err := r.DB.GetContext(ctx, &n,
		`SELECT COUNT(*) FROM agent_message WHERE conversation_id=? AND user_id=? AND role='user'`,
		convID.Int64(), userID)
	return n, err
}
```

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/repo/ -run TestCountByConversation -v -count=1`
Expected: PASS。

- [ ] **Step 5: 提交**

```bash
git add internal/repo/agent_message.go internal/repo/agent_message_test.go
git commit -m "feat(repo): AgentMessage.CountByConversation 统计用户消息数(判第2轮)"
```

---

### Task 5: agent — 自动生成标题逻辑 `title.go`（纯函数，无 provider 依赖）

**Files:**
- Create: `internal/agent/title.go`
- Test: `internal/agent/title_test.go`

用接口抽象 LLM，让测试可注入 fake，orchestrator/title 都不直接依赖 `provider`。

- [ ] **Step 1: 写失败测试 — sanitizeTitle + shouldGenerate + buildTitleInput + GenerateTitle 全流程**

创建 `internal/agent/title_test.go`：
```go
package agent

import (
	"context"
	"errors"
	"strings"
	"testing"

	"zhiwei/internal/repo"
)

// fakeLLM 实现 titleLLM，断言 Chat 入参并返回预设内容/错误。
type fakeLLM struct {
	out string
	err error
}

func (f *fakeLLM) Chat(_ context.Context, _ titleChatReq) (string, error) {
	return f.out, f.err
}

func TestSanitizeTitle(t *testing.T) {
	cases := map[string]string{
		`"项目排期讨论"`:      "项目排期讨论",
		"《周报整理》":        "周报整理",
		"关于待办。":          "关于待办",
		"  带空格的标题  ":    "带空格的标题",
		"第一行\n第二行":      "第一行",
	}
	for in, want := range cases {
		if got := sanitizeTitle(in); got != want {
			t.Errorf("sanitizeTitle(%q)=%q want %q", in, got, want)
		}
	}
}

func TestShouldGenerate(t *testing.T) {
	cases := []struct {
		source string
		count  int
		title  string
		want   bool
	}{
		{"", 2, "", true},            // 空标题 + 第2轮 → 生成
		{"", 2, "新对话", true},       // 占位标题 → 生成
		{"auto", 3, "旧自动标题", true}, // auto + 仍可覆盖
		{"manual", 2, "", false},     // 手动改过 → 永不覆盖
		{"", 1, "", false},           // 未到第 2 轮
		{"", 2, "真实标题", false},    // 有真实标题且非 auto → 不生成
	}
	for _, c := range cases {
		if got := shouldGenerate(c.title, c.source, c.count); got != c.want {
			t.Errorf("shouldGenerate(%q,%q,%d)=%v want %v", c.title, c.source, c.count, got, c.want)
		}
	}
}

func TestGenerateTitleDeps(t *testing.T) {
	// 用最小 repo 替身验证 deps 接线（title/count/update 被正确调用）。
	msgs := []repo.AgentMessage{
		{Role: "user", Content: "帮我看下本周待办"},
		{Role: "assistant", Content: "好的，以下是待办"},
	}
	deps := &titleDeps{
		state:  titleState{title: "", source: ""},
		count:  2,
		msgs:   msgs,
		llm:    &fakeLLM{out: `"本周待办梳理"`},
	}
	got, err := deps.generate(context.Background())
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if got != "本周待办梳理" {
		t.Errorf("title=%q", got)
	}
	if deps.updatedTo != "本周待办梳理" || deps.updatedSrc != "auto" {
		t.Errorf("未写入 auto: %+v", deps)
	}
}

func TestGenerateTitleManualSkip(t *testing.T) {
	deps := &titleDeps{state: titleState{title: "x", source: "manual"}, count: 5}
	if _, err := deps.generate(context.Background()); !errors.Is(err, errTitleSkip) {
		t.Errorf("manual 应跳过(errTitleSkip), got %v", err)
	}
}

func TestGenerateTitleLLMFailSilent(t *testing.T) {
	deps := &titleDeps{
		state: titleState{title: "", source: ""},
		count: 2,
		llm:   &fakeLLM{err: errors.New("boom")},
	}
	if _, err := deps.generate(context.Background()); !errors.Is(err, errTitleSkip) {
		t.Errorf("LLM 失败应静默跳过(errTitleSkip), got %v", err)
	}
}

// 保证生成标题不含换行/引号（对 LLM 输出鲁棒性回归）
func TestGenerateTitleNoGarbage(t *testing.T) {
	deps := &titleDeps{
		state: titleState{title: "", source: ""},
		count: 2,
		msgs:  []repo.AgentMessage{{Role: "user", Content: "hi"}},
		llm:   &fakeLLM{out: "标题\n还有解释文字"},
	}
	got, err := deps.generate(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if strings.ContainsAny(got, "\n") {
		t.Errorf("标题不应含换行: %q", got)
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/agent/ -run 'TestSanitizeTitle|TestShouldGenerate|TestGenerateTitle' -v -count=1`
Expected: FAIL — `title.go` 未定义（sanitizeTitle/shouldGenerate/titleDeps 等未定义）。

- [ ] **Step 3: 实现 title.go**

创建 `internal/agent/title.go`：
```go
package agent

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"unicode/utf8"

	"zhiwei/internal/ids"
	"zhiwei/internal/provider"
	"zhiwei/internal/repo"
)

// titleLLM 抽象 LLM 调用（测试注入 fake，生产传 provider.LLMProvider 适配器）。
type titleLLM interface {
	Chat(ctx context.Context, req titleChatReq) (string, error)
}

type titleChatReq struct {
	Model       string
	System      string
	User        string
	Temperature float64
}

// llmAdapter 把 provider.LLMProvider 适配成 titleLLM。
type llmAdapter struct{ p provider.LLMProvider }

func (a llmAdapter) Chat(ctx context.Context, req titleChatReq) (string, error) {
	resp, err := a.p.Chat(ctx, provider.ChatRequest{
		Model: req.Model, System: req.System, User: req.User, Temperature: req.Temperature,
	})
	return resp.Content, err
}

// errTitleSkip 表示「本次不生成/生成失败但静默跳过」——非错误，调用方据此不报错。
var errTitleSkip = errors.New("title generation skipped")

// titlePrompt 系统指令：要求 ≤15 字中文短标题、只输出标题本身。
const titlePrompt = "根据下面的对话内容，生成一个不超过15个中文字符的简短标题。" +
	"只输出标题本身，不要加引号、不要解释、不要标点结尾、不要换行。"

// titleSourceManual/Auto 标题来源取值（与 DB title_source 列一致）。
const (
	titleSourceManual = "manual"
	titleSourceAuto   = "auto"
)

// placeholderTitle 列表占位标题（与前端 c.title || '新对话' 一致）。
const placeholderTitle = "新对话"

// sanitizeTitle 清洗 LLM 输出：去首尾空白/引号/书名号/句末标点、只取首行、截断 256。
func sanitizeTitle(s string) string {
	s = strings.TrimSpace(s)
	// 只取第一行（LLM 可能附解释）
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	s = strings.TrimSpace(s)
	s = strings.Trim(s, `"'`+"“”‘’《》")
	s = strings.TrimRight(s, "。.！!？?，,、；;：:")
	return truncateRunes(s, 256)
}

// truncateRunes 按 rune 截断到 max，避免截断多字节字符。
func truncateRunes(s string, max int) string {
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	return string([]rune(s)[:max])
}

// shouldGenerate 判定是否需要生成：第 2 轮后、标题为空/占位/auto、且非 manual。
func shouldGenerate(title, source string, userCount int) bool {
	if source == titleSourceManual {
		return false
	}
	if userCount < 2 {
		return false
	}
	return title == "" || title == placeholderTitle || source == titleSourceAuto
}

// buildTitleInput 把对话前若干条拼成给 LLM 的 user 文本。
func buildTitleInput(msgs []repo.AgentMessage) string {
	var b strings.Builder
	for _, m := range msgs {
		if m.Role != "user" && m.Role != "assistant" {
			continue
		}
		if m.Kind != "" && m.Kind != "text" {
			continue // 跳过工具调用/结果/思考，只留纯对话文本
		}
		role := "用户"
		if m.Role == "assistant" {
			role = "助手"
		}
		content := m.Content
		if len([]rune(content)) > 200 {
			content = string([]rune(content)[:200]) // 单条限长，控 prompt 体积
		}
		fmt.Fprintf(&b, "%s：%s\n", role, content)
	}
	return b.String()
}

// ---- deps：把 repo 访问与 LLM 抽成可注入的依赖，便于单测与生产装配 ----

// titleState 生成前会话的标题状态快照。
type titleState struct {
	title  string
	source string
}

// titleDeps 自动生成标题的依赖（测试替身可字段注入，生产用 newTitleDeps 接 repo）。
type titleDeps struct {
	state      titleState
	count      int
	msgs       []repo.AgentMessage
	llm        titleLLM
	model      string
	updatedTo  string // 测试断言：最终写入的标题
	updatedSrc string // 测试断言：最终写入的来源
}

// newTitleDeps 从 repo 构造生产依赖。llm 须是 titleLLM（生产传 llmAdapter{provider}）。
func newTitleDeps(ctx context.Context, uid int64, cid ids.ID, convs *repo.AgentConversationRepo,
	msgs *repo.AgentMessageRepo, llm titleLLM, model string) *titleDeps {
	d := &titleDeps{llm: llm, model: model}
	if t, s, err := convs.TitleState(ctx, uid, cid); err == nil {
		d.state = titleState{t, s}
	}
	if n, err := msgs.CountByConversation(ctx, uid, cid); err == nil {
		d.count = n
	}
	if list, err := msgs.ListByConversation(ctx, uid, cid); err == nil {
		d.msgs = list
	}
	return d
}

// generate 执行一次生成判定+生成。返回 (新标题, err)：errTitleSkip 表示跳过（静默），
// 其余 err 为 repo/LLM 真实错误（调用方也应静默，仅记日志）。成功返回已清洗标题。
func (d *titleDeps) generate(ctx context.Context) (string, error) {
	if !shouldGenerate(d.state.title, d.state.source, d.count) {
		return "", errTitleSkip
	}
	out, err := d.llm.Chat(ctx, titleChatReq{
		Model: d.model, System: titlePrompt, User: buildTitleInput(d.msgs), Temperature: 0.3,
	})
	if err != nil {
		return "", errTitleSkip // LLM 失败静默
	}
	title := sanitizeTitle(out)
	if title == "" {
		return "", errTitleSkip
	}
	d.updatedTo, d.updatedSrc = title, titleSourceAuto
	return title, nil
}

// GenerateTitle 生产入口：拉 repo 状态 → 判定 → 生成 → 写回 auto。任何非致命路径都静默
// （失败/跳过仅记日志）。ctx 脱离请求、带超时；内部读-判-写避免覆盖用户刚改的 manual。
// llm 是 provider.LLMProvider，内部适配成 titleLLM（title.go 不直接依赖 provider 之外的耦合）。
func GenerateTitle(ctx context.Context, uid int64, cid ids.ID, convs *repo.AgentConversationRepo,
	msgs *repo.AgentMessageRepo, llm provider.LLMProvider, model string) {
	d := newTitleDeps(ctx, uid, cid, convs, msgs, llmAdapter{llm}, model)
	title, err := d.generate(ctx)
	if err != nil { // errTitleSkip 或真实错误都静默
		if !errors.Is(err, errTitleSkip) {
			log.Printf("[agent] 自动生成标题失败(静默) conv=%s: %v", cid, err)
		}
		return
	}
	// CAS：写回前再读一次 source，若已被并发改成 manual 则放弃，绝不覆盖用户刚改的标题。
	if _, s, e := convs.TitleState(ctx, uid, cid); e == nil && s == titleSourceManual {
		return
	}
	if err := convs.UpdateTitle(ctx, uid, cid, title, titleSourceAuto); err != nil {
		log.Printf("[agent] 写回自动标题失败(静默) conv=%s: %v", cid, err)
	}
}
```

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/agent/ -run 'TestSanitizeTitle|TestShouldGenerate|TestGenerateTitle' -v -count=1`
Expected: PASS（全部纯单测，无需 DB）。

- [ ] **Step 5: 提交**

```bash
git add internal/agent/title.go internal/agent/title_test.go
git commit -m "feat(agent): 自动生成标题逻辑 title.go(第2轮后/auto/manual优先/失败静默)"
```

---

### Task 6: orchestrator — OnTurnComplete 回调钩子

**Files:**
- Modify: `internal/agent/orchestrator.go`
- Test: `internal/agent/orchestrator_test.go`

- [ ] **Step 1: 写失败测试 — 每轮收尾回调被调用**

在 `internal/agent/orchestrator_test.go` 追加：
```go
// TestOrchestratorOnTurnComplete 锁定：每轮 runTurn 收尾会调用 OnTurnComplete（若装配）。
func TestOrchestratorOnTurnComplete(t *testing.T) {
	db, err := repo.NewDB(orchDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	convRepo := &repo.AgentConversationRepo{DB: db}
	msgRepo := &repo.AgentMessageRepo{DB: db}
	ctx := t.Context()
	conv := &repo.AgentConversation{Title: "t"}
	if err := convRepo.Create(ctx, conv); err != nil {
		t.Fatal(err)
	}
	if full, err := convRepo.Get(ctx, 1, conv.ID); err == nil {
		conv = full
	}

	msgData, _ := json.Marshal(map[string]any{"message": map[string]any{"content": []map[string]any{
		{"type": "text", "text": "答复"},
	}}})
	fake := &FakeRuntime{Script: [][]Event{{{Type: EvAssistantMessage, Data: msgData}}}}
	orch := NewOrchestrator(rtFor(fake), convRepo, msgRepo)

	called := 0
	var gotConvID ids.ID
	orch.OnTurnComplete = func(_ context.Context, c *repo.AgentConversation) {
		called++
		gotConvID = c.ID
	}
	if _, err := orch.RunTurn(ctx, conv, "你好"); err != nil {
		t.Fatalf("RunTurn: %v", err)
	}
	if called != 1 {
		t.Errorf("OnTurnComplete 应被调用 1 次, got %d", called)
	}
	if gotConvID != conv.ID {
		t.Errorf("回调收到的 convID=%s want %s", gotConvID, conv.ID)
	}
}
```
（若 `orchestrator_test.go` 未 import `ids`，补 `"zhiwei/internal/ids"`。）

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/agent/ -run TestOrchestratorOnTurnComplete -v -count=1`
Expected: FAIL — `orch.OnTurnComplete` 未定义（或回调未被调用）。

- [ ] **Step 3: 实现回调字段 + 收尾调用**

`internal/agent/orchestrator.go`：
- 结构体加字段（`Persona` 后）：
```go
	// OnTurnComplete 可选：每轮 runTurn 收尾（Touch 之后）调用，供主装配挂「自动生成标题」等
	// 每轮副作用。必须快速返回、不得阻塞 runTurn——耗时工作由实现方自行起 goroutine。
	// nil → 不调用（既有行为/测试不变）。
	OnTurnComplete func(ctx context.Context, conv *repo.AgentConversation)
```
- 在 `runTurn` 的 `_ = o.Conversations.Touch(ctx, conv.ID)`（`:176`）之后、`send(StreamFrame{Type: "turn_end"...})`（`:178`）之前，加：
```go
	// 每轮收尾钩子（可选）：自动生成标题等副作用。快速返回，耗时工作由回调自行异步。
	if o.OnTurnComplete != nil {
		o.OnTurnComplete(ctx, conv)
	}
```

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/agent/ -run TestOrchestratorOnTurnComplete -v -count=1`
Expected: PASS。

- [ ] **Step 5: 全包回归 + 提交**

Run: `go build ./... && go vet ./... && go test ./internal/agent/ -count=1`
Expected: build/vet 干净，测试 PASS（DB 测试 skip 若未设 DSN）。

```bash
git add internal/agent/orchestrator.go internal/agent/orchestrator_test.go
git commit -m "feat(agent): orchestrator 加 OnTurnComplete 每轮收尾回调钩子"
```

---

### Task 7: handler — PATCH 改标题 / DELETE 软删 / title/generate

**Files:**
- Modify: `internal/agent/handlers.go`
- Test: `internal/agent/handlers_test.go`

- [ ] **Step 1: 写失败测试 — 三端点**

在 `internal/agent/handlers_test.go` 追加：
```go
// TestConversationTitleAndDelete 锁定：PATCH 改标题(manual)、DELETE 软删(列表消失)、越权 404。
func TestConversationTitleAndDelete(t *testing.T) {
	db, err := repo.NewDB(orchDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	convRepo := &repo.AgentConversationRepo{DB: db}
	msgRepo := &repo.AgentMessageRepo{DB: db}
	ctx := t.Context()
	conv := &repo.AgentConversation{Title: "原标题"}
	if err := convRepo.Create(ctx, conv); err != nil {
		t.Fatal(err)
	}
	h := &AgentHandler{Conversations: convRepo, Messages: msgRepo}
	r := chi.NewRouter()
	r.Use(injectUser(1))
	RegisterAgent(r, h)
	cid := conv.ID.String()

	// PATCH 改标题 → 200，title_source=manual
	patch := httptest.NewRequest("PATCH", "/api/agent/conversations/"+cid,
		strings.NewReader(`{"title":"手动标题"}`))
	prec := httptest.NewRecorder()
	r.ServeHTTP(prec, patch)
	if prec.Code != http.StatusOK {
		t.Fatalf("PATCH code=%d body=%s", prec.Code, prec.Body.String())
	}
	var out repo.AgentConversation
	_ = json.Unmarshal(prec.Body.Bytes(), &out)
	if out.Title != "手动标题" || out.TitleSource != "manual" {
		t.Errorf("PATCH 结果异常: %+v", out)
	}

	// DELETE 软删 → 204
	del := httptest.NewRequest("DELETE", "/api/agent/conversations/"+cid, nil)
	drec := httptest.NewRecorder()
	r.ServeHTTP(drec, del)
	if drec.Code != http.StatusNoContent {
		t.Fatalf("DELETE code=%d body=%s", drec.Code, drec.Body.String())
	}
	// 列表查不到
	list, err := convRepo.List(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	for _, x := range list {
		if x.ID.String() == cid {
			t.Error("已软删会话不应在列表中")
		}
	}

	// 越权：user_id=2 操作 user_id=1 的会话 → 404
	conv2 := &repo.AgentConversation{Title: "user2 看不到的"}
	if err := convRepo.Create(ctx, conv2); err != nil {
		t.Fatal(err)
	}
	r2 := chi.NewRouter()
	r2.Use(injectUser(2))
	RegisterAgent(r2, h)
	p2 := httptest.NewRequest("PATCH", "/api/agent/conversations/"+conv2.ID.String(),
		strings.NewReader(`{"title":"越权"}`))
	p2rec := httptest.NewRecorder()
	r2.ServeHTTP(p2rec, p2)
	if p2rec.Code != http.StatusNotFound {
		t.Errorf("越权 PATCH 应 404, got %d", p2rec.Code)
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/agent/ -run TestConversationTitleAndDelete -v -count=1`
Expected: FAIL — PATCH/DELETE 路由不存在（404 或方法不允许）。

- [ ] **Step 3: 实现 handler 方法 + 路由**

`internal/agent/handlers.go`：
- 路由（`RegisterAgent`，在 `r.Get("/api/agent/conversations/{cid}", h.getConversation)` 后）加：
```go
	r.Patch("/api/agent/conversations/{cid}", h.patchConversation)
	r.Delete("/api/agent/conversations/{cid}", h.deleteConversation)
	r.Post("/api/agent/conversations/{cid}/title/generate", h.generateTitle)
```
- 加方法（`getConversation` 后）：
```go
// patchConversation 手动改标题：写 title_source=manual。越权/不存在 → 404。
func (h *AgentHandler) patchConversation(w http.ResponseWriter, r *http.Request) {
	uid, ok := reqUserID(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	cid, err := ids.ParseID(chi.URLParam(r, "cid"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid cid"})
		return
	}
	var body struct {
		Title string `json:"title"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid body"})
		return
	}
	title := strings.TrimSpace(body.Title)
	if title == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "title required"})
		return
	}
	if err := h.Conversations.UpdateTitle(r.Context(), uid, cid, title, titleSourceManual); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "conversation not found"})
		return
	}
	full, err := h.Conversations.Get(r.Context(), uid, cid)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "conversation not found"})
		return
	}
	writeJSON(w, http.StatusOK, full)
}

// deleteConversation 软删除：status→archived。幂等（已归档也 204）。越权/不存在 → 404。
func (h *AgentHandler) deleteConversation(w http.ResponseWriter, r *http.Request) {
	uid, ok := reqUserID(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	cid, err := ids.ParseID(chi.URLParam(r, "cid"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid cid"})
		return
	}
	// 先确认存在且归属当前用户（Archive 幂等不报错，越权需显式 404）。
	if _, err := h.Conversations.Get(r.Context(), uid, cid); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "conversation not found"})
		return
	}
	if err := h.Conversations.Archive(r.Context(), uid, cid); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// generateTitle 手动触发一次自动生成（兜底）。装配了 Gen 才可用，否则 503。
func (h *AgentHandler) generateTitle(w http.ResponseWriter, r *http.Request) {
	uid, ok := reqUserID(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	cid, err := ids.ParseID(chi.URLParam(r, "cid"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid cid"})
		return
	}
	if h.Gen == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "标题生成不可用"})
		return
	}
	title, err := h.Gen(r.Context(), uid, cid)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"title": title, "title_source": titleSourceAuto})
}
```
- 结构体 `AgentHandler` 加字段（`Hub` 后）：
```go
	Gen func(ctx context.Context, uid int64, cid ids.ID) (string, error) // 手动生成标题（nil 时端点 503）
```
- 加 import `"strings"`（若未 import；检查现有 import 块）。

> 注：`titleSourceManual` 在 `title.go` 已定义（同包，可直接用）。`generateTitle` 端点走 `h.Gen` 同步入口，需在 Task 8 配一个同步版的 `Gen` 闭包（复用 `GenerateTitle` 逻辑但同步返回新标题）。

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/agent/ -run TestConversationTitleAndDelete -v -count=1`
Expected: PASS。

- [ ] **Step 5: 提交**

```bash
git add internal/agent/handlers.go internal/agent/handlers_test.go
git commit -m "feat(agent): 会话 PATCH 改标题/DELETE 软删/title 生成 三端点"
```

---

### Task 8: main.go 装配 — OnTurnComplete 异步生成 + Gen 同步入口

**Files:**
- Modify: `cmd/zhiwei-server/main.go`
- Modify: `internal/agent/title.go`（加同步入口 `GenerateTitleSync`，返回新标题）

- [ ] **Step 1: 加同步入口 GenerateTitleSync**

在 `internal/agent/title.go` 末尾加：
```go
// GenerateTitleSync 同步版：跑一次生成，返回新标题（已写回 auto）。跳过/失败返回 ("", err)，
// 供 handler 的 title/generate 端点用（让用户能手动触发并拿到结果）。
func GenerateTitleSync(ctx context.Context, uid int64, cid ids.ID, convs *repo.AgentConversationRepo,
	msgs *repo.AgentMessageRepo, llm provider.LLMProvider, model string) (string, error) {
	d := newTitleDeps(ctx, uid, cid, convs, msgs, llmAdapter{llm}, model)
	title, err := d.generate(ctx)
	if err != nil {
		return "", err
	}
	if _, s, e := convs.TitleState(ctx, uid, cid); e == nil && s == titleSourceManual {
		return "", errTitleSkip
	}
	if err := convs.UpdateTitle(ctx, uid, cid, title, titleSourceAuto); err != nil {
		return "", err
	}
	return title, nil
}
```

- [ ] **Step 2: 装配 OnTurnComplete（异步） + Gen（同步）**

`cmd/zhiwei-server/main.go`，在 `orch.Persona = ...` 块之后、`agent.RegisterAgent(...)` 之前加：
```go
		// 自动生成标题：每轮收尾异步跑（第 2 轮后、标题为空/占位/auto 时生成，manual 优先、失败静幕）。
		orch.OnTurnComplete = func(ctx context.Context, conv *repo.AgentConversation) {
			go func() {
				gctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				agent.GenerateTitle(gctx, conv.UserID, conv.ID, agentConvs, agentMsgs, llm, agentModel)
			}()
		}
```
在 `agent.RegisterAgent(r, &agent.AgentHandler{...})` 字面量里加一行（`Configs:` 后）：
```go
			Gen: func(ctx context.Context, uid int64, cid ids.ID) (string, error) {
				return agent.GenerateTitleSync(ctx, uid, cid, agentConvs, agentMsgs, llm, agentModel)
			},
```
（确认 `agentConvs`/`agentMsgs`/`llm`/`agentModel` 在该作用域可见——它们在 main.go 上方已定义。）

- [ ] **Step 3: 编译验证**

Run: `go build ./... && go vet ./...`
Expected: 干净，无错。

- [ ] **Step 4: 提交**

```bash
git add cmd/zhiwei-server/main.go internal/agent/title.go
git commit -m "feat(agent): 装配自动生成标题(OnTurnComplete异步 + title/generate同步)"
```

---

### Task 9: 端到端 — 第 2 轮后自动生成标题（真 repo + fake LLM）

**Files:**
- Test: `internal/agent/title_test.go`（追加集成式用例，或新建 `internal/agent/title_integration_test.go`）

> 说明：GenerateTitle 依赖 repo，需 DB。用 `orchDSN(t)`。LLM 用 `fakeLLM`（title.go 已定义接口）。

- [ ] **Step 1: 写集成测试 — 第 2 轮生成 auto 标题 + manual 不覆盖**

创建 `internal/agent/title_integration_test.go`：
```go
package agent

import (
	"context"
	"testing"

	"zhiwei/internal/repo"
)

// 跑两轮真 user 消息，第 2 轮后 GenerateTitle 应把标题写成 auto。
func TestGenerateTitleAfterSecondTurn(t *testing.T) {
	db, err := repo.NewDB(orchDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	convRepo := &repo.AgentConversationRepo{DB: db}
	msgRepo := &repo.AgentMessageRepo{DB: db}
	ctx := t.Context()
	conv := &repo.AgentConversation{}
	if err := convRepo.Create(ctx, conv); err != nil {
		t.Fatal(err)
	}
	cid := conv.ID
	// 2 条 user 消息
	for _, txt := range []string{"帮我看下本周待办", "按优先级排序"} {
		if err := msgRepo.Append(ctx, &repo.AgentMessage{ConversationID: &cid, Role: "user", Content: txt}); err != nil {
			t.Fatal(err)
		}
	}
	flm := &fakeLLM{out: "本周待办梳理"}
	GenerateTitle(ctx, 1, cid, convRepo, msgRepo, llmAdapter{flm}, "test-model")

	title, source, err := convRepo.TitleState(ctx, 1, cid)
	if err != nil {
		t.Fatal(err)
	}
	if title != "本周待办梳理" || source != titleSourceAuto {
		t.Errorf("got title=%q source=%q, want 本周待办梳理/auto", title, source)
	}
}

// manual 标题永不覆盖。
func TestGenerateTitleManualNeverOverwritten(t *testing.T) {
	db, err := repo.NewDB(orchDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	convRepo := &repo.AgentConversationRepo{DB: db}
	msgRepo := &repo.AgentMessageRepo{DB: db}
	ctx := t.Context()
	conv := &repo.AgentConversation{Title: "我手动的标题"}
	if err := convRepo.Create(ctx, conv); err != nil {
		t.Fatal(err)
	}
	if err := convRepo.UpdateTitle(ctx, 1, conv.ID, "我手动的标题", titleSourceManual); err != nil {
		t.Fatal(err)
	}
	cid := conv.ID
	for _, txt := range []string{"q1", "q2"} {
		if err := msgRepo.Append(ctx, &repo.AgentMessage{ConversationID: &cid, Role: "user", Content: txt}); err != nil {
			t.Fatal(err)
		}
	}
	GenerateTitle(ctx, 1, cid, convRepo, msgRepo, llmAdapter{&fakeLLM{out: "不该出现的标题"}}, "m")

	title, source, _ := convRepo.TitleState(ctx, 1, cid)
	if title != "我手动的标题" || source != titleSourceManual {
		t.Errorf("manual 标题被覆盖: title=%q source=%q", title, source)
	}
}
```

- [ ] **Step 2: 跑测试确认通过**

Run: `go test ./internal/agent/ -run 'TestGenerateTitleAfterSecondTurn|TestGenerateTitleManualNeverOverwritten' -v -count=1`
Expected: PASS（需 `TEST_MYSQL_DSN`，否则 skip）。

- [ ] **Step 3: 提交**

```bash
git add internal/agent/title_integration_test.go
git commit -m "test(agent): 自动生成标题端到端(第2轮生成auto/manual不覆盖)"
```

---

### Task 10: 前端 — 列表项行内编辑/删除

**Files:**
- Modify: `web/index.html`（列表项，`:752-755`）
- Modify: `web/app.js`（新增编辑/删除函数 + 暴露）

- [ ] **Step 1: 列表项加编辑/删除按钮 + 行内 input**

`web/index.html`，把 `:752-755` 的列表项块替换为：
```html
        <div v-for="c in agentConversations" :key="c.id" class="agent-convo-item" :class="{active: c.id===agentConvId}" @click="selectAgentConversation(c)">
          <template v-if="agentEditConvId !== c.id">
            <b>{{ c.title || '新对话' }}</b>
            <span class="muted" style="font-size:var(--fs-xs); display:flex; justify-content:space-between; align-items:center">
              <span>{{ fmtTime(c.last_active_at) }}</span>
              <span class="agent-convo-ops" @click.stop>
                <button class="btn xs" title="编辑标题" @click="startEditConv(c)">✏️</button>
                <button class="btn xs danger" title="删除会话" @click="deleteAgentConversation(c)">🗑</button>
              </span>
            </span>
          </template>
          <template v-else>
            <input class="agent-convo-edit" v-model="agentEditTitle"
              @click.stop
                   @keyup.enter="saveAgentTitle(c)"
                   @keyup.esc="cancelEditConv"
                   @blur="saveAgentTitle(c)"
                   ref="agentEditInput" autofocus />
          </template>
        </div>
```

- [ ] **Step 2: app.js 加状态 + 三个函数**

在 `web/app.js` 声明 `agentConversations`/`agentConvId` 附近（约 `:2356`）加：
```js
const agentEditConvId = ref(null);   // 正在编辑标题的会话 id（行内 input 态）
const agentEditTitle = ref('');      // 行内编辑的标题临时值
```

在 `selectAgentConversation` 附近加函数：
```js
// 进入行内编辑：记下目标会话 + 临时标题；不触发选中。
function startEditConv(c) {
  agentEditConvId.value = c.id;
  agentEditTitle.value = c.title || '';
}
function cancelEditConv() {
  agentEditConvId.value = null;
  agentEditTitle.value = '';
}
// 失焦/回车保存：PATCH 改标题(manual)，成功重拉列表。空标题或未变则取消。
async function saveAgentTitle(c) {
  if (agentEditConvId.value !== c.id) return; // 已取消（重复 blur/enter）
  const editing = agentEditConvId.value;
  agentEditConvId.value = null;
  const title = agentEditTitle.value.trim();
  if (!title || title === (c.title || '')) return;
  try {
    await api('PATCH', '/api/agent/conversations/' + c.id, { title });
    await loadAgentConversations();
  } catch (e) {
    notify(e.message || '保存标题失败', 'error');
  }
}
// 软删除：确认后 DELETE，若删的是当前会话则清空主区。
async function deleteAgentConversation(c) {
  if (!confirm('删除会话「' + (c.title || '新对话') + '」？删除后可在数据库恢复。')) return;
  try {
    await api('DELETE', '/api/agent/conversations/' + c.id);
    if (agentConvId.value === c.id) {
      agentConvId.value = null;
      agentMessages.value = [];
    }
    await loadAgentConversations();
    notify('会话已删除');
  } catch (e) {
    notify(e.message || '删除失败', 'error');
  }
}
```

在 `return { ... }`（约 `:3069`）的返回对象里加：`startEditConv, cancelEditConv, saveAgentTitle, deleteAgentConversation, agentEditConvId, agentEditTitle`。

- [ ] **Step 3: 小按钮样式**

`web/index.html` 的 `<style>` 里（`.agent-convo-item` 相关处）加：
```css
  .agent-convo-ops { display: none; gap: 4px; }
  .agent-convo-item:hover .agent-convo-ops { display: inline-flex; }
  .btn.xs { padding: 1px 6px; font-size: var(--fs-xs); }
  .btn.xs.danger { color: var(--danger); }
  .agent-convo-edit { width: 100%; box-sizing: border-box; font-size: var(--fs-sm); }
```

- [ ] **Step 4: 重算 web 指纹**

Run: `make hash-web`
Expected: 生成新的 `web/app.<hash>.js` 副本，`index.html` 引用更新。

- [ ] **Step 5: 浏览器冒烟（dev 服务器）**

Run: `make dev`（端口 8081），浏览器打开登录后进入问知微 tab。
验证：
- 列表项 hover 出现 ✏️/🗑；
- ✏️ 变行内 input，改标题回车 → 列表刷新为新标题；
- 🗑 确认 → 会话从列表消失；若删的是当前会话，右侧清空；
- 发两条消息后，标题自动变成简短标题（auto）。

- [ ] **Step: 提交**

```bash
git add web/index.html web/app.js
git commit -m "feat(web): 会话列表行内编辑标题+软删除按钮"
```

---

### Task 11: 全量回归 + webapp-testing 验证

**Files:** 无新增。

- [ ] **Step 1: go 全量构建 + vet + 测试**

Run:
```bash
go build ./... && go vet ./...
go test ./internal/repo/ -count=1
go test ./internal/agent/ -count=1
```
Expected: build/vet 干净；repo/agent 测试 PASS（DB 测试需 `TEST_MYSQL_DSN`）。

- [ ] **Step 2: 用 webapp-testing 技能做前端回归（可选）**

用 `webapp-testing` 技能打开 dev 服务器，跑一遍 Task 10 Step 5 的冒烟清单，截图确认编辑/删除/自动标题可用。

- [ ] **Step 3: 合并前迁移号核对**

Run: `git fetch origin main 2>/dev/null; git ls-tree origin/main --name-only migrations/ | grep -E '\.up\.sql$' | sort | tail -3`
若 main 已超过 `000020`，把本特性两个迁移文件重编号到 main 最大值 +1（`000021_*` 起），并更新 Task 1 commit。

- [ ] **Step 4: 收尾提交（如有重编号/fixup）**

```bash
git add -A
git commit -m "chore: 会话标题特性回归 + 迁移号核对" 
```

---

## Self-Review 结果

**Spec 覆盖核对：**
- §1 决策表（软删除=archived / title_source / 第2轮异步 / manual优先 / 失败静默）→ Task 1(列)+Task 3(Archive)+Task 5(generateTitle 判定)+Task 6(钩子)+Task 8(装配)。✅
- §3 数据层（迁移/UpdateTitle/TitleState/Archive/CountByConversation）→ Task 1/2/3/4。✅
- §4 API（PATCH/DELETE/title-generate + 越权404）→ Task 7。✅
- §5 自动生成（OnTurnComplete 钩子/装配/判定/CAS）→ Task 5/6/8/9。✅
- §6 前端（行内编辑/删除/清空主区/hash-web）→ Task 10。✅
- §7 测试（repo/handler/生成）→ Task 2/3/4/7/9。✅
- §8 风险（撞号/IDOR/成本/长度/范围外）→ Task 1 注记 + Task 11 Step 3 + 全程 user_id 过滤。✅

**占位符扫描：** 无 TBD/TODO；所有代码块含完整可运行内容；签名跨任务一致（`UpdateTitle(ctx,userID,id,title,source)` / `Archive(ctx,userID,id)` / `TitleState(ctx,userID,id)` / `CountByConversation(ctx,userID,convID)` / `GenerateTitle(ctx,uid,cid,convs,msgs,llm,model)` / `GenerateTitleSync(...)` / `shouldGenerate(title,source,count)` / `sanitizeTitle(s)` / `buildTitleInput(msgs)` / `titleDeps` / `llmAdapter` / `fakeLLM`）。

**类型一致性：** `titleSourceManual/Auto` 常量、`placeholderTitle`、`titlePrompt`、`errTitleSkip` 在 Task 5 定义，Task 7/8/9 引用，命名一致。`Gen` 字段 Task 7 定义、Task 8 装配，一致。

**无遗漏。计划完整。**
