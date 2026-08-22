package repo

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"testing"

	"zhiwei/internal/ids"
)

// TestSpeakerCRUD 覆盖 SpeakerRepo 的增删改查全链路（真实 MySQL）：
// 创建回填雪花 ID、Get 读回、UpdateName / UpdateEmbedding 改字段、
// List 只返回 active、Delete 后查不到。未设 TEST_MYSQL_DSN 时经 TestDSN 跳过。
func TestSpeakerCRUD(t *testing.T) {
	db, err := NewDB(TestDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	r := &SpeakerRepo{DB: db}
	ctx := context.Background()

	// 创建：Source/Status 留空由 Create 兜底默认；embedding 存 4 字节占位
	sp := &Speaker{Name: "说话人ab12c", Source: "auto", Embedding: []byte{0, 0, 0, 0}, SampleCount: 1}
	if err := r.Create(ctx, sp); err != nil {
		t.Fatalf("create: %v", err)
	}
	if sp.ID == 0 {
		t.Fatal("id 未回填")
	}

	// Get：字段读回正确，UserID/Status 默认值已落库
	got, err := r.Get(ctx, sp.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Name != "说话人ab12c" || got.Source != "auto" {
		t.Fatalf("got %+v", got)
	}
	if got.UserID != 1 || got.Status != "active" || got.SampleCount != 1 {
		t.Fatalf("默认值/字段异常: %+v", got)
	}
	if !bytes.Equal(got.Embedding, []byte{0, 0, 0, 0}) {
		t.Fatalf("embedding 读回不一致: %v", got.Embedding)
	}

	// UpdateName：改名后应可读回新名
	if err := r.UpdateName(ctx, sp.ID, "张三"); err != nil {
		t.Fatalf("updateName: %v", err)
	}
	if g2, _ := r.Get(ctx, sp.ID); g2.Name != "张三" {
		t.Fatalf("name=%s", g2.Name)
	}

	// UpdateEmbedding：声纹重建后灾备 BLOB 应被覆盖
	if err := r.UpdateEmbedding(ctx, sp.ID, []byte{1, 2, 3, 4}); err != nil {
		t.Fatalf("updateEmbedding: %v", err)
	}
	if g3, _ := r.Get(ctx, sp.ID); !bytes.Equal(g3.Embedding, []byte{1, 2, 3, 4}) {
		t.Fatalf("embedding 未更新: %v", g3.Embedding)
	}

	// List：active 名册非空（含刚建的这条）
	list, err := r.List(ctx)
	if err != nil || len(list) == 0 {
		t.Fatalf("list: %v %d", err, len(list))
	}

	// Delete：删除后再 Get 应返回 sql.ErrNoRows
	if err := r.Delete(ctx, sp.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := r.Get(ctx, sp.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("删除后仍可查到: err=%v", err)
	}
}

// TestSpeakerMergeInto 覆盖声纹页「手动合并」repo 层：建 target + 2 个 source 说话人，
// 把 3 段挂到 source，MergeInto 后：段 speaker_id 改指 target、source 行删除、target 保留。
// 对应后端 POST /api/speakers/merge（声纹 tab 手动合并）。未设 TEST_MYSQL_DSN 时跳过。
func TestSpeakerMergeInto(t *testing.T) {
	db, err := NewDB(TestDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	speakers := &SpeakerRepo{DB: db}
	sessions := &SessionRepo{DB: db}
	transcripts := &TranscriptRepo{DB: db}

	// session + transcript + 3 段
	sid := ids.New()
	if err := sessions.Create(ctx, &AudioSession{ID: sid, Source: "test", Filename: "t.wav", StoragePath: "t.wav", Status: "processing"}); err != nil {
		t.Fatal(err)
	}
	tr := &Transcript{SessionID: sid, Language: "zh-CN"}
	if err := transcripts.Create(ctx, tr); err != nil {
		t.Fatal(err)
	}
	if err := transcripts.InsertSegments(ctx, []TranscriptSegment{
		{TranscriptID: tr.ID, SequenceNo: 1, SpeakerLabel: "0", Text: "a", StartMS: 0, EndMS: 1000},
		{TranscriptID: tr.ID, SequenceNo: 2, SpeakerLabel: "1", Text: "b", StartMS: 1000, EndMS: 2000},
		{TranscriptID: tr.ID, SequenceNo: 3, SpeakerLabel: "1", Text: "c", StartMS: 2000, EndMS: 3000},
	}); err != nil {
		t.Fatal(err)
	}
	segs, _ := transcripts.ListSegments(ctx, tr.ID)
	target := &Speaker{Name: "目标"}
	srcA := &Speaker{Name: "源A"}
	srcB := &Speaker{Name: "源B"}
	for _, s := range []*Speaker{target, srcA, srcB} {
		if err := speakers.Create(ctx, s); err != nil {
			t.Fatal(err)
		}
	}
	// seg1 → srcA；seg2/seg3 → srcB
	if err := transcripts.SetSegmentSpeakerByID(ctx, tr.ID, segs[0].ID, srcA.ID); err != nil {
		t.Fatal(err)
	}
	if err := transcripts.SetSegmentSpeakerByID(ctx, tr.ID, segs[1].ID, srcB.ID); err != nil {
		t.Fatal(err)
	}
	if err := transcripts.SetSegmentSpeakerByID(ctx, tr.ID, segs[2].ID, srcB.ID); err != nil {
		t.Fatal(err)
	}

	// 合并 srcA + srcB → target
	merged, err := speakers.MergeInto(ctx, target.ID, []ids.ID{srcA.ID, srcB.ID})
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	if merged != 3 {
		t.Fatalf("应改指 3 段，实际 %d", merged)
	}
	// source 行已删
	if _, err := speakers.Get(ctx, srcA.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("srcA 未删除: %v", err)
	}
	if _, err := speakers.Get(ctx, srcB.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("srcB 未删除: %v", err)
	}
	// target 保留
	if _, err := speakers.Get(ctx, target.ID); err != nil {
		t.Fatalf("target 不应被删: %v", err)
	}
	// 全部段改指 target
	after, _ := transcripts.ListSegments(ctx, tr.ID)
	for _, s := range after {
		if s.SpeakerID == nil || *s.SpeakerID != target.ID {
			t.Fatalf("段 %d 未改指 target: %+v", s.SequenceNo, s.SpeakerID)
		}
	}
}
