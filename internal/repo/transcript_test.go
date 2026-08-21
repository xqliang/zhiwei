package repo

import (
	"context"
	"testing"

	"zhiwei/internal/ids"
)

// TestTranscriptUpdateAndRecompute 覆盖 ASR 就地编辑落库链路：
// UpdateSegmentText 改文本 + 跨 transcript 作用域静默忽略 + RecomputeFullText 重算。
func TestTranscriptUpdateAndRecompute(t *testing.T) {
	db, err := NewDB(TestDSN(t))
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
