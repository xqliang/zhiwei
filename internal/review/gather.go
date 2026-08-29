package review

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"zhiwei/internal/ids"
	"zhiwei/internal/profile"
	"zhiwei/internal/repo"
)

// ErrTopicNotFound 表示话题不存在（sentinel）。上层 HTTP handler 用 errors.Is 识别它，
// 把「话题不存在」映射为 404 而非默认的 502（区分"客户端给了错 id"与"生成链路故障"）。
var ErrTopicNotFound = errors.New("话题不存在")

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

// gatherSessionSignals 从单个 session 的 transcript + 说话人情绪汇聚 P3 报告洞察信号：
//   - 声学场景/整体氛围（transcript.acoustic_scene / overall_mood）拼成一行带时刻前缀的描述；
//   - 每个说话人的情绪点（speaker_session_state），情绪类别经 profile.EmotionToValence 映射为效价，
//     并把 speaker_id 回显为 person 显示名（未绑定则退回 speaker_label）。
//
// when 为该会话的展示时刻（如 "15:04"）。SpeakerStates / Persons 未注入（旧装配）时静默降级：
// 情绪点为空、说话人退回 label。日报/周报共用，避免逻辑重复。
func (g *Generator) gatherSessionSignals(ctx context.Context, when string, sessionID ids.ID, tr *repo.Transcript) (acoustic string, emotions []EmotionLine) {
	// 声学场景 + 整体氛围：任一非空即产出一行 "时刻 场景·氛围"。
	if tr.AcousticScene != "" || tr.OverallMood != "" {
		note := tr.AcousticScene
		if tr.OverallMood != "" {
			if note != "" {
				note += "·" // 场景与氛围都在时用间隔符连接
			}
			note += tr.OverallMood
		}
		acoustic = fmt.Sprintf("%s %s", when, note)
	}
	// 说话人情绪点：仅当 SpeakerStates 已注入时汇聚（nil 守卫兼容旧装配）。
	if g.SpeakerStates != nil {
		states, err := g.SpeakerStates.ListBySession(ctx, reviewUserID, sessionID)
		if err == nil { // best-effort：查询失败不阻断整体汇聚
			for _, st := range states {
				speaker := st.SpeakerLabel
				// speaker_id → person 显示名（Persons 已注入且该声纹已绑定人物时）。
				if st.SpeakerID != nil && g.Persons != nil {
					if p, err := g.Persons.GetBySpeaker(ctx, *st.SpeakerID); err == nil && p != nil {
						speaker = p.DisplayName
					}
				}
				emotions = append(emotions, EmotionLine{
					When: when, Speaker: speaker,
					Emotion: st.Emotion, Valence: profile.EmotionToValence(st.Emotion),
					MicroMood: st.MicroEmotion, MentalState: st.MentalState,
				})
			}
		}
	}
	return acoustic, emotions
}

// gatherDaily 汇聚当天数据为 DailyInput（现有 repo + Go 内切窗）。
func (g *Generator) gatherDaily(ctx context.Context, date time.Time) (DailyInput, error) {
	start, end := dayRange(date)
	in := DailyInput{Date: start}

	// 记忆：把窗口 [start,end) 整体下推到 SQL（Since=下界含等于 / Before=上界不含），
	// 而不是「Since 下界 + 倒序取前 N 行再 Go 内滤 <end」——后者对过去的日期会取到全是
	// 晚于窗口的最新行，Go 侧 inRange 全滤掉 → 明明有数据却汇聚为空。Limit 用 repo 上限 200
	// （单日记忆量远小于此）。inRange 保留仅作 event_at 为 NULL 的防御。按话题分组
	mrows, err := g.Memories.List(ctx, repo.MemoryFilter{UserID: reviewUserID, Since: &start, Before: &end, Limit: 200})
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
	todos, err := g.Todos.List(ctx, reviewUserID, "", nil)
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
	sessions, err := g.Sessions.List(ctx, reviewUserID, 200, 0)
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
		// P3：汇聚当天声学环境（transcript）+ 说话人情绪（speaker_session_state）。
		note, ems := g.gatherSessionSignals(ctx, s.CreatedAt.Format("15:04"), s.ID, tr)
		if note != "" {
			in.AcousticNotes = append(in.AcousticNotes, note)
		}
		in.EmotionLines = append(in.EmotionLines, ems...)
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
	// 挂漫画（失败静默 → Comic 为 nil，报告照常）
	var dailyContent DailyContent
	if json.Unmarshal(raw, &dailyContent) == nil {
		if comic := g.tryAttachComic(ctx, dailyContent.Narrative, dailyContent.MoodJourney, dailyContent.Scenes); comic != nil {
			dailyContent.Comic = comic
			if nb, err := json.Marshal(dailyContent); err == nil {
				raw = nb
			}
		}
	}
	if err := g.Reviews.UpsertDaily(ctx, reviewUserID, date, json.RawMessage(raw), "ready"); err != nil {
		return nil, fmt.Errorf("落库日报: %w", err)
	}
	return g.Reviews.GetDaily(ctx, reviewUserID, date)
}

// gatherWeekly 汇聚本周数据为 WeeklyInput。weekStart 应为周一 00:00。
func (g *Generator) gatherWeekly(ctx context.Context, weekStart time.Time) (WeeklyInput, error) {
	ws, _ := dayRange(weekStart)
	weekEnd := ws.AddDate(0, 0, 6)  // 周日（存 weekly_review.week_end）
	rangeEnd := ws.AddDate(0, 0, 7) // [ws, rangeEnd) 半开区间
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

	// 本周记忆（按话题 + 每日计数）：按天分页拉取。
	// 历史坑：一周记忆可能超过 repo 单次上限 200，若一把 List(Limit:500) 会被 listWhere
	// 静默夹成 50（500>200 → 回退默认 50）→ 数据被 truncate；且旧写法只给 Since 下界、
	// 倒序取前 N 行，对过去的周会全是晚于窗口的行、Go 侧 inRange 全滤掉 → 汇聚为空。
	// 改为对 7 天各取一次，每天把窗口 [dayStart, dayEnd) 下推到 SQL（Since/Before），
	// 单日 200 上限足够；顺带按天累加 DailyMemoryCnt（trends 就绪序列）。
	byTopic := map[string][]string{}
	var order []string
	for i := 0; i < 7; i++ {
		dayStart := ws.AddDate(0, 0, i)
		dayEnd := dayStart.AddDate(0, 0, 1)
		mrows, err := g.Memories.List(ctx, repo.MemoryFilter{UserID: reviewUserID, Since: &dayStart, Before: &dayEnd, Limit: 200})
		if err != nil {
			return in, fmt.Errorf("汇聚 memory: %w", err)
		}
		for _, m := range mrows {
			if m.EventAt == nil { // SQL 已窗口化到当天；此处仅防御 event_at 为 NULL
				continue
			}
			in.DailyMemoryCnt[i]++
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
	}
	for _, tn := range order {
		in.MemoriesByTopic = append(in.MemoriesByTopic, TopicLines{Topic: tn, Lines: byTopic[tn]})
	}

	// 待办：本周完成 + 未完成；完成按 updated_at 天桶计数
	todos, err := g.Todos.List(ctx, reviewUserID, "", nil)
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

	// P3：汇聚本周声学环境 + 说话人情绪（按周窗过滤 session，逐会话取 transcript/情绪）。
	// 与 gatherDaily 复用 gatherSessionSignals；Sessions 有界 200，Go 内按 [ws, rangeEnd) 切窗。
	sessions, err := g.Sessions.List(ctx, reviewUserID, 200, 0)
	if err != nil {
		return in, fmt.Errorf("汇聚 session: %w", err)
	}
	for _, s := range sessions {
		if !inRange(s.CreatedAt, ws, rangeEnd) {
			continue
		}
		tr, err := g.Transcripts.GetBySession(ctx, s.ID)
		if err != nil {
			continue // 无转写不阻断
		}
		note, ems := g.gatherSessionSignals(ctx, s.CreatedAt.Format("01-02 15:04"), s.ID, tr)
		if note != "" {
			in.AcousticNotes = append(in.AcousticNotes, note)
		}
		in.EmotionLines = append(in.EmotionLines, ems...)
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
	// 挂漫画（失败静默 → Comic 为 nil，报告照常）。Weekly 无 MoodJourney 传 nil。
	var weeklyContent WeeklyContent
	if json.Unmarshal(raw, &weeklyContent) == nil {
		if comic := g.tryAttachComic(ctx, weeklyContent.Narrative, nil, weeklyContent.Scenes); comic != nil {
			weeklyContent.Comic = comic
			if nb, err := json.Marshal(weeklyContent); err == nil {
				raw = nb
			}
		}
	}
	if err := g.Reviews.UpsertWeekly(ctx, reviewUserID, ws, weekEnd, json.RawMessage(raw), "ready"); err != nil {
		return nil, fmt.Errorf("落库周报: %w", err)
	}
	return g.Reviews.GetWeekly(ctx, reviewUserID, ws)
}

// gatherTopicStatus 汇聚某话题数据为 TopicStatusInput。话题不存在返回 error。
func (g *Generator) gatherTopicStatus(ctx context.Context, topicID ids.ID) (TopicStatusInput, error) {
	var in TopicStatusInput
	tp, err := g.Topics.Get(ctx, reviewUserID, topicID)
	if err != nil {
		// 话题不存在（无此行）用 sentinel 包裹，供 handler errors.Is → 404；
		// 其余 DB 错误按原样上抛（handler 默认 502）。
		if errors.Is(err, sql.ErrNoRows) {
			return in, fmt.Errorf("%w: id=%d", ErrTopicNotFound, topicID.Int64())
		}
		return in, fmt.Errorf("查询话题: %w", err)
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
