# 知微 Agent 行为路由 + 检索种子门控 实现计划（Phase 1）

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 修「未知/专业/常识问题被生硬关联用户数据」——默认 system prompt 加分场景路由；检索种子加个人信号门控并改写措辞。

**Architecture:** 两处只影响「发给 dsh 的文本」（不改落库，沿用 D2 约束）。① `internal/config/config.go` 的 `DSH_SYSTEM_PROMPT` 默认值改写，加「常识题自答/个人题才调工具/不懂直说」的路由。② `internal/agent/context.go` 的 `Seeds()` 在召回前加个人信号正则门控，并把种子块措辞从「可能相关的我的记忆」改为中性「与该问题可能相关的背景记忆」。设置页预览自动跟随，无需改前端。

**Tech Stack:** Go（标准库 `regexp`）， testify 风格原生 `testing`，grep/gofmt。

**Spec:** `docs/superpowers/specs/2026-08-31-agent-behavior-routing-design.md`

---

### Task 1: 默认 system prompt 加分场景路由

**Files:**
- Modify: `internal/config/config.go:165`
- Test: `internal/config/config_test.go`（`TestAgentConfigDefaults`，:88）

- [ ] **Step 1: 写失败测试**

在 `internal/config/config_test.go` 顶部 import 块加 `"strings"`：

```go
import (
	"os"
	"strings"
	"testing"
)
```

在 `TestAgentConfigDefaults`（:88）函数体末尾（`ReviewDailyCron` 断言之后、函数收尾 `}` 之前）追加：

```go
	// 默认 system prompt 须含行为路由关键词（常识题自答、不硬关联用户数据、不懂直说），
	// 锁定 Phase 1 行为意图（不断言整串，避免脆弱）。
	sp := cfg.DSHSystemPrompt
	for _, kw := range []string{"分场景", "直接基于你自己的知识", "如实说明"} {
		if !strings.Contains(sp, kw) {
			t.Errorf("DSHSystemPrompt 应含路由关键词 %q: %q", kw, sp)
		}
	}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `cd /Users/jyxc-dz-0100360/work/fun/zhiwei-water18-0822/.claude/worktrees/agent-behavior-routing && go test ./internal/config/ -run TestAgentConfigDefaults -v`
Expected: FAIL —— `DSHSystemPrompt 应含路由关键词 "分场景"...`（当前默认串无这些词）。

- [ ] **Step 3: 改默认 system prompt**

`internal/config/config.go:165`，把整行替换为（用反引号 raw string 承载多行提示）：

```go
		DSHSystemPrompt: getenv("DSH_SYSTEM_PROMPT", `你是知微(zhiwei)，用户的个人助理，用简体中文亲切、简洁地回答。
请按问题类型分场景处理：
1) 一般知识、专业术语、名词解释、常识等问题：直接基于你自己的知识回答，不要调用读取用户数据的工具，也不要生硬地关联到用户的记忆或指标。
2) 只有问题明确关于用户本人（含「我/我的」或涉及其日程/记录/指标/待办等）时，才调用工具读取该用户的数据作答。
3) 不确定或不懂时：如实说明，不要编造，也不要用用户的数据拼凑答案。
只有在需要用户本人数据时才调用工具；不要臆测用户没有的记忆或数据。`),
```

- [ ] **Step 4: 跑测试确认通过**

Run: `cd /Users/jyxc-dz-0100360/work/fun/zhiwei-water18-0822/.claude/worktrees/agent-behavior-routing && go test ./internal/config/ -run TestAgentConfigDefaults -v`
Expected: PASS。

- [ ] **Step 5: 提交**

```bash
cd /Users/jyxc-dz-0100360/work/fun/zhiwei-water18-0822/.claude/worktrees/agent-behavior-routing
git add internal/config/config.go internal/config/config_test.go
git commit -m "feat(agent): 默认 system prompt 加分场景路由，常识题不再硬关联用户数据"
```

---

### Task 2: 检索种子个人信号门控 + 改写措辞

**Files:**
- Modify: `internal/agent/context.go`（imports :3-11、`Seeds()` :107-121）
- Test: `internal/agent/retrieval_wire_test.go`（`TestOrchestratorSeedsInjection` :75，新增 `TestSeedsGateSkipsNonPersonal`）

- [ ] **Step 1: 写失败测试**

**1a.** 改 `retrieval_wire_test.go` 的 `TestOrchestratorSeedsInjection`：把 query 改为含个人信号，并断言新措辞/无旧措辞。将：

```go
	const raw = "猫应该怎么养"
	if _, err := orch.RunTurn(ctx, conv, raw); err != nil {
		t.Fatalf("RunTurn: %v", err)
	}
	if !strings.Contains(fake.LastText, "布偶猫SEED") || !strings.Contains(fake.LastText, raw) {
		t.Errorf("发给 dsh 文本应含种子标题+原始问题: %q", fake.LastText)
	}
	if fake.LastText == raw {
		t.Errorf("应前置种子(≠原始): %q", fake.LastText)
	}
```

替换为：

```go
	const raw = "我的猫应该怎么养" // 含个人信号「我」→ 命中门控，注入种子
	if _, err := orch.RunTurn(ctx, conv, raw); err != nil {
		t.Fatalf("RunTurn: %v", err)
	}
	if !strings.Contains(fake.LastText, "布偶猫SEED") || !strings.Contains(fake.LastText, raw) {
		t.Errorf("发给 dsh 文本应含种子标题+原始问题: %q", fake.LastText)
	}
	if !strings.Contains(fake.LastText, "与该问题可能相关的背景记忆") {
		t.Errorf("种子块应用新措辞: %q", fake.LastText)
	}
	if strings.Contains(fake.LastText, "可能相关的我的记忆") {
		t.Errorf("不应再用旧措辞: %q", fake.LastText)
	}
	if fake.LastText == raw {
		t.Errorf("应前置种子(≠原始): %q", fake.LastText)
	}
```

**1b.** 在 `retrieval_wire_test.go` 文件末尾新增负例测试：

```go
// TestSeedsGateSkipsNonPersonal：query 无个人信号（我/咱/自己/本人）时，即使有相关记忆
// 也不注入种子——常识/名词解释题不再被生硬关联用户数据（Phase 1 门控）。
func TestSeedsGateSkipsNonPersonal(t *testing.T) {
	db, err := repo.NewDB(orchDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	convRepo := &repo.AgentConversationRepo{DB: db}
	msgRepo := &repo.AgentMessageRepo{DB: db}
	mem := &repo.MemoryRepo{DB: db}
	ctx := t.Context()
	_ = seedMem(t, mem, "猫的常见习性GATESEED") // 含「猫」：与 query 同向量，本会被召回
	r := &retrieve.Retriever{Memories: mem, Embedder: fakeEmbedder{}, TopK: 5}
	if _, err := r.Backfill(ctx, toolUserID, 500); err != nil {
		t.Fatal(err)
	}

	conv := &repo.AgentConversation{Title: "种子门控"}
	if err := convRepo.Create(ctx, conv); err != nil {
		t.Fatal(err)
	}
	msgData, _ := json.Marshal(map[string]any{"message": map[string]any{"content": []map[string]any{
		{"type": "text", "text": "好的"},
	}}})
	fake := &FakeRuntime{Script: [][]Event{{{Type: EvAssistantMessage, Data: msgData}}}}
	orch := NewOrchestrator(rtFor(fake), convRepo, msgRepo)
	orch.Ctx = &ProfileContext{Retrieve: r} // 只装 Retrieve，测门控

	const raw = "猫的常见习性" // 名词/常识问法，无个人信号 → 不注入种子
	if _, err := orch.RunTurn(ctx, conv, raw); err != nil {
		t.Fatalf("RunTurn: %v", err)
	}
	if strings.Contains(fake.LastText, "背景记忆") || strings.Contains(fake.LastText, "猫的常见习性GATESEED") {
		t.Errorf("常识题不应注入种子: %q", fake.LastText)
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `cd /Users/jyxc-dz-0100360/work/fun/zhiwei-water18-0822/.claude/worktrees/agent-behavior-routing && go test ./internal/agent/ -run 'TestOrchestratorSeedsInjection|TestSeedsGateSkipsNonPersonal' -v`
Expected: FAIL —— `TestOrchestratorSeedsInjection` 报「种子块应用新措辞」（仍旧措辞）；`TestSeedsGateSkipsNonPersonal` 报「常识题不应注入种子」（当前无门控，种子被注入）。

- [ ] **Step 3: 实现门控 + 改写措辞**

**3a.** `internal/agent/context.go` import 块（:3-11）加 `"regexp"`：

```go
import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	"zhiwei/internal/profile"
	"zhiwei/internal/repo"
	"zhiwei/internal/retrieve"
)
```

**3b.** 在 `Seeds()` 定义之前新增正则变量：

```go
// personalSignal 命中「问题关于用户本人」的信号；仅当 query 命中时才跑召回+注入种子。
// 常识/名词解释/一般知识题（如「ASL 是什么」「猫的习性」）不含这些词 → 不注入，
// 从源头避免「啥都跟你数据有关」的误导，也顺带省一次 embedding 调用。
var personalSignal = regexp.MustCompile(`我|咱|自己|本人`)
```

**3c.** 把 `Seeds()`（:107-121）整函数替换为（含门控 + 新措辞 + 更新注释）：

```go
// Seeds 按本轮 query 召回 top-k 相关记忆，拼成上下文头的「相关记忆」块。
// 门控：仅当 query 命中个人信号（我/咱/自己/本人）时才召回——常识/名词解释题不注入，
// 避免「啥都跟你数据有关」的误导，也省一次 embedding 调用。
// 无 Retrieve / query 空 / query 无个人信号 / 无命中 → ""。每轮一次 query 向量化（未配 embedder 时 Retrieve=nil 不触发）。
// userID 指定「谁」的记忆（2B-B：由 runTurn 传 conv.UserID，多用户隔离，绝不召回别人的记忆）。
func (pc *ProfileContext) Seeds(ctx context.Context, userID int64, query string) string {
	if pc == nil || pc.Retrieve == nil || strings.TrimSpace(query) == "" {
		return ""
	}
	// 个人信号门控：仅当 query 关于用户本人时才召回；否则不注入。
	if !personalSignal.MatchString(query) {
		return ""
	}
	ms, err := pc.Retrieve.Search(ctx, userID, query, "", 0) // limit=0 → Retriever.TopK
	if err != nil || len(ms) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("与该问题可能相关的背景记忆（仅供参考，不相关请忽略）：")
	for _, m := range ms {
		b.WriteString("\n- " + m.Title)
	}
	return b.String()
}
```

- [ ] **Step 4: 跑测试确认通过**

Run: `cd /Users/jyxc-dz-0100360/work/fun/zhiwei-water18-0822/.claude/worktrees/agent-behavior-routing && go test ./internal/agent/ -run 'TestOrchestratorSeedsInjection|TestSeedsGateSkipsNonPersonal' -v`
Expected: PASS（两条均绿）。

- [ ] **Step 5: 格式化 + 全量回归**

Run:
```bash
cd /Users/jyxc-dz-0100360/work/fun/zhiwei-water18-0822/.claude/worktrees/agent-behavior-routing
gofmt -w internal/agent/context.go internal/agent/retrieval_wire_test.go
go build ./...
go vet ./internal/agent/... ./internal/config/...
go test ./internal/agent/... ./internal/config/...
```
Expected: build/vet 无错；测试全绿。

- [ ] **Step 6: 提交**

```bash
cd /Users/jyxc-dz-0100360/work/fun/zhiwei-water18-0822/.claude/worktrees/agent-behavior-routing
git add internal/agent/context.go internal/agent/retrieval_wire_test.go
git commit -m "feat(agent): 检索种子加个人信号门控 + 改写措辞，常识题不再注入"
```

---

## Self-Review 记录

**Spec 覆盖：**
- §3 默认 system prompt 路由 → Task 1 ✓
- §4 种子门控 + 措辞 → Task 2 ✓（门控 + 措辞 + 取舍已在 spec/计划注明）
- §5 前端无需改动 → 计划无需前端任务（已确认预览自动跟随、种子不预览）✓
- §6 测试（更新现有种子测试 / 新增负例 / 锁定默认 prompt）→ Task 1 Step1、Task 2 Step1 ✓

**Placeholder 扫描：** 无 TBD/TODO；每步含完整代码与命令。

**类型一致性：** `personalSignal`（regexp）、`Seeds` 签名不变；测试用既有 `seedMem`/`orchDSN`/`rtFor`/`FakeRuntime`/`fakeEmbedder`/`ProfileContext`/`retrieve.Retriever`（均在 `retrieval_wire_test.go`/同包已定义）。

**风险备注：** 门控过严漏种子为 spec 已确认的可接受取舍；改只影响发给 dsh 文本，不改落库。
