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

// EmotionLine 是一个人物情绪点（P3 汇聚，spec §4）。
type EmotionLine struct {
	When        string  // 会话时间/标识
	Speaker     string  // 说话人
	Emotion     string  // 类别
	Valence     float64 // 效价（EmotionToValence 映射）
	MicroMood   string  // 微情绪
	MentalState string  // 精神状态
}

// DailyInput 是日报的汇聚输入（spec §11.1 输入：当天 memory 按 topic + todo 变化 + timeline 统计 + 对话概况）。
type DailyInput struct {
	Date            time.Time
	MemoriesByTopic []TopicLines  // 当天记忆按话题分组
	TodosNew        []string      // 当天新增待办标题
	TodosDone       []string      // 当天完成
	TodosOpen       []string      // 仍未完成（confirmed 未 done）
	SessionCount    int           // 当天录音条数
	TotalDurationMS int64         // 当天录音总时长
	SegmentCount    int           // 当天转写分段数
	Speakers        []string      // 当天出现的说话人
	ConversationCnt int           // 当天 agent 对话条数（概况，可为 0）
	EmotionLines    []EmotionLine // P3：当天人物情绪点
	AcousticNotes   []string      // P3：当天会话的声学场景/氛围描述行
}

// WeeklyInput 是周报的汇聚输入（spec §11.2 输入：本周日报 + memory/todo + topic 活动 + 每日序列）。
type WeeklyInput struct {
	WeekStart       time.Time
	WeekEnd         time.Time
	DailyHeadlines  []string     // 本周每日日报 headline（缺失日留空串占位）
	MemoriesByTopic []TopicLines // 本周记忆按话题
	TodosDone       []string
	TodosOpen       []string
	DailyMemoryCnt  []int         // 每日记忆数序列（trends 就绪）
	DailyTodoDone   []int         // 每日完成待办数序列
	EmotionLines    []EmotionLine // P3：本周人物情绪点
	AcousticNotes   []string      // P3：本周声学场景/氛围描述行
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
	// P3：情绪观察 + 场景（供 LLM 洞察推断）
	if len(in.EmotionLines) > 0 {
		sb.WriteString("\n当天情绪观察（按时段/说话人）：\n")
		for _, e := range in.EmotionLines {
			fmt.Fprintf(&sb, "- %s %s：%s（效价 %.1f）%s%s\n", e.When, e.Speaker, e.Emotion, e.Valence,
				moodTail(e.MicroMood), moodTail(e.MentalState))
		}
	}
	if len(in.AcousticNotes) > 0 {
		writeLines(&sb, "当天声学场景/氛围", in.AcousticNotes)
	}
	return sb.String()
}

// moodTail 微情绪/精神状态追加片段（空则空串）。
func moodTail(s string) string {
	if s == "" {
		return ""
	}
	return "·" + s
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
	if len(in.EmotionLines) > 0 {
		sb.WriteString("\n本周情绪观察：\n")
		for _, e := range in.EmotionLines {
			fmt.Fprintf(&sb, "- %s %s：%s（效价 %.1f）%s%s\n", e.When, e.Speaker, e.Emotion, e.Valence,
				moodTail(e.MicroMood), moodTail(e.MentalState))
		}
	}
	if len(in.AcousticNotes) > 0 {
		writeLines(&sb, "本周声学场景/氛围", in.AcousticNotes)
	}
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
