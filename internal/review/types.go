// Package review 是报告子系统（spec §11）：从现有 repo 汇聚数据，直调 Ark 上的
// DeepSeek 模型（不经 dsh），产出结构化日报/周报/话题状态 JSON 并落库。
// 被 cron、/api/reviews/*、MCP generate_report 三处复用（spec §5.2/§7.3/§13）。
package review

import "encoding/json"

// ---- §11.1 日报 ----

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

// ComicImage 报告漫画（一张多格连环画，P4）。
type ComicImage struct {
	Caption  string `json:"caption"`   // 整体小标题（可选）
	ImageURL string `json:"image_url"` // TOS 长期 URL
}

// DailyContent 是日报的结构化输出（落 daily_review.content）。
// 字段对齐 spec §11.1：headline / highlights / decisions / todos{new,done,open}
// / insights / tomorrow / topic_distribution。
type DailyContent struct {
	Headline          string       `json:"headline"`           // 一句话总述当天
	Highlights        []string     `json:"highlights"`         // 当天要点（3~7 条）
	Decisions         []string     `json:"decisions"`          // 当天做出的决定
	Todos             DailyTodos   `json:"todos"`              // 待办三分组
	Insights          []string     `json:"insights"`           // 归纳/洞察
	Tomorrow          []string     `json:"tomorrow"`           // 明日计划（只引当天 confirmed 未完成 todo，见 §11.1 约束）
	TopicDistribution []TopicCount `json:"topic_distribution"` // 当天记忆的话题分布（图表就绪）
	// ---- P3 深度增强（spec §3）----
	Narrative   string       `json:"narrative"`    // 叙事总结：一段话概括当天状态/情绪/场景走向（有温度，不罗列）
	MoodJourney []MoodPoint  `json:"mood_journey"` // 当天情绪走向（情绪点序列）
	Patterns    []string     `json:"patterns"`     // 跨记忆/时段发现的细微规律/微情绪/状态推断
	Scenes      []SceneCount `json:"scenes"`       // 当天声学场景分布（图表就绪）
	Comic       *ComicImage  `json:"comic,omitempty"` // 报告漫画（P4；未生成时省略，守 no-null 契约）
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
	// ---- P3 深度增强（spec §3）----
	Narrative string       `json:"narrative"` // 本周叙事总结
	Patterns  []string     `json:"patterns"`  // 本周规律
	Scenes    []SceneCount `json:"scenes"`    // 本周场景分布
	Comic     *ComicImage  `json:"comic,omitempty"` // 报告漫画（P4；未生成时省略，守 no-null 契约）
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

// ---- nil 切片兜底（M5）----
// 模型可能省略某些数组字段 → 解析后为 nil 切片 → JSON 序列化成 null，违反 prompt
// 「数组无内容用 []」的图表就绪契约（前端 .map 会因 null 报错）。以下 normalize* 在
// 序列化前把 nil 切片补成非 nil 空切片（[]T{}），保证输出是 [] 而非 null。
// 注：带 omitempty 的可选字段（如 Trend.Labels）保持 nil 即被省略，不在兜底范围。

// normalizeDaily 兜底日报所有切片字段（含 todos 三分组）。
func normalizeDaily(c *DailyContent) {
	if c.Highlights == nil {
		c.Highlights = []string{}
	}
	if c.Decisions == nil {
		c.Decisions = []string{}
	}
	if c.Todos.New == nil {
		c.Todos.New = []string{}
	}
	if c.Todos.Done == nil {
		c.Todos.Done = []string{}
	}
	if c.Todos.Open == nil {
		c.Todos.Open = []string{}
	}
	if c.Insights == nil {
		c.Insights = []string{}
	}
	if c.Tomorrow == nil {
		c.Tomorrow = []string{}
	}
	if c.TopicDistribution == nil {
		c.TopicDistribution = []TopicCount{}
	}
	if c.MoodJourney == nil {
		c.MoodJourney = []MoodPoint{}
	}
	if c.Patterns == nil {
		c.Patterns = []string{}
	}
	if c.Scenes == nil {
		c.Scenes = []SceneCount{}
	}
}

// normalizeWeekly 兜底周报顶层切片，以及每个 by_topic / trends 元素的内部切片。
func normalizeWeekly(c *WeeklyContent) {
	if c.ByTopic == nil {
		c.ByTopic = []WeeklyTopic{}
	}
	for i := range c.ByTopic {
		if c.ByTopic[i].KeyEvents == nil {
			c.ByTopic[i].KeyEvents = []string{}
		}
		if c.ByTopic[i].OpenTodos == nil {
			c.ByTopic[i].OpenTodos = []string{}
		}
		if c.ByTopic[i].Risks == nil {
			c.ByTopic[i].Risks = []string{}
		}
	}
	if c.Trends == nil {
		c.Trends = []Trend{}
	}
	for i := range c.Trends {
		if c.Trends[i].Series == nil {
			c.Trends[i].Series = []float64{}
		}
	}
	if c.Risks == nil {
		c.Risks = []string{}
	}
	if c.NextWeek == nil {
		c.NextWeek = []string{}
	}
	if c.Patterns == nil {
		c.Patterns = []string{}
	}
	if c.Scenes == nil {
		c.Scenes = []SceneCount{}
	}
}

// normalizeTopicStatus 兜底话题状态所有切片字段。
func normalizeTopicStatus(c *TopicStatusContent) {
	if c.Milestones == nil {
		c.Milestones = []string{}
	}
	if c.Decisions == nil {
		c.Decisions = []string{}
	}
	if c.OpenTodos == nil {
		c.OpenTodos = []string{}
	}
	if c.Risks == nil {
		c.Risks = []Risk{}
	}
	if c.Blockers == nil {
		c.Blockers = []string{}
	}
}
