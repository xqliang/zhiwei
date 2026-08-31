package pipeline

import (
	"context"
	"testing"

	"zhiwei/internal/ids"
	"zhiwei/internal/repo"
	"zhiwei/internal/repotest"
)

func TestStageEmotionProfileDisabledSkips(t *testing.T) {
	h := stageEmotionProfile(StageDeps{EmotionProfileEnabled: false})
	if err := h(context.Background(), nil, ids.New()); err != nil {
		t.Errorf("关闭时应 no-op 返回 nil, got %v", err)
	}
	h2 := stageEmotionProfile(StageDeps{EmotionProfileEnabled: true, PersonMetrics: nil})
	if err := h2(context.Background(), nil, ids.New()); err != nil {
		t.Errorf("缺 PersonMetrics 应 no-op 返回 nil, got %v", err)
	}
}

func TestStageEmotionProfileWritesMetrics(t *testing.T) {
	db, err := repo.NewDB(repotest.DSN(t))
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	sessions := &repo.SessionRepo{DB: db}
	persons := &repo.PersonRepo{DB: db}
	metrics := &repo.PersonMetricRepo{DB: db}
	states := &repo.SpeakerSessionStateRepo{DB: db}
	speakers := &repo.SpeakerRepo{DB: db}

	sid := ids.New()
	if err := sessions.Create(ctx, &repo.AudioSession{ID: sid, Source: "web_upload", Filename: "x.wav", Status: "done"}); err != nil {
		t.Fatal(err)
	}
	sess, _ := sessions.Get(ctx, 1, sid)

	sp := &repo.Speaker{UserID: 1, Name: "甲"}
	_ = speakers.Create(ctx, sp)
	p := &repo.Person{UserID: 1, DisplayName: "甲", SpeakerID: &sp.ID}
	_ = persons.Create(ctx, p)

	spOrphan := &repo.Speaker{UserID: 1, Name: "乙"}
	_ = speakers.Create(ctx, spOrphan)

	tid := ids.New()
	_ = states.InsertBatch(ctx, []repo.SpeakerSessionState{
		{UserID: 1, TranscriptID: tid, SessionID: sid, SpeakerLabel: "1", SpeakerID: &sp.ID, Emotion: "喜悦", Confidence: 0.8},
		{UserID: 1, TranscriptID: tid, SessionID: sid, SpeakerLabel: "2", SpeakerID: &spOrphan.ID, Emotion: "焦虑", Confidence: 0.6},
	})

	d := StageDeps{
		DB: db, Sessions: sessions, Persons: persons, PersonMetrics: metrics, SpeakerStates: states,
		EmotionProfileEnabled: true,
	}
	if err := stageEmotionProfile(d)(ctx, nil, sid); err != nil {
		t.Fatalf("stage 应成功: %v", err)
	}

	rows, err := metrics.ListByPerson(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	var found *repo.PersonMetric
	for i := range rows {
		if rows[i].MetricKey == "emotion" {
			found = &rows[i]
		}
	}
	if found == nil {
		t.Fatal("甲的 emotion metric 未写入")
	}
	if found.ValueNum == nil || *found.ValueNum <= 0 {
		t.Errorf("喜悦 valence 应 >0, got %v", found.ValueNum)
	}
	if found.ValueText == nil || *found.ValueText != "喜悦" {
		t.Errorf("value_text 应=喜悦, got %v", found.ValueText)
	}
	if found.Source != "auto" {
		t.Errorf("source 应=auto, got %q", found.Source)
	}
	_ = sess

	// 幂等：重跑不重复写
	if err := stageEmotionProfile(d)(ctx, nil, sid); err != nil {
		t.Fatal(err)
	}
	rows2, _ := metrics.ListByPerson(ctx, p.ID)
	cnt := 0
	for _, r := range rows2 {
		if r.MetricKey == "emotion" {
			cnt++
		}
	}
	if cnt != 1 {
		t.Errorf("幂等:重跑后应仍 1 条 emotion, got %d", cnt)
	}
}

// TestStageEmotionProfileSameSpeakerDedup 同人情绪测点去重（2026-08-31）：碎片在场归并后
// 同一真人两个标签（主 3s 平静 / 碎片 1s 焦虑）各有情绪行——汇总应只写**一个** emotion
// 测点（主标签的读数），不污染人物情绪时序。
func TestStageEmotionProfileSameSpeakerDedup(t *testing.T) {
	db, err := repo.NewDB(repotest.DSN(t))
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	sessions := &repo.SessionRepo{DB: db}
	transcripts := &repo.TranscriptRepo{DB: db}
	persons := &repo.PersonRepo{DB: db}
	metrics := &repo.PersonMetricRepo{DB: db}
	states := &repo.SpeakerSessionStateRepo{DB: db}
	speakers := &repo.SpeakerRepo{DB: db}

	sid := ids.New()
	if err := sessions.Create(ctx, &repo.AudioSession{ID: sid, Source: "web_upload", Filename: "x.wav", Status: "done"}); err != nil {
		t.Fatal(err)
	}
	sp := &repo.Speaker{UserID: 1, Name: "甲"}
	_ = speakers.Create(ctx, sp)
	p := &repo.Person{UserID: 1, DisplayName: "甲", SpeakerID: &sp.ID}
	_ = persons.Create(ctx, p)
	t.Cleanup(func() {
		_, _ = db.ExecContext(ctx, `DELETE FROM person_metric WHERE person_id = ?`, p.ID.Int64())
		_, _ = db.ExecContext(ctx, `DELETE FROM person WHERE id = ?`, p.ID.Int64())
		_, _ = speakers.DB.ExecContext(ctx, `DELETE FROM speaker WHERE id = ?`, sp.ID.Int64())
	})

	// 转写段给标签时长：主标签 3s、碎片标签 1s（同人）
	tr := &repo.Transcript{SessionID: sid, Language: "zh-CN"}
	if err := transcripts.Create(ctx, tr); err != nil {
		t.Fatal(err)
	}
	conf := 0.9
	if err := transcripts.InsertSegments(ctx, []repo.TranscriptSegment{
		{TranscriptID: tr.ID, SequenceNo: 1, SpeakerLabel: "1", Text: "主要", StartMS: 0, EndMS: 3000, Confidence: &conf},
		{TranscriptID: tr.ID, SequenceNo: 2, SpeakerLabel: "2", Text: "碎片", StartMS: 3100, EndMS: 4100, Confidence: &conf},
	}); err != nil {
		t.Fatal(err)
	}

	tid := ids.New()
	_ = states.InsertBatch(ctx, []repo.SpeakerSessionState{
		{UserID: 1, TranscriptID: tid, SessionID: sid, SpeakerLabel: "1", SpeakerID: &sp.ID, Emotion: "平静", Confidence: 0.9},
		{UserID: 1, TranscriptID: tid, SessionID: sid, SpeakerLabel: "2", SpeakerID: &sp.ID, Emotion: "焦虑", Confidence: 0.6},
	})

	d := StageDeps{
		DB: db, Sessions: sessions, Transcripts: transcripts, Persons: persons,
		PersonMetrics: metrics, SpeakerStates: states, EmotionProfileEnabled: true,
	}
	if err := stageEmotionProfile(d)(ctx, nil, sid); err != nil {
		t.Fatalf("stage 应成功: %v", err)
	}
	rows, err := metrics.ListByPerson(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	n, val := 0, ""
	for _, r := range rows {
		if r.MetricKey == "emotion" {
			n++
			if r.ValueText != nil {
				val = *r.ValueText
			}
		}
	}
	if n != 1 || val != "平静" {
		t.Fatalf("同人两行应只写 1 个「平静」测点，实际 %d 个（值=%q）", n, val)
	}
}
