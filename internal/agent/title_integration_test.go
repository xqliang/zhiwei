package agent

import (
	"context"
	"testing"

	"zhiwei/internal/provider"
	"zhiwei/internal/repo"
)

// fakeLLMProvider 实现 provider.LLMProvider（与 GenerateTitle 的第 6 形参签名一致），
// 直接返回预设内容/错误，不触碰真实 Ark。用它走完整的生产入口 GenerateTitle（真 repo 读-判-写回）。
// 注意：不能复用 title_test.go 里的 fakeLLM——那个实现的是内部 titleLLM 接口
// （Chat(ctx, titleChatReq)），签名与 provider.LLMProvider（Chat(ctx, provider.ChatRequest)）不同。
type fakeLLMProvider struct {
	out string // 预设返回内容
	err error  // 预设返回错误（非 nil 时 GenerateTitle 会静默跳过）
}

func (f *fakeLLMProvider) Chat(_ context.Context, _ provider.ChatRequest) (provider.ChatResponse, error) {
	return provider.ChatResponse{Content: f.out}, f.err
}

// 跑两轮真 user 消息，第 2 轮后 GenerateTitle 应把标题写成 auto。
func TestGenerateTitleAfterSecondTurn(t *testing.T) {
	db, err := repo.NewDB(orchDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	convRepo := &repo.AgentConversationRepo{DB: db}
	msgRepo := &repo.AgentMessageRepo{DB: db}
	ctx := t.Context()
	conv := &repo.AgentConversation{}
	if err := convRepo.Create(ctx, conv); err != nil {
		t.Fatal(err)
	}
	cid := conv.ID
	// 2 条 user 消息（两轮）
	for _, txt := range []string{"帮我看下本周待办", "按优先级排序"} {
		if err := msgRepo.Append(ctx, &repo.AgentMessage{ConversationID: &cid, Role: "user", Content: txt}); err != nil {
			t.Fatal(err)
		}
	}
	// LLM 返回预设标题，走完整生产入口（内部 llmAdapter 会把 provider 适配成 titleLLM）。
	GenerateTitle(ctx, 1, cid, convRepo, msgRepo, &fakeLLMProvider{out: "本周待办梳理"}, "test-model")

	title, source, err := convRepo.TitleState(ctx, 1, cid)
	if err != nil {
		t.Fatal(err)
	}
	if title != "本周待办梳理" || source != titleSourceAuto {
		t.Errorf("got title=%q source=%q, want 本周待办梳理/auto", title, source)
	}
}

// manual 标题永不覆盖：即便满足生成条件，写回前的 CAS 也应因 source=manual 放弃。
func TestGenerateTitleManualNeverOverwritten(t *testing.T) {
	db, err := repo.NewDB(orchDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	convRepo := &repo.AgentConversationRepo{DB: db}
	msgRepo := &repo.AgentMessageRepo{DB: db}
	ctx := t.Context()
	conv := &repo.AgentConversation{Title: "我手动的标题"}
	if err := convRepo.Create(ctx, conv); err != nil {
		t.Fatal(err)
	}
	// 明确把来源标成 manual（模拟用户手动改名）。
	if err := convRepo.UpdateTitle(ctx, 1, conv.ID, "我手动的标题", titleSourceManual); err != nil {
		t.Fatal(err)
	}
	cid := conv.ID
	for _, txt := range []string{"q1", "q2"} {
		if err := msgRepo.Append(ctx, &repo.AgentMessage{ConversationID: &cid, Role: "user", Content: txt}); err != nil {
			t.Fatal(err)
		}
	}
	GenerateTitle(ctx, 1, cid, convRepo, msgRepo, &fakeLLMProvider{out: "不该出现的标题"}, "m")

	title, source, _ := convRepo.TitleState(ctx, 1, cid)
	if title != "我手动的标题" || source != titleSourceManual {
		t.Errorf("manual 标题被覆盖: title=%q source=%q", title, source)
	}
}
