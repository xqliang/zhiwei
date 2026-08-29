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
