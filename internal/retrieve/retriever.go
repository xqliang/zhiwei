package retrieve

import (
	"context"
	"sort"
	"strings"
	"time"

	"zhiwei/internal/provider"
	"zhiwei/internal/repo"
)

// timeNow 便于单测覆盖（recency 用）。
var timeNow = time.Now

// Retriever 语义检索服务：embed query → 已嵌候选暴力 cosine → 混合打分 → topK。
// Embedder 为 nil / query 空 / 无已嵌候选 → Search 返回 nil（调用方退回关键词）。
type Retriever struct {
	Memories *repo.MemoryRepo
	Embedder provider.EmbeddingProvider
	TopK     int // 默认种子条数（context 头用）；Search 的 limit 显式传入
}

// Search 混合召回该用户 active 记忆。typ 非空按 type 过滤。limit<=0 用 TopK。
func (r *Retriever) Search(ctx context.Context, userID int64, query, typ string, limit int) ([]repo.Memory, error) {
	if r == nil || r.Embedder == nil || query == "" {
		return nil, nil
	}
	if limit <= 0 {
		limit = r.TopK
	}
	if limit <= 0 {
		limit = 20 // 防御：TopK 未配(0)时兜个合理上限，避免 out[:0] 恒返回空
	}
	qv, err := r.Embedder.Embed(ctx, []string{query})
	if err != nil || len(qv) == 0 {
		return nil, err
	}
	cands, err := r.Memories.ListEmbeddedCandidates(ctx, userID, 2000)
	if err != nil {
		return nil, err
	}
	nowT := timeNow()
	type scored struct {
		m repo.Memory
		s float64
	}
	var out []scored
	for _, m := range cands {
		if typ != "" && m.Type != typ {
			continue
		}
		v := DecodeF32(m.Embedding)
		if v == nil {
			continue
		}
		sim := cosine(qv[0], v)
		kw := keywordScore(query, m.Title, m.Content)
		at := m.EventAt
		if at == nil {
			at = &m.CreatedAt
		}
		out = append(out, scored{m, blend(sim, kw, recencyScore(at, nowT), m.Importance)})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].s > out[j].s })
	if len(out) > limit {
		out = out[:limit]
	}
	res := make([]repo.Memory, len(out))
	for i := range out {
		res[i] = out[i].m
	}
	return res, nil
}

// Backfill 给该用户 active 且未嵌的记忆回填向量（title+content 拼嵌入文本），逐条 UPDATE。
// 返回成功回填条数。embedder 为 nil → (0,nil)。
func (r *Retriever) Backfill(ctx context.Context, userID int64, limit int) (int, error) {
	if r == nil || r.Embedder == nil {
		return 0, nil
	}
	rows, err := r.Memories.ListForEmbedding(ctx, userID, limit)
	if err != nil || len(rows) == 0 {
		return 0, err
	}
	texts := make([]string, len(rows))
	for i, m := range rows {
		texts[i] = embedText(m.Title, m.Content)
	}
	vecs, err := r.Embedder.Embed(ctx, texts)
	if err != nil {
		return 0, err
	}
	n := 0
	for i, m := range rows {
		if i >= len(vecs) || len(vecs[i]) == 0 {
			continue
		}
		if err := r.Memories.SetEmbeddingExt(ctx, r.Memories.DB, m.ID, EncodeF32(vecs[i])); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

func embedText(title, content string) string {
	if content == "" {
		return title
	}
	return title + "。" + content
}

// keywordScore 粗粒度关键词命中：query（trim+lower）是否为 title+content（lower）子串 → 1/0。
func keywordScore(query, title, content string) float64 {
	q := normalize(query)
	if q == "" {
		return 0
	}
	if strings.Contains(normalize(title+"。"+content), q) {
		return 1
	}
	return 0
}

func normalize(s string) string { return strings.ToLower(strings.TrimSpace(s)) }
