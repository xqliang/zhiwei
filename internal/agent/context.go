package agent

import (
	"context"
	"fmt"
	"regexp"
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
	// 注意：日期不再由此处输出，改由 DateTimeHead 无条件统一注入（避免 owner 存在时日期重复）。
	return fmt.Sprintf("关于用户本人：%s（背景信息，自然运用，不必复述）",
		strings.Join(parts, "；"))
}

// DateTimeHead 组装「当前日期 + 时区」背景句：无条件每轮注入（不依赖 owner 画像），
// 让 agent 始终知道「今天几号 / 什么时区」。now 由调用方传 time.Now()（服务端本就可用系统
// 时间；单测经该参数注入固定时刻做断言）。时区取服务端本地时区：now.Zone() 给缩写（如 CST），
// now.Format("-07:00") 给 UTC 偏移（如 +08:00）。
func DateTimeHead(now time.Time) string {
	zone, _ := now.Zone() // 时区缩写，如 CST / UTC（第二个返回值是偏移秒数，这里用格式化取 ±hh:mm）
	return fmt.Sprintf("今天是 %s（%s, UTC%s）。",
		now.Format("2006-01-02"), zone, now.Format("-07:00"))
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

// personalSignal 命中「问题关于用户本人」的信号；仅当 query 命中时才跑召回+注入种子。
// 口径与 system prompt 的「关于用户本人的问题」保持一致（prompt 示例：「张三是谁」
// 「上周录音里聊了什么」）：除人称代词外，也认用户数据域名词（录音/待办/话题/时间线/
// 日程/画像/指标/情绪等）与「是谁」人物指代——它们不含「我」字但明确指向用户数据。
// 常识/名词解释/一般知识题（如「ASL 是什么」「猫的习性」）不含这些词 → 不注入，
// 从源头避免「啥都跟你数据有关」的误导，也顺带省一次 embedding 调用。
// 注：域名词偶有误伤（如「衡量学习的指标」），代价仅一次多余的 embedding 调用 +
// 中性措辞的种子块（模型被明示「不相关请忽略」），可接受；不为此上模型分类。
var personalSignal = regexp.MustCompile(`我|咱|自己|本人|是谁|录音|聊了|聊过|聊天记录|待办|话题|时间线|日程|画像|指标|情绪|心情`)

// Seeds 按本轮 query 召回 top-k 相关记忆，拼成上下文头的「相关记忆」块。
// 门控：仅当 query 命中个人信号（见 personalSignal：人称代词 + 用户数据域名词 + 「是谁」）
// 时才召回——常识/名词解释题不注入，避免「啥都跟你数据有关」的误导，也省一次 embedding 调用。
// 无 Retrieve / query 空 / query 无个人信号 / 无命中 → ""。每轮一次 query 向量化（未配 embedder 时 Retrieve=nil 不触发）。
// userID 指定「谁」的记忆（2B-B：由 runTurn 传 conv.UserID，多用户隔离，绝不召回别人的记忆）。
func (pc *ProfileContext) Seeds(ctx context.Context, userID int64, query string) string {
	if pc == nil || pc.Retrieve == nil || strings.TrimSpace(query) == "" {
		return ""
	}
	// 个人信号门控：仅当 query 关于用户本人时才召回；否则不注入。
	if !personalSignal.MatchString(query) {
		return ""
	}
	ms, err := pc.Retrieve.Search(ctx, userID, query, "", 0) // limit=0 → Retriever.TopK
	if err != nil || len(ms) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("与该问题可能相关的背景记忆（仅供参考，不相关请忽略）：")
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
