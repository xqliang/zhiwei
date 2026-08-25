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
	Headline          string       `json:"headline"`           // 一句话总述当天
	Highlights        []string     `json:"highlights"`         // 当天要点（3~7 条）
	Decisions         []string     `json:"decisions"`          // 当天做出的决定
	Todos             DailyTodos   `json:"todos"`              // 待办三分组
	Insights          []string     `json:"insights"`           // 归纳/洞察
	Tomorrow          []string     `json:"tomorrow"`           // 明日计划（只引当天 confirmed 未完成 todo，见 §11.1 约束）
	TopicDistribution []TopicCount `json:"topic_distribution"` // 当天记忆的话题分布（图表就绪）
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
