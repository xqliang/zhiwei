package agent

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"unicode/utf8"

	"zhiwei/internal/ids"
	"zhiwei/internal/provider"
	"zhiwei/internal/repo"
)

// titleLLM 抽象 LLM 调用（测试注入 fake，生产传 provider.LLMProvider 适配器）。
type titleLLM interface {
	Chat(ctx context.Context, req titleChatReq) (string, error)
}

type titleChatReq struct {
	Model       string
	System      string
	User        string
	Temperature float64
}

// llmAdapter 把 provider.LLMProvider 适配成 titleLLM。
type llmAdapter struct{ p provider.LLMProvider }

func (a llmAdapter) Chat(ctx context.Context, req titleChatReq) (string, error) {
	resp, err := a.p.Chat(ctx, provider.ChatRequest{
		Model: req.Model, System: req.System, User: req.User, Temperature: req.Temperature,
	})
	return resp.Content, err
}

// errTitleSkip 表示「本次不生成/生成失败但静默跳过」——非错误，调用方据此不报错。
var errTitleSkip = errors.New("title generation skipped")

// titlePrompt 系统指令：要求 ≤15 字中文短标题、只输出标题本身。
const titlePrompt = "根据下面的对话内容，生成一个不超过15个中文字符的简短标题。" +
	"只输出标题本身，不要加引号、不要解释、不要标点结尾、不要换行。"

// titleSourceManual/Auto 标题来源取值（与 DB title_source 列一致）。
const (
	titleSourceManual = "manual"
	titleSourceAuto   = "auto"
)

// placeholderTitle 列表占位标题（与前端 c.title || '新对话' 一致）。
const placeholderTitle = "新对话"

// sanitizeTitle 清洗 LLM 输出：去首尾空白/引号/书名号/句末标点、只取首行、截断 256。
func sanitizeTitle(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	s = strings.TrimSpace(s)
	s = strings.Trim(s, `"'`+"“”‘’《》")
	s = strings.TrimRight(s, "。.！!？?，,、；;：:")
	return truncateRunes(s, 256)
}

// truncateRunes 按 rune 截断到 max，避免截断多字节字符。
func truncateRunes(s string, max int) string {
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	return string([]rune(s)[:max])
}

// shouldGenerate 判定是否需要生成：第 2 轮后、标题为空/占位/auto、且非 manual。
func shouldGenerate(title, source string, userCount int) bool {
	if source == titleSourceManual {
		return false
	}
	if userCount < 2 {
		return false
	}
	return title == "" || title == placeholderTitle || source == titleSourceAuto
}

// buildTitleInput 把对话前若干条拼成给 LLM 的 user 文本。
func buildTitleInput(msgs []repo.AgentMessage) string {
	var b strings.Builder
	for _, m := range msgs {
		if m.Role != "user" && m.Role != "assistant" {
			continue
		}
		if m.Kind != "" && m.Kind != "text" {
			continue // 跳过工具调用/结果/思考，只留纯对话文本
		}
		role := "用户"
		if m.Role == "assistant" {
			role = "助手"
		}
		content := m.Content
		if len([]rune(content)) > 200 {
			content = string([]rune(content)[:200]) // 单条限长，控 prompt 体积
		}
		fmt.Fprintf(&b, "%s：%s\n", role, content)
	}
	return b.String()
}

// ---- deps：把 repo 访问与 LLM 抽成可注入依赖，便于单测与生产装配 ----

// titleState 生成前会话的标题状态快照。
type titleState struct {
	title  string
	source string
}

// titleDeps 自动生成标题的依赖（测试替身可字段注入，生产用 newTitleDeps 接 repo）。
type titleDeps struct {
	state      titleState
	count      int
	msgs       []repo.AgentMessage
	llm        titleLLM
	model      string
	updatedTo  string // 测试断言：最终写入的标题
	updatedSrc string // 测试断言：最终写入的来源
}

// newTitleDeps 从 repo 构造生产依赖。llm 须是 titleLLM（生产传 llmAdapter{provider}）。
func newTitleDeps(ctx context.Context, uid int64, cid ids.ID, convs *repo.AgentConversationRepo,
	msgs *repo.AgentMessageRepo, llm titleLLM, model string) *titleDeps {
	d := &titleDeps{llm: llm, model: model}
	if t, s, err := convs.TitleState(ctx, uid, cid); err == nil {
		d.state = titleState{t, s}
	}
	if n, err := msgs.CountByConversation(ctx, uid, cid); err == nil {
		d.count = n
	}
	if list, err := msgs.ListByConversation(ctx, uid, cid); err == nil {
		d.msgs = list
	}
	return d
}

// generate 执行一次生成判定+生成。返回 (新标题, err)：errTitleSkip 表示跳过（静默），
// 其余 err 为 repo/LLM 真实错误（调用方也应静默，仅记日志）。成功返回已清洗标题。
func (d *titleDeps) generate(ctx context.Context) (string, error) {
	if !shouldGenerate(d.state.title, d.state.source, d.count) {
		return "", errTitleSkip
	}
	out, err := d.llm.Chat(ctx, titleChatReq{
		Model: d.model, System: titlePrompt, User: buildTitleInput(d.msgs), Temperature: 0.3,
	})
	if err != nil {
		return "", errTitleSkip // LLM 失败静默
	}
	title := sanitizeTitle(out)
	if title == "" {
		return "", errTitleSkip
	}
	d.updatedTo, d.updatedSrc = title, titleSourceAuto
	return title, nil
}

// GenerateTitle 生产入口：拉 repo 状态 → 判定 → 生成 → 写回 auto。任何非致命路径都静默
// （失败/跳过仅记日志）。ctx 脱离请求、带超时；内部读-判-写避免覆盖用户刚改的 manual。
// llm 是 provider.LLMProvider，内部适配成 titleLLM（title.go 不直接依赖 provider 之外的耦合）。
func GenerateTitle(ctx context.Context, uid int64, cid ids.ID, convs *repo.AgentConversationRepo,
	msgs *repo.AgentMessageRepo, llm provider.LLMProvider, model string) {
	d := newTitleDeps(ctx, uid, cid, convs, msgs, llmAdapter{llm}, model)
	title, err := d.generate(ctx)
	if err != nil { // errTitleSkip 或真实错误都静默
		if !errors.Is(err, errTitleSkip) {
			log.Printf("[agent] 自动生成标题失败(静默) conv=%s: %v", cid, err)
		}
		return
	}
	// CAS：写回前再读一次 source，若已被并发改成 manual 则放弃，绝不覆盖用户刚改的标题。
	if _, s, e := convs.TitleState(ctx, uid, cid); e == nil && s == titleSourceManual {
		return
	}
	if err := convs.UpdateTitle(ctx, uid, cid, title, titleSourceAuto); err != nil {
		log.Printf("[agent] 写回自动标题失败(静默) conv=%s: %v", cid, err)
	}
}
