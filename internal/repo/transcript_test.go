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

// TestSetSegmentSpeaker 覆盖 speaker stage 回填链路：
// SetSegmentSpeaker 按 label 批量回填 + 作用域防跨会话；
// SetSegmentSpeakerByID 单段换人 + 作用域防跨会话；
// ListSpeakersForTranscript 聚合视图按首段 sequence_no 定序。
func TestSetSegmentSpeaker(t *testing.T) {
	db, err := NewDB(TestDSN(t))
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
