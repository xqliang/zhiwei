package retrieve

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/jmoiron/sqlx"

	"zhiwei/internal/ids"
	"zhiwei/internal/repo"
)

func retrieveTestDB(t *testing.T) *sqlx.DB {
	t.Helper()
	dsn := os.Getenv("TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("TEST_MYSQL_DSN 未设置，跳过集成测试")
	}
	db, err := repo.NewDB(dsn)
	if err != nil {
		t.Fatal(err)
	}
	return db
}

// fakeEmbedder：含关键词则某维为 1，做可控语义。实现 provider.EmbeddingProvider。
type fakeEmbedder struct{}

func (fakeEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i, t := range texts {
		cat, dog := 0.0, 0.0
		if strings.Contains(t, "猫") {
			cat = 1
		}
		if strings.Contains(t, "狗") {
			dog = 1
		}
		out[i] = []float32{float32(cat), float32(dog)}
	}
	return out, nil
}

func TestRetrieverBackfillAndSearch(t *testing.T) {
	db := retrieveTestDB(t)
	mem := &repo.MemoryRepo{DB: db}
	ctx := t.Context()
	seed := func(title string) ids.ID {
		m := &repo.Memory{Type: "fact", Title: title, Content: title, EpistemicType: "observed",
			Confidence: 0.8, Importance: 0.5, Status: "active", TranscriptSegmentIDs: ids.List{}}
		_ = mem.InsertExt(ctx, mem.DB, []*repo.Memory{m})
		t.Cleanup(func() { _, _ = mem.DB.Exec("DELETE FROM memory WHERE id=?", m.ID.Int64()) })
		return m.ID
	}
	catID := seed("我养了一只猫RVX")
	_ = seed("邻居有条狗RVX")

	r := &Retriever{Memories: mem, Embedder: fakeEmbedder{}, TopK: 5}
	n, err := r.Backfill(ctx, 1, 500)
	if err != nil || n < 2 {
		t.Fatalf("backfill n=%d err=%v", n, err)
	}
	got, err := r.Search(ctx, 1, "猫", "", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) == 0 || got[0].ID != catID {
		t.Fatalf("检索「猫」应 catID 居首, got %+v", got)
	}
}
