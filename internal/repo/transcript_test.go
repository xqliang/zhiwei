package repo

import (
	"context"
	"testing"
	"time"

	"zhiwei/internal/ids"
	"zhiwei/internal/repotest"
)

// TestTranscriptUpdateAndRecompute 覆盖 ASR 就地编辑落库链路：
// UpdateSegmentText 改文本 + 跨 transcript 作用域静默忽略 + RecomputeFullText 重算。
func TestTranscriptUpdateAndRecompute(t *testing.T) {
	db, err := NewDB(repotest.DSN(t))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	tr := &TranscriptRepo{DB: db}

	// 建 session + transcript + 两段转写
	sid := ids.New()
	if err := (&SessionRepo{DB: db}).Create(ctx, &AudioSession{
		ID: sid, Source: "web_upload", Filename: "a.wav",
		StoragePath: "/tmp/a.wav", Status: "completed",
	}); err != nil {
		t.Fatal(err)
	}
	tc := &Transcript{SessionID: sid, Language: "zh-CN"}
	if err := tr.Create(ctx, tc); err != nil {
		t.Fatal(err)
	}
	conf := 0.8
	segs := []TranscriptSegment{
		{TranscriptID: tc.ID, SequenceNo: 1, SpeakerLabel: "1", Text: "原始第一段", StartMS: 0, EndMS: 1000, Confidence: &conf},
		{TranscriptID: tc.ID, SequenceNo: 2, SpeakerLabel: "2", Text: "原始第二段", StartMS: 1000, EndMS: 2000, Confidence: &conf},
	}
	if err := tr.InsertSegments(ctx, segs); err != nil {
		t.Fatal(err)
	}

	// 改第一段文本
	if err := tr.UpdateSegmentText(ctx, tc.ID, segs[0].ID, "修正后第一段"); err != nil {
		t.Fatalf("UpdateSegmentText: %v", err)
	}
	got, _ := tr.ListSegments(ctx, tc.ID)
	if got[0].Text != "修正后第一段" {
		t.Fatalf("段文本未更新: %s", got[0].Text)
	}
	if got[1].Text != "原始第二段" {
		t.Fatalf("第二段不应变动: %s", got[1].Text)
	}

	// 跨 transcript 作用域：用不存在的 transcript id 更新本段，应静默忽略（rows=0）
	if err := tr.UpdateSegmentText(ctx, ids.New(), segs[0].ID, "不应写入"); err != nil {
		t.Fatalf("跨作用域调用报错: %v", err)
	}
	got2, _ := tr.ListSegments(ctx, tc.ID)
	if got2[0].Text != "修正后第一段" {
		t.Fatalf("跨 transcript 更新不应生效: %s", got2[0].Text)
	}

	// RecomputeFullText：拼成 "[1] 修正后第一段\n[2] 原始第二段"，置信度=0.8
	if err := tr.RecomputeFullText(ctx, tc.ID); err != nil {
		t.Fatalf("RecomputeFullText: %v", err)
	}
	full, _ := tr.GetBySession(ctx, sid)
	want := "[1] 修正后第一段\n[2] 原始第二段"
	if full.FullText == nil || *full.FullText != want {
		t.Fatalf("full_text=%v want=%s", full.FullText, want)
	}
	if full.Confidence == nil || *full.Confidence != 0.8 {
		t.Fatalf("confidence=%v want=0.8", full.Confidence)
	}
}

// TestSetSegmentSpeaker 覆盖 speaker stage 回填链路：
// SetSegmentSpeaker 按 label 批量回填 + 作用域防跨会话；
// SetSegmentSpeakerByID 单段换人 + 作用域防跨会话；
// ListSpeakersForTranscript 聚合视图按首段 sequence_no 定序。
func TestSetSegmentSpeaker(t *testing.T) {
	db, err := NewDB(repotest.DSN(t))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	tr := &TranscriptRepo{DB: db}
	spk := &SpeakerRepo{DB: db}

	// 建 session + transcript + 三段（标签 1/1/2）
	sid := ids.New()
	if err := (&SessionRepo{DB: db}).Create(ctx, &AudioSession{
		ID: sid, Source: "web_upload", Filename: "a.wav",
		StoragePath: "/tmp/a.wav", Status: "completed",
	}); err != nil {
		t.Fatal(err)
	}
	tc := &Transcript{SessionID: sid, Language: "zh-CN"}
	if err := tr.Create(ctx, tc); err != nil {
		t.Fatal(err)
	}
	segs := []TranscriptSegment{
		{TranscriptID: tc.ID, SequenceNo: 1, SpeakerLabel: "1", Text: "一", StartMS: 0, EndMS: 1000},
		{TranscriptID: tc.ID, SequenceNo: 2, SpeakerLabel: "1", Text: "二", StartMS: 1000, EndMS: 2000},
		{TranscriptID: tc.ID, SequenceNo: 3, SpeakerLabel: "2", Text: "三", StartMS: 2000, EndMS: 3000},
	}
	if err := tr.InsertSegments(ctx, segs); err != nil {
		t.Fatal(err)
	}

	// 两个说话人
	spA := &Speaker{Name: "甲", Source: "enrolled"}
	spB := &Speaker{Name: "乙", Source: "auto"}
	if err := spk.Create(ctx, spA); err != nil {
		t.Fatal(err)
	}
	if err := spk.Create(ctx, spB); err != nil {
		t.Fatal(err)
	}

	// 按 label 批量回填：label "1" → 甲，label "2" → 乙
	if err := tr.SetSegmentSpeaker(ctx, tc.ID, "1", spA.ID); err != nil {
		t.Fatalf("SetSegmentSpeaker(1): %v", err)
	}
	if err := tr.SetSegmentSpeaker(ctx, tc.ID, "2", spB.ID); err != nil {
		t.Fatalf("SetSegmentSpeaker(2): %v", err)
	}
	got, _ := tr.ListSegments(ctx, tc.ID)
	if got[0].SpeakerID == nil || *got[0].SpeakerID != spA.ID ||
		got[1].SpeakerID == nil || *got[1].SpeakerID != spA.ID {
		t.Fatalf("label 1 应回填甲, got %+v %+v", got[0].SpeakerID, got[1].SpeakerID)
	}
	if got[2].SpeakerID == nil || *got[2].SpeakerID != spB.ID {
		t.Fatalf("label 2 应回填乙, got %+v", got[2].SpeakerID)
	}

	// 作用域防护：用错误 transcript id 批量回填，rows=0 静默、不改动
	if err := tr.SetSegmentSpeaker(ctx, ids.New(), "1", spB.ID); err != nil {
		t.Fatalf("跨作用域批量回填报错: %v", err)
	}
	got2, _ := tr.ListSegments(ctx, tc.ID)
	if *got2[0].SpeakerID != spA.ID {
		t.Fatalf("跨 transcript 批量回填不应生效")
	}

	// 单段换人：把第 3 段（原乙）换成甲
	if err := tr.SetSegmentSpeakerByID(ctx, tc.ID, segs[2].ID, spA.ID); err != nil {
		t.Fatalf("SetSegmentSpeakerByID: %v", err)
	}
	got3, _ := tr.ListSegments(ctx, tc.ID)
	if got3[2].SpeakerID == nil || *got3[2].SpeakerID != spA.ID {
		t.Fatalf("单段换人未生效: %+v", got3[2].SpeakerID)
	}

	// 单段换人作用域防护：错误 transcript id 不应改动
	if err := tr.SetSegmentSpeakerByID(ctx, ids.New(), segs[2].ID, spB.ID); err != nil {
		t.Fatalf("跨作用域单段换人报错: %v", err)
	}
	got4, _ := tr.ListSegments(ctx, tc.ID)
	if *got4[2].SpeakerID != spA.ID {
		t.Fatalf("跨 transcript 单段换人不应生效")
	}

	// 聚合视图：此时段 1/2/3 都归甲（首段 sequence_no=1），只应有 1 个说话人、3 段
	list, err := tr.ListSpeakersForTranscript(ctx, tc.ID)
	if err != nil {
		t.Fatalf("ListSpeakersForTranscript: %v", err)
	}
	if len(list) != 1 || list[0].SpeakerID != spA.ID || list[0].SegmentCount != 3 {
		t.Fatalf("聚合视图错误: %+v", list)
	}
}

// TestTranscriptMergeSegments 覆盖 timeline「合并连续同人段成一条」repo 层：
// 建 3 段(前2段同人连续)，合并前 2 段 → keeper 文本=拼接、时间=[min,max]、speaker_id=target，
// 第 2 段删除，第 3 段保留。对应后端 POST /sessions/{id}/segments/merge。未设 TEST_MYSQL_DSN 跳过。
func TestTranscriptMergeSegments(t *testing.T) {
	db, err := NewDB(repotest.DSN(t))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	tr := &TranscriptRepo{DB: db}
	speakers := &SpeakerRepo{DB: db}

	sid := ids.New()
	if err := (&SessionRepo{DB: db}).Create(ctx, &AudioSession{
		ID: sid, Source: "test", Filename: "m.wav", StoragePath: "/tmp/m.wav", Status: "completed",
	}); err != nil {
		t.Fatal(err)
	}
	tc := &Transcript{SessionID: sid, Language: "zh-CN"}
	if err := tr.Create(ctx, tc); err != nil {
		t.Fatal(err)
	}
	if err := tr.InsertSegments(ctx, []TranscriptSegment{
		{TranscriptID: tc.ID, SequenceNo: 1, SpeakerLabel: "0", Text: "前半", StartMS: 0, EndMS: 1500},
		{TranscriptID: tc.ID, SequenceNo: 2, SpeakerLabel: "0", Text: "后半", StartMS: 1500, EndMS: 3000},
		{TranscriptID: tc.ID, SequenceNo: 3, SpeakerLabel: "1", Text: "他人", StartMS: 3000, EndMS: 4000},
	}); err != nil {
		t.Fatal(err)
	}
	segs, _ := tr.ListSegments(ctx, tc.ID)
	sp := &Speaker{Name: "目标"}
	if err := speakers.Create(ctx, sp); err != nil {
		t.Fatal(err)
	}
	// 合并 seg[0]+seg[1] → keeper=seg[0]
	if err := tr.MergeSegments(ctx, tc.ID, segs[0].ID, []ids.ID{segs[1].ID}, "前半后半", 0, 3000, sp.ID); err != nil {
		t.Fatalf("merge: %v", err)
	}
	after, _ := tr.ListSegments(ctx, tc.ID)
	if len(after) != 2 {
		t.Fatalf("合并后应剩 2 段，实际 %d", len(after))
	}
	// keeper(序号最小) 文本拼接 + 时间 [0,3000] + speaker_id=target
	merged := after[0]
	if merged.Text != "前半后半" || merged.StartMS != 0 || merged.EndMS != 3000 {
		t.Fatalf("keeper 合并异常: text=%q [%d,%d]", merged.Text, merged.StartMS, merged.EndMS)
	}
	if merged.SpeakerID == nil || *merged.SpeakerID != sp.ID {
		t.Fatalf("keeper speaker_id 未设为目标: %+v", merged.SpeakerID)
	}
	// 第 3 段保留不变
	if after[1].Text != "他人" {
		t.Fatalf("未参与合并的第3段应保留: %q", after[1].Text)
	}
}

// TestListSegmentsInWallClockWindow 验证跨 session 墙钟窗口查询：
// 段的墙钟时间 = session.created_at + start_ms；窗口外的 session 不入选；
// DESC+LIMIT 裁剪保留最近；结果按墙钟正序返回。
func TestListSegmentsInWallClockWindow(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	sessions := &SessionRepo{DB: db}
	transcripts := &TranscriptRepo{DB: db}
	// 用独立 user_id 隔离本用例：墙钟窗口查询按 user 维度过滤，共享测试库里
	// 其他用例/历史遗留的 user_id=1 段（created_at≈now）会落入窗口造成计数漂移，
	// 故本用例的 transcript 全部挂到一个全新 user_id 下，天然与其它数据隔离。
	uid := ids.New().Int64()

	// session A：窗口外（30 分钟前创建）——即便文本命中也不该入选
	sa := &AudioSession{ID: ids.New(), Source: "web_upload", Filename: "old.wav", StoragePath: "/tmp/old.wav", Status: "completed"}
	sa.CreatedAt = time.Now().Add(-30 * time.Minute)
	if err := sessions.Create(ctx, sa); err != nil {
		t.Fatal(err)
	}
	// 手动改 created_at（Create 用的 DB 默认值）：直接 UPDATE 保证窗口判定用目标时间
	if _, err := db.ExecContext(ctx, `UPDATE audio_session SET created_at = ? WHERE id = ?`, sa.CreatedAt, sa.ID.Int64()); err != nil {
		t.Fatal(err)
	}
	tra := &Transcript{UserID: uid, SessionID: sa.ID, Language: "zh-CN"}
	_ = transcripts.Create(ctx, tra)
	_ = transcripts.InsertSegments(ctx, []TranscriptSegment{
		{TranscriptID: tra.ID, SequenceNo: 1, SpeakerLabel: "1", Text: "老录音内容", StartMS: 0, EndMS: 1000},
	})

	// session B：紧邻当前录音之前 5 分钟创建（窗口内）
	sb := &AudioSession{ID: ids.New(), Source: "web_upload", Filename: "prev.wav", StoragePath: "/tmp/prev.wav", Status: "completed"}
	sb.CreatedAt = time.Now().Add(-5 * time.Minute)
	if err := sessions.Create(ctx, sb); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE audio_session SET created_at = ? WHERE id = ?`, sb.CreatedAt, sb.ID.Int64()); err != nil {
		t.Fatal(err)
	}
	trb := &Transcript{UserID: uid, SessionID: sb.ID, Language: "zh-CN"}
	_ = transcripts.Create(ctx, trb)
	_ = transcripts.InsertSegments(ctx, []TranscriptSegment{
		{TranscriptID: trb.ID, SequenceNo: 1, SpeakerLabel: "1", Text: "前一段录音", StartMS: 0, EndMS: 2000},
	})

	// session C：当前录音（now 创建）
	sc := &AudioSession{ID: ids.New(), Source: "web_upload", Filename: "cur.wav", StoragePath: "/tmp/cur.wav", Status: "processing"}
	sc.CreatedAt = time.Now()
	if err := sessions.Create(ctx, sc); err != nil {
		t.Fatal(err)
	}
	// 同 A/B：Create 不回填 created_at，手动 UPDATE，让墙钟计算与断言基于已知的 sc.CreatedAt
	if _, err := db.ExecContext(ctx, `UPDATE audio_session SET created_at = ? WHERE id = ?`, sc.CreatedAt, sc.ID.Int64()); err != nil {
		t.Fatal(err)
	}
	trc := &Transcript{UserID: uid, SessionID: sc.ID, Language: "zh-CN"}
	_ = transcripts.Create(ctx, trc)
	_ = transcripts.InsertSegments(ctx, []TranscriptSegment{
		{TranscriptID: trc.ID, SequenceNo: 1, SpeakerLabel: "1", Text: "当前录音开头", StartMS: 0, EndMS: 1000},
		{TranscriptID: trc.ID, SequenceNo: 2, SpeakerLabel: "2", Text: "当前录音后段", StartMS: 60000, EndMS: 61000},
	})

	// 窗口 = [now-10min, now+2min]（当前录音时长按 70s 计）
	got, err := transcripts.ListSegmentsInWallClockWindow(ctx, uid,
		time.Now().Add(-10*time.Minute), time.Now().Add(2*time.Minute), 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("窗口内应 3 段（B 的 1 段 + C 的 2 段），实际 %d: %+v", len(got), got)
	}
	// 正序：B 的段在 C 之前；C 内按 start_ms
	if got[0].Text != "前一段录音" || got[1].Text != "当前录音开头" || got[2].Text != "当前录音后段" {
		t.Fatalf("顺序错误: %s / %s / %s", got[0].Text, got[1].Text, got[2].Text)
	}
	// 墙钟时间正确性：C 第 2 段 = sc.created_at + 60s
	want := sc.CreatedAt.Add(60 * time.Second)
	if diff := got[2].WallTime.Sub(want); diff < -time.Second || diff > time.Second {
		t.Fatalf("wall_time 应≈ created_at+60s，差 %v", diff)
	}
	// LIMIT 裁剪保留最近：上限 2 → C 的两段（最新）
	got2, _ := transcripts.ListSegmentsInWallClockWindow(ctx, uid,
		time.Now().Add(-10*time.Minute), time.Now().Add(2*time.Minute), 2)
	if len(got2) != 2 || got2[0].Text != "当前录音开头" {
		t.Fatalf("裁剪应保留最近的 2 段，实际 %+v", got2)
	}
}

// TestReassignSpeakerInTranscript 覆盖 timeline「用此段录音纹」录入后的批量改判：
// 把本 transcript 内所有 speaker_id = fromID 的段改判为 toID，返回改动行数；
// 不碰 speaker_id 为其他人或 NULL 的段，也不越过 transcript 作用域波及其他会话。
func TestReassignSpeakerInTranscript(t *testing.T) {
	db, err := NewDB(repotest.DSN(t))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	tr := &TranscriptRepo{DB: db}
	spk := &SpeakerRepo{DB: db}
	sessions := &SessionRepo{DB: db}
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	// mkTranscript 建一个 session + 空 transcript，返回 transcript。
	mkTranscript := func(fname string) *Transcript {
		sid := ids.New()
		must(sessions.Create(ctx, &AudioSession{
			ID: sid, Source: "web_upload", Filename: fname,
			StoragePath: "/tmp/" + fname, Status: "completed",
		}))
		tc := &Transcript{SessionID: sid, Language: "zh-CN"}
		must(tr.Create(ctx, tc))
		return tc
	}

	tc := mkTranscript("a.wav")
	segs := []TranscriptSegment{
		{TranscriptID: tc.ID, SequenceNo: 1, SpeakerLabel: "1", Text: "一", StartMS: 0, EndMS: 1000},
		{TranscriptID: tc.ID, SequenceNo: 2, SpeakerLabel: "1", Text: "二", StartMS: 1000, EndMS: 2000},
		{TranscriptID: tc.ID, SequenceNo: 3, SpeakerLabel: "2", Text: "三", StartMS: 2000, EndMS: 3000},
		{TranscriptID: tc.ID, SequenceNo: 4, SpeakerLabel: "1", Text: "四", StartMS: 3000, EndMS: 4000},
	}
	must(tr.InsertSegments(ctx, segs))

	spA := &Speaker{Name: "甲", Source: "auto"}
	spB := &Speaker{Name: "乙", Source: "auto"}
	spNew := &Speaker{Name: "新录入", Source: "enrolled"}
	for _, s := range []*Speaker{spA, spB, spNew} {
		must(spk.Create(ctx, s))
	}
	// 段1、段2 → 甲；段3 → 乙；段4 保持 NULL（未解析）
	must(tr.SetSegmentSpeakerByID(ctx, tc.ID, segs[0].ID, spA.ID))
	must(tr.SetSegmentSpeakerByID(ctx, tc.ID, segs[1].ID, spA.ID))
	must(tr.SetSegmentSpeakerByID(ctx, tc.ID, segs[2].ID, spB.ID))

	// 另一会话也有一段归甲——验证作用域：不该被本 transcript 的改判波及。
	other := mkTranscript("b.wav")
	otherSegs := []TranscriptSegment{
		{TranscriptID: other.ID, SequenceNo: 1, SpeakerLabel: "1", Text: "别的会话", StartMS: 0, EndMS: 1000},
	}
	must(tr.InsertSegments(ctx, otherSegs))
	must(tr.SetSegmentSpeakerByID(ctx, other.ID, otherSegs[0].ID, spA.ID))

	// 改判：本 transcript 内 甲 → 新录入
	n, err := tr.ReassignSpeakerInTranscript(ctx, tc.ID, spA.ID, spNew.ID)
	if err != nil {
		t.Fatalf("ReassignSpeakerInTranscript: %v", err)
	}
	if n != 2 {
		t.Fatalf("应改判 2 段（段1/段2 原属甲），实际 %d", n)
	}
	got, _ := tr.ListSegments(ctx, tc.ID)
	if got[0].SpeakerID == nil || *got[0].SpeakerID != spNew.ID ||
		got[1].SpeakerID == nil || *got[1].SpeakerID != spNew.ID {
		t.Fatalf("段1/2 应改判为新录入, got %+v %+v", got[0].SpeakerID, got[1].SpeakerID)
	}
	if got[2].SpeakerID == nil || *got[2].SpeakerID != spB.ID {
		t.Fatalf("段3（乙）不应变动, got %+v", got[2].SpeakerID)
	}
	if got[3].SpeakerID != nil {
		t.Fatalf("段4（未解析）不应被改判, got %+v", got[3].SpeakerID)
	}
	// 作用域：另一会话的甲段不动
	gotOther, _ := tr.ListSegments(ctx, other.ID)
	if gotOther[0].SpeakerID == nil || *gotOther[0].SpeakerID != spA.ID {
		t.Fatalf("另一会话的甲段不应被本 transcript 改判波及, got %+v", gotOther[0].SpeakerID)
	}
}

// TestCorrectSegmentSpeakerAndClearOnManual 覆盖幽灵声纹纠正的标记读写 + 手动/重识别清标记：
// CorrectSegmentSpeaker 按 label 整组改判并写 corrected_from；手动换人(SetSegmentSpeakerByID)、
// 整人改判(ReassignSpeakerSegments)、重新识别(ClearSegmentSpeakers)三条路径都应清掉标记。
func TestCorrectSegmentSpeakerAndClearOnManual(t *testing.T) {
	db, err := NewDB(repotest.DSN(t))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	tr := &TranscriptRepo{DB: db}
	speakers := &SpeakerRepo{DB: db}

	// 两个说话人：ghost=被顶掉的历史人，real=真正说话人
	ghost := &Speaker{Name: "铉晔", Source: "auto"}
	real := &Speaker{Name: "说话人real", Source: "auto"}
	if err := speakers.Create(ctx, ghost); err != nil {
		t.Fatal(err)
	}
	if err := speakers.Create(ctx, real); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = speakers.Delete(context.Background(), ghost.ID); _ = speakers.Delete(context.Background(), real.ID) })

	sid := ids.New()
	if err := (&SessionRepo{DB: db}).Create(ctx, &AudioSession{
		ID: sid, Source: "web_upload", Filename: "a.wav", StoragePath: "/tmp/a.wav", Status: "completed",
	}); err != nil {
		t.Fatal(err)
	}
	tc := &Transcript{SessionID: sid, Language: "zh-CN"}
	if err := tr.Create(ctx, tc); err != nil {
		t.Fatal(err)
	}
	segs := []TranscriptSegment{
		{TranscriptID: tc.ID, SequenceNo: 1, SpeakerLabel: "2", Text: "幽灵段甲", StartMS: 0, EndMS: 1000},
		{TranscriptID: tc.ID, SequenceNo: 2, SpeakerLabel: "2", Text: "幽灵段乙", StartMS: 1000, EndMS: 2000},
	}
	if err := tr.InsertSegments(ctx, segs); err != nil {
		t.Fatal(err)
	}
	// 先把 label "2" 归到 ghost（模拟回填结果）
	if err := tr.SetSegmentSpeaker(ctx, tc.ID, "2", ghost.ID); err != nil {
		t.Fatal(err)
	}

	// 纠正：label "2" 从 ghost 改判给 real，写 corrected_from=ghost
	if err := tr.CorrectSegmentSpeaker(ctx, tc.ID, "2", ghost.ID, real.ID); err != nil {
		t.Fatalf("CorrectSegmentSpeaker: %v", err)
	}
	got, _ := tr.ListSegments(ctx, tc.ID)
	for _, s := range got {
		if s.SpeakerID == nil || *s.SpeakerID != real.ID {
			t.Fatalf("段 %d 应改判给 real，实际 %+v", s.SequenceNo, s.SpeakerID)
		}
		if s.CorrectedFromSpeakerID == nil || *s.CorrectedFromSpeakerID != ghost.ID {
			t.Fatalf("段 %d 应有 corrected_from=ghost，实际 %+v", s.SequenceNo, s.CorrectedFromSpeakerID)
		}
	}

	// 手动单段换人 → 清标记
	if err := tr.SetSegmentSpeakerByID(ctx, tc.ID, got[0].ID, ghost.ID); err != nil {
		t.Fatal(err)
	}
	got, _ = tr.ListSegments(ctx, tc.ID)
	if got[0].CorrectedFromSpeakerID != nil {
		t.Fatalf("手动换人后应清标记，实际 %+v", got[0].CorrectedFromSpeakerID)
	}
	if got[1].CorrectedFromSpeakerID == nil {
		t.Fatalf("未手动改的段标记不应被清")
	}

	// 整人改判 → 清标记
	if _, err := tr.ReassignSpeakerSegments(ctx, tc.ID, real.ID, ghost.ID); err != nil {
		t.Fatal(err)
	}
	got, _ = tr.ListSegments(ctx, tc.ID)
	if got[1].CorrectedFromSpeakerID != nil {
		t.Fatalf("整人改判后应清标记，实际 %+v", got[1].CorrectedFromSpeakerID)
	}

	// 重新纠正后再 ClearSegmentSpeakers → 清标记 + 清 speaker_id
	if err := tr.CorrectSegmentSpeaker(ctx, tc.ID, "2", ghost.ID, real.ID); err != nil {
		t.Fatal(err)
	}
	if err := tr.ClearSegmentSpeakers(ctx, tc.ID); err != nil {
		t.Fatal(err)
	}
	got, _ = tr.ListSegments(ctx, tc.ID)
	for _, s := range got {
		if s.SpeakerID != nil || s.CorrectedFromSpeakerID != nil {
			t.Fatalf("重新识别后 speaker_id 与标记都应清 NULL，实际 %+v / %+v", s.SpeakerID, s.CorrectedFromSpeakerID)
		}
	}
}
