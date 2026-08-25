package memory

// conversation_e2e_test.go 是对话转记忆的端到端集成测试（真 MySQL + fakeLLM）：
// 写入一段对话（agent_conversation + agent_message）→ ExtractConversation →
// 断言候选落库（conversation_id 标记、session_id NULL）+ 幂等（重跑不翻倍）。
// 门禁 TEST_MYSQL_DSN；插入共享表（memory/memory_topic/agent_message/agent_conversation）
// 全部 t.Cleanup 清理，防并行 worktree 串扰。LLM 用 fake，不触真 Ark。

import (
	"context"
	"fmt"
	"os"
	"testing"

	"zhiwei/internal/ids"
	"zhiwei/internal/repo"
)

// TestMain 初始化雪花 ID 节点（repo 方法与 ids.New() 生成主键需要）。
// package memory 原有单测用字面量 ids，不需要；本 e2e 与 conversation_test 的 ids.New() 需要。
func TestMain(m *testing.M) {
	_ = ids.Init(1)
	os.Exit(m.Run())
}

// TestExtractConversationE2E 覆盖「对话 → 候选 → 闸门 → dedup → 落库 + 幂等」全链路（§12 验收级）。
func TestExtractConversationE2E(t *testing.T) {
	db, err := repo.NewDB(repo.TestDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	convs := &repo.AgentConversationRepo{DB: db}
	msgsRepo := &repo.AgentMessageRepo{DB: db}
	deps := ConversationExtractDeps{
		DB:            db,
		AgentMessages: msgsRepo,
		Topics:        &repo.TopicRepo{DB: db},
		Memories:      &repo.MemoryRepo{DB: db},
		MemoryTopics:  &repo.MemoryTopicRepo{DB: db},
		Model:         "fake-model",
		Prompt:        "对话抽取系统指令",
		PromptVersion: "conversation_extraction_v1",
		Window:        10,
		Gate:          GateConfig{MinConf: 0.6, TodoConf: 0.85},
	}

	// 建对话 + 写入两条消息（一问一答）。
	conv := &repo.AgentConversation{Title: "e2e 对话"}
	if err := convs.Create(ctx, conv); err != nil {
		t.Fatal(err)
	}
	convID := conv.ID
	t.Cleanup(func() {
		bg := context.Background()
		_ = deps.MemoryTopics.DeleteByConversationExt(bg, db, convID)
		_ = deps.Memories.DeleteByConversationExt(bg, db, convID)
		_, _ = db.ExecContext(bg, `DELETE FROM agent_message WHERE conversation_id = ?`, convID.Int64())
		_, _ = db.ExecContext(bg, `DELETE FROM agent_conversation WHERE id = ?`, convID.Int64())
	})
	for _, mm := range []repo.AgentMessage{
		{ConversationID: &convID, Role: "user", Kind: "text", Content: "我最近开始每天早上跑步"},
		{ConversationID: &convID, Role: "assistant", Kind: "text", Content: "很好，坚持多久了？"},
	} {
		m := mm // 取址安全
		if err := msgsRepo.Append(ctx, &m); err != nil {
			t.Fatal(err)
		}
	}

	// fakeLLM：候选标题带唯一后缀，避免与共享库/并行 worktree 的同名 active 记忆佐证碰撞
	//（碰撞会让候选被 D1 跳过，Kept 变 0，破坏断言）。两次 ExtractConversation = 两窗调用，备两份响应。
	uniq := ids.New().String()
	resp := fmt.Sprintf(`{"candidates":[
	  {"type":"preference","title":"晨跑习惯%s","content":"用户最近开始每天早上跑步（e2e唯一%s）",
	   "epistemic_type":"observed","importance":0.6,"confidence":0.9,
	   "is_todo":false,"todo_due":null,"topics":[],"block_index":1}
	]}`, uniq, uniq)
	deps.LLM = &fakeLLM{responses: []string{resp, resp}}

	// 第一次抽取
	res, err := ExtractConversation(ctx, deps, convID)
	if err != nil {
		t.Fatalf("ExtractConversation: %v", err)
	}
	if res.Kept < 1 {
		t.Fatalf("Kept = %d, want >= 1", res.Kept)
	}
	if res.Messages != 2 {
		t.Fatalf("Messages = %d, want 2", res.Messages)
	}

	// 落库校验：SELECT *（safe 模式）取回对话记忆，conversation_id 命中、session_id NULL
	var got repo.Memory
	if err := db.GetContext(ctx, &got,
		`SELECT * FROM memory WHERE conversation_id = ? LIMIT 1`, convID.Int64()); err != nil {
		t.Fatalf("查对话记忆(safe-mode SELECT *): %v", err)
	}
	if got.ConversationID == nil || *got.ConversationID != convID {
		t.Errorf("conversation_id 未落库: %v", got.ConversationID)
	}
	if got.SessionID != nil {
		t.Errorf("对话记忆 session_id 应为 NULL, got %v", *got.SessionID)
	}

	countByConv := func() int {
		var n int
		if err := db.GetContext(ctx, &n,
			`SELECT COUNT(*) FROM memory WHERE conversation_id = ?`, convID.Int64()); err != nil {
			t.Fatalf("计数: %v", err)
		}
		return n
	}
	first := countByConv()
	if first < 1 {
		t.Fatalf("首次落库 %d 条, want >= 1", first)
	}

	// 幂等：再跑一次，按 conversation_id 先删后插 → 条数不翻倍
	if _, err := ExtractConversation(ctx, deps, convID); err != nil {
		t.Fatalf("ExtractConversation 第二次: %v", err)
	}
	if second := countByConv(); second != first {
		t.Fatalf("幂等失败：第二次 %d 条, 第一次 %d 条（应相等）", second, first)
	}
}
