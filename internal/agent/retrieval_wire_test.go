package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"zhiwei/internal/ids"
	"zhiwei/internal/repo"
	"zhiwei/internal/retrieve"
)

// fakeEmbedder：含「猫」某维=1、「狗」另一维=1，做可控语义。实现 provider.EmbeddingProvider。
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

func seedMem(t *testing.T, mem *repo.MemoryRepo, title string) ids.ID {
	t.Helper()
	m := &repo.Memory{Type: "fact", Title: title, Content: title, EpistemicType: "observed",
		Confidence: 0.8, Importance: 0.5, Status: "active", TranscriptSegmentIDs: ids.List{}}
	if err := mem.InsertExt(t.Context(), mem.DB, []*repo.Memory{m}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = mem.DB.Exec("DELETE FROM memory WHERE id=?", m.ID.Int64()) })
	return m.ID
}

// TestSearchMemoryHybrid：装 Retrieve 后 search_memory 走向量混合，语义「猫」命中猫记忆居首；
// Retrieve=nil 时退回关键词兜底不报错。
func TestSearchMemoryHybrid(t *testing.T) {
	md, _ := p2dDeps(t)
	ctx := t.Context()
	catID := seedMem(t, md.Memory, "我喜欢猫SH")
	_ = seedMem(t, md.Memory, "邻居的狗SH")
	r := &retrieve.Retriever{Memories: md.Memory, Embedder: fakeEmbedder{}, TopK: 5}
	if _, err := r.Backfill(ctx, toolUserID, 500); err != nil {
		t.Fatal(err)
	}
	md.Retrieve = r
	res, _, err := searchMemoryHandler(md)(ctx, nil, searchMemoryArgs{Query: "猫", Limit: 5})
	if err != nil {
		t.Fatal(err)
	}
	var out []memoryOut
	if err := json.Unmarshal([]byte(mcpText(t, res)), &out); err != nil {
		t.Fatal(err)
	}
	if len(out) == 0 || out[0].ID != catID {
		t.Fatalf("混合检索「猫」应 catID 居首: %+v", out)
	}
	// nil Retrieve → 关键词兜底不报错（用独占词命中）
	md.Retrieve = nil
	if _, _, err := searchMemoryHandler(md)(ctx, nil, searchMemoryArgs{Query: "猫SH", Limit: 5}); err != nil {
		t.Fatalf("关键词兜底不应报错: %v", err)
	}
}

// TestOrchestratorSeedsInjection：装 Ctx.Retrieve 后，发给 dsh 的文本前置「相关记忆」种子
// （含命中记忆标题）+ 原始问题；落库 user 消息仍为原始（D2 不变）。
func TestOrchestratorSeedsInjection(t *testing.T) {
	db, err := repo.NewDB(orchDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	convRepo := &repo.AgentConversationRepo{DB: db}
	msgRepo := &repo.AgentMessageRepo{DB: db}
	mem := &repo.MemoryRepo{DB: db}
	ctx := t.Context()
	_ = seedMem(t, mem, "我在养一只布偶猫SEED")
	r := &retrieve.Retriever{Memories: mem, Embedder: fakeEmbedder{}, TopK: 5}
	if _, err := r.Backfill(ctx, toolUserID, 500); err != nil {
		t.Fatal(err)
	}

	conv := &repo.AgentConversation{Title: "种子注入"}
	if err := convRepo.Create(ctx, conv); err != nil {
		t.Fatal(err)
	}
	msgData, _ := json.Marshal(map[string]any{"message": map[string]any{"content": []map[string]any{
		{"type": "text", "text": "好的"},
	}}})
	fake := &FakeRuntime{Script: [][]Event{{{Type: EvAssistantMessage, Data: msgData}}}}
	orch := NewOrchestrator(fake, convRepo, msgRepo)
	orch.Ctx = &ProfileContext{Retrieve: r} // 只装 Retrieve（无 Persons → Head 为空，只测种子）

	const raw = "猫应该怎么养"
	if _, err := orch.RunTurn(ctx, conv, raw); err != nil {
		t.Fatalf("RunTurn: %v", err)
	}
	if !strings.Contains(fake.LastText, "布偶猫SEED") || !strings.Contains(fake.LastText, raw) {
		t.Errorf("发给 dsh 文本应含种子标题+原始问题: %q", fake.LastText)
	}
	if fake.LastText == raw {
		t.Errorf("应前置种子(≠原始): %q", fake.LastText)
	}
	msgs, err := msgRepo.ListByConversation(ctx, 1, conv.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) == 0 || msgs[0].Content != raw {
		t.Errorf("落库 user 消息应为原始文本(不含种子): %+v", msgs)
	}
}
