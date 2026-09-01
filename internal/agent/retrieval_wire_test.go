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
	res, _, err := searchMemoryHandler(md, toolUserID)(ctx, nil, searchMemoryArgs{Query: "猫", Limit: 5})
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
	if _, _, err := searchMemoryHandler(md, toolUserID)(ctx, nil, searchMemoryArgs{Query: "猫SH", Limit: 5}); err != nil {
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
	orch := NewOrchestrator(rtFor(fake), convRepo, msgRepo)
	orch.Ctx = &ProfileContext{Retrieve: r} // 只装 Retrieve（无 Persons → Head 为空，只测种子）

	const raw = "我的猫应该怎么养" // 含个人信号「我」→ 命中门控，注入种子
	if _, err := orch.RunTurn(ctx, conv, raw); err != nil {
		t.Fatalf("RunTurn: %v", err)
	}
	if !strings.Contains(fake.LastText, "布偶猫SEED") || !strings.Contains(fake.LastText, raw) {
		t.Errorf("发给 dsh 文本应含种子标题+原始问题: %q", fake.LastText)
	}
	if !strings.Contains(fake.LastText, "与该问题可能相关的背景记忆") {
		t.Errorf("种子块应用新措辞: %q", fake.LastText)
	}
	if strings.Contains(fake.LastText, "可能相关的我的记忆") {
		t.Errorf("不应再用旧措辞: %q", fake.LastText)
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

// TestSeedsGateSkipsNonPersonal：query 无个人信号（人称代词/用户数据域名词/「是谁」）时，
// 即使有相关记忆也不注入种子——常识/名词解释题不再被生硬关联用户数据（Phase 1 门控）。
func TestSeedsGateSkipsNonPersonal(t *testing.T) {
	db, err := repo.NewDB(orchDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	convRepo := &repo.AgentConversationRepo{DB: db}
	msgRepo := &repo.AgentMessageRepo{DB: db}
	mem := &repo.MemoryRepo{DB: db}
	ctx := t.Context()
	_ = seedMem(t, mem, "猫的常见习性GATESEED") // 含「猫」：与 query 同向量，本会被召回
	r := &retrieve.Retriever{Memories: mem, Embedder: fakeEmbedder{}, TopK: 5}
	if _, err := r.Backfill(ctx, toolUserID, 500); err != nil {
		t.Fatal(err)
	}

	conv := &repo.AgentConversation{Title: "种子门控"}
	if err := convRepo.Create(ctx, conv); err != nil {
		t.Fatal(err)
	}
	msgData, _ := json.Marshal(map[string]any{"message": map[string]any{"content": []map[string]any{
		{"type": "text", "text": "好的"},
	}}})
	fake := &FakeRuntime{Script: [][]Event{{{Type: EvAssistantMessage, Data: msgData}}}}
	orch := NewOrchestrator(rtFor(fake), convRepo, msgRepo)
	orch.Ctx = &ProfileContext{Retrieve: r} // 只装 Retrieve，测门控

	const raw = "猫的常见习性" // 名词/常识问法，无个人信号 → 不注入种子
	if _, err := orch.RunTurn(ctx, conv, raw); err != nil {
		t.Fatalf("RunTurn: %v", err)
	}
	if strings.Contains(fake.LastText, "背景记忆") || strings.Contains(fake.LastText, "猫的常见习性GATESEED") {
		t.Errorf("常识题不应注入种子: %q", fake.LastText)
	}
}

// TestSeedsGatePersonalVariants：门控口径须与 system prompt 的「关于用户本人」一致——
// 不含「我」字但明确指向用户数据的问法（「张三是谁」「上周录音里聊了什么」）也应注入种子。
func TestSeedsGatePersonalVariants(t *testing.T) {
	db, err := repo.NewDB(orchDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	convRepo := &repo.AgentConversationRepo{DB: db}
	msgRepo := &repo.AgentMessageRepo{DB: db}
	mem := &repo.MemoryRepo{DB: db}
	ctx := t.Context()
	_ = seedMem(t, mem, "和张三聊了猫的事GV")
	r := &retrieve.Retriever{Memories: mem, Embedder: fakeEmbedder{}, TopK: 5}
	if _, err := r.Backfill(ctx, toolUserID, 500); err != nil {
		t.Fatal(err)
	}

	msgData, _ := json.Marshal(map[string]any{"message": map[string]any{"content": []map[string]any{
		{"type": "text", "text": "好的"},
	}}})

	for _, raw := range []string{"张三是谁", "上周录音里聊了什么"} {
		conv := &repo.AgentConversation{Title: "种子门控变体:" + raw}
		if err := convRepo.Create(ctx, conv); err != nil {
			t.Fatal(err)
		}
		fake := &FakeRuntime{Script: [][]Event{{{Type: EvAssistantMessage, Data: msgData}}}}
		orch := NewOrchestrator(rtFor(fake), convRepo, msgRepo)
		orch.Ctx = &ProfileContext{Retrieve: r}
		if _, err := orch.RunTurn(ctx, conv, raw); err != nil {
			t.Fatalf("RunTurn(%q): %v", raw, err)
		}
		if !strings.Contains(fake.LastText, "背景记忆") || !strings.Contains(fake.LastText, "和张三聊了猫的事GV") {
			t.Errorf("个人数据问法 %q 应注入种子: %q", raw, fake.LastText)
		}
	}
}
