package profile

import (
	"encoding/json"
	"fmt"
	"strings"

	"zhiwei/internal/ids"
)

// Subject 是 LLM 对「这条信息属于谁」的指代（Go 侧做确定性解析，见 service.go resolveSubject）：
//
//	self=第一人称「我」 | speaker:Name=说话人 | mentioned:Name=对话里提到的具名他人
//	relation:TYPE=关系指代（如「我老婆」→ TYPE=配偶）
type Subject struct {
	Kind     string `json:"kind"`     // self|speaker|mentioned|relation
	Name     string `json:"name"`     // speaker/mentioned 时的名字
	Relation string `json:"relation"` // kind=relation 时的关系类型（如 配偶）
}

// Fact 是 LLM 输出的一条画像事实（闸门前后通用载体）。四个平面：
// attribute（属性）/ relationship（关系）/ event（大事记）/ metric（时序个人指标）。
type Fact struct {
	Plane   string  // attribute|relationship|event|metric
	Subject Subject // 信息归属的人物指代

	// ---- attribute 平面 ----
	AttrKey   string // 目录 key（落库以目录校验，未知 key 仍可用归「其他」）
	Value     string
	ValueType string // LLM 给的类型提示（仅供参考，落库以目录为准）

	// ---- relationship 平面 ----
	RelationType string  // 关系类型枚举
	Related      Subject // 关系对端人物指代
	Direction    string  // upstream|downstream|peer（上下游）
	OrgName      string  // 组织名（组织关系）
	Label        string  // 自由称呼（「大儿子」）

	// ---- event 平面（P2 大事记）----
	EventType        string // 里程碑|聚会|会议|旅行|健康|成就|挫折|负面|其他
	EventTitle       string
	EventDescription string
	OccurredAt       string // 原始字符串（YYYY-MM-DD / YYYY-MM / RFC3339），时间解析放 service 层（parseEventAt）
	EndAt            string
	EventLocation    string

	// ---- metric 平面（P3 时序个人指标）----
	// 注意：value_text 字段名刻意用 MetricValueText，与 attribute 平面的 Value 区分
	//（两者语义不同：Value 是属性值，MetricValueText 是测点的类别描述读数）。
	MetricKey       string   // 指标键（emotion|weight|sleep|mood_energy|diet|health，见 MetricCatalog）
	ValueNum        *float64 // 数值读数（可空；Numeric 指标必填，曲线只画非空者）
	MetricValueText string   // 类别描述读数（可空；非 Numeric 指标必填）
	Unit            string   // 单位（可空；空时落库回退 MetricDefOf(key).Unit）
	MeasuredAt      string   // 原始测点时间字符串（RFC3339/"2006-01-02 15:04"/"2006-01-02"），解析放 service 层（parseMetricAt，保留时刻）

	// ---- 通用 ----
	Confidence    float64
	EpistemicType string // observed|inferred|predicted|suggested
	BlockIndex    int    // 来源对话块序号（1-based，0=未知）

	// 编排层填充（LLM 不产出）
	SegmentIDs []ids.ID // provenance：来源块的 segment id
}

var validPlanes = map[string]bool{"attribute": true, "relationship": true, "event": true, "metric": true}

// validSubjectKinds 是人物指代 Subject.Kind（也用于 Related.Kind）的合法取值。
// 非法或缺失的指代无法归属到具体人物，直接丢弃该条（宁少勿错）。
var validSubjectKinds = map[string]bool{"self": true, "speaker": true, "mentioned": true, "relation": true}

var validEpistemic = map[string]bool{
	"observed": true, "inferred": true, "predicted": true, "suggested": true,
}

// ValidRelations 关系类型枚举（与迁移注释一致）。
var ValidRelations = map[string]bool{
	"配偶": true, "子女": true, "父母": true, "兄弟姐妹": true, "亲戚": true,
	"朋友": true, "同事": true, "领导": true, "下属": true,
	"客户": true, "供应商": true, "合作方": true, "组织": true, "其他": true,
}

var validDirections = map[string]bool{"upstream": true, "downstream": true, "peer": true, "": true}

// ValidEventTypes 事件类型枚举（spec §4.4：开放分类，解析层收敛为 9 类）。
var ValidEventTypes = map[string]bool{
	"里程碑": true, "聚会": true, "会议": true, "旅行": true, "健康": true,
	"成就": true, "挫折": true, "负面": true, "其他": true,
}

type rawSubject struct {
	Kind     string `json:"kind"`
	Name     string `json:"name"`
	Relation string `json:"relation"`
}

type rawFact struct {
	Plane            string     `json:"plane"`
	Subject          rawSubject `json:"subject"`
	AttrKey          string     `json:"attr_key"`
	Value            string     `json:"value"`
	ValueType        string     `json:"value_type"`
	RelationType     string     `json:"relation_type"`
	Related          rawSubject `json:"related"`
	Direction        string     `json:"direction"`
	OrgName          string     `json:"org_name"`
	Label            string     `json:"label"`
	EventType        string     `json:"event_type"`
	EventTitle       string     `json:"title"`
	EventDescription string     `json:"description"`
	OccurredAt       string     `json:"occurred_at"`
	EndAt            string     `json:"end_at"`
	EventLocation    string     `json:"location"`
	// ---- metric 平面 ----（value_text 为 metric 专用 json key，与 attribute 的 value、event 的 title 不冲突）
	MetricKey       string   `json:"metric_key"`
	ValueNum        *float64 `json:"value_num"`
	MetricValueText string   `json:"value_text"`
	Unit            string   `json:"unit"`
	MeasuredAt      string   `json:"measured_at"`

	Confidence    float64 `json:"confidence"`
	EpistemicType string  `json:"epistemic_type"`
	BlockIndex    int     `json:"block_index"`
}

// ParseFacts 解析 LLM 输出。容错风格同 memory.ParseCandidates：截取首个 { 到末个 }，
// 天然剥掉前后废话与 markdown 围栏；彻底非法 JSON 返回 error（stage 走重试）。
// 条目级问题（非法 plane/枚举/空字段）直接丢弃该条——宁少勿错，不整体失败。
// epistemic_type 缺省视为 observed（画像事实多为对话直陈）。
func ParseFacts(raw string) ([]Fact, error) {
	s := strings.TrimSpace(raw)
	if i := strings.Index(s, "{"); i >= 0 {
		if j := strings.LastIndex(s, "}"); j > i {
			s = s[i : j+1]
		}
	}
	var out struct {
		Facts []rawFact `json:"facts"`
	}
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		return nil, fmt.Errorf("画像抽取结果解析失败: %w", err)
	}
	facts := make([]Fact, 0, len(out.Facts))
	for _, rf := range out.Facts {
		f := Fact{
			Plane:            rf.Plane,
			Subject:          trimSubject(rf.Subject),
			AttrKey:          strings.TrimSpace(rf.AttrKey),
			Value:            strings.TrimSpace(rf.Value),
			ValueType:        strings.TrimSpace(rf.ValueType),
			RelationType:     strings.TrimSpace(rf.RelationType),
			Related:          trimSubject(rf.Related),
			Direction:        strings.TrimSpace(rf.Direction),
			OrgName:          strings.TrimSpace(rf.OrgName),
			Label:            strings.TrimSpace(rf.Label),
			EventType:        strings.TrimSpace(rf.EventType),
			EventTitle:       strings.TrimSpace(rf.EventTitle),
			EventDescription: strings.TrimSpace(rf.EventDescription),
			OccurredAt:       strings.TrimSpace(rf.OccurredAt),
			EndAt:            strings.TrimSpace(rf.EndAt),
			EventLocation:    strings.TrimSpace(rf.EventLocation),
			MetricKey:        strings.TrimSpace(rf.MetricKey),
			ValueNum:         rf.ValueNum,
			MetricValueText:  strings.TrimSpace(rf.MetricValueText),
			Unit:             strings.TrimSpace(rf.Unit),
			MeasuredAt:       strings.TrimSpace(rf.MeasuredAt),
			Confidence:       clamp01(rf.Confidence),
			EpistemicType:    strings.TrimSpace(rf.EpistemicType),
			BlockIndex:       rf.BlockIndex,
		}
		if !validPlanes[f.Plane] || !validDirections[f.Direction] {
			continue
		}
		if !validSubjectKinds[f.Subject.Kind] {
			continue // 主体指代非法/缺失：无法归属，宁少勿错直接丢
		}
		if f.EpistemicType == "" {
			f.EpistemicType = "observed"
		}
		if !validEpistemic[f.EpistemicType] {
			continue
		}
		switch f.Plane {
		case "attribute":
			if f.AttrKey == "" || f.Value == "" {
				continue
			}
		case "relationship":
			// 关系对端也须是合法人物指代（成员校验，已含非空判断）
			if !ValidRelations[f.RelationType] || !validSubjectKinds[f.Related.Kind] {
				continue
			}
		case "event":
			// related 为可选增强信息，service 层解析容错（解析不到存空），此处不校验——与 relationship 平面强制校验 Related.Kind 不同。
			if !ValidEventTypes[f.EventType] || f.EventTitle == "" {
				continue // 非法事件类型或空标题：无法落库
			}
		case "metric":
			// 指标键必须在目录内（时序曲线的键须收敛，目录外一律丢）。
			if !ValidMetricKey(f.MetricKey) {
				continue
			}
			// 载荷校验（硬约束 6：Numeric 键要有 value_num，曲线才能画）：
			//   Numeric 指标 → 必须给 value_num；非 Numeric 指标 → 必须给 value_text。
			// measured_at 允许空——service 层 applyMetricFact 用 fallback 回退（保证 NOT NULL）。
			if MetricDefOf(f.MetricKey).Numeric {
				if f.ValueNum == nil {
					continue
				}
			} else if f.MetricValueText == "" {
				continue
			}
		}
		facts = append(facts, f)
	}
	return facts, nil
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

// trimSubject 对 Subject 的每个子字段做 TrimSpace。Kind/Relation 要参与枚举校验，
// Name 要用于后续人物归属解析（见 service.go resolveSubject）——带空格的 " Alice "
// 必须先归一化成 "Alice"，否则匹配不上同一人物。Subject 与 Related 复用同一处理。
func trimSubject(s rawSubject) Subject {
	return Subject{
		Kind:     strings.TrimSpace(s.Kind),
		Name:     strings.TrimSpace(s.Name),
		Relation: strings.TrimSpace(s.Relation),
	}
}
