# Agent Chatbot · P3a 报告后端 (Reports Backend) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 在 Go 侧交付**报告子系统后端**（spec §11：日报 + 周报 + 话题/项目状态）。新建 `internal/review` 包，`Generator` 从现有 `internal/repo` 汇聚数据 → 直调 Ark 上的 DeepSeek 模型（复用 `provider.ArkLLM`，**不经 dsh agent 运行时**）→ 版本化 prompt 产出**严格 JSON** → 解析进 Go 结构 → 落 `daily_review`/`weekly_review`/`topic_status`（表已在 000005，无新迁移）→ 返回对象。同时提供 MCP 报告工具（`generate_report`/`get_topic_status`）与 HTTP 端点（`/api/reviews/*`、`GET /api/topics/{id}/status`），以及日/周报定时器。

**SCOPE: BACKEND ONLY.** 报告**前端页**（`web/` 报告卡、SVG 曲线、日/周切换）**明确不在本计划内**——由协调者在最后统一集成 web/*。本计划只覆盖 Go：Generator、prompts、HTTP handlers、MCP 工具逻辑、cron。

**Architecture:** `review.Generator` 进程内、复用主服务已开的 `*sqlx.DB`/repo（一个池、无子进程）。报告是**批量直调 LLM**（D5），可被 cron / `/api/reviews/*` / MCP `generate_report` 三处复用，故不绕 dsh。**关键测试缝**：把「汇聚（DB）」与「渲染=建 prompt + 调 LLM + 解析 JSON + 定状态（纯逻辑）」分离——纯逻辑用 **mock LLM + 手搭输入**单测（无 MySQL）；「汇聚 + 编排 + 落库」用**独立库集成测试**。这与 `internal/memory.Extractor`（持 `provider.LLMProvider`、mock LLM 单测、repo 由 stage 另接）的既有范式同构。

**Tech Stack:** Go 1.25、`github.com/jmoiron/sqlx`、chi/v5、雪花 `ids.ID`、`provider.ArkLLM`（OpenAI 兼容 `/chat/completions`，`provider.LLMProvider` 接口）。模型 `cfg.AgentModel`（Ark 上的 DeepSeek 模型/endpoint id）。**go.mod 无 cron 库**（无 robfig/cron）→ 定时器用标准库 `time.Timer`/`time.AfterFunc` 自实现最小调度（见 Task 8）。

**依据：** spec `docs/superpowers/specs/2026-08-24-agent-chatbot-system-design.md` §5.2（Generator）、§11/11.1/11.2/11.3（日/周/话题状态 + 输出 JSON）、§7.3（`generate_report` 工具）、§13（API）。现有范式：`internal/memory/extract.go`+`candidate.go`（LLM 调用 + JSON 解析容错）、`internal/pipeline/stage_extract.go`（汇聚 + 落库编排）、`internal/agent/mcp_tools.go`（MCP 工具形态）、`internal/api/todo.go`（HTTP handler + Register 范式）。仓储：`internal/repo/{review,topic_status,memory,todo,topic,session,transcript}.go`。

**贯穿约定：**
- **中文详细注释**（项目规则，新人可懂）。所有导出符号带 doc 注释。
- LLM 调用范式（对齐 `memory.Extractor`）：`provider.LLMProvider.Chat(ctx, provider.ChatRequest{Model, System: <prompt>, User: <汇聚数据>})`；**不设 Temperature**（默认，报告求稳）。
- JSON 解析容错（对齐 `memory.ParseCandidates`）：截取首个 `{` 到末个 `}` 剥掉代码围栏/前后废话，再 `json.Unmarshal`；彻底非法 → 返回 error（调用方置 `status=failed`）。
- 可空 JSON 列用 `*json.RawMessage`（对齐 `repo.DailyReview.Content`）；落库经 `ReviewRepo.UpsertDaily/UpsertWeekly`（`(user_id, date/week_start)` upsert，天然幂等）与 `TopicStatusRepo.Insert`（追加式历史快照）。
- 单用户 MVP：`user_id=1`（与 `mcp_tools.go` 的 `toolUserID`、`cmd/dedup-todos` 一致）。
- **日/周窗口在 Go 内切**（不新增 repo 方法、不改现有 repo 文件）：用现有 `List/ListActive/GetDaily` 等拉近段有界数据，按 `event_at`/`created_at`/`updated_at` 落在 `[start,end)` 过滤。单用户低量下可接受；「按日期范围的 repo 方法」列为后续优化（本计划不做）。
- 单测：mock LLM（实现 `provider.LLMProvider` 的 `Chat`，返回 canned JSON），**无 MySQL**。集成测试连独立库：`TEST_MYSQL_DSN="zhiwei:zhiwei@tcp(127.0.0.1:3307)/zhiwei_agentchat_test?parseTime=true&charset=utf8mb4&multiStatements=true"`（共享 `zhiwei_test` 被并行 worktree 冲，**勿用** `make test-integration`）。红灯=编译失败/断言失败，绿灯=目标测试通过。
- **包边界（关键，同 worktree 包内执行）**：全部报告逻辑在**新包 `internal/review`**；MCP 报告工具逻辑也在 `internal/review`（导出 `RegisterReportTools(s *mcp.Server, gen *Generator)`，**不改** `internal/agent/mcp_server.go`/`mcp_tools.go`）；HTTP handler 在**新文件** `internal/api/review.go`（导出 `RegisterReviews(r chi.Router, gen *review.Generator)`，**不改** `internal/api/router.go`）。`cmd/zhiwei-server/main.go` 的接线、路由挂载、cron 启动、工具注册一律作为末尾「COORDINATOR INTEGRATION」说明，**本计划不改 main.go**。

---

## File Structure

| 文件 | 责任 | 动作 |
|---|---|---|
| `internal/review/types.go` | §11 三种报告的 content Go 结构（严格 JSON tag）+ 汇聚输入结构 | Create |
| `internal/review/types_test.go` | content 结构 JSON round-trip 单测（无 DB） | Create |
| `prompts/review_daily_v1.md` | 日报系统 prompt（中文、只输出严格 JSON） | Create |
| `prompts/review_weekly_v1.md` | 周报系统 prompt | Create |
| `prompts/topic_status_v1.md` | 话题状态系统 prompt | Create |
| `internal/review/parse.go` | 三个解析器（剥围栏 + 严格 JSON → content 结构） | Create |
| `internal/review/parse_test.go` | 解析器单测（含围栏/废话/空/非法容错，无 DB） | Create |
| `internal/review/prompt.go` | 三个 prompt builder（汇聚输入 → user message 文本） | Create |
| `internal/review/prompt_test.go` | builder 单测（确定性、无 DB） | Create |
| `internal/review/generator.go` | `Generator` 结构 + 纯渲染核 `generateDaily/Weekly/TopicStatus`（mock LLM 可测） | Create |
| `internal/review/generator_test.go` | 渲染核单测（mock LLM，无 MySQL：ok / LLM err / 解析 err） | Create |
| `internal/review/gather.go` | `gatherDaily/Weekly/TopicStatus`（经 repo 汇聚，Go 内切窗）+ `Daily/Weekly/TopicStatus` 编排（汇聚→渲染→落库→返回） | Create |
| `internal/review/gather_test.go` | 编排集成测试（独立库 + mock LLM：落库 status=ready、幂等重生成、失败置 failed） | Create |
| `internal/review/tools.go` | MCP 报告工具（`generate_report`/`get_topic_status`）+ `RegisterReportTools(s, gen)` | Create |
| `internal/review/tools_test.go` | 工具单测（集成库 + mock LLM，直调 handler 断言 JSON schema） | Create |
| `internal/api/review.go` | HTTP：GET/POST daily・weekly、GET topic status + `RegisterReviews(r, gen)` | Create |
| `internal/api/review_test.go` | handler 测试（httptest + 集成库 + mock LLM） | Create |
| `internal/review/schedule.go` | `time.Timer` 最小调度器（日报/周报定时触发）+ 纯 `nextFireAt` 计算 | Create |
| `internal/review/schedule_test.go` | `nextFireAt` 纯单测（无 DB/无时钟依赖：注入 now） | Create |

**类型契约：**
- `review.Generator{ LLM provider.LLMProvider; Model string; DailyPrompt/WeeklyPrompt/TopicStatusPrompt string; DailyPromptVer/WeeklyPromptVer/TopicStatusPromptVer string; Reviews *repo.ReviewRepo; TopicStatuses *repo.TopicStatusRepo; Memories *repo.MemoryRepo; Todos *repo.TodoRepo; Topics *repo.TopicRepo; Sessions *repo.SessionRepo; Transcripts *repo.TranscriptRepo }`
- `review.NewGenerator(...)` 构造函数（参数与上字段对应）。
- 编排方法：`(g *Generator) Daily(ctx, date time.Time) (*repo.DailyReview, error)`；`Weekly(ctx, weekStart time.Time) (*repo.WeeklyReview, error)`；`TopicStatus(ctx, topicID ids.ID) (*repo.TopicStatus, error)`。
- 导出注册：`review.RegisterReportTools(s *mcp.Server, gen *Generator)`；`api.RegisterReviews(r chi.Router, gen *review.Generator)`。

---

## 任务清单（先落盘，细节见下）

- [ ] **Task 1**: `internal/review` 包骨架 + §11 content 结构（types.go）+ mock LLM 测试桩 + round-trip 单测
- [ ] **Task 2**: 三个版本化 prompt 文件（review_daily_v1 / review_weekly_v1 / topic_status_v1，中文、严格 JSON）
- [ ] **Task 3**: 三个 JSON 解析器（parse.go）+ 容错单测（剥围栏 / 废话 / 空 / 非法）
- [ ] **Task 4**: 三个 prompt builder（prompt.go：汇聚输入 → user message）+ 确定性单测
- [ ] **Task 5**: `Generator` 结构 + 纯渲染核 `generateDaily/Weekly/TopicStatus`（mock LLM 单测：ok / LLM 失败 / 解析失败）
- [ ] **Task 6**: 日报汇聚 + 编排 `Daily()`（gather.go）+ 集成测试（独立库：落库 ready、幂等重生成、失败置 failed）
- [ ] **Task 7**: 周报汇聚 + 编排 `Weekly()` + 集成测试
- [ ] **Task 8**: 话题状态汇聚 + 编排 `TopicStatus()` + 集成测试
- [ ] **Task 9**: MCP 报告工具 `generate_report`/`get_topic_status` + `RegisterReportTools` + 单测
- [ ] **Task 10**: HTTP handler `internal/api/review.go`（GET/POST daily・weekly、GET topic status）+ `RegisterReviews` + httptest 测试
- [ ] **Task 11**: 最小定时器 `schedule.go`（日/周报触发）+ `nextFireAt` 纯单测
- [ ] **Task 12**: COORDINATOR INTEGRATION 说明（main.go 接线 / 路由 / cron / 工具注册，本计划不改这些文件）

---

## Task 1: 包骨架 + §11 content 结构 + mock LLM 测试桩

**Files:** `internal/review/types.go`（create）、`internal/review/types_test.go`（create）

> 先把 §11 三种报告的输出 JSON 定成 Go 结构（**schema 契约**，后续解析/prompt/自检都对齐它）。字段名与 §11.1/11.2/11.3 逐一对齐，json tag 用 snake_case。可空/可省用 `omitempty`。

- [ ] **Step 1: 写 content 结构（types.go）**

```go
// Package review 是报告子系统（spec §11）：从现有 repo 汇聚数据，直调 Ark 上的
// DeepSeek 模型（不经 dsh），产出结构化日报/周报/话题状态 JSON 并落库。
// 被 cron、/api/reviews/*、MCP generate_report 三处复用（spec §5.2/§7.3/§13）。
package review

import "encoding/json"

// ---- §11.1 日报 ----

// DailyContent 是日报的结构化输出（落 daily_review.content）。
// 字段对齐 spec §11.1：headline / highlights / decisions / todos{new,done,open}
// / insights / tomorrow / topic_distribution。
type DailyContent struct {
	Headline          string          `json:"headline"`           // 一句话总述当天
	Highlights        []string        `json:"highlights"`         // 当天要点（3~7 条）
	Decisions         []string        `json:"decisions"`          // 当天做出的决定
	Todos             DailyTodos      `json:"todos"`              // 待办三分组
	Insights          []string        `json:"insights"`           // 归纳/洞察
	Tomorrow          []string        `json:"tomorrow"`           // 明日计划（只引当天 confirmed 未完成 todo，见 §11.1 约束）
	TopicDistribution []TopicCount    `json:"topic_distribution"` // 当天记忆的话题分布（图表就绪）
}

// DailyTodos 是日报里的待办三分组（spec §11.1 todos{new,done,open}）。
type DailyTodos struct {
	New  []string `json:"new"`  // 当天新增
	Done []string `json:"done"` // 当天完成
	Open []string `json:"open"` // 仍未完成（confirmed 未 done）
}

// TopicCount 是「话题→计数」的图表就绪项（日报话题分布 / 通用）。
type TopicCount struct {
	Topic string `json:"topic"`
	Count int    `json:"count"`
}

// ---- §11.2 周报 ----

// WeeklyContent 是周报的结构化输出（落 weekly_review.content）。
// 字段对齐 spec §11.2：headline / by_topic / trends / risks / next_week。
type WeeklyContent struct {
	Headline string        `json:"headline"`  // 一句话总述本周
	ByTopic  []WeeklyTopic `json:"by_topic"`  // 按话题的进展视图
	Trends   []Trend       `json:"trends"`    // 曲线就绪数据（每日记忆数、todo 完成数…）
	Risks    []string      `json:"risks"`     // 全局风险
	NextWeek []string      `json:"next_week"` // 下周计划
}

// WeeklyTopic 是周报里单个话题的进展块（spec §11.2 by_topic[]）。
type WeeklyTopic struct {
	Topic     string   `json:"topic"`
	Progress  float64  `json:"progress"`   // 0..1 概略进展
	KeyEvents []string `json:"key_events"` // 本周关键事件
	OpenTodos []string `json:"open_todos"` // 未完成待办
	Risks     []string `json:"risks"`      // 该话题风险
}

// Trend 是一条曲线（spec §11.2 trends[{metric, series[]}]）。
// Labels 可选（x 轴，如日期串），Series 为 y 值序列；二者同长时前端按点对齐。
type Trend struct {
	Metric string    `json:"metric"`
	Labels []string  `json:"labels,omitempty"`
	Series []float64 `json:"series"`
}

// ---- §11.3 话题/项目状态 ----

// TopicStatusContent 是话题状态快照（落 topic_status.content）。
// 字段对齐 spec §11.3：summary / progress / milestones / decisions
// / open_todos / risks[{desc,severity}] / blockers。
// Progress 取 0..1 概略进展；「阶段」语义由 milestones 承载（避免 union 类型，保严格 JSON）。
type TopicStatusContent struct {
	Summary    string   `json:"summary"`
	Progress   float64  `json:"progress"` // 0..1
	Milestones []string `json:"milestones"`
	Decisions  []string `json:"decisions"`
	OpenTodos  []string `json:"open_todos"`
	Risks      []Risk   `json:"risks"`
	Blockers   []string `json:"blockers"`
}

// Risk 是带严重度的风险项（spec §11.3 risks[{desc,severity}]）。
// Severity 取 low|medium|high（prompt 内约束枚举）。
type Risk struct {
	Desc     string `json:"desc"`
	Severity string `json:"severity"`
}

// mustJSON 把 content 结构序列化为 json.RawMessage（落库/返回用；结构可控故不会失败）。
func mustJSON(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return json.RawMessage(b)
}
```

- [ ] **Step 2: mock LLM 测试桩 + round-trip 单测（types_test.go）**

放一个可复用的 mock LLM（后续 Task 3/5/6… 单测共用）：

```go
package review

import (
	"context"
	"encoding/json"
	"testing"

	"zhiwei/internal/provider"
)

// fakeLLM 是单测用 mock：Chat 返回预置 Reply（或 Err），并记录收到的 System/User。
// 实现 provider.LLMProvider，故无需 MySQL/网络即可测「渲染核」。
type fakeLLM struct {
	Reply    string
	Err      error
	GotReq   provider.ChatRequest
}

func (f *fakeLLM) Chat(_ context.Context, req provider.ChatRequest) (provider.ChatResponse, error) {
	f.GotReq = req
	if f.Err != nil {
		return provider.ChatResponse{}, f.Err
	}
	return provider.ChatResponse{Content: f.Reply, TotalTokens: 42}, nil
}

func TestDailyContentRoundTrip(t *testing.T) {
	in := DailyContent{
		Headline: "今天完成了 X", Highlights: []string{"a", "b"},
		Todos: DailyTodos{New: []string{"n1"}, Done: []string{}, Open: []string{"o1"}},
		TopicDistribution: []TopicCount{{Topic: "工作", Count: 3}},
	}
	b := mustJSON(in)
	var out DailyContent
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	if out.Headline != in.Headline || len(out.Highlights) != 2 || out.Todos.New[0] != "n1" {
		t.Errorf("round-trip 丢字段: %+v", out)
	}
}
```

- [ ] **Step 3: 编译 + 单测（无 MySQL）**

Run: `go test ./internal/review/`
Expected: `TestDailyContentRoundTrip` 通过；包可编译。

- [ ] **Commit:** `feat(review): §11 报告 content 结构 + mock LLM 测试桩`

---

## Task 2: 三个版本化 prompt 文件（中文、严格 JSON）

**Files:** `prompts/review_daily_v1.md`、`prompts/review_weekly_v1.md`、`prompts/topic_status_v1.md`（均 create）

> 版本 = 文件名 stem（如 `review_daily_v1`），进 trace（对齐 `cmd/zhiwei-server/main.go` 的 `filepath.Base(...).TrimSuffix(".md")`）。prompt 只作 **system 指令**；汇聚数据由 builder 拼进 user message（Task 4）。**强约束：只输出 JSON、无代码围栏、无多余文字**（与 `extraction_v3.md` 同风格）。schema 与 Task 1 的 content 结构逐字对齐。

- [ ] **Step 1: `prompts/review_daily_v1.md`**

```markdown
# 知微日报生成 prompt（版本：review_daily_v1）

你是个人 AI 助手「知微」的日报生成器。输入是我某一天的数据：按话题分组的记忆、待办变化、时间线统计、对话概况。你的任务：归纳当天，产出结构化日报。

## 规则
1. 只根据输入数据归纳，不编造未出现的事项。
2. headline：一句话概括当天（20 字内）。
3. highlights：当天要点 3~7 条，每条一句完整中文。
4. decisions：当天做出的决定；无则空数组。
5. todos.new / todos.done / todos.open：分别列当天新增 / 当天完成 / 仍未完成的待办标题（字符串数组）。
6. tomorrow：明日计划，**只能引用输入中「未完成(confirmed 未 done)」的待办**，不得凭空生成新任务。
7. insights：基于当天数据的归纳/观察；无则空数组。
8. topic_distribution：当天记忆按话题计数，形如 [{"topic":"工作","count":3}]，按 count 降序。

## 输出格式
只输出 JSON，不要任何其他文字或代码围栏。字段固定如下（数组无内容用 []）：

{"headline":"","highlights":[],"decisions":[],"todos":{"new":[],"done":[],"open":[]},"insights":[],"tomorrow":[],"topic_distribution":[{"topic":"","count":0}]}
```

- [ ] **Step 2: `prompts/review_weekly_v1.md`**

```markdown
# 知微周报生成 prompt（版本：review_weekly_v1）

你是个人 AI 助手「知微」的周报生成器。输入是我本周的数据：本周若干份日报摘要、本周记忆/待办、话题活动、每日统计序列。你的任务：归纳本周，产出结构化周报。

## 规则
1. 只根据输入数据归纳，不编造。
2. headline：一句话概括本周（20 字内）。
3. by_topic：按话题给进展块，每块含 topic（名）、progress（0~1 小数，概略完成度）、key_events（本周关键事件字符串数组）、open_todos（未完成待办标题数组）、risks（该话题风险字符串数组）。
4. trends：曲线就绪数据数组，每条含 metric（指标名，如「每日记忆数」「每日完成待办数」）、labels（可选 x 轴标签，如日期）、series（数值数组，与输入的每日序列对应）。
5. risks：本周全局风险；无则空数组。
6. next_week：下周计划；优先基于未完成待办与本周风险。

## 输出格式
只输出 JSON，不要任何其他文字或代码围栏。字段固定如下（数组无内容用 []）：

{"headline":"","by_topic":[{"topic":"","progress":0,"key_events":[],"open_todos":[],"risks":[]}],"trends":[{"metric":"","labels":[],"series":[]}],"risks":[],"next_week":[]}
```

- [ ] **Step 3: `prompts/topic_status_v1.md`**

```markdown
# 知微话题状态生成 prompt（版本：topic_status_v1）

你是个人 AI 助手「知微」的话题/项目状态生成器。输入是某个话题（项目/主题）的数据：该话题的记忆时间线、待办（未完成/已完成）、最近活动。你的任务：给出该话题的整体状态快照。

## 规则
1. 只根据输入数据归纳，不编造。
2. summary：该话题当前状态的一段话概述。
3. progress：0~1 小数，概略整体进展（无法判断给 0）。
4. milestones：已达成或计划中的里程碑/阶段（字符串数组）。
5. decisions：该话题相关的关键决定；无则空数组。
6. open_todos：未完成待办标题数组。
7. risks：风险数组，每项含 desc（风险描述）与 severity（严重度，取 "low"|"medium"|"high"）。
8. blockers：当前阻塞项（字符串数组）；无则空数组。

## 输出格式
只输出 JSON，不要任何其他文字或代码围栏。字段固定如下（数组无内容用 []）：

{"summary":"","progress":0,"milestones":[],"decisions":[],"open_todos":[],"risks":[{"desc":"","severity":"low"}],"blockers":[]}
```

- [ ] **Step 4: 校验文件存在**

Run: `ls prompts/review_daily_v1.md prompts/review_weekly_v1.md prompts/topic_status_v1.md`
Expected: 三个文件均存在。（无代码测试；内容正确性由 Task 3/4 解析/构建单测间接覆盖。）

- [ ] **Commit:** `feat(review): 日报/周报/话题状态 v1 prompt（严格 JSON）`

---

## Task 3: 三个 JSON 解析器 + 容错单测

**Files:** `internal/review/parse.go`（create）、`internal/review/parse_test.go`（create）

> 解析器把 LLM 文本 → content 结构。容错策略**逐字对齐** `memory.ParseCandidates`：先 `stripToJSON`（截首个 `{` 到末个 `}`，天然剥掉 markdown 围栏与前后废话），再 `json.Unmarshal`。彻底非法 JSON 返回 error（调用方置 `status=failed`）。**纯函数、无 DB**，是 §11 schema 的第一道单测防线。

- [ ] **Step 1: 写 parse.go**

```go
package review

import (
	"encoding/json"
	"fmt"
	"strings"
)

// stripToJSON 截取首个 '{' 到末个 '}'，剥掉模型可能输出的代码围栏/前后废话。
// 与 memory.ParseCandidates 的容错同构（那里注释「宁粗勿丢」）。
func stripToJSON(raw string) string {
	s := strings.TrimSpace(raw)
	if i := strings.Index(s, "{"); i >= 0 {
		if j := strings.LastIndex(s, "}"); j > i {
			return s[i : j+1]
		}
	}
	return s
}

// ParseDaily 解析日报 JSON；非法 JSON 返回 error（调用方置 status=failed）。
func ParseDaily(raw string) (*DailyContent, error) {
	var c DailyContent
	if err := json.Unmarshal([]byte(stripToJSON(raw)), &c); err != nil {
		return nil, fmt.Errorf("日报 JSON 解析失败: %w", err)
	}
	return &c, nil
}

// ParseWeekly 解析周报 JSON。
func ParseWeekly(raw string) (*WeeklyContent, error) {
	var c WeeklyContent
	if err := json.Unmarshal([]byte(stripToJSON(raw)), &c); err != nil {
		return nil, fmt.Errorf("周报 JSON 解析失败: %w", err)
	}
	return &c, nil
}

// ParseTopicStatus 解析话题状态 JSON。
func ParseTopicStatus(raw string) (*TopicStatusContent, error) {
	var c TopicStatusContent
	if err := json.Unmarshal([]byte(stripToJSON(raw)), &c); err != nil {
		return nil, fmt.Errorf("话题状态 JSON 解析失败: %w", err)
	}
	return &c, nil
}
```

- [ ] **Step 2: 写 parse_test.go（纯单测，无 MySQL）**

覆盖：① 干净 JSON；② 带 ```json 代码围栏 + 前后废话；③ 非法 JSON → error；④ 空/最小 JSON `{}` → 零值结构不报错。

```go
package review

import "testing"

func TestParseDailyStripsFence(t *testing.T) {
	raw := "好的，这是日报：\n```json\n{\"headline\":\"H\",\"highlights\":[\"x\"],\"todos\":{\"new\":[],\"done\":[],\"open\":[\"o\"]},\"topic_distribution\":[{\"topic\":\"工作\",\"count\":2}]}\n```\n"
	c, err := ParseDaily(raw)
	if err != nil {
		t.Fatal(err)
	}
	if c.Headline != "H" || len(c.Highlights) != 1 || c.Todos.Open[0] != "o" || c.TopicDistribution[0].Count != 2 {
		t.Errorf("解析结果异常: %+v", c)
	}
}

func TestParseDailyInvalid(t *testing.T) {
	if _, err := ParseDaily("这不是 JSON，模型跑偏了"); err == nil {
		t.Error("非法 JSON 应返回 error")
	}
}

func TestParseTopicStatusRisks(t *testing.T) {
	raw := `{"summary":"s","progress":0.5,"risks":[{"desc":"缺人","severity":"high"}],"blockers":[]}`
	c, err := ParseTopicStatus(raw)
	if err != nil {
		t.Fatal(err)
	}
	if c.Progress != 0.5 || len(c.Risks) != 1 || c.Risks[0].Severity != "high" {
		t.Errorf("解析结果异常: %+v", c)
	}
}

func TestParseWeeklyMinimal(t *testing.T) {
	c, err := ParseWeekly(`{}`)
	if err != nil || c == nil {
		t.Fatalf("空对象应解析为零值结构: %v", err)
	}
}
```

- [ ] **Step 3: 单测**

Run: `go test ./internal/review/`
Expected: 4 个解析测试通过。

- [ ] **Commit:** `feat(review): 报告 JSON 解析器（剥围栏 + 严格解析）`

---

## Task 4: 三个 prompt builder + 汇聚输入结构 + 确定性单测

**Files:** `internal/review/prompt.go`（create）、`internal/review/prompt_test.go`（create）

> builder 把「汇聚输入结构」渲染成发给 LLM 的 **user message 文本**（system 用 prompt 文件）。对齐 `memory.buildUserMessage` 的做法（`strings.Builder` 拼分节文本）。**输入结构在此定义**（gather 产出、builder 消费，Task 6~8 填充）。builder 是**纯函数、确定性、无 DB**，可直接单测。

- [ ] **Step 1: 写 prompt.go（输入结构 + 三个 builder）**

```go
package review

import (
	"fmt"
	"strings"
	"time"
)

// ---- 汇聚输入结构（gather 产出 → builder 消费）----

// TopicLines 是「话题 → 该话题下若干条文本」的分组（记忆按话题归并用）。
type TopicLines struct {
	Topic string
	Lines []string
}

// DailyInput 是日报的汇聚输入（spec §11.1 输入：当天 memory 按 topic + todo 变化 + timeline 统计 + 对话概况）。
type DailyInput struct {
	Date            time.Time
	MemoriesByTopic []TopicLines // 当天记忆按话题分组
	TodosNew        []string     // 当天新增待办标题
	TodosDone       []string     // 当天完成
	TodosOpen       []string     // 仍未完成（confirmed 未 done）
	SessionCount    int          // 当天录音条数
	TotalDurationMS int64        // 当天录音总时长
	SegmentCount    int          // 当天转写分段数
	Speakers        []string     // 当天出现的说话人
	ConversationCnt int          // 当天 agent 对话条数（概况，可为 0）
}

// WeeklyInput 是周报的汇聚输入（spec §11.2 输入：本周日报 + memory/todo + topic 活动 + 每日序列）。
type WeeklyInput struct {
	WeekStart       time.Time
	WeekEnd         time.Time
	DailyHeadlines  []string     // 本周每日日报 headline（缺失日留空串占位）
	MemoriesByTopic []TopicLines // 本周记忆按话题
	TodosDone       []string
	TodosOpen       []string
	DailyMemoryCnt  []int        // 每日记忆数序列（trends 就绪）
	DailyTodoDone   []int        // 每日完成待办数序列
}

// TopicStatusInput 是话题状态的汇聚输入（spec §11.3 输入：该 topic 的 memory 时间线 + todo + 最近活动）。
type TopicStatusInput struct {
	TopicName    string
	MemoryLines  []string // 按时间排序的记忆行（含事件时间）
	OpenTodos    []string
	DoneTodos    []string
	LastActiveAt *time.Time
}

// fmtDate 统一日期格式（YYYY-MM-DD），确定性输出便于单测。
func fmtDate(t time.Time) string { return t.Format("2006-01-02") }

// writeLines 把带标题的字符串列表写进 builder；空列表写「（无）」。
func writeLines(sb *strings.Builder, title string, lines []string) {
	fmt.Fprintf(sb, "%s：\n", title)
	if len(lines) == 0 {
		sb.WriteString("（无）\n")
		return
	}
	for _, l := range lines {
		fmt.Fprintf(sb, "- %s\n", l)
	}
}

// BuildDailyUser 组装日报 user message。
func BuildDailyUser(in DailyInput) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "日期：%s\n\n", fmtDate(in.Date))
	sb.WriteString("当天记忆（按话题）：\n")
	if len(in.MemoriesByTopic) == 0 {
		sb.WriteString("（无）\n")
	}
	for _, g := range in.MemoriesByTopic {
		fmt.Fprintf(&sb, "【%s】\n", g.Topic)
		for _, l := range g.Lines {
			fmt.Fprintf(&sb, "- %s\n", l)
		}
	}
	sb.WriteString("\n")
	writeLines(&sb, "当天新增待办", in.TodosNew)
	writeLines(&sb, "当天完成待办", in.TodosDone)
	writeLines(&sb, "未完成待办", in.TodosOpen)
	fmt.Fprintf(&sb, "\n时间线统计：录音 %d 条、总时长 %d 秒、转写 %d 段、说话人 [%s]、对话 %d 条\n",
		in.SessionCount, in.TotalDurationMS/1000, in.SegmentCount, strings.Join(in.Speakers, "、"), in.ConversationCnt)
	return sb.String()
}

// BuildWeeklyUser 组装周报 user message。
func BuildWeeklyUser(in WeeklyInput) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "周范围：%s ~ %s\n\n", fmtDate(in.WeekStart), fmtDate(in.WeekEnd))
	writeLines(&sb, "本周每日日报要点", in.DailyHeadlines)
	sb.WriteString("\n本周记忆（按话题）：\n")
	if len(in.MemoriesByTopic) == 0 {
		sb.WriteString("（无）\n")
	}
	for _, g := range in.MemoriesByTopic {
		fmt.Fprintf(&sb, "【%s】\n", g.Topic)
		for _, l := range g.Lines {
			fmt.Fprintf(&sb, "- %s\n", l)
		}
	}
	sb.WriteString("\n")
	writeLines(&sb, "本周完成待办", in.TodosDone)
	writeLines(&sb, "未完成待办", in.TodosOpen)
	fmt.Fprintf(&sb, "\n每日记忆数序列：%v\n每日完成待办数序列：%v\n", in.DailyMemoryCnt, in.DailyTodoDone)
	return sb.String()
}

// BuildTopicStatusUser 组装话题状态 user message。
func BuildTopicStatusUser(in TopicStatusInput) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "话题名称：%s\n", in.TopicName)
	if in.LastActiveAt != nil {
		fmt.Fprintf(&sb, "最近活动：%s\n", fmtDate(*in.LastActiveAt))
	}
	sb.WriteString("\n")
	writeLines(&sb, "记忆时间线", in.MemoryLines)
	writeLines(&sb, "未完成待办", in.OpenTodos)
	writeLines(&sb, "已完成待办", in.DoneTodos)
	return sb.String()
}
```

- [ ] **Step 2: 写 prompt_test.go（确定性、无 MySQL）**

```go
package review

import (
	"strings"
	"testing"
	"time"
)

func TestBuildDailyUserDeterministic(t *testing.T) {
	in := DailyInput{
		Date:            time.Date(2026, 8, 24, 0, 0, 0, 0, time.UTC),
		MemoriesByTopic: []TopicLines{{Topic: "工作", Lines: []string{"完成设计稿"}}},
		TodosNew:        []string{"发邮件"},
		SessionCount:    2, TotalDurationMS: 65000, SegmentCount: 10, Speakers: []string{"我", "张三"},
	}
	out := BuildDailyUser(in)
	for _, want := range []string{"日期：2026-08-24", "【工作】", "完成设计稿", "发邮件", "录音 2 条", "总时长 65 秒", "张三"} {
		if !strings.Contains(out, want) {
			t.Errorf("缺片段 %q，实际：\n%s", want, out)
		}
	}
	// 空分组占位
	if !strings.Contains(BuildDailyUser(DailyInput{Date: in.Date}), "（无）") {
		t.Error("空输入应含「（无）」占位")
	}
}

func TestBuildTopicStatusUser(t *testing.T) {
	out := BuildTopicStatusUser(TopicStatusInput{TopicName: "Rust 学习", OpenTodos: []string{"读完第 5 章"}})
	if !strings.Contains(out, "话题名称：Rust 学习") || !strings.Contains(out, "读完第 5 章") {
		t.Errorf("话题状态 user message 异常：\n%s", out)
	}
}
```

- [ ] **Step 3: 单测**

Run: `go test ./internal/review/`
Expected: builder 测试通过。

- [ ] **Commit:** `feat(review): 报告 prompt builder + 汇聚输入结构`

---

## Task 5: `Generator` 结构 + 纯渲染核（mock LLM 单测）

**Files:** `internal/review/generator.go`（create）、`internal/review/generator_test.go`（create）

> `Generator` 持 LLM + Model + 三份 prompt/版本 + 全部 repo（gather/persist 用，Task 6~8 才用到）。本任务只做**纯渲染核** `generateDaily/Weekly/TopicStatus`：`建 user message → Chat → 解析 → (content, rawJSON)`，**不碰 DB**。这是「渲染」与「汇聚/落库」的测试缝——mock LLM 即可覆盖成功 / LLM 失败 / 解析失败三径，**无 MySQL**。对齐 `memory.Extractor` 持 `provider.LLMProvider` 的形态。

- [ ] **Step 1: 写 generator.go**

```go
package review

import (
	"context"
	"encoding/json"
	"fmt"

	"zhiwei/internal/provider"
	"zhiwei/internal/repo"
)

// reviewUserID 单用户 MVP 固定 1（与 agent.toolUserID 一致）。
const reviewUserID = 1

// Generator 是报告引擎（spec §5.2）：从 repo 汇聚数据，直调 Ark 上的 DeepSeek
// 模型（provider.LLMProvider），产出结构化报告并落库。被 cron / HTTP / MCP 三处复用。
// LLM 用接口类型，单测可注入 mock（无需真 Ark/MySQL）。
type Generator struct {
	LLM   provider.LLMProvider // *provider.ArkLLM 实现之；单测传 mock
	Model string               // Ark 上的 DeepSeek 模型/endpoint id（cfg.AgentModel）

	// 版本化 prompt 内容 + 版本号（文件名 stem），由 main 用 os.ReadFile 装入。
	DailyPrompt, WeeklyPrompt, TopicStatusPrompt          string
	DailyPromptVer, WeeklyPromptVer, TopicStatusPromptVer string

	// 落库仓储
	Reviews       *repo.ReviewRepo
	TopicStatuses *repo.TopicStatusRepo
	// 汇聚仓储（只读）
	Memories    *repo.MemoryRepo
	Todos       *repo.TodoRepo
	Topics      *repo.TopicRepo
	Sessions    *repo.SessionRepo
	Transcripts *repo.TranscriptRepo
}

// NewGenerator 构造 Generator（参数与字段对应；main 装配时注入）。
func NewGenerator(llm provider.LLMProvider, model string,
	dailyPrompt, dailyVer, weeklyPrompt, weeklyVer, topicPrompt, topicVer string,
	reviews *repo.ReviewRepo, topicStatuses *repo.TopicStatusRepo,
	memories *repo.MemoryRepo, todos *repo.TodoRepo, topics *repo.TopicRepo,
	sessions *repo.SessionRepo, transcripts *repo.TranscriptRepo) *Generator {
	return &Generator{
		LLM: llm, Model: model,
		DailyPrompt: dailyPrompt, DailyPromptVer: dailyVer,
		WeeklyPrompt: weeklyPrompt, WeeklyPromptVer: weeklyVer,
		TopicStatusPrompt: topicPrompt, TopicStatusPromptVer: topicVer,
		Reviews: reviews, TopicStatuses: topicStatuses,
		Memories: memories, Todos: todos, Topics: topics,
		Sessions: sessions, Transcripts: transcripts,
	}
}

// generateDaily 纯渲染核：user message → Chat → 解析 → (content, rawJSON)。不碰 DB。
func (g *Generator) generateDaily(ctx context.Context, in DailyInput) (*DailyContent, json.RawMessage, error) {
	resp, err := g.LLM.Chat(ctx, provider.ChatRequest{Model: g.Model, System: g.DailyPrompt, User: BuildDailyUser(in)})
	if err != nil {
		return nil, nil, fmt.Errorf("日报 LLM 调用: %w", err)
	}
	c, err := ParseDaily(resp.Content)
	if err != nil {
		return nil, nil, err
	}
	return c, mustJSON(c), nil
}

// generateWeekly 纯渲染核。
func (g *Generator) generateWeekly(ctx context.Context, in WeeklyInput) (*WeeklyContent, json.RawMessage, error) {
	resp, err := g.LLM.Chat(ctx, provider.ChatRequest{Model: g.Model, System: g.WeeklyPrompt, User: BuildWeeklyUser(in)})
	if err != nil {
		return nil, nil, fmt.Errorf("周报 LLM 调用: %w", err)
	}
	c, err := ParseWeekly(resp.Content)
	if err != nil {
		return nil, nil, err
	}
	return c, mustJSON(c), nil
}

// generateTopicStatus 纯渲染核。
func (g *Generator) generateTopicStatus(ctx context.Context, in TopicStatusInput) (*TopicStatusContent, json.RawMessage, error) {
	resp, err := g.LLM.Chat(ctx, provider.ChatRequest{Model: g.Model, System: g.TopicStatusPrompt, User: BuildTopicStatusUser(in)})
	if err != nil {
		return nil, nil, fmt.Errorf("话题状态 LLM 调用: %w", err)
	}
	c, err := ParseTopicStatus(resp.Content)
	if err != nil {
		return nil, nil, err
	}
	return c, mustJSON(c), nil
}
```

- [ ] **Step 2: 写 generator_test.go（mock LLM，无 MySQL）**

复用 Task 1 的 `fakeLLM`。三径：成功 / LLM 错误 / 解析错误。断言 `fakeLLM.GotReq.System==prompt`、`Model==g.Model`。

```go
package review

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestGenerateDailyOK(t *testing.T) {
	f := &fakeLLM{Reply: `{"headline":"今天很好","highlights":["a"]}`}
	g := &Generator{LLM: f, Model: "m", DailyPrompt: "SYS"}
	c, raw, err := g.generateDaily(context.Background(), DailyInput{Date: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	if c.Headline != "今天很好" || len(raw) == 0 {
		t.Errorf("内容异常: %+v", c)
	}
	if f.GotReq.System != "SYS" || f.GotReq.Model != "m" {
		t.Errorf("Chat 请求未带 prompt/model: %+v", f.GotReq)
	}
}

func TestGenerateDailyLLMErr(t *testing.T) {
	g := &Generator{LLM: &fakeLLM{Err: errors.New("boom")}, Model: "m"}
	if _, _, err := g.generateDaily(context.Background(), DailyInput{}); err == nil {
		t.Error("LLM 错误应上抛")
	}
}

func TestGenerateDailyParseErr(t *testing.T) {
	g := &Generator{LLM: &fakeLLM{Reply: "模型跑偏没给 JSON"}, Model: "m"}
	if _, _, err := g.generateDaily(context.Background(), DailyInput{}); err == nil {
		t.Error("解析失败应上抛")
	}
}
```

- [ ] **Step 3: 单测**

Run: `go test ./internal/review/`
Expected: 三径测试通过；包编译。

- [ ] **Commit:** `feat(review): Generator 结构 + 纯渲染核（mock LLM 单测）`

---

## Task 6: 日报汇聚 + 编排 `Daily()` + 集成测试

**Files:** `internal/review/gather.go`（create）、`internal/review/gather_test.go`（create，含 `TestMain`/`testDSN`）

> `gatherDaily` 用**现有 repo** + **Go 内切当天窗口**产出 `DailyInput`（不新增 repo 方法）。`Daily()` 编排：汇聚 → `generateDaily` → 落库（成功 `ready`+content / 失败 `failed`+nil）→ 读回返回。`Daily()` 语义=**强制重生成**（HTTP GET 的「有则取、无则生成」分支在 Task 10 handler 里做；POST/工具走 Daily 强制生成）。落库经 `ReviewRepo.UpsertDaily`（`(user_id, review_date)` upsert，天然幂等重生成）。

- [ ] **Step 1: 写 gather.go（gatherDaily + Daily + 共用 helper）**

```go
package review

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"zhiwei/internal/repo"
)

// dayRange 返回 date 所在自然日的 [start, end)（保 date 的时区）。
func dayRange(date time.Time) (start, end time.Time) {
	y, m, d := date.Date()
	start = time.Date(y, m, d, 0, 0, 0, 0, date.Location())
	return start, start.AddDate(0, 0, 1)
}

// inRange 判断 t 是否落在 [start, end)。
func inRange(t, start, end time.Time) bool {
	return !t.Before(start) && t.Before(end)
}

// gatherDaily 汇聚当天数据为 DailyInput（现有 repo + Go 内切窗）。
func (g *Generator) gatherDaily(ctx context.Context, date time.Time) (DailyInput, error) {
	start, end := dayRange(date)
	in := DailyInput{Date: start}

	// 记忆：Since=start 取 event_at>=start（倒序），Go 内再滤 <end；按话题分组
	mrows, err := g.Memories.List(ctx, repo.MemoryFilter{Since: &start, Limit: 200})
	if err != nil {
		return in, fmt.Errorf("汇聚 memory: %w", err)
	}
	byTopic := map[string][]string{}
	var order []string
	for _, m := range mrows {
		if m.EventAt == nil || !inRange(*m.EventAt, start, end) {
			continue
		}
		topics := []string{"未归类"}
		if len(m.Topics) > 0 {
			topics = topics[:0]
			for _, tp := range m.Topics {
				topics = append(topics, tp.Name)
			}
		}
		for _, tn := range topics {
			if _, ok := byTopic[tn]; !ok {
				order = append(order, tn)
			}
			byTopic[tn] = append(byTopic[tn], m.Title)
		}
	}
	for _, tn := range order {
		in.MemoriesByTopic = append(in.MemoriesByTopic, TopicLines{Topic: tn, Lines: byTopic[tn]})
	}

	// 待办：非 dismissed 全量（有界 200），Go 内按 created_at/updated_at/status 分组
	todos, err := g.Todos.List(ctx, "", nil)
	if err != nil {
		return in, fmt.Errorf("汇聚 todo: %w", err)
	}
	for _, td := range todos {
		if inRange(td.CreatedAt, start, end) {
			in.TodosNew = append(in.TodosNew, td.Title)
		}
		if td.Status == "done" && inRange(td.UpdatedAt, start, end) {
			in.TodosDone = append(in.TodosDone, td.Title)
		}
		if td.Status == "confirmed" {
			in.TodosOpen = append(in.TodosOpen, td.Title)
		}
	}

	// 时间线统计：当天 session（有界 200），累加时长；分段/说话人 best-effort 遍历当天 session
	sessions, err := g.Sessions.List(ctx, 200, 0)
	if err != nil {
		return in, fmt.Errorf("汇聚 session: %w", err)
	}
	speakerSet := map[string]bool{}
	for _, s := range sessions {
		if !inRange(s.CreatedAt, start, end) {
			continue
		}
		in.SessionCount++
		in.TotalDurationMS += s.DurationMS
		tr, err := g.Transcripts.GetBySession(ctx, s.ID)
		if err != nil {
			continue // 无转写不阻断统计
		}
		segs, err := g.Transcripts.ListSegments(ctx, tr.ID)
		if err != nil {
			continue
		}
		in.SegmentCount += len(segs)
		for _, seg := range segs {
			if seg.SpeakerLabel != "" {
				speakerSet[seg.SpeakerLabel] = true
			}
		}
	}
	for sp := range speakerSet {
		in.Speakers = append(in.Speakers, sp)
	}
	// 注：对话概况(ConversationCnt) P3a 暂置 0——避免给 Generator 加 AgentConversationRepo 依赖；
	// spec §11.1 列它为输入之一，作为后续 enrich（低优先），不阻断日报主体。
	return in, nil
}

// Daily 生成并落库当天日报（强制重生成）；成功置 ready，LLM/解析失败置 failed 并上抛 error。
// 返回读回的行（含 id/status/content），供 handler/工具直接响应。
func (g *Generator) Daily(ctx context.Context, date time.Time) (*repo.DailyReview, error) {
	in, err := g.gatherDaily(ctx, date)
	if err != nil {
		return nil, err
	}
	_, raw, genErr := g.generateDaily(ctx, in)
	if genErr != nil {
		// 失败也落一行 failed，便于前端展示「生成失败可重试」
		if perr := g.Reviews.UpsertDaily(ctx, reviewUserID, date, nil, "failed"); perr != nil {
			return nil, fmt.Errorf("落库 failed 状态: %w (原始错误: %v)", perr, genErr)
		}
		return nil, genErr
	}
	if err := g.Reviews.UpsertDaily(ctx, reviewUserID, date, json.RawMessage(raw), "ready"); err != nil {
		return nil, fmt.Errorf("落库日报: %w", err)
	}
	return g.Reviews.GetDaily(ctx, reviewUserID, date)
}
```

- [ ] **Step 2: 写 gather_test.go（`TestMain` + `testDSN` + 日报集成测试）**

```go
package review

import (
	"context"
	"os"
	"testing"
	"time"

	"zhiwei/internal/ids"
	"zhiwei/internal/repo"
)

// TestMain 初始化雪花 ID（集成测试造数据 ids.New() 会用）。与 repo/agent 测试包一致。
func TestMain(m *testing.M) {
	_ = ids.Init(1)
	os.Exit(m.Run())
}

func testDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("TEST_MYSQL_DSN 未设置，跳过集成测试")
	}
	return dsn
}

// newGenWithFake 用真实 repo（独立库）+ 注入的 fakeLLM 装配 Generator。
func newGenWithFake(t *testing.T, f *fakeLLM) *Generator {
	t.Helper()
	db, err := repo.NewDB(testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	return &Generator{
		LLM: f, Model: "test-model",
		DailyPrompt: "SYS-DAILY", WeeklyPrompt: "SYS-WEEKLY", TopicStatusPrompt: "SYS-TOPIC",
		Reviews: &repo.ReviewRepo{DB: db}, TopicStatuses: &repo.TopicStatusRepo{DB: db},
		Memories: &repo.MemoryRepo{DB: db}, Todos: &repo.TodoRepo{DB: db}, Topics: &repo.TopicRepo{DB: db},
		Sessions: &repo.SessionRepo{DB: db}, Transcripts: &repo.TranscriptRepo{DB: db},
	}
}

func TestDailyPersistReady(t *testing.T) {
	f := &fakeLLM{Reply: `{"headline":"当天要点","highlights":["h1"]}`}
	g := newGenWithFake(t, f)
	ctx := context.Background()
	day := time.Date(2030, 1, 2, 0, 0, 0, 0, time.UTC) // 远期日期避开真实数据
	t.Cleanup(func() { _ = g.Reviews.UpsertDaily(ctx, reviewUserID, day, nil, "pending") })

	// 种一条当天记忆，验证被汇聚（不强绑断言其入 prompt，主要验证落库链路）
	sid := ids.New()
	ev := day.Add(10 * time.Hour)
	_ = g.Memories.InsertExt(ctx, g.Memories.DB, []*repo.Memory{{
		Type: "event", Title: "集成测试记忆", Content: "xxx", EpistemicType: "observed",
		SessionID: sid, Status: "active", EventAt: &ev, Confidence: 0.9,
	}})
	t.Cleanup(func() { _ = g.Memories.DeleteBySessionExt(context.Background(), g.Memories.DB, sid) })

	row, err := g.Daily(ctx, day)
	if err != nil {
		t.Fatal(err)
	}
	if row == nil || row.Status != "ready" || row.Content == nil {
		t.Fatalf("日报应 ready 且有 content: %+v", row)
	}

	// 幂等重生成：再调一次仍 ready，且 GetDaily 只一行（UpsertDaily 覆盖）
	if _, err := g.Daily(ctx, day); err != nil {
		t.Fatal(err)
	}
	got, _ := g.Reviews.GetDaily(ctx, reviewUserID, day)
	if got == nil || got.Status != "ready" {
		t.Errorf("重生成后应仍 ready: %+v", got)
	}
}

func TestDailyLLMFailMarksFailed(t *testing.T) {
	g := newGenWithFake(t, &fakeLLM{Reply: "模型没给 JSON"}) // 解析失败
	ctx := context.Background()
	day := time.Date(2030, 1, 3, 0, 0, 0, 0, time.UTC)
	t.Cleanup(func() { _ = g.Reviews.UpsertDaily(ctx, reviewUserID, day, nil, "pending") })
	if _, err := g.Daily(ctx, day); err == nil {
		t.Error("解析失败应上抛 error")
	}
	got, _ := g.Reviews.GetDaily(ctx, reviewUserID, day)
	if got == nil || got.Status != "failed" {
		t.Errorf("失败应落 status=failed: %+v", got)
	}
}
```

> **测试数据隔离**：用远期日期（2030-…）避开真实/并行数据；`t.Cleanup` 重置该日行 + 删种子 memory（`daily_review` 无删方法，故 cleanup 用 `UpsertDaily(...,"pending")` 复位）。

- [ ] **Step 3: 集成测试**

Run: `TEST_MYSQL_DSN="zhiwei:zhiwei@tcp(127.0.0.1:3307)/zhiwei_agentchat_test?parseTime=true&charset=utf8mb4&multiStatements=true" go test ./internal/review/ -run 'TestDaily'`
Expected: `TestDailyPersistReady`、`TestDailyLLMFailMarksFailed` 通过。（未设 DSN 时 Skip。）

- [ ] **Commit:** `feat(review): 日报汇聚 + Daily 编排（落库 ready/failed + 幂等）`

---

## Task 7: 周报汇聚 + 编排 `Weekly()` + 集成测试

**Files:** `internal/review/gather.go`（append）、`internal/review/gather_test.go`（append）

> `gatherWeekly` 复用**已有 `Daily` 落库**：取本周 7 天 `daily_review` 的 headline 作「每日要点」（缺失日留空串占位），叠加本周记忆（按话题）、待办、每日计数序列（trends 就绪）。`Weekly()` 编排同 `Daily()`（落 `weekly_review`，`(user_id, week_start)` upsert 幂等）。周起点 = 周一（调用方传入；调度器 Task 11 计算）。

- [ ] **Step 1: append gatherWeekly + Weekly 到 gather.go**

```go
// gatherWeekly 汇聚本周数据为 WeeklyInput。weekStart 应为周一 00:00。
func (g *Generator) gatherWeekly(ctx context.Context, weekStart time.Time) (WeeklyInput, error) {
	ws, _ := dayRange(weekStart)
	weekEnd := ws.AddDate(0, 0, 6)      // 周日（存 weekly_review.week_end）
	rangeEnd := ws.AddDate(0, 0, 7)     // [ws, rangeEnd) 半开区间
	in := WeeklyInput{WeekStart: ws, WeekEnd: weekEnd}

	// 每日日报 headline + 每日完成待办数序列（按天桶）
	in.DailyHeadlines = make([]string, 7)
	in.DailyMemoryCnt = make([]int, 7)
	in.DailyTodoDone = make([]int, 7)
	for i := 0; i < 7; i++ {
		day := ws.AddDate(0, 0, i)
		if row, err := g.Reviews.GetDaily(ctx, reviewUserID, day); err == nil && row != nil && row.Content != nil {
			if dc, err := ParseDaily(string(*row.Content)); err == nil {
				in.DailyHeadlines[i] = dc.Headline
			}
		}
	}

	// 本周记忆（按话题 + 每日计数）
	mrows, err := g.Memories.List(ctx, repo.MemoryFilter{Since: &ws, Limit: 500})
	if err != nil {
		return in, fmt.Errorf("汇聚 memory: %w", err)
	}
	byTopic := map[string][]string{}
	var order []string
	for _, m := range mrows {
		if m.EventAt == nil || !inRange(*m.EventAt, ws, rangeEnd) {
			continue
		}
		dayIdx := int(m.EventAt.Sub(ws).Hours()) / 24
		if dayIdx >= 0 && dayIdx < 7 {
			in.DailyMemoryCnt[dayIdx]++
		}
		names := []string{"未归类"}
		if len(m.Topics) > 0 {
			names = names[:0]
			for _, tp := range m.Topics {
				names = append(names, tp.Name)
			}
		}
		for _, tn := range names {
			if _, ok := byTopic[tn]; !ok {
				order = append(order, tn)
			}
			byTopic[tn] = append(byTopic[tn], m.Title)
		}
	}
	for _, tn := range order {
		in.MemoriesByTopic = append(in.MemoriesByTopic, TopicLines{Topic: tn, Lines: byTopic[tn]})
	}

	// 待办：本周完成 + 未完成；完成按 updated_at 天桶计数
	todos, err := g.Todos.List(ctx, "", nil)
	if err != nil {
		return in, fmt.Errorf("汇聚 todo: %w", err)
	}
	for _, td := range todos {
		if td.Status == "done" && inRange(td.UpdatedAt, ws, rangeEnd) {
			in.TodosDone = append(in.TodosDone, td.Title)
			dayIdx := int(td.UpdatedAt.Sub(ws).Hours()) / 24
			if dayIdx >= 0 && dayIdx < 7 {
				in.DailyTodoDone[dayIdx]++
			}
		}
		if td.Status == "confirmed" {
			in.TodosOpen = append(in.TodosOpen, td.Title)
		}
	}
	return in, nil
}

// Weekly 生成并落库本周周报（强制重生成）。成功 ready / 失败 failed 并上抛。
func (g *Generator) Weekly(ctx context.Context, weekStart time.Time) (*repo.WeeklyReview, error) {
	in, err := g.gatherWeekly(ctx, weekStart)
	if err != nil {
		return nil, err
	}
	ws, _ := dayRange(weekStart)
	weekEnd := ws.AddDate(0, 0, 6)
	_, raw, genErr := g.generateWeekly(ctx, in)
	if genErr != nil {
		if perr := g.Reviews.UpsertWeekly(ctx, reviewUserID, ws, weekEnd, nil, "failed"); perr != nil {
			return nil, fmt.Errorf("落库 failed 状态: %w (原始错误: %v)", perr, genErr)
		}
		return nil, genErr
	}
	if err := g.Reviews.UpsertWeekly(ctx, reviewUserID, ws, weekEnd, json.RawMessage(raw), "ready"); err != nil {
		return nil, fmt.Errorf("落库周报: %w", err)
	}
	return g.Reviews.GetWeekly(ctx, reviewUserID, ws)
}
```

- [ ] **Step 2: append 周报集成测试到 gather_test.go**

```go
func TestWeeklyPersistReady(t *testing.T) {
	f := &fakeLLM{Reply: `{"headline":"本周总结","by_topic":[{"topic":"工作","progress":0.5,"key_events":["e1"],"open_todos":[],"risks":[]}],"trends":[{"metric":"每日记忆数","series":[1,0,0,0,0,0,0]}],"risks":[],"next_week":["x"]}`}
	g := newGenWithFake(t, f)
	ctx := context.Background()
	ws := time.Date(2030, 1, 7, 0, 0, 0, 0, time.UTC) // 2030-01-07 是周一
	we := ws.AddDate(0, 0, 6)
	t.Cleanup(func() { _ = g.Reviews.UpsertWeekly(ctx, reviewUserID, ws, we, nil, "pending") })

	row, err := g.Weekly(ctx, ws)
	if err != nil {
		t.Fatal(err)
	}
	if row == nil || row.Status != "ready" || row.Content == nil {
		t.Fatalf("周报应 ready 且有 content: %+v", row)
	}
	var wc WeeklyContent
	if err := json.Unmarshal(*row.Content, &wc); err != nil || wc.Headline != "本周总结" {
		t.Errorf("周报 content 异常: %+v (err=%v)", wc, err)
	}
}
```

（顶部 import 需含 `encoding/json`。）

- [ ] **Step 3: 集成测试**

Run: `TEST_MYSQL_DSN="...zhiwei_agentchat_test..." go test ./internal/review/ -run 'TestWeekly'`
Expected: `TestWeeklyPersistReady` 通过。

- [ ] **Commit:** `feat(review): 周报汇聚 + Weekly 编排（7 日日报 + trends 序列）`

---

## Task 8: 话题状态汇聚 + 编排 `TopicStatus()` + 集成测试

**Files:** `internal/review/gather.go`（append）、`internal/review/gather_test.go`（append）

> `gatherTopicStatus` 用现有 `Topics.Get` + `Memories.ListByTopic` + `Todos.ListByTopic` 汇聚。落库经 `TopicStatusRepo.Insert`——**追加式历史快照**（`topic_status` 无 status 列，见 §6.2），故与日/周报不同：**失败时不插入、直接上抛**（无 failed 行概念）。返回 `GetLatest`。

- [ ] **Step 1: append gatherTopicStatus + TopicStatus 到 gather.go**

```go
// gatherTopicStatus 汇聚某话题数据为 TopicStatusInput。话题不存在返回 error。
func (g *Generator) gatherTopicStatus(ctx context.Context, topicID ids.ID) (TopicStatusInput, error) {
	var in TopicStatusInput
	tp, err := g.Topics.Get(ctx, topicID)
	if err != nil {
		return in, fmt.Errorf("话题不存在: %w", err)
	}
	in.TopicName = tp.Name

	mrows, err := g.Memories.ListByTopic(ctx, topicID) // 已按 event_at DESC
	if err != nil {
		return in, fmt.Errorf("汇聚话题 memory: %w", err)
	}
	// 时间线转升序展示；记录最近活动时间（DESC 首行）
	for i := len(mrows) - 1; i >= 0; i-- {
		m := mrows[i]
		line := m.Title
		if m.EventAt != nil {
			line = fmt.Sprintf("[%s] %s", fmtDate(*m.EventAt), m.Title)
		}
		in.MemoryLines = append(in.MemoryLines, line)
	}
	if len(mrows) > 0 && mrows[0].EventAt != nil {
		in.LastActiveAt = mrows[0].EventAt
	}

	todos, err := g.Todos.ListByTopic(ctx, topicID) // 含 done，不含 dismissed
	if err != nil {
		return in, fmt.Errorf("汇聚话题 todo: %w", err)
	}
	for _, td := range todos {
		if td.Status == "done" {
			in.DoneTodos = append(in.DoneTodos, td.Title)
		} else {
			in.OpenTodos = append(in.OpenTodos, td.Title)
		}
	}
	return in, nil
}

// TopicStatus 生成并追加落库某话题的状态快照（现算 + 落 topic_status）。
// 失败直接上抛（topic_status 无 status 列，不落 failed 行）。返回最新快照。
func (g *Generator) TopicStatus(ctx context.Context, topicID ids.ID) (*repo.TopicStatus, error) {
	in, err := g.gatherTopicStatus(ctx, topicID)
	if err != nil {
		return nil, err
	}
	_, raw, genErr := g.generateTopicStatus(ctx, in)
	if genErr != nil {
		return nil, genErr
	}
	if err := g.TopicStatuses.Insert(ctx, reviewUserID, topicID, json.RawMessage(raw)); err != nil {
		return nil, fmt.Errorf("落库话题状态: %w", err)
	}
	return g.TopicStatuses.GetLatest(ctx, topicID)
}
```

（gather.go 顶部 import 需含 `zhiwei/internal/ids`。）

- [ ] **Step 2: append 话题状态集成测试到 gather_test.go**

```go
func TestTopicStatusPersist(t *testing.T) {
	f := &fakeLLM{Reply: `{"summary":"进行中","progress":0.4,"milestones":["m1"],"open_todos":[],"risks":[{"desc":"缺资料","severity":"medium"}],"blockers":[]}`}
	g := newGenWithFake(t, f)
	ctx := context.Background()

	tp := &repo.Topic{Name: "集成测试话题", Status: "active", CreatedBy: "user"}
	if err := g.Topics.Create(ctx, tp); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = g.Topics.Delete(context.Background(), tp.ID)
		_, _ = g.TopicStatuses.DB.ExecContext(context.Background(), `DELETE FROM topic_status WHERE topic_id = ?`, tp.ID.Int64())
	})

	row, err := g.TopicStatus(ctx, tp.ID)
	if err != nil {
		t.Fatal(err)
	}
	if row == nil || row.Content == nil {
		t.Fatalf("话题状态应有快照: %+v", row)
	}
	var tc TopicStatusContent
	if err := json.Unmarshal(*row.Content, &tc); err != nil || tc.Progress != 0.4 || len(tc.Risks) != 1 {
		t.Errorf("话题状态 content 异常: %+v (err=%v)", tc, err)
	}

	// 失败路径：解析失败不插新行、直接上抛
	g.LLM = &fakeLLM{Reply: "没有 JSON"}
	if _, err := g.TopicStatus(ctx, tp.ID); err == nil {
		t.Error("解析失败应上抛")
	}
}
```

- [ ] **Step 3: 集成测试**

Run: `TEST_MYSQL_DSN="...zhiwei_agentchat_test..." go test ./internal/review/ -run 'TestTopicStatus'`
Expected: `TestTopicStatusPersist` 通过。

- [ ] **Commit:** `feat(review): 话题状态汇聚 + TopicStatus 编排（追加式快照）`

---

## Task 9: MCP 报告工具 + `RegisterReportTools`

**Files:** `internal/review/tools.go`（create）、`internal/review/tools_test.go`（create）

> spec §7.3 `generate_report` + §7.1 `get_topic_status`。**逻辑放 `package review`**（不改 `internal/agent/mcp_server.go`/`mcp_tools.go`）；导出 `RegisterReportTools(s *mcp.Server, gen *Generator)` 供协调者在 `agent.NewMCPServer` 之后调用注册。工具 handler 形态对齐 `agent/mcp_tools.go`（go-sdk `func(ctx, *mcp.CallToolRequest, In) (*mcp.CallToolResult, any, error)`，结果 `json.Marshal` 进 `TextContent`）。handler 拆成 `xxxHandler(gen)` 便于直测。

- [ ] **Step 1: 写 tools.go**

```go
package review

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"zhiwei/internal/ids"
)

// RegisterReportTools 把报告工具注册到（已由 agent.NewMCPServer 建好的）MCP server。
// 协调者装配：s := agent.NewMCPServer(deps); review.RegisterReportTools(s, gen)。
func RegisterReportTools(s *mcp.Server, gen *Generator) {
	mcp.AddTool(s, &mcp.Tool{
		Name:        "generate_report",
		Description: "生成结构化报告。type=daily|weekly|topic_status；target 可选：daily/weekly 传日期(YYYY-MM-DD，空=今天/本周)，topic_status 传话题 id。返回报告对象。",
	}, generateReportHandler(gen))

	mcp.AddTool(s, &mcp.Tool{
		Name:        "get_topic_status",
		Description: "取某话题(项目/主题)的整体状态快照(进展/里程碑/未完成待办/风险/阻塞)。现算并落库，返回最新快照。",
	}, getTopicStatusHandler(gen))
}

// jsonResult 把任意值 JSON 序列化为单个 TextContent 结果（对齐 agent/mcp_tools.go）。
func jsonResult(v any) (*mcp.CallToolResult, any, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, nil, err
	}
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(b)}}}, nil, nil
}

// mondayOf 返回 t 所在自然周的周一 00:00（周报周起点）。
func mondayOf(t time.Time) time.Time {
	start, _ := dayRange(t)
	// time.Weekday: Sunday=0..Saturday=6；换成距周一的偏移（周一=0）
	offset := (int(start.Weekday()) + 6) % 7
	return start.AddDate(0, 0, -offset)
}

type generateReportArgs struct {
	Type   string `json:"type" jsonschema:"报告类型: daily|weekly|topic_status"`
	Target string `json:"target,omitempty" jsonschema:"daily/weekly 传日期 YYYY-MM-DD(空=今天/本周)；topic_status 传话题 id"`
}

func generateReportHandler(gen *Generator) func(context.Context, *mcp.CallToolRequest, generateReportArgs) (*mcp.CallToolResult, any, error) {
	return func(ctx context.Context, _ *mcp.CallToolRequest, a generateReportArgs) (*mcp.CallToolResult, any, error) {
		switch a.Type {
		case "daily":
			day := time.Now()
			if a.Target != "" {
				d, err := time.Parse("2006-01-02", a.Target)
				if err != nil {
					return nil, nil, fmt.Errorf("target 日期非法(需 YYYY-MM-DD): %w", err)
				}
				day = d
			}
			row, err := gen.Daily(ctx, day)
			if err != nil {
				return nil, nil, err
			}
			return jsonResult(row)
		case "weekly":
			base := time.Now()
			if a.Target != "" {
				d, err := time.Parse("2006-01-02", a.Target)
				if err != nil {
					return nil, nil, fmt.Errorf("target 日期非法(需 YYYY-MM-DD): %w", err)
				}
				base = d
			}
			row, err := gen.Weekly(ctx, mondayOf(base))
			if err != nil {
				return nil, nil, err
			}
			return jsonResult(row)
		case "topic_status":
			tid, err := ids.ParseID(a.Target)
			if err != nil {
				return nil, nil, fmt.Errorf("target 需为话题 id: %w", err)
			}
			row, err := gen.TopicStatus(ctx, tid)
			if err != nil {
				return nil, nil, err
			}
			return jsonResult(row)
		default:
			return nil, nil, fmt.Errorf("未知报告类型: %q", a.Type)
		}
	}
}

type getTopicStatusArgs struct {
	TopicID string `json:"topic_id" jsonschema:"话题 id"`
}

func getTopicStatusHandler(gen *Generator) func(context.Context, *mcp.CallToolRequest, getTopicStatusArgs) (*mcp.CallToolResult, any, error) {
	return func(ctx context.Context, _ *mcp.CallToolRequest, a getTopicStatusArgs) (*mcp.CallToolResult, any, error) {
		tid, err := ids.ParseID(a.TopicID)
		if err != nil {
			return nil, nil, fmt.Errorf("topic_id 非法: %w", err)
		}
		row, err := gen.TopicStatus(ctx, tid)
		if err != nil {
			return nil, nil, err
		}
		return jsonResult(row)
	}
}
```

- [ ] **Step 2: 写 tools_test.go（集成库 + mock LLM，直调 handler）**

```go
package review

import (
	"context"
	"encoding/json"
	"testing"

	"zhiwei/internal/repo"
)

func TestGenerateReportTopicStatusTool(t *testing.T) {
	f := &fakeLLM{Reply: `{"summary":"s","progress":0.3,"risks":[],"blockers":[]}`}
	g := newGenWithFake(t, f)
	ctx := context.Background()
	tp := &repo.Topic{Name: "工具测试话题", Status: "active", CreatedBy: "user"}
	if err := g.Topics.Create(ctx, tp); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = g.Topics.Delete(context.Background(), tp.ID)
		_, _ = g.TopicStatuses.DB.ExecContext(context.Background(), `DELETE FROM topic_status WHERE topic_id = ?`, tp.ID.Int64())
	})

	res, _, err := generateReportHandler(g)(ctx, nil, generateReportArgs{Type: "topic_status", Target: tp.ID.String()})
	if err != nil {
		t.Fatal(err)
	}
	// 结果是 repo.TopicStatus 行的 JSON；断言可解回且带 content
	tc := res.Content[0].(*mcp.TextContent).Text
	var row repo.TopicStatus
	if err := json.Unmarshal([]byte(tc), &row); err != nil || row.Content == nil {
		t.Errorf("工具结果异常: %s (err=%v)", tc, err)
	}
}

func TestGenerateReportBadType(t *testing.T) {
	g := &Generator{LLM: &fakeLLM{}} // 不触 DB（bad type 提前返回）
	if _, _, err := generateReportHandler(g)(context.Background(), nil, generateReportArgs{Type: "xxx"}); err == nil {
		t.Error("未知类型应报错")
	}
}
```

（import 需含 `github.com/modelcontextprotocol/go-sdk/mcp`。`TestGenerateReportBadType` 无 DSN 也能跑——bad type 在触库前返回。）

- [ ] **Step 3: 测试**

Run: `go test ./internal/review/ -run 'TestGenerateReportBadType'`（无 DSN）+ 带 DSN 跑 `TestGenerateReportTopicStatusTool`。
Expected: 通过。

- [ ] **Commit:** `feat(review): MCP 报告工具 generate_report/get_topic_status + RegisterReportTools`

---

## Task 10: HTTP handler `internal/api/review.go` + `RegisterReviews`

**Files:** `internal/api/review.go`（create）、`internal/api/review_test.go`（create）

> spec §13：`GET/POST /api/reviews/daily`、`GET/POST /api/reviews/weekly`、`GET /api/topics/{id}/status`。**GET = 有则取、无则生成（latest-or-generate）**；**POST /generate = 强制重生成**。新文件 `package api`，导出 `RegisterReviews(r chi.Router, gen *review.Generator)`——**不改 router.go**（协调者接线）。复用 `package api` 现有 `writeJSON(w, v)` / `writeJSONError(w, msg, code)`（在 `query.go`）。generator 的 `Reviews`/`TopicStatuses` 字段导出，handler 直接用其做「latest」读。

- [ ] **Step 1: 写 review.go**

```go
package api

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"zhiwei/internal/ids"
	"zhiwei/internal/review"
)

// ReviewHandler 提供报告读取/生成端点（后端；报告前端页由协调者集成 web/*）。
type ReviewHandler struct {
	Gen *review.Generator
}

// RegisterReviews 挂载 /api/reviews/* 与 /api/topics/{id}/status（router.go 统一接线在协调者侧）。
func RegisterReviews(r chi.Router, gen *review.Generator) {
	h := &ReviewHandler{Gen: gen}
	r.Get("/api/reviews/daily", h.getDaily)
	r.Post("/api/reviews/daily/generate", h.generateDaily)
	r.Get("/api/reviews/weekly", h.getWeekly)
	r.Post("/api/reviews/weekly/generate", h.generateWeekly)
	r.Get("/api/topics/{id}/status", h.getTopicStatus)
}

// parseDateOrToday 解析 ?date=YYYY-MM-DD；空则用今天。第二返回值 ok=false 表示格式非法。
func parseDateOrToday(s string) (time.Time, bool) {
	if s == "" {
		return time.Now(), true
	}
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// mondayOf 返回 t 所在周的周一 00:00（周报周起点；api 侧本地实现，不跨包引用）。
func mondayOf(t time.Time) time.Time {
	y, m, d := t.Date()
	start := time.Date(y, m, d, 0, 0, 0, 0, t.Location())
	offset := (int(start.Weekday()) + 6) % 7
	return start.AddDate(0, 0, -offset)
}

// getDaily：有 ready 日报则取，否则生成（latest-or-generate）。
func (h *ReviewHandler) getDaily(w http.ResponseWriter, r *http.Request) {
	date, ok := parseDateOrToday(r.URL.Query().Get("date"))
	if !ok {
		writeJSONError(w, "date 需 YYYY-MM-DD", http.StatusBadRequest)
		return
	}
	if row, err := h.Gen.Reviews.GetDaily(r.Context(), 1, date); err == nil && row != nil && row.Status == "ready" {
		writeJSON(w, row)
		return
	}
	row, err := h.Gen.Daily(r.Context(), date)
	if err != nil {
		writeJSONError(w, err.Error(), http.StatusBadGateway)
		return
	}
	writeJSON(w, row)
}

// generateDaily：强制重生成（POST，可选 body {date}）。
func (h *ReviewHandler) generateDaily(w http.ResponseWriter, r *http.Request) {
	var body struct{ Date string `json:"date"` }
	_ = json.NewDecoder(r.Body).Decode(&body)
	date, ok := parseDateOrToday(body.Date)
	if !ok {
		writeJSONError(w, "date 需 YYYY-MM-DD", http.StatusBadRequest)
		return
	}
	row, err := h.Gen.Daily(r.Context(), date)
	if err != nil {
		writeJSONError(w, err.Error(), http.StatusBadGateway)
		return
	}
	writeJSON(w, row)
}

// getWeekly：有 ready 周报则取，否则生成。
func (h *ReviewHandler) getWeekly(w http.ResponseWriter, r *http.Request) {
	base, ok := parseDateOrToday(r.URL.Query().Get("week_start"))
	if !ok {
		writeJSONError(w, "week_start 需 YYYY-MM-DD", http.StatusBadRequest)
		return
	}
	ws := mondayOf(base)
	if row, err := h.Gen.Reviews.GetWeekly(r.Context(), 1, ws); err == nil && row != nil && row.Status == "ready" {
		writeJSON(w, row)
		return
	}
	row, err := h.Gen.Weekly(r.Context(), ws)
	if err != nil {
		writeJSONError(w, err.Error(), http.StatusBadGateway)
		return
	}
	writeJSON(w, row)
}

// generateWeekly：强制重生成（POST，可选 body {week_start}）。
func (h *ReviewHandler) generateWeekly(w http.ResponseWriter, r *http.Request) {
	var body struct{ WeekStart string `json:"week_start"` }
	_ = json.NewDecoder(r.Body).Decode(&body)
	base, ok := parseDateOrToday(body.WeekStart)
	if !ok {
		writeJSONError(w, "week_start 需 YYYY-MM-DD", http.StatusBadRequest)
		return
	}
	row, err := h.Gen.Weekly(r.Context(), mondayOf(base))
	if err != nil {
		writeJSONError(w, err.Error(), http.StatusBadGateway)
		return
	}
	writeJSON(w, row)
}

// getTopicStatus：有快照则取最新，否则生成；?refresh=1 强制重算。
func (h *ReviewHandler) getTopicStatus(w http.ResponseWriter, r *http.Request) {
	tid, err := ids.ParseID(chi.URLParam(r, "id"))
	if err != nil {
		writeJSONError(w, "invalid id", http.StatusBadRequest)
		return
	}
	if r.URL.Query().Get("refresh") != "1" {
		if row, err := h.Gen.TopicStatuses.GetLatest(r.Context(), tid); err == nil && row != nil {
			writeJSON(w, row)
			return
		}
	}
	row, err := h.Gen.TopicStatus(r.Context(), tid)
	if err != nil {
		writeJSONError(w, err.Error(), http.StatusBadGateway)
		return
	}
	writeJSON(w, row)
}
```

- [ ] **Step 2: 写 review_test.go（httptest + 集成库 + 本地 mock LLM）**

`fakeLLM` 在 review 包内不可见 → api 测试定义本地 mock（实现 `provider.LLMProvider`）。用 `review.Generator` 结构体字面量（字段导出）+ 真 repo 装配。

```go
package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"zhiwei/internal/ids"
	"zhiwei/internal/provider"
	"zhiwei/internal/repo"
	"zhiwei/internal/review"
)

func TestMain(m *testing.M) { _ = ids.Init(1); os.Exit(m.Run()) }

type stubLLM struct{ reply string }

func (s stubLLM) Chat(context.Context, provider.ChatRequest) (provider.ChatResponse, error) {
	return provider.ChatResponse{Content: s.reply}, nil
}

func TestGetDailyGeneratesWhenMissing(t *testing.T) {
	dsn := os.Getenv("TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("TEST_MYSQL_DSN 未设置")
	}
	db, err := repo.NewDB(dsn)
	if err != nil {
		t.Fatal(err)
	}
	gen := &review.Generator{
		LLM: stubLLM{reply: `{"headline":"HTTP 日报"}`}, Model: "m", DailyPrompt: "S",
		Reviews: &repo.ReviewRepo{DB: db}, TopicStatuses: &repo.TopicStatusRepo{DB: db},
		Memories: &repo.MemoryRepo{DB: db}, Todos: &repo.TodoRepo{DB: db}, Topics: &repo.TopicRepo{DB: db},
		Sessions: &repo.SessionRepo{DB: db}, Transcripts: &repo.TranscriptRepo{DB: db},
	}
	r := chi.NewRouter()
	RegisterReviews(r, gen)
	day, _ := time.Parse("2006-01-02", "2031-02-02")
	t.Cleanup(func() { _ = gen.Reviews.UpsertDaily(context.Background(), 1, day, nil, "pending") })

	req := httptest.NewRequest(http.MethodGet, "/api/reviews/daily?date=2031-02-02", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("状态码 %d, body=%s", rec.Code, rec.Body.String())
	}
	var row repo.DailyReview
	if err := json.Unmarshal(rec.Body.Bytes(), &row); err != nil || row.Status != "ready" {
		t.Errorf("应返回 ready 日报: %s (err=%v)", rec.Body.String(), err)
	}
}
```

> `date` 参数格式错误（如 `?date=xx`）应得 400——补一条 `TestGetDailyBadDate`（无需 DSN：400 在触库前返回）。

```go
func TestGetDailyBadDate(t *testing.T) {
	gen := &review.Generator{} // 400 在触库前返回，无需 DB
	r := chi.NewRouter()
	RegisterReviews(r, gen)
	req := httptest.NewRequest(http.MethodGet, "/api/reviews/daily?date=notadate", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("非法日期应 400, got %d", rec.Code)
	}
}
```

- [ ] **Step 3: 测试**

Run: `go test ./internal/api/ -run 'TestGetDailyBadDate'`（无 DSN）+ 带 DSN 跑 `TestGetDailyGeneratesWhenMissing`。
Expected: 通过。

- [ ] **Commit:** `feat(api): /api/reviews/* + /api/topics/{id}/status（latest-or-generate）`

---

## Task 11: 最小定时器 `schedule.go` + `nextFireAt` 纯单测

**Files:** `internal/review/schedule.go`（create）、`internal/review/schedule_test.go`（create）

> **go.mod 无 cron 库**（无 robfig/cron），依「优先成熟开源方案，但仅日/周两个定时需求不值得引第三方」——用标准库 `time.Timer` 自实现**最小调度**：只取 `ZW_REVIEW_DAILY_CRON`（5 字段 `m h * * *`）里的**分钟+小时**，日报每天该时刻触发、周报每周一该时刻触发。**不支持完整 cron 语义**（day/month/weekday 字段忽略），够本期用；如需完整 cron 后续再引库。可测的核心是**纯 `nextDaily/nextWeekly`（注入 now）**，不依赖真实时钟/DB。调度循环仿 `internal/pipeline/pool.go` 的 `time.NewTicker` + `select{ctx.Done()}` 范式。

- [ ] **Step 1: 写 schedule.go**

```go
package review

import (
	"context"
	"log"
	"strconv"
	"strings"
	"time"
)

// parseCronHM 从 5 字段 cron（"m h * * *"）取分钟+小时；解析失败回退 22:00。
// 只支持纯数字的 m/h 字段（本期日/周报够用），不解析 */范围/列表。
func parseCronHM(expr string) (hour, min int) {
	hour, min = 22, 0 // 默认 22:00（对齐 cfg 默认 "0 22 * * *"）
	f := strings.Fields(expr)
	if len(f) >= 2 {
		if v, err := strconv.Atoi(f[0]); err == nil && v >= 0 && v < 60 {
			min = v
		}
		if v, err := strconv.Atoi(f[1]); err == nil && v >= 0 && v < 24 {
			hour = v
		}
	}
	return hour, min
}

// nextDaily 返回 now 之后最近的「每天 hh:mm」时刻（若今日该时刻已过则明天）。
func nextDaily(now time.Time, hour, min int) time.Time {
	y, m, d := now.Date()
	t := time.Date(y, m, d, hour, min, 0, 0, now.Location())
	if !t.After(now) {
		t = t.AddDate(0, 0, 1)
	}
	return t
}

// nextWeekly 返回 now 之后最近的「周一 hh:mm」时刻。
func nextWeekly(now time.Time, hour, min int) time.Time {
	t := nextDaily(now, hour, min)
	for t.Weekday() != time.Monday {
		t = t.AddDate(0, 0, 1)
	}
	return t
}

// Scheduler 是报告定时器：日报每天、周报每周一，均在 hh:mm 触发。
type Scheduler struct {
	Gen        *Generator
	Hour, Min  int
}

// NewScheduler 从 cron 表达式取 hh:mm 构造调度器。
func NewScheduler(gen *Generator, cronExpr string) *Scheduler {
	h, m := parseCronHM(cronExpr)
	return &Scheduler{Gen: gen, Hour: h, Min: m}
}

// Start 启动日报 + 周报两个调度 goroutine；随 ctx 取消退出。
// 触发时对「当前日期/本周」生成（Daily/Weekly 幂等 upsert，重复触发安全）。
func (s *Scheduler) Start(ctx context.Context) {
	go s.loop(ctx, "daily", nextDaily, func(now time.Time) {
		if _, err := s.Gen.Daily(ctx, now); err != nil {
			log.Printf("[review] 日报定时生成失败: %v", err)
		}
	})
	go s.loop(ctx, "weekly", nextWeekly, func(now time.Time) {
		if _, err := s.Gen.Weekly(ctx, mondayOf(now)); err != nil {
			log.Printf("[review] 周报定时生成失败: %v", err)
		}
	})
}

// loop 通用调度循环：算下次触发 → Timer → 触发 fire → 重排；ctx 取消即退。
func (s *Scheduler) loop(ctx context.Context, name string,
	next func(time.Time, int, int) time.Time, fire func(time.Time)) {
	for {
		now := time.Now()
		at := next(now, s.Hour, s.Min)
		timer := time.NewTimer(at.Sub(now))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
			log.Printf("[review] %s 定时触发 @ %s", name, at.Format(time.RFC3339))
			fire(time.Now())
		}
	}
}
```

- [ ] **Step 2: 写 schedule_test.go（纯单测，注入 now，无 DB/无真实时钟）**

```go
package review

import (
	"testing"
	"time"
)

func TestParseCronHM(t *testing.T) {
	if h, m := parseCronHM("0 22 * * *"); h != 22 || m != 0 {
		t.Errorf("期望 22:00, got %d:%d", h, m)
	}
	if h, m := parseCronHM("30 9 * * *"); h != 9 || m != 30 {
		t.Errorf("期望 09:30, got %d:%d", h, m)
	}
	if h, m := parseCronHM("garbage"); h != 22 || m != 0 {
		t.Errorf("非法应回退 22:00, got %d:%d", h, m)
	}
}

func TestNextDaily(t *testing.T) {
	now := time.Date(2026, 8, 24, 10, 0, 0, 0, time.UTC) // 周一 10:00
	// 今天 22:00 未过 → 今天 22:00
	if got := nextDaily(now, 22, 0); !got.Equal(time.Date(2026, 8, 24, 22, 0, 0, 0, time.UTC)) {
		t.Errorf("got %s", got)
	}
	// 今天 09:00 已过 → 明天 09:00
	if got := nextDaily(now, 9, 0); !got.Equal(time.Date(2026, 8, 25, 9, 0, 0, 0, time.UTC)) {
		t.Errorf("got %s", got)
	}
}

func TestNextWeekly(t *testing.T) {
	now := time.Date(2026, 8, 25, 10, 0, 0, 0, time.UTC) // 周二
	got := nextWeekly(now, 22, 0)
	if got.Weekday() != time.Monday || !got.Equal(time.Date(2026, 8, 31, 22, 0, 0, 0, time.UTC)) {
		t.Errorf("下个周一 22:00 应为 2026-08-31, got %s (%s)", got, got.Weekday())
	}
}
```

- [ ] **Step 3: 单测**

Run: `go test ./internal/review/ -run 'TestParseCronHM|TestNext'`
Expected: 三个纯逻辑测试通过（无需 DSN）。

- [ ] **Commit:** `feat(review): 日/周报最小定时器（time.Timer，非完整 cron）`

---

## Task 12: COORDINATOR INTEGRATION（本计划不改这些文件）

> 以下接线由**协调者**在集成期完成，**本计划不改** `cmd/zhiwei-server/main.go`、`internal/agent/mcp_server.go`、`internal/api/router.go`。此处给出**待插入片段**，避免与并行分支（agent runtime/前端）改同一文件冲突。

- [ ] **A. 装 prompt + 构造 Generator（main.go，`db`/repo 已就绪处之后）**

```go
// 报告 prompt（版本 = 文件名 stem，对齐现有 extraction prompt 装法）
dailyBytes, _ := os.ReadFile("prompts/review_daily_v1.md")
weeklyBytes, _ := os.ReadFile("prompts/review_weekly_v1.md")
topicBytes, _ := os.ReadFile("prompts/topic_status_v1.md")
ver := func(p string) string { return strings.TrimSuffix(filepath.Base(p), ".md") }

// 报告模型：优先 cfg.AgentModel；为空则回退强模型（AgentModel 默认 "" → 必须回退，否则 model 空串调用失败）
reportModel := cfg.AgentModel
if reportModel == "" {
	reportModel = cfg.LLMStrongModel
}
reviews := &repo.ReviewRepo{DB: db}
topicStatuses := &repo.TopicStatusRepo{DB: db}
reviewer := review.NewGenerator(
	llm, reportModel, // 复用已构造的 provider.NewArkLLM(...) 结果（报告直调 Ark）
	string(dailyBytes), ver("prompts/review_daily_v1.md"),
	string(weeklyBytes), ver("prompts/review_weekly_v1.md"),
	string(topicBytes), ver("prompts/topic_status_v1.md"),
	reviews, topicStatuses, memories, todos, topics, sessions, transcripts,
)
```

- [ ] **B. 注册 MCP 报告工具（在 `mcpSrv := agent.NewMCPServer(...)` 之后）**

```go
review.RegisterReportTools(mcpSrv, reviewer) // generate_report / get_topic_status
```

- [ ] **C. 挂 HTTP 路由（与其它 api.RegisterXxx 并列）**

```go
api.RegisterReviews(r, reviewer) // GET/POST /api/reviews/daily·weekly + GET /api/topics/{id}/status
```
> 注：`/api/topics/{id}/status` 与 `internal/api/topic.go` 已有的 `/api/topics/...` 路由**路径不冲突**（chi 按 pattern 区分），可并存。

- [ ] **D. 启动定时器（在 `pool.Start(ctx)` 附近，复用 signal ctx）**

```go
review.NewScheduler(reviewer, cfg.ReviewDailyCron).Start(ctx) // 日报每天、周报每周一 @ cron 时刻
```

- [ ] **验收（协调者集成后）**
  - `go build ./...` 通过；`go vet ./internal/review/ ./internal/api/` 干净。
  - `POST /api/reviews/daily/generate` 返回 `status:ready` + content；`GET /api/reviews/daily?date=` latest-or-generate。
  - dsh 对话里「生成今天报告」→ 命中 `generate_report` → 报告卡数据（前端集成后）。
  - 边车关闭（`ZW_AGENT_ENABLED=false`）时报告仍可生成（报告直调 Ark，不依赖 dsh，spec §3.3）。

---

## 自检 / 覆盖核对（对照 spec，交付前逐条确认）

**§11 输出 JSON schema 一致性**（Task 1 结构 ↔ prompt Task 2 ↔ 解析 Task 3）：
- 日报 §11.1：`headline/highlights[]/decisions[]/todos{new,done,open}/insights[]/tomorrow[]/topic_distribution[{topic,count}]` → `DailyContent` 字段逐一对齐 ✅
- 周报 §11.2：`headline/by_topic[{topic,progress,key_events,open_todos,risks}]/trends[{metric,series[]}]/risks[]/next_week[]` → `WeeklyContent`/`WeeklyTopic`/`Trend`（`Trend` 额外含可选 `labels`，图表就绪，兼容 §9.2 SVG 曲线）✅
- 话题状态 §11.3：`summary/progress/milestones[]/decisions[]/open_todos[]/risks[{desc,severity}]/blockers[]` → `TopicStatusContent`/`Risk` ✅（`progress` 取 0..1；「阶段」语义落 `milestones`，已在 Task 1 注明，避免 union 类型破坏严格解析）

**§5.2 Generator**：`Daily(date)/Weekly(range)/TopicStatus(topicID)` → 结构化对象 + 持久化，LLM 用 Ark（`provider.LLMProvider`，`*ArkLLM` 实现），被 cron/API/MCP 三处复用 ✅（Task 6/7/8/9/10/11）

**§7.3 + §7.1 工具**：`generate_report(type,target)`、`get_topic_status(topic_id)` 在 `package review`、导出 `RegisterReportTools`，不改 `agent/mcp_*.go` ✅（Task 9）

**§13 API**：`GET/POST /api/reviews/daily`、`GET/POST /api/reviews/weekly`、`GET /api/topics/{id}/status` ✅（Task 10）；GET=latest-or-generate、POST=强制重生成

**约束核对**：
- 无新迁移（表在 000005）✅；`Content` 用 `*json.RawMessage` ✅
- LLM/解析失败 → daily/weekly 落 `status=failed`；topic_status 无 status 列故直接上抛（已注明差异）✅
- 幂等重生成：daily/weekly `UpsertDaily/UpsertWeekly` 按唯一键覆盖；topic_status 追加历史 ✅
- 单测 mock LLM 无 MySQL（Task 1/3/4/5/9-bad/10-bad/11）；集成测试独立库 `zhiwei_agentchat_test` + `t.Cleanup`（Task 6/7/8/9/10）✅
- 包边界：全部报告逻辑在 `internal/review`；HTTP 在新 `internal/api/review.go`；main.go/mcp_server.go/router.go 仅在 Task 12 以「待插入片段」说明，本计划不改 ✅
- 中文详细注释、复用成熟库（stdlib time.Timer 替代引 cron 库，已说明理由）✅

**类型名一致性**：`DailyContent/WeeklyContent/TopicStatusContent/DailyTodos/TopicCount/WeeklyTopic/Trend/Risk/TopicLines/DailyInput/WeeklyInput/TopicStatusInput/Generator/Scheduler` 全程唯一、无同名歧义；repo 复用 `repo.DailyReview/WeeklyReview/TopicStatus/ReviewRepo/TopicStatusRepo` 与既有签名（`UpsertDaily/GetDaily/UpsertWeekly/GetWeekly/Insert/GetLatest`）一致 ✅

**范围红线**：报告**前端页**（web/ 报告卡、SVG、日/周切换）不在本计划，协调者最后集成 ✅

