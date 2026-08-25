package review

import (
	"context"
	"encoding/json"
	"fmt"

	"zhiwei/internal/provider"
	"zhiwei/internal/repo"
)

// reviewUserID 单用户 MVP 固定 1（与 agent.toolUserID 一致）。
const reviewUserID = 1

// Generator 是报告引擎（spec §5.2）：从 repo 汇聚数据，直调 Ark 上的 DeepSeek
// 模型（provider.LLMProvider），产出结构化报告并落库。被 cron / HTTP / MCP 三处复用。
// LLM 用接口类型，单测可注入 mock（无需真 Ark/MySQL）。
type Generator struct {
	LLM   provider.LLMProvider // *provider.ArkLLM 实现之；单测传 mock
	Model string               // Ark 上的 DeepSeek 模型/endpoint id（cfg.AgentModel）

	// 版本化 prompt 内容 + 版本号（文件名 stem），由 main 用 os.ReadFile 装入。
	DailyPrompt, WeeklyPrompt, TopicStatusPrompt          string
	DailyPromptVer, WeeklyPromptVer, TopicStatusPromptVer string

	// 落库仓储
	Reviews       *repo.ReviewRepo
	TopicStatuses *repo.TopicStatusRepo
	// 汇聚仓储（只读）
	Memories    *repo.MemoryRepo
	Todos       *repo.TodoRepo
	Topics      *repo.TopicRepo
	Sessions    *repo.SessionRepo
	Transcripts *repo.TranscriptRepo
}

// NewGenerator 构造 Generator（参数与字段对应；main 装配时注入）。
func NewGenerator(llm provider.LLMProvider, model string,
	dailyPrompt, dailyVer, weeklyPrompt, weeklyVer, topicPrompt, topicVer string,
	reviews *repo.ReviewRepo, topicStatuses *repo.TopicStatusRepo,
	memories *repo.MemoryRepo, todos *repo.TodoRepo, topics *repo.TopicRepo,
	sessions *repo.SessionRepo, transcripts *repo.TranscriptRepo) *Generator {
	return &Generator{
		LLM: llm, Model: model,
		DailyPrompt: dailyPrompt, DailyPromptVer: dailyVer,
		WeeklyPrompt: weeklyPrompt, WeeklyPromptVer: weeklyVer,
		TopicStatusPrompt: topicPrompt, TopicStatusPromptVer: topicVer,
		Reviews: reviews, TopicStatuses: topicStatuses,
		Memories: memories, Todos: todos, Topics: topics,
		Sessions: sessions, Transcripts: transcripts,
	}
}

// generateDaily 纯渲染核：user message → Chat → 解析 → (content, rawJSON)。不碰 DB。
func (g *Generator) generateDaily(ctx context.Context, in DailyInput) (*DailyContent, json.RawMessage, error) {
	resp, err := g.LLM.Chat(ctx, provider.ChatRequest{Model: g.Model, System: g.DailyPrompt, User: BuildDailyUser(in)})
	if err != nil {
		return nil, nil, fmt.Errorf("日报 LLM 调用: %w", err)
	}
	c, err := ParseDaily(resp.Content)
	if err != nil {
		return nil, nil, err
	}
	normalizeDaily(c) // 兜底 nil 切片 → []，避免落库/返回时序列化成 null（M5）
	return c, mustJSON(c), nil
}

// generateWeekly 纯渲染核。
func (g *Generator) generateWeekly(ctx context.Context, in WeeklyInput) (*WeeklyContent, json.RawMessage, error) {
	resp, err := g.LLM.Chat(ctx, provider.ChatRequest{Model: g.Model, System: g.WeeklyPrompt, User: BuildWeeklyUser(in)})
	if err != nil {
		return nil, nil, fmt.Errorf("周报 LLM 调用: %w", err)
	}
	c, err := ParseWeekly(resp.Content)
	if err != nil {
		return nil, nil, err
	}
	normalizeWeekly(c) // 兜底 nil 切片 → []（含 by_topic/trends 元素内部切片）（M5）
	return c, mustJSON(c), nil
}

// generateTopicStatus 纯渲染核。
func (g *Generator) generateTopicStatus(ctx context.Context, in TopicStatusInput) (*TopicStatusContent, json.RawMessage, error) {
	resp, err := g.LLM.Chat(ctx, provider.ChatRequest{Model: g.Model, System: g.TopicStatusPrompt, User: BuildTopicStatusUser(in)})
	if err != nil {
		return nil, nil, fmt.Errorf("话题状态 LLM 调用: %w", err)
	}
	c, err := ParseTopicStatus(resp.Content)
	if err != nil {
		return nil, nil, err
	}
	normalizeTopicStatus(c) // 兜底 nil 切片 → []，避免序列化成 null（M5）
	return c, mustJSON(c), nil
}
