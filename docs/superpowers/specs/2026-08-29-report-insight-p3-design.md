# P3 报告深度增强（D）实现设计

- 日期：2026-08-29
- 总纲：`docs/superpowers/specs/2026-08-29-conversation-insight-roadmap-design.md`（本文件是其 **P3 / D 子项** 的实现 spec）
- 前置：P1（声学环境/逐说话人情绪）、P2（人物情绪 PersonMetric）已合入 main
- 分支/worktree：`feat/report-insight`

## 1. 目标与范围

把日报/周报从「要点罗列」升级为**有深度的洞察式叙事**：消费 P1 的声学环境/整体氛围 + P2 的人物情绪，让报告能推断**状态如何、在哪类场景、情绪走向、跨记忆的细微规律**，而非罗列事项。

**核心决策（已与用户确认）**：
- **重构整个报告结构**：DailyContent/WeeklyContent 增列叙事与洞察字段（narrative 叙事总结、mood 情绪走向、patterns 发现的规律/微情绪），让「深度」有专门载体。
- **日报 + 周报都消费情绪环境信号**：两者都汇聚并基于 P1/P2 信号推断。

## 2. 现状（调研实证，file:line）

- **报告子系统** `internal/review`：`gather.go`（汇聚 DailyInput/WeeklyInput）→ `prompt.go`（BuildDailyUser/BuildWeeklyUser 拼文本）→ Ark LLM（doubao）→ `parse.go`（解析 DailyContent/WeeklyContent JSON）→ 落库 + 前端渲染。
- **DailyInput**（`internal/review/prompt.go:18`）：`Date/MemoriesByTopic/TodosNew/TodosDone/TodosOpen/SessionCount/TotalDurationMS/SegmentCount/Speakers/ConversationCnt`。
- **DailyContent**（`internal/review/types.go`）：`Headline/Highlights/Decisions/Todos/Insights/Tomorrow/TopicDistribution`。
- **gatherDaily**（`internal/review/gather.go:32`）：汇聚记忆（按话题）、待办、时间线统计（session 数/时长/段数/说话人）。**当前不消费情绪/环境信号**。
- **P1 落库**：`transcript.acoustic_scene/background_sounds/weather_cues/overall_mood`（会话级）；`speaker_session_state.emotion/micro_emotion/mental_state`（每说话人每会话）。
- **P2 落库**：`PersonMetric`（metric_key=emotion, value_num=valence, value_text=类别, measured_at, source=auto）。
- **前端渲染**（`web/app.js:2582`）：`report-list` 组件按 `defs=[['要点','highlights'],['决定','decisions'],['洞察','insights'],['明日','tomorrow']]` 遍历渲染。数据驱动，加字段即加一节。

## 3. 报告结构重构（DailyContent / WeeklyContent）

**DailyContent 新增字段**（保留原字段，增列深度字段）：
```go
type DailyContent struct {
	Headline          string
	Highlights        []string
	Decisions         []string
	Todos             DailyTodos
	Insights          []string
	Tomorrow          []string
	TopicDistribution []TopicCount
	// ---- P3 深度增强 ----
	Narrative    string       `json:"narrative"`     // 叙事总结：一段话概括当天状态/情绪/场景走向（有温度，不罗列）
	MoodJourney  []MoodPoint  `json:"mood_journey"`  // 当天情绪走向（按时段/会话的情绪点序列）
	Patterns     []string     `json:"patterns"`      // 跨记忆/时段发现的细微规律/微情绪/状态推断
	Scenes       []SceneCount `json:"scenes"`        // 当天声学场景分布（会议/车内/户外… → 计数，图表就绪）
}
type MoodPoint struct {
	When   string  `json:"when"`   // 时段/会话标识
	Mood   string  `json:"mood"`   // 情绪类别
	Valence float64 `json:"valence"` // 效价 −1..1
	Note   string  `json:"note"`   // 微情绪/状态一句话
}
type SceneCount struct {
	Scene string `json:"scene"`
	Count int    `json:"count"`
}
```
**WeeklyContent 类似增列**：`Narrative`（本周叙事）、`MoodTrend`（本周情绪趋势，可复用 MoodPoint 或简化）、`Patterns`（本周规律）、`Scenes`（本周场景分布）。

> 保留原 Highlights/Insights 等（向后兼容 + 前端渐进增强）；新增字段为「深度」主载体。

## 4. gather 增强（汇聚情绪/环境信号）

**DailyInput 新增字段**：
```go
type DailyInput struct {
	// ... 原字段 ...
	EmotionLines []EmotionLine // 当天人物情绪点（每会话每说话人）
	AcousticNotes []string     // 当天会话的声学场景/氛围描述行（供 LLM 推断）
}
type EmotionLine struct {
	When       string  // 会话时间/标识
	Speaker    string  // 说话人
	Emotion    string  // 类别
	Valence    float64 // 效价
	MicroMood  string  // 微情绪
	MentalState string // 精神状态
}
```
**gatherDaily 新增汇聚**：
- 遍历当天 session 的 transcript → 取 acoustic_scene/overall_mood → 拼 AcousticNotes（如「10:30 会议·室内·专注」）。
- 遍历当天 session 的 speaker_session_state（有 speaker_id 的）→ 拼 EmotionLines（每说话人情绪点，Valence 用 EmotionToValence 映射）。
- user_id：review 暂 reviewUserID=1（对齐现有 gather 的硬编码，多用户随全局隔离）。

**WeeklyInput 类似**：EmotionLines/AcousticNotes 按周汇聚。

## 5. prompt 改造（引导洞察推断，不罗列）

**BuildDailyUser/BuildWeeklyUser 增列**：把 EmotionLines/AcousticNotes 拼进喂 LLM 的文本（新增「当天情绪观察」「当天场景」小节）。

**system prompt 增列深度指令**：
- Narrative：要求「像懂你的朋友写的一段总结，有温度、有走向，不罗列事项」。
- Patterns：要求「基于情绪+场景+记忆，推断 2-4 条不易察觉的规律/状态（如『连续会议后情绪走低』『户外时段心情好转』），只基于给定信号推断、不编造』。
- MoodJourney / Scenes：结构化抽取。

**关键约束**：洞察必须**基于给定信号**（情绪/环境/记忆），严禁编造无据的规律——在 prompt 里明确「无信号支撑的推断标为存疑或不写」。

## 6. 前端渲染（report-list 增列）

**`web/app.js:2582` defs 增列**：
```js
const defs = [
  ['要点', 'highlights'], ['决定', 'decisions'], ['洞察', 'insights'], ['明日', 'tomorrow'],
  ['叙事', 'narrative'],        // P3：叙事总结（字符串，单段）
  ['规律', 'patterns'],          // P3：发现的规律（字符串数组）
  ['情绪走向', 'mood_journey'],  // P3：情绪点序列（对象数组，需专门渲染或简化）
  ['场景分布', 'scenes'],        // P3：场景分布（对象数组，类似话题分布）
];
```
- `narrative` 是单字符串，`report-list` 需支持「单段文本」渲染（或新增组件）。
- `mood_journey`/`scenes` 是对象数组——可仿 `topic_distribution`（现有有分布渲染）做 chips/曲线；最小化可先渲染成文本行。
- 周报类似。

## 7. 数据流

```
gatherDaily（新增汇聚情绪/环境）
   ├ 记忆 / 待办 / 时间线统计（原）
   ├ EmotionLines（speaker_session_state + EmotionToValence）
   └ AcousticNotes（transcript.acoustic_scene/overall_mood）
        ↓ 拼进 DailyInput
prompt BuildDailyUser（新增情绪/场景小节 + 深度 system 指令）
        ↓ Ark LLM（doubao）
DailyContent（新增 Narrative/MoodJourney/Patterns/Scenes）
        ↓ 落库 + 前端 report-list 增列渲染
```

## 8. 测试

- **gather**（`gather_test.go`）：造带 speaker_session_state + transcript 环境列的会话，断言 DailyInput/WeeklyInput 新增 EmotionLines/AcousticNotes 汇聚对（valence 映射、按天过滤）。
- **prompt**（`prompt_test.go`）：BuildDailyUser 输出含「情绪观察/场景」小节；深度指令在 system prompt 里。
- **parse**（`parse_test.go`）：DailyContent JSON 含新字段能解析（Narrative/MoodJourney/Patterns/Scenes）。
- **前端**：`node --check web/app.js` + report-list 渲染新字段（冒烟）。

## 9. 已定决策（不留 TBD）

| 项 | 决策 |
|----|------|
| 报告结构 | 重构 DailyContent/WeeklyContent，增列 Narrative/Mood/Patterns/Scenes（保留原字段） |
| 信号消费 | 日报 + 周报都汇聚并基于情绪/环境推断 |
| gather 增强 | 新增 EmotionLines（speaker_session_state→valence）+ AcousticNotes（transcript 环境） |
| prompt | 深度 system 指令 + 情绪/场景小节；洞察必须基于给定信号、禁编造 |
| 前端 | report-list defs 增列叙事/规律/情绪走向/场景分布 |
| valence | 复用 P2 的 profile.EmotionToValence |

## 10. 待 plan 阶段决定（非阻塞）

- MoodJourney/Scenes 前端渲染形式（chips/曲线 vs 文本行，最小化可先文本）。
- Narrative 单段在 report-list 的渲染（report-list 当前只接数组，需扩展支持单字符串或新增组件）。
- 「洞察必须基于信号」的 prompt 措辞（平衡「有深度」与「不编造」）。
- EmotionLines 按 session 时间排序的 When 标识格式。

## 11. 风险

- **LLM 编造**：洞察可能过度推断——靠 prompt 约束「基于给定信号、无据标存疑」+ 后续人工抽验缓解。
- **报告体积/时延**：输入增情绪/场景 + 输出增叙事/洞察，token 增、时延增——可接受（报告本就低频、异步）。
- **前后端对齐**：DailyContent 新字段与前端 defs 须同步，漏改则前端不显示——靠 parse 测试 + 冒烟覆盖。
- **reviewUserID 隔离**：gather 暂硬编码 reviewUserID=1（现状），多用户随全局——非本 spec 引入。
