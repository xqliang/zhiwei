package repo

import (
	"encoding/json"
	"testing"

	"zhiwei/internal/ids"
	"zhiwei/internal/repotest"
)

func TestSpeakerSessionState(t *testing.T) {
	db, err := NewDB(repotest.DSN(t))
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	sid, tid := ids.New(), ids.New()
	r := &SpeakerSessionStateRepo{DB: db}
	rows := []SpeakerSessionState{
		{UserID: 1, TranscriptID: tid, SessionID: sid, SpeakerLabel: "1", Emotion: "平静", MicroEmotion: "专注", MentalState: "投入", Confidence: 0.8},
		{UserID: 1, TranscriptID: tid, SessionID: sid, SpeakerLabel: "2", Emotion: "焦虑", Confidence: 0.6},
	}
	if err := r.InsertBatch(ctx, rows); err != nil {
		t.Fatalf("InsertBatch: %v", err)
	}
	got, err := r.ListBySession(ctx, 1, sid)
	if err != nil {
		t.Fatalf("ListBySession: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 rows, got %d", len(got))
	}
	// 越权：user 2 看不到
	got2, _ := r.ListBySession(ctx, 2, sid)
	if len(got2) != 0 {
		t.Errorf("越权应 0 行, got %d", len(got2))
	}
}

func TestTranscriptSetAcoustic(t *testing.T) {
	db, err := NewDB(repotest.DSN(t))
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	tr := &Transcript{SessionID: ids.New(), Language: "zh-CN"}
	trepo := &TranscriptRepo{DB: db}
	if err := trepo.Create(ctx, tr); err != nil {
		t.Fatal(err)
	}
	bg := json.RawMessage(`["键盘","车流"]`)
	if err := trepo.SetAcoustic(ctx, tr.ID, "室内", &bg, "无", "专注工作"); err != nil {
		t.Fatalf("SetAcoustic: %v", err)
	}
	got, err := trepo.GetBySession(ctx, tr.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if got.AcousticScene != "室内" || got.WeatherCues != "无" || got.OverallMood != "专注工作" {
		t.Errorf("环境列未写入: %+v", got)
	}
	if got.BackgroundSounds == nil || string(*got.BackgroundSounds) == "" {
		t.Error("background_sounds 未写入")
	}
}

// TestDedupStatesBySpeaker 同人去重（2026-08-31，纯函数）：碎片在场归并后同一真人多标签
// 的情绪行折成每人一行——保留发言时长最大标签的情绪；未解析（speaker_id 空）行原样保留。
func TestDedupStatesBySpeaker(t *testing.T) {
	sid := ids.New()
	rows := []SpeakerSessionState{
		{ID: 1, SpeakerLabel: "speaker_0", SpeakerID: &sid, Emotion: "平静"}, // 主标签 51s
		{ID: 2, SpeakerLabel: "speaker_1", SpeakerID: &sid, Emotion: "焦虑"}, // 碎片 4s
		{ID: 3, SpeakerLabel: "speaker_2", Emotion: "未知人"},                  // 未解析：保留
	}
	dur := map[string]int64{"speaker_0": 51000, "speaker_1": 4000}
	got := DedupStatesBySpeaker(rows, dur)
	if len(got) != 2 {
		t.Fatalf("应剩 2 行（思敏 1 行 + 未解析 1 行），实际 %d: %+v", len(got), got)
	}
	if got[0].Emotion != "平静" || got[0].SpeakerLabel != "speaker_0" {
		t.Fatalf("同人应保留主标签(时长 51s)的「平静」，实际 %+v", got[0])
	}
	if got[1].SpeakerLabel != "speaker_2" {
		t.Fatalf("未解析行应原样保留，实际 %+v", got[1])
	}

	// 时长反转：碎片标签反而更长 → 保留后来者（时长优先于出现顺序）
	rows2 := []SpeakerSessionState{
		{ID: 4, SpeakerLabel: "a", SpeakerID: &sid, Emotion: "先出现但短"},
		{ID: 5, SpeakerLabel: "b", SpeakerID: &sid, Emotion: "后出现但长"},
	}
	got2 := DedupStatesBySpeaker(rows2, map[string]int64{"a": 1000, "b": 9000})
	if len(got2) != 1 || got2[0].Emotion != "后出现但长" {
		t.Fatalf("时长最大的标签应胜出，实际 %+v", got2)
	}

	// 单行 / 全未解析：原样返回
	one := []SpeakerSessionState{{ID: 6, SpeakerLabel: "x", SpeakerID: &sid}}
	if got3 := DedupStatesBySpeaker(one, nil); len(got3) != 1 || got3[0].ID != 6 {
		t.Fatalf("单行应原样返回，实际 %+v", got3)
	}
}

// TestLabelDurations 标签发言总时长统计（同人去重的判据）。
func TestLabelDurations(t *testing.T) {
	segs := []TranscriptSegment{
		{SpeakerLabel: "a", StartMS: 0, EndMS: 3000},
		{SpeakerLabel: "a", StartMS: 4000, EndMS: 6000},
		{SpeakerLabel: "b", StartMS: 7000, EndMS: 7500},
	}
	got := LabelDurations(segs)
	if got["a"] != 5000 || got["b"] != 500 {
		t.Fatalf("a=5000 b=500, 实际 %+v", got)
	}
}
