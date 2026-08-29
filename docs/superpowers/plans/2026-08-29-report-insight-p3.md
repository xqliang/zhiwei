# P3 报告深度增强（D）实现计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax.

**Goal:** 把日报/周报从「要点罗列」升级为「洞察式叙事」：gather 汇聚 P1 声学环境/氛围 + P2 人物情绪，prompt 引导 LLM 推断规律/情绪走向，报告结构增列 Narrative/MoodJourney/Patterns/Scenes，前端渲染。

**Architecture:** 数据流 gather（新增 EmotionLines/AcousticNotes 汇聚）→ prompt（BuildDaily/WeeklyUser 加小节 + system prompt 深度指令）→ Ark LLM（doubao）→ DailyContent/WeeklyContent 增列深度字段 → 落库 + 前端 report-list 增列。parse.go 已用通用 json.Unmarshal，新字段自动解析，无需改。

**Tech Stack:** Go + Ark LLM（doubao）+ 现有 review 子系统；Vue3 CDN 前端。复用 P2 的 `profile.EmotionToValence`。

**规格：** `docs/superpowers/specs/2026-08-29-report-insight-p3-design.md`。

**测试约定：** review 包测试照 `gather_test.go`/`prompt_test.go`/`parse_test.go` 现有模式（`repotest.DSN`）；Go 改动后 `go build ./... && go vet ./...`；前端改完 `make hash-web` + `node --check web/app.js`。MySQL 在 127.0.0.1:3307。**无新迁移**（P3 不改数据模型，只汇聚已有 P1/P2 落库的数据）。worktree 内 testdata 未跟踪。每任务末尾提交。

**关键既有事实（已核实）：**
- `DailyContent`/`WeeklyContent` 在 `internal/review/types.go`；`DailyInput`/`WeeklyInput` 在 `internal/review/prompt.go`。
- system prompt 从 `prompts/review_daily_v1.md`/`review_weekly_v1.md` 加载（main.go 读文件传 NewGenerator）；user message 由 `BuildDailyUser`/`BuildWeeklyUser`（prompt.go）拼。
- `parse.go` 用 `json.Unmarshal`（stripToJSON 后），新字段自动解析，**无需改 parse**。
- 前端 `report-list` 组件（`web/app.js`）只接 `items: Array`；DailyContent 字段渲染在 defs（`web/app.js:2582`）。
- `profile.EmotionToValence(emotion)`（P2）复用。
- gather 暂硬编码 `reviewUserID=1`（现状，非本 spec 引入）。

---

### Task 1: types — DailyContent/WeeklyContent 增列深度字段

**Files:** Modify `internal/review/types.go`

- [ ] **Step 1: DailyContent 增列**

`internal/review/types.go` 的 `DailyContent` 结构体（末尾）加：
```go
	// ---- P3 深度增强（spec §3）----
	Narrative   string       `json:"narrative"`    // 叙事总结：一段话概括当天状态/情绪/场景走向（有温度，不罗列）
	MoodJourney []MoodPoint  `json:"mood_journey"` // 当天情绪走向（情绪点序列）
	Patterns    []string     `json:"patterns"`     // 跨记忆/时段发现的细微规律/微情绪/状态推断
	Scenes      []SceneCount `json:"scenes"`       // 当天声学场景分布（图表就绪）
```
`WeeklyContent` 结构体（末尾）加：
```go
	// ---- P3 深度增强（spec §3）----
	Narrative string       `json:"narrative"` // 本周叙事总结
	Patterns  []string     `json:"patterns"`  // 本周规律
	Scenes    []SceneCount `json:"scenes"`    // 本周场景分布
```
在 types.go 加新类型定义（文件顶部或 DailyContent 前）：
```go
// MoodPoint 是一个情绪点（P3 情绪走向）。
type MoodPoint struct {
	When    string  `json:"when"`    // 时段/会话标识
	Mood    string  `json:"mood"`    // 情绪类别
	Valence float64 `json:"valence"` // 效价 −1..1
	Note    string  `json:"note"`    // 微情绪/状态一句话
}

// SceneCount 是「场景→计数」（P3 场景分布，图表就绪）。
type SceneCount struct {
	Scene string `json:"scene"`
	Count int    `json:"count"`
}
```

- [ ] **Step 2: build + 提交**

Run: `go build ./...`
```bash
git add internal/review/types.go
git commit -m "feat(review): DailyContent/WeeklyContent 增列 Narrative/MoodJourney/Patterns/Scenes"
```

---

### Task 2: prompt — DailyInput/WeeklyInput 加情绪/场景字段 + BuildDaily/WeeklyUser 加小节

**Files:** Modify `internal/review/prompt.go`

- [ ] **Step 1: DailyInput/WeeklyInput 增列**

`internal/review/prompt.go` 的 `DailyInput`（末尾）加：
```go
	EmotionLines  []EmotionLine // P3：当天人物情绪点
	AcousticNotes []string      // P3：当天会话的声学场景/氛围描述行
```
`WeeklyInput`（末尾）加：
```go
	EmotionLines  []EmotionLine // P3：本周人物情绪点
	AcousticNotes []string      // P3：本周声学场景/氛围描述行
```
加类型定义（DailyInput 附近）：
```go
// EmotionLine 是一个人物情绪点（P3 汇聚，spec §4）。
type EmotionLine struct {
	When        string  // 会话时间/标识
	Speaker     string  // 说话人
	Emotion     string  // 类别
	Valence     float64 // 效价（EmotionToValence 映射）
	MicroMood   string  // 微情绪
	MentalState string  // 精神状态
}
```

- [ ] **Step 2: BuildDailyUser 加情绪/场景小节**

`internal/review/prompt.go` 的 `BuildDailyUser`（return 前，时间线统计那行之后）加：
```go
	// P3：情绪观察 + 场景（供 LLM 洞察推断）
	if len(in.EmotionLines) > 0 {
		sb.WriteString("\n当天情绪观察（按时段/说话人）：\n")
		for _, e := range in.EmotionLines {
			fmt.Fprintf(&amp;sb, "- %s %s：%s（效价 %.1f）%s%s\n", e.When, e.Speaker, e.Emotion, e.Valence,
				moodTail(e.MicroMood), moodTail(e.MentalState))
		}
	}
	if len(in.AcousticNotes) > 0 {
		writeLines(&amp;sb, "当天声学场景/氛围", in.AcousticNotes)
	}
```
`BuildWeeklyUser`（末尾）类似加：
```go
	if len(in.EmotionLines) > 0 {
		sb.WriteString("\n本周情绪观察：\n")
		for _, e := range in.EmotionLines {
			fmt.Fprintf(&amp;sb, "- %s %s：%s（效价 %.1f）%s%s\n", e.When, e.Speaker, e.Emotion, e.Valence,
				moodTail(e.MicroMood), moodTail(e.MentalState))
		}
	}
	if len(in.AcousticNotes) > 0 {
		writeLines(&amp;sb, "本周声学场景/氛围", in.AcousticNotes)
	}
```
> ⚠️ 上面代码块里的 `&amp;` 是 HTML 转义，写文件时还原成 `&amp;`。
加 helper（prompt.go 内）：
```go
// moodTail 微情绪/精神状态追加片段（空则空串）。
func moodTail(s string) string {
	if s == "" {
		return ""
	}
	return "·" + s
}
```

- [ ] **Step 3: build + 提交**

Run: `go build ./...`
```bash
git add internal/review/prompt.go
git commit -m "feat(review): DailyInput/WeeklyInput 加情绪/场景字段 + BuildDaily/WeeklyUser 加小节"
```

---

### Task 3: system prompt — review_daily/weekly_v1.md 加深度指令

**Files:** Modify `prompts/review_daily_v1.md` / `prompts/review_weekly_v1.md`

- [ ] **Step 1: 读现状**

先读 `prompts/review_daily_v1.md` 全文，理解现有指令风格（JSON schema 定义、约束）。

- [ ] **Step 2: 日报 prompt 加深度指令 + JSON schema 新字段**

在 `prompts/review_daily_v1.md` 的 JSON schema 说明里补新字段定义，并加深度指令：
- JSON schema 增列（在现有字段后）：
```
"narrative": "一段话（80-150字）像懂你的朋友总结今天的状态、情绪、场景走向，有温度、不罗列事项",
"mood_journey": [{"when":"时段/会话","mood":"情绪类别","valence":-1到1,"note":"微情绪/状态一句话"}],
"patterns": ["基于情绪+场景+记忆推断的2-4条不易察觉的规律/状态，如『连续会议后情绪走低』"],
"scenes": [{"scene":"场景名","count":次数}]
```
- 深度指令（约束「基于信号、禁编造」）：
```
【深度要求】
- narrative：不要罗列做了什么事，要写出状态、情绪、场景的走向与意味。
- patterns：只基于给定的情绪/场景/记忆信号推断规律；没有信号支撑的不要写，或明确标注『存疑』。严禁编造无据的因果。
- 若当天无情绪/场景信号，narrative 与 patterns 可空或从记忆本身温和推断。
```

- [ ] **Step 3: 周报 prompt 类似**

`prompts/review_weekly_v1.md` 加 narrative/patterns/scenes 的 JSON schema + 类似的「基于信号、禁编造」深度指令（周报侧重本周趋势/规律）。

- [ ] **Step 4: 提交**
```bash
git add prompts/review_daily_v1.md prompts/review_weekly_v1.md
git commit -m "feat(prompts): 日报/周报 prompt 加深度指令 + narrative/mood/patterns/scenes schema"
```

---

### Task 4: gather — 汇聚情绪/环境信号

**Files:** Modify `internal/review/gather.go`；`internal/review/generator.go`（Generator 加依赖）；`cmd/zhiwei-server/main.go`（注入依赖）；`internal/review/gather_test.go`（测试）

- [ ] **Step 1: Generator 加依赖 + main.go 注入**

`internal/review/generator.go` 的 `Generator` 结构体加字段（照现有 Transcripts/Todos 风格）：
```go
	SpeakerStates *repo.SpeakerSessionStateRepo // P3：说话人情绪（gather 汇聚用）
	Transcripts   *repo.TranscriptRepo          // 可能已有；用于读 acoustic_scene/overall_mood
	Persons       *repo.PersonRepo              // P3：speaker_id → person 名（情绪行显示说话人名）
```
（先读 Generator 现有字段，确认 Transcripts 是否已有；缺则加。）
`cmd/zhiwei-server/main.go` 构造 `review.NewGenerator(...)` 处补传 `&repo.SpeakerSessionStateRepo{DB: db}` / `&repo.PersonRepo{DB: db}`（照现有传参）。先读 NewGenerator 签名确认形参顺序。

- [ ] **Step 2: gatherDaily 汇聚情绪/环境**

`internal/review/gather.go` 的 `gatherDaily`：在遍历当天 session 统计的那段（已有 `sessions` 遍历 + `Transcripts.GetBySession`/`ListSegments`）里，**新增**：
```go
	// P3：汇聚当天情绪（speaker_session_state）+ 声学场景（transcript）
	for _, s := range sessions {  // 照现有 sessions 遍历
		if !inRange(s.CreatedAt, start, end) { continue }
		tr, err := g.Transcripts.GetBySession(ctx, s.ID)
		if err == nil {
			if tr.AcousticScene != "" || tr.OverallMood != "" {
				note := tr.AcousticScene
				if tr.OverallMood != "" { note += "·" + tr.OverallMood }
				in.AcousticNotes = append(in.AcousticNotes, fmt.Sprintf("%s %s", s.CreatedAt.Format("15:04"), note))
			}
		}
		if g.SpeakerStates != nil {
			states, _ := g.SpeakerStates.ListBySession(ctx, reviewUserID, s.ID)
			for _, st := range states {
				speaker := st.SpeakerLabel
				if st.SpeakerID != nil {
					if p, err := g.Persons.Get(ctx, *st.SpeakerID); err == nil { speaker = p.DisplayName }
				}
				in.EmotionLines = append(in.EmotionLines, EmotionLine{
					When: s.CreatedAt.Format("15:04"), Speaker: speaker,
					Emotion: st.Emotion, Valence: profile.EmotionToValence(st.Emotion),
					MicroMood: st.MicroEmotion, MentalState: st.MentalState,
				})
			}
		}
	}
```
> **注意**：`g.SpeakerStates`/`g.Persons`/`g.Transcripts` nil 守卫（兼容旧装配/测试）。`profile.EmotionToValence` 需 import `zhiwei/internal/profile`。`s.CreatedAt` 用会话创建时间。若现有 sessions 遍历结构不同（变量名/session 类型），照 gather.go 实际的 `sessions` 遍历改写。gatherWeekly 类似增列（按周 sessions）。

- [ ] **Step 3: 测试**

`internal/review/gather_test.go`：照现有 gatherDaily 测试，造带 speaker_session_state（speaker_id→person）+ transcript 环境列的会话，断言 DailyInput.EmotionLines/AcousticNotes 汇聚对（valence 映射、speaker 名、按天过滤）。

- [ ] **Step 4: build + 提交**

Run: `go build ./... && go vet ./... && go test ./internal/review/ -run TestGather -count=1`
```bash
git add internal/review/gather.go internal/review/generator.go internal/review/gather_test.go cmd/zhiwei-server/main.go
git commit -m "feat(review): gather 汇聚情绪/环境信号(EmotionLines+AcousticNotes)供报告洞察"
```

---

### Task 5: 前端 — report-list 增列叙事/规律/情绪走向/场景分布

**Files:** Modify `web/app.js` / `web/index.html`；`make hash-web`

- [ ] **Step 1: report-list 支持单段 narrative**

`web/app.js` 的 `report-list` 组件当前只接 `items: Array`。新增一个「单段文本」渲染组件（或扩 report-list 支持 `text` prop）：
```js
app.component('report-text', {
  props: { label: { type: String, default: '' }, text: { type: String, default: '' } },
  template: `<div class="report-sec"><div class="report-sec-title">{{ label }}</div><p v-if="text" style="margin:0; line-height:1.7">{{ text }}</p><div v-else class="muted">（无）</div></div>`,
});
```

- [ ] **Step 2: defs 增列（narrative/patterns 用现有 report-list，mood_journey/scenes 需对象渲染）**

`web/app.js:2582` 的 defs 增列。`narrative`（单段）用新 `report-text`；`patterns`（数组）用 `report-list`；`mood_journey`/`scenes`（对象数组）最小化渲染成文本行或 chips：
```js
// 在日报/周报渲染区，现有 defs（report-list 遍历）之外，增列：
// narrative: <report-text label="叙事" :text="c.narrative" />
// patterns:  <report-list label="规律" :items="c.patterns" />
// mood_journey: 遍历 c.mood_journey 渲染「时段 情绪(效价) · 微情绪」
// scenes: 遍历 c.scenes 渲染 chips「场景 ×N」（仿 topic_distribution）
```
（读 `web/app.js` 报告渲染区 `defs`/`reportContent` 的实际模板结构，把新字段插进去；mood_journey/scenes 若无现成 chips 渲染，先做简单文本行 `<div>{{ m.when }} {{ m.mood }} {{ m.note }}</div>`。）

- [ ] **Step 3: hash-web + 校验**

Run: `make hash-web && node --check web/app.js`

- [ ] **Step 4: 提交**
```bash
git add web/app.js web/index.html
git commit -m "feat(web): 报告增列叙事/规律/情绪走向/场景分布渲染"
```

---

### Task 6: 全量回归

- [ ] **Step 1: 全量 build/vet + review 包测试**

Run: `go build ./... && go vet ./...`；`TEST_MYSQL_DSN=... go test ./internal/review/ ./internal/profile/ -count=1`。Expected: 全 PASS。

- [ ] **Step 2: 报告生成冒烟（best-effort）**

若有 .env + MySQL：`POST /api/reviews/daily/generate` 生成日报，确认 DailyContent 含 narrative/patterns/scenes（有情绪/环境信号时）。无则靠 gather/prompt 测试覆盖。

---

## Self-Review 结果

**Spec 覆盖：** §3 报告结构→Task1；§4 gather 增强→Task4；§5 prompt→Task2/3；§6 前端→Task5；§8 测试→各任务；§9 决策全覆盖。parse.go 不改（json.Unmarshal 通用）已在关键事实说明。✅

**占位符：** Task2 的 `&amp;` 是显式 HTML 转义标注（还原为 `&`）；Task4 的 sessions 遍历「照实际改写」是**明确的适配指引**（非模糊 TODO，附了完整代码+适配说明）。其余代码完整。

**类型一致性：** `MoodPoint`/`SceneCount`/`EmotionLine`（types/prompt）Task1/2 定义、Task4/5 引用；`profile.EmotionToValence` 复用 P2；`EmotionLines`/`AcousticNotes` 字段名跨 Task2(gather输入)/Task4(汇聚)/Task5(前端)一致。

**关键正确性点：** ①洞察「基于信号禁编造」靠 prompt 约束；②gather 情绪用 P2 valence 映射；③speaker_id→person 名回显；④nil 守卫兼容旧装配；⑤narrative 单段用新 report-text 组件（report-list 只接数组已核实）。

**无新迁移**（P3 只汇聚已有落库数据）→ 无撞号风险。
