package agent

import (
	"context"
	"fmt"
	"strings"
	"time"

	"zhiwei/internal/profile"
	"zhiwei/internal/repo"
	"zhiwei/internal/retrieve"
)

// ProfileContext 组装「发给 dsh 的对话上下文头」：把 owner「我」的概要 + 关键属性 + 当天日期
// 拼成一句自然语言背景，供 Orchestrator 每轮前置到发给 dsh 的文本（让 agent 天然「认识我」）。
//
// 关键约束（设计 D2）：它只影响「发给 dsh 的文本」，绝不改落库——持久化的 user 消息与流式回显
// 仍是用户的原始输入。ProfileContext 只读 Persons/Attributes，永不写库。它是 Orchestrator 的
// 可选依赖（nil → 不注入，退化为既有行为）。
//
// 本期是 spec §10 上下文头的轻量版：只放 owner 概要 + 当天日期，检索种子 / 最近摘要留后续。
type ProfileContext struct {
	Persons    *repo.PersonRepo
	Attributes *repo.PersonAttributeRepo
	// Retrieve 可选：非 nil 时按本轮 query 注入 top-k 相关记忆「种子」到上下文头（spec §10）。
	Retrieve *retrieve.Retriever
}

// headMaxAttrs 上下文头最多纳入的关键属性条数（防止头过长喧宾夺主、浪费 token）。
const headMaxAttrs = 6

// headPriorityKeys 优先纳入上下文头的高信号属性键；其余 active 属性按 ListByPerson 的自然
// 顺序补足到 headMaxAttrs 条。选这些是因为它们最能帮 agent 快速「认识我」（职业/城市/性格…）。
var headPriorityKeys = []string{"occupation", "industry", "city", "education", "personality", "birthday"}

// Head 组装对话上下文头（owner 概要 + 关键 active 属性 + 当天日期）。
//
// userID 指定「谁」的画像（2B-B：由 runTurn 传 conv.UserID，多用户隔离，绝不串用别人的 owner）。
// 无 owner（未 bootstrap）/ 读库失败 / 既无 summary 也无任何 active 属性 → 返回 ""；
// 调用方据此不加前缀（退化为原行为，绝不阻断对话）。
//
// now 由调用方传入（runTurn 传 time.Now()）：一是服务端本就可用系统时间，二是便于单测注入
// 固定日期做断言。
func (pc *ProfileContext) Head(ctx context.Context, userID int64, now time.Time) string {
	if pc == nil || pc.Persons == nil {
		return ""
	}
	owner, err := pc.Persons.GetOwner(ctx, userID)
	if err != nil || owner == nil {
		return "" // owner 未建立或读失败：不注入（降级，不因画像问题影响对话）
	}

	var parts []string
	// owner 概要（人物简介）：有则作为第一句背景。
	if owner.Summary != nil {
		if s := strings.TrimSpace(*owner.Summary); s != "" {
			parts = append(parts, s)
		}
	}
	// 关键 active 属性：优先键在前、其余补足到 headMaxAttrs 条，各格式化成「中文名：值」。
	if pc.Attributes != nil {
		if attrs, err := pc.Attributes.ListByPerson(ctx, owner.ID); err == nil {
			parts = append(parts, pickKeyAttrs(attrs)...)
		}
	}

	if len(parts) == 0 {
		return "" // 无任何可用背景：不注入
	}
	return fmt.Sprintf("今天是 %s。关于用户本人：%s（背景信息，自然运用，不必复述）",
		now.Format("2006-01-02"), strings.Join(parts, "；"))
}

// AssemblePersona 把可配置的 identity + soul 拼成「每轮注入的人设前言」：
// 二者都为空则返回 ""（不注入，退化为进程级 persona/DSH_SYSTEM_PROMPT）。作为注入头的第一段前置，
// 让 agent 每轮都据此人设扮演（不重启 dsh、编辑即时生效——因为随 session/prompt 的文本一起发过去）。
func AssemblePersona(identity, soul string) string {
	identity = strings.TrimSpace(identity)
	soul = strings.TrimSpace(soul)
	var b []string
	if identity != "" {
		b = append(b, "你的身份设定：\n"+identity)
	}
	if soul != "" {
		b = append(b, "你的性格与说话风格：\n"+soul)
	}
	if len(b) == 0 {
		return ""
	}
	return strings.Join(b, "\n\n")
}

// Seeds 按本轮 query 召回 top-k 相关记忆，拼成上下文头的「相关记忆」块；
// 无 Retrieve / query 空 / 无命中 → ""。每轮一次 query 向量化（未配 embedder 时 Retrieve=nil 不触发）。
// userID 指定「谁」的记忆（2B-B：由 runTurn 传 conv.UserID，多用户隔离，绝不召回别人的记忆）。
func (pc *ProfileContext) Seeds(ctx context.Context, userID int64, query string) string {
	if pc == nil || pc.Retrieve == nil || strings.TrimSpace(query) == "" {
		return ""
	}
	ms, err := pc.Retrieve.Search(ctx, userID, query, "", 0) // limit=0 → Retriever.TopK
	if err != nil || len(ms) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("可能相关的我的记忆（供参考，不必逐条复述）：")
	for _, m := range ms {
		b.WriteString("\n- " + m.Title)
	}
	return b.String()
}

// pickKeyAttrs 从 owner 的属性行里挑出「关键 active 属性」并格式化成「中文名：值」短语：
// 先按 headPriorityKeys 的优先顺序纳入，再用其余 active 属性（按传入的自然顺序）补足，
// 最多 headMaxAttrs 条。只取 active（pending/superseded/dismissed 不进上下文头）。
func pickKeyAttrs(attrs []repo.PersonAttribute) []string {
	out := make([]string, 0, headMaxAttrs)
	used := make(map[int]bool) // 已纳入的行下标，避免第二趟重复纳入优先键的行
	add := func(i int) {
		if len(out) >= headMaxAttrs || used[i] {
			return
		}
		a := attrs[i]
		out = append(out, profile.Def(a.AttrKey).Label+"："+a.ValueText)
		used[i] = true
	}
	// 第一趟：优先键（按 headPriorityKeys 的顺序，而非属性行顺序）。
	for _, pk := range headPriorityKeys {
		for i := range attrs {
			if attrs[i].Status == "active" && attrs[i].AttrKey == pk {
				add(i)
			}
		}
	}
	// 第二趟：其余 active 属性（按 ListByPerson 的自然顺序），补足到上限。
	for i := range attrs {
		if attrs[i].Status == "active" {
			add(i)
		}
	}
	return out
}
