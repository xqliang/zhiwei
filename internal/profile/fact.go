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

// Fact 是 LLM 输出的一条画像事实（闸门前后通用载体）。P1-P4 六个平面：
// attribute（属性）/ relationship（关系）/ event（大事记）/ metric（时序指标）/ cycle（周期日程）/ activity（生活轨迹）。
type Fact struct {
	Plane   string  // attribute|relationship|event|metric|cycle|activity
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

	// ---- metric 平面（P3 时序指标）----
	MetricKey   string // emotion|state|weight|sleep_late|diet|health
	MetricValue string // LLM 原始值（数值或类别），Go 侧分流 value_num/value_text
	MetricUnit  string
	MeasuredAt  string // 原始日期串；空则 service 落 session 时间

	// ---- cycle 平面（P3 周期/日程，敏感）----
	CycleType     string // menstrual|medication|injection|followup
	CycleLabel    string
	AnchorDate    string // YYYY-MM-DD 原始串
	PeriodDays    int
	DurationDays  int
	Dosage        string
	FrequencyText string // 频次（'每日两次'）；rawFact 的 json 标签是 frequency（非 frequency_text），对齐 prompt 契约

	// ---- activity 平面（P4 生活轨迹）----
	ActivityText string // 做什么（开会/写代码/打球…）；Go 字段名 ActivityText，rawFact 的 json 标签是 activity（同 FrequencyText/frequency 桥接先例）
	Tool         string // 什么工具（手机/电脑/健身房…）
	Location     string // 与 event 平面 EventLocation 同风格的自由文本；由 rawFact 的 json:"location" 填充（与 EventLocation 同源——一条 fact 只有一个 plane，event/activity 互斥不污染，见 rawFact 处的重复标签告警注释）
	CommuteMode  string // 通勤方式中文短串（地铁/开车/步行…；不做枚举强校验）
	StartedAt    string // 原始日期串（YYYY-MM-DD/RFC3339），解析在 service 层 parseEventAt
	DurationMin  int    // 持续分钟

	// ---- 通用 ----
	Confidence    float64
	EpistemicType string // observed|inferred|predicted|suggested
	BlockIndex    int    // 来源对话块序号（1-based，0=未知）

	// 编排层填充（LLM 不产出）
	SegmentIDs []ids.ID // provenance：来源块的 segment id
}

var validPlanes = map[string]bool{"attribute": true, "relationship": true, "event": true, "metric": true, "cycle": true, "activity": true}

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

// ValidMetricKeys 指标 key 枚举（spec §4.5：时序测点流的 6 种）。
var ValidMetricKeys = map[string]bool{
	"emotion": true, "state": true, "weight": true,
	"sleep_late": true, "diet": true, "health": true,
}

// ValidCycleTypes 周期类型枚举（spec §4.6：敏感周期/日程的 4 种）。
var ValidCycleTypes = map[string]bool{
	"menstrual": true, "medication": true, "injection": true, "followup": true,
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
	MetricKey        string     `json:"metric_key"`
	MetricValue      string     `json:"metric_value"`
	MetricUnit       string     `json:"metric_unit"`
	MeasuredAt       string     `json:"measured_at"`
	CycleType        string     `json:"cycle_type"`
	CycleLabel       string     `json:"cycle_label"`
	AnchorDate       string     `json:"anchor_date"`
	PeriodDays       int        `json:"period_days"`
	DurationDays     int        `json:"duration_days"`
	Dosage           string     `json:"dosage"`
	FrequencyText    string     `json:"frequency"` // Go 字段 FrequencyText，json 标签 frequency——对齐 prompt v3 契约
	ActivityText     string     `json:"activity"`  // Go 字段 ActivityText，json 标签 activity——对齐 prompt 契约（同 FrequencyText/frequency 先例）
	Tool             string     `json:"tool"`
	// 注意 activity 的 location 复用上面 EventLocation 的 json:"location"——同一 json 键不能有两个 Go
	// 字段（重复标签会让二者都被 encoding/json 忽略）；一条 fact 非 event 即 activity，ParseFacts 里
	// activity 平面的 Location 从 rf.EventLocation 取值即可，故此处不再单列 Location 字段。
	CommuteMode   string  `json:"commute_mode"`
	StartedAt     string  `json:"started_at"`
	DurationMin   int     `json:"duration_min"`
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
			MetricValue:      strings.TrimSpace(rf.MetricValue),
			MetricUnit:       strings.TrimSpace(rf.MetricUnit),
			MeasuredAt:       strings.TrimSpace(rf.MeasuredAt),
			CycleType:        strings.TrimSpace(rf.CycleType),
			CycleLabel:       strings.TrimSpace(rf.CycleLabel),
			AnchorDate:       strings.TrimSpace(rf.AnchorDate),
			PeriodDays:       rf.PeriodDays,   // int，不 trim
			DurationDays:     rf.DurationDays, // int，不 trim
			Dosage:           strings.TrimSpace(rf.Dosage),
			FrequencyText:    strings.TrimSpace(rf.FrequencyText),
			ActivityText:     strings.TrimSpace(rf.ActivityText),
			Tool:             strings.TrimSpace(rf.Tool),
			Location:         strings.TrimSpace(rf.EventLocation), // activity 复用 json:"location"（见 rawFact 注释）
			CommuteMode:      strings.TrimSpace(rf.CommuteMode),
			StartedAt:        strings.TrimSpace(rf.StartedAt),
			DurationMin:      rf.DurationMin, // int，不 trim
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
			// 时序测点：非法指标 key 或空值无法落库；measured_at 可空（service 落 session 时间）。
			if !ValidMetricKeys[f.MetricKey] || f.MetricValue == "" {
				continue
			}
		case "cycle":
			// 周期记录：仅强制合法 cycle_type；label/anchor 可空（如纯随访、生理期无药名）。
			if !ValidCycleTypes[f.CycleType] {
				continue
			}
		case "activity":
			// 活动流：仅强制 activity 非空（started_at 可空，service 落 session 时间；tool 等
			// 全可空——「下午去游泳了」没说工具地点也是有效活动）。
			if f.ActivityText == "" {
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
