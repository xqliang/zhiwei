package profile

import (
	"context"
	"fmt"
	"strconv"
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
	Prompt string // prompts/profile_extraction_v3.md 内容
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

// factKey 批内去重自然键：**按平面镜像各自的 DB 自然键**（各平面只取该平面的判别字段），
// 与 ParseFacts 的 plane switch 对称演进——新增平面须同时加两处 case（此处 + ParseFacts）。
//
// 为何按平面而非统一全字段 join：统一 join 会不断追加字段，某个字段选错就静默塌缩或
// 过度区分（cycle 曾误纳 AnchorDate，与 DB 自然键 (session,person,type,label) 不一致——
// 同 type/label 不同 anchor 的两条在此不塌缩、却在 Service 单事务里被 DB 自然键 dedup 成
// 「先到的赢」而非「高置信赢」）。按平面镜像 DB 自然键根除此类漂移。各平面的键：
//
//	attribute    : subject + attr_key + value
//	relationship : subject + related(subject) + relation_type
//	event        : subject + event_type + title
//	metric       : subject + metric_key + metric_value + measured_at（测点流：同键多采样各自成行）
//	cycle        : subject + cycle_type + cycle_label（镜像 DB 自然键，**不含 anchor**）
//	activity     : subject + activity + tool + location + commute_mode + started_at + duration_min（测点流：同活动不同时刻各自成行）
//
// subjectKey 纳入 Kind/Name/Relation 三段：kind=relation 的指代身份藏在 Relation 里
// （「我老婆」→ Relation=配偶，Name 空）。若只取 Kind+Name，「我老婆是老师」与「我妈是老师」
// 会塌缩成同键——比下游 DB 自然键（resolveSubject 解析后是两个不同 person_id）更激进，方向反了。
//
// default 兜底：未来新增平面若漏写 case，用全字段 join 而非塌缩成某个已知平面的键——
// 宁可过度区分（漏去重）也不静默误判两条不同事实为同一条（错误模式更安全、更易发现）。
func factKey(f Fact) string {
	subj := subjectKey(f.Subject)
	switch f.Plane {
	case "attribute":
		return "attribute\x00" + subj + "\x00" + f.AttrKey + "\x00" + f.Value
	case "relationship":
		return "relationship\x00" + subj + "\x00" + subjectKey(f.Related) + "\x00" + f.RelationType
	case "event":
		return "event\x00" + subj + "\x00" + f.EventType + "\x00" + f.EventTitle
	case "metric":
		return "metric\x00" + subj + "\x00" + f.MetricKey + "\x00" + f.MetricValue + "\x00" + f.MeasuredAt
	case "cycle":
		return "cycle\x00" + subj + "\x00" + f.CycleType + "\x00" + f.CycleLabel
	case "activity":
		return "activity\x00" + subj + "\x00" + f.ActivityText + "\x00" + f.Tool + "\x00" +
			f.Location + "\x00" + f.CommuteMode + "\x00" + f.StartedAt + "\x00" + strconv.Itoa(f.DurationMin)
	default:
		return f.Plane + "\x00" + subj + "\x00" + f.AttrKey + f.Value + f.RelationType + f.Related.Name +
			f.EventType + f.EventTitle + f.MetricKey + f.MetricValue + f.CycleType + f.CycleLabel
	}
}

// subjectKey Subject 的去重序列化（Subject 与 Related 共用）：Kind/Name/Relation 三段
// 均纳入——见 factKey 顶注释对 kind=relation 指代的说明。
func subjectKey(s Subject) string {
	return s.Kind + "\x00" + s.Name + "\x00" + s.Relation
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
