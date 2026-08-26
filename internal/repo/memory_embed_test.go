package repo

import (
	"testing"

	"zhiwei/internal/ids"
)

func TestMemoryEmbeddingStore(t *testing.T) {
	db := testDB(t)
	r := &MemoryRepo{DB: db}
	ctx := t.Context()
	m := &Memory{Type: "fact", Title: "向量存取T", Content: "内容", EpistemicType: "observed",
		Confidence: 0.8, Importance: 0.5, Status: "active", TranscriptSegmentIDs: ids.List{}}
	if err := r.InsertExt(ctx, r.DB, []*Memory{m}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = r.DB.Exec("DELETE FROM memory WHERE id=?", m.ID.Int64()) })

	// 初始应在「待嵌」列表（embedding NULL）
	need, err := r.ListForEmbedding(ctx, 1, 500)
	if err != nil {
		t.Fatal(err)
	}
	if !containsMemID(need, m.ID) {
		t.Fatal("新记忆应在待嵌列表")
	}
	// 写入向量
	if err := r.SetEmbeddingExt(ctx, r.DB, m.ID, []byte{1, 2, 3, 4}); err != nil {
		t.Fatal(err)
	}
	// 现应在「已嵌候选」、消失于「待嵌」
	cand, err := r.ListEmbeddedCandidates(ctx, 1, 500)
	if err != nil {
		t.Fatal(err)
	}
	if !containsMemID(cand, m.ID) {
		t.Fatal("写向量后应在已嵌候选")
	}
	need2, _ := r.ListForEmbedding(ctx, 1, 500)
	if containsMemID(need2, m.ID) {
		t.Error("写向量后不应再在待嵌列表")
	}
}

func containsMemID(rows []Memory, id ids.ID) bool {
	for _, r := range rows {
		if r.ID == id {
			return true
		}
	}
	return false
}
