package memory

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"zhiwei/internal/ids"
)

// Candidate 是 LLM 输出的一条记忆候选（闸门前后的载体）。
type Candidate struct {
	Type               string // event|fact|decision|idea|problem|preference
	Title              string
	Content            string
	EpistemicType      string // observed|inferred|suggested
	Importance         float64
	Confidence         float64
	IsTodo             bool
	TodoDue            *time.Time
	TopicID            *ids.ID // LLM 直接给的已有 topic 归属
	SuggestedTopicName string  // LLM 建议的新主题名

	// 以下由编排层填充（LLM 不产出）
	BlockIndex int       // 候选来源块在窗口内的序号（1-based，0=未知）
	SegmentIDs []ids.ID  // provenance：来源块的 segment id 列表
	EventAt    time.Time // 近似事件时间 = 会话基准 + 块起点偏移
	TodoStatus string    // suggested|confirmed；非 todo 为空（闸门填充）
}

var validTypes = map[string]bool{
	"event": true, "fact": true, "decision": true,
	"idea": true, "problem": true, "preference": true,
}

var validEpistemic = map[string]bool{
	"observed": true, "inferred": true, "suggested": true,
}

type rawCandidate struct {
	Type               string  `json:"type"`
	Title              string  `json:"title"`
	Content            string  `json:"content"`
	EpistemicType      string  `json:"epistemic_type"`
	Importance         float64 `json:"importance"`
	Confidence         float64 `json:"confidence"`
	IsTodo             bool    `json:"is_todo"`
	TodoDue            string  `json:"todo_due"`
	TopicID            *string `json:"topic_id"`
	SuggestedTopicName string  `json:"suggested_topic_name"`
	BlockIndex         int     `json:"block_index"`
}

// ParseCandidates 解析 LLM 输出为候选列表。容错：截取首个 { 到末个 }，
// 天然剥掉模型可能输出的前后废话与 markdown 代码围栏。
// 彻底非法的 JSON 返回 error（由 stage 走重试）；
// 字段级问题（非法时间等）降级处理不整体失败。
func ParseCandidates(raw string) ([]Candidate, error) {
	s := strings.TrimSpace(raw)
	if i := strings.Index(s, "{"); i >= 0 {
		if j := strings.LastIndex(s, "}"); j > i {
			s = s[i : j+1]
		}
	}
	var out struct {
		Candidates []rawCandidate `json:"candidates"`
	}
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		return nil, fmt.Errorf("抽取结果解析失败: %w", err)
	}
	cands := make([]Candidate, 0, len(out.Candidates))
	for _, rc := range out.Candidates {
		c := Candidate{
			Type:               rc.Type,
			Title:              rc.Title,
			Content:            rc.Content,
			EpistemicType:      rc.EpistemicType,
			Importance:         clamp01(rc.Importance),
			Confidence:         clamp01(rc.Confidence),
			IsTodo:             rc.IsTodo,
			SuggestedTopicName: strings.TrimSpace(rc.SuggestedTopicName),
			BlockIndex:         rc.BlockIndex,
		}
		if rc.TodoDue != "" {
			if du, err := time.Parse(time.RFC3339, rc.TodoDue); err == nil {
				c.TodoDue = &du
			} // 非法时间：置空保留候选
		}
		if rc.TopicID != nil {
			if id, err := ids.ParseID(*rc.TopicID); err == nil {
				c.TopicID = &id
			} // 非法 id：视为无归属
		}
		cands = append(cands, c)
	}
	return cands, nil
}

// GateConfig 是质量闸门阈值（来自配置）。
type GateConfig struct {
	MinConf  float64 // 候选最低置信度，低于丢弃
	TodoConf float64 // todo 直接入库为 confirmed 的阈值，低于降级 suggested
}

// ApplyGate 应用质量闸门：枚举外类型、置信度不足、内容过短的候选丢弃；
// todo 按置信度决定 suggested/confirmed。返回通过者。
func ApplyGate(cands []Candidate, cfg GateConfig) []Candidate {
	out := make([]Candidate, 0, len(cands))
	for _, c := range cands {
		if !validTypes[c.Type] || !validEpistemic[c.EpistemicType] {
			continue
		}
		if c.Confidence < cfg.MinConf {
			continue
		}
		if len([]rune(c.Content)) < 8 {
			continue
		}
		if c.IsTodo {
			if c.Confidence >= cfg.TodoConf {
				c.TodoStatus = "confirmed"
			} else {
				c.TodoStatus = "suggested"
			}
		}
		out = append(out, c)
	}
	return out
}

func clamp01(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}
