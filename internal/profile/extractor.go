package profile

import (
	"context"
	"fmt"
	"strings"

	"zhiwei/internal/ids"
	"zhiwei/internal/memory"
	"zhiwei/internal/provider"
)

// ExtractStats 是一次 Extract 的调用统计（写 job.trace 用）。
type ExtractStats struct {
	Windows int // LLM 调用次数（窗口数）
	Tokens  int // 累计 token 用量
}

// PersonRef 是进入抽取 prompt 的已知人物名单（稳定引用，避免 LLM 每次发明新指代）。
type PersonRef struct {
	ID      ids.ID
	Name    string
	Aliases string // 逗号分隔别名（P1 空；aliases 属性扩展后续接）
	IsOwner bool
}

// Extractor 用 LLM 从对话块抽取画像事实：按窗口逐次调用、合并去重、回填溯源。
// 结构与 memory.Extractor 对齐（同一套窗口切分与 provenance 思路）。
type Extractor struct {
	LLM    provider.LLMProvider
	Model  string // 模型名（Tier 1 flash）
	Prompt string // prompts/profile_extraction_v2.md 内容
	Window int    // 窗口大小（块数），<=0 时 memory.SplitWindows 内部回退默认 10

	// stats 记录最近一次 Extract 的统计（每个 stage 各自 new 一个，无并发共享）。
	stats ExtractStats
}

func (e *Extractor) Stats() ExtractStats { return e.stats }

// Extract 抽取全部对话块。跨窗口同自然键（见 factKey：平面+主体身份+内容+关系对端身份）
// 的重复视为同一事实，保留置信度高者。
func (e *Extractor) Extract(ctx context.Context, blocks []memory.Block, persons []PersonRef) ([]Fact, error) {
	e.stats = ExtractStats{}
	var all []Fact
	seen := map[string]int{} // 自然键 -> 在 all 中的下标
	for winIdx, win := range memory.SplitWindows(blocks, e.Window) {
		resp, err := e.LLM.Chat(ctx, provider.ChatRequest{
			Model:  e.Model,
			System: e.Prompt,
			User:   buildProfileUserMessage(win, persons),
		})
		if err != nil {
			return nil, fmt.Errorf("LLM 调用: %w", err)
		}
		e.stats.Windows++
		e.stats.Tokens += resp.TotalTokens
		facts, err := ParseFacts(resp.Content)
		if err != nil {
			return nil, fmt.Errorf("第 %d 窗口解析失败: %w", winIdx+1, err)
		}
		for _, f := range facts {
			f.SegmentIDs = factProvenance(win, f.BlockIndex)
			key := factKey(f)
			if idx, ok := seen[key]; ok {
				if f.Confidence > all[idx].Confidence {
					all[idx] = f
				}
				continue
			}
			seen[key] = len(all)
			all = append(all, f)
		}
	}
	return all, nil
}

// factProvenance 由 block_index 定位来源块；越界（0 或超范围）用整个窗口的
// segment 并集兜底（宁粗勿丢，同 memory.blockProvenance 思路）。
func factProvenance(win []memory.Block, idx int) []ids.ID {
	if idx >= 1 && idx <= len(win) {
		return win[idx-1].SegmentIDs
	}
	var segs []ids.ID
	for _, b := range win {
		segs = append(segs, b.SegmentIDs...)
	}
	return segs
}

// factKey 批内去重自然键：平面 + 主体身份(kind/name/relation) + 内容(attr_key/value)
// + 关系类型 + 关系对端身份(kind/name/relation) + 事件判别(event_type/title)。
//
// 主体与对端的 Relation 字段必须纳入：kind=relation 的指代其身份藏在 Relation 里
// （「我老婆」→ Relation=配偶，Name 为空）。若只取 Kind+Name，「我老婆是老师」与
// 「我妈是老师」的键都会退化成 attribute|relation||occupation|老师 而被误判为同一条、
// 静默塌缩——这比下游 DB 自然键（含 resolveSubject 解析后的 person_id，两个不同人）
// 更激进，去重方向反了。故补 Subject.Relation / Related.Kind / Related.Relation 三个判别字段。
//
// event 平面同理：主体多为 self、attr/relation 字段全空，若不纳入 event_type/title
// 判别，「旅行·去云南」与「聚会·同学会」两条都会塌缩成 event|self||... 被误判同一条。
// 故末尾追加 EventType/EventTitle 两个判别字段（防批内塌缩：同 key 不同事件）。
func factKey(f Fact) string {
	return f.Plane + "\x00" +
		f.Subject.Kind + "\x00" + f.Subject.Name + "\x00" + f.Subject.Relation + "\x00" +
		f.AttrKey + "\x00" + f.Value + "\x00" +
		f.RelationType + "\x00" +
		f.Related.Kind + "\x00" + f.Related.Name + "\x00" + f.Related.Relation + "\x00" +
		f.EventType + "\x00" + f.EventTitle
}

// buildProfileUserMessage 组装用户消息：对话块 + 已知人物名单。
func buildProfileUserMessage(win []memory.Block, persons []PersonRef) string {
	var sb strings.Builder
	sb.WriteString("对话块列表（格式：序号|说话人|文本）：\n")
	for i, b := range win {
		speaker := b.SpeakerLabel
		if speaker == "" {
			speaker = "未知"
		}
		fmt.Fprintf(&sb, "%d|%s|%s\n", i+1, speaker, b.Text)
	}
	sb.WriteString("\n已知人物列表（格式：person_id|名字|备注），subject 请优先引用已知人物：\n")
	if len(persons) == 0 {
		sb.WriteString("（暂无）\n")
	}
	for _, p := range persons {
		note := ""
		if p.IsOwner {
			note = "（用户本人，subject 用 {\"kind\":\"self\"}）"
		}
		fmt.Fprintf(&sb, "%s|%s|%s\n", p.ID.String(), p.Name, note)
	}
	return sb.String()
}
