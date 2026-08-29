package review

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"

	"zhiwei/internal/ids"
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
	// P3：情绪/环境汇聚（供报告洞察）。旧装配可能不注入，gather 侧有 nil 守卫。
	SpeakerStates *repo.SpeakerSessionStateRepo // 说话人整体情绪（gather 汇聚用）
	Persons       *repo.PersonRepo              // speaker_id → person 名（情绪行显示说话人名）

	// P4：报告漫画（可选）。Comic 为 nil 时不生成漫画；ComicStorage 为 nil 时退回 data URL。
	Comic        provider.ComicProvider // nil = 不生成漫画
	ComicStorage TOSImageUploader       // 存漫画图（nil 时 data URL 兜底）
}

// TOSImageUploader 存图接口（*storage.TOSClient 实现之）。
type TOSImageUploader interface {
	UploadImage(ctx context.Context, b64Data, key string) (string, error)
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

// ---- P4 报告漫画：派生分镜 → 出图 → 存图 ----

// buildComicPrompt 用 LLM 从报告叙事派生 Seedream 多格漫画 prompt。
func (g *Generator) buildComicPrompt(ctx context.Context, narrative string, mood []MoodPoint, scenes []SceneCount) (string, error) {
	sys := "你是漫画分镜师。根据用户的一天总结，写一个文生图 prompt：画成 6-12 格连环画（网格或横条），统一扁平插画风、暖色调、同一主角、每格一个场景、整体风格一致。只输出 prompt 本身（中文画面描述），不要解释。"
	usr := "一天总结：\n" + narrative + "\n\n情绪点：" + fmt.Sprintf("%v", mood) + "\n场景：" + fmt.Sprintf("%v", scenes)
	resp, err := g.LLM.Chat(ctx, provider.ChatRequest{Model: g.Model, System: sys, User: usr})
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(resp.Content), nil
}

// storeComicImage 存漫画图返回可访问 URL：优先 TOS 长期 URL；无 TOS 退回 data URL。
func (g *Generator) storeComicImage(ctx context.Context, b64 string) (string, error) {
	if g.ComicStorage != nil {
		return g.ComicStorage.UploadImage(ctx, b64, "comics/"+ids.New().String()+".jpeg")
	}
	return "data:image/jpeg;base64," + b64, nil
}

// tryAttachComic 生成漫画（派生→出图→存图），失败静默返回 nil。
func (g *Generator) tryAttachComic(ctx context.Context, narrative string, mood []MoodPoint, scenes []SceneCount) *ComicImage {
	if g.Comic == nil {
		return nil
	}
	prompt, err := g.buildComicPrompt(ctx, narrative, mood, scenes)
	if err != nil {
		log.Printf("[review] 漫画分镜派生失败(降级): %v", err)
		return nil
	}
	b64, err := g.Comic.Generate(ctx, prompt)
	if err != nil {
		log.Printf("[review] 漫画出图失败(降级): %v", err)
		return nil
	}
	url, err := g.storeComicImage(ctx, b64)
	if err != nil {
		log.Printf("[review] 漫画存图失败(降级): %v", err)
		return nil
	}
	return &ComicImage{ImageURL: url}
}
