package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"zhiwei/internal/profile"
	"zhiwei/internal/repo"
)

// registerProfileTools 注册画像（人物系统）相关的 MCP 工具（P2）：
//   - 读：get_profile（读我）、get_person（按名读某人）——只读，返回 JSON 画像。
//   - 写-提议：propose_profile_attr（改属性）、propose_profile_event（记大事记）、
//     propose_profile_relationship（加人物/组织关系）——绝不直接写画像，只 Create 一条 pending
//     agent_proposal（{old?,new}），用户经确认端点确认后才在单事务内经 profile.Service 的
//     Ext 变体落库（apply-once，见 proposals.go）。
//
// 全部限某一 userID（由 NewMCPServer 注入、透传给各 handler 工厂；2B-A 起替代写死的 toolUserID，
// 当前装配仍传 1），写目标恒为该用户的 owner「我」。「owner 概要注入对话头」由
// ProfileContext.Head 承担（见 context.go / orchestrator.go），不在此工具层。
func registerProfileTools(s *mcp.Server, d MCPDeps, userID int64) {
	// ---- 读工具 ----
	mcp.AddTool(s, &mcp.Tool{
		Name:        "get_profile",
		Description: "读取我（画像 owner「我」）的个人画像：显示名、简介、属性列表(键/值/认知类型/状态) 与大事记(类型/标题/时间/状态)。无参。owner 尚未建立时返回空画像({found:false})。",
	}, getProfileHandler(d, userID))

	mcp.AddTool(s, &mcp.Tool{
		Name:        "get_person",
		Description: "按姓名读取某个人物的画像(属性 + 大事记)。入参 name(精确匹配显示名)。找不到返回 {found:false}。",
	}, getPersonHandler(d, userID))

	mcp.AddTool(s, &mcp.Tool{
		Name:        "get_metrics",
		Description: "读取我的时序个人指标(第 5 平面 person_metric)：情绪/体重/睡眠/精力/饮食/健康/身高/腰围/胸围/臀围/体脂率等测点序列。可选 metric_key 过滤(emotion|weight|sleep|mood_energy|diet|health|height|waist|chest|hip|body_fat)，留空返回全部。按指标键分组返回，每组含 key/label(中文名)/unit(单位)/numeric(是否数值型) 及 points(测点，每点含 measured_at/value_num/value_text/status，按时间升序)。owner 尚未建立时返回 {found:false}。",
	}, getMetricsHandler(d, userID))

	// ---- 写-提议工具 ----
	mcp.AddTool(s, &mcp.Tool{
		Name:        "propose_profile_attr",
		Description: "提议更新我的一条画像属性(不立即生效，返回待确认提议；用户确认后才落库)。attr_key 必须是画像目录内的合法字段键(如 occupation/city/hobbies)。",
	}, proposeProfileAttrHandler(d, userID))

	mcp.AddTool(s, &mcp.Tool{
		Name:        "propose_profile_event",
		Description: "提议给我记录一条大事记(不立即生效，返回待确认提议)。event_type 必须是合法事件类型(里程碑|聚会|会议|旅行|健康|成就|挫折|负面|其他)，title 非空。",
	}, proposeProfileEventHandler(d, userID))

	mcp.AddTool(s, &mcp.Tool{
		Name:        "propose_profile_relationship",
		Description: "提议给我(画像 owner「我」)新增一条人物关系或组织关系(不立即生效，返回待确认提议；用户确认后才落库)。relation_type 必须合法(配偶|子女|父母|兄弟姐妹|亲戚|朋友|同事|领导|下属|客户|供应商|合作方|组织|其他)；related_person_name(关联到某人，如「我朋友李四」) 与 org_name(关联到某组织) 至少给一个。确认时若 related_person_name 对应人物不存在会自动新建。",
	}, proposeProfileRelationshipHandler(d, userID))

	mcp.AddTool(s, &mcp.Tool{
		Name:        "propose_profile_metric",
		Description: "提议给我记录一个时序指标测点(不立即生效，返回待确认提议；用户确认后才落库)。metric_key 必须合法(emotion|weight|sleep|mood_energy|diet|health|height|waist|chest|hip|body_fat)。数值型指标(体重/睡眠/情绪/精力/身高/腰围/胸围/臀围/体脂率)必须给 value_num；类别型指标(饮食/健康)必须给 value_text。unit 可选(留空取目录默认，如 kg/h/cm)；measured_at 可选(如 2026-07-20 / '2026-07-20 15:04' / RFC3339，解析失败或留空取当前时间)。",
	}, proposeProfileMetricHandler(d, userID))

	mcp.AddTool(s, &mcp.Tool{
		Name:        "propose_profile_cycle",
		Description: "提议给我记录一条健康周期/日程(敏感，不立即生效，返回待确认提议；用户确认后才落库)。cycle_type 必须合法(menstrual|medication|injection|followup)。label(名称，如药名；生理期可空)、anchor_date(上次开始日 YYYY-MM-DD)、period_days(周期天数)、duration_days(持续天数)、dosage(剂量)、frequency(频次) 均可选。下次预测由 anchor+period 估算，仅供参考、非医疗建议。",
	}, proposeProfileCycleHandler(d, userID))

	mcp.AddTool(s, &mcp.Tool{
		Name:        "propose_profile_activity",
		Description: "提议给我记录一条生活轨迹活动(不立即生效，返回待确认提议)。activity(做什么，必填)；tool(工具/载体)、location(地点)、commute_mode(通勤方式)、started_at(开始时间 YYYY-MM-DD/RFC3339，留空取当前)、duration_min(时长分钟) 均可选。",
	}, proposeProfileActivityHandler(d, userID))
}

// ---- 读工具输出结构（JSON tag 直接暴露给模型；status 只含 active/pending）----

type profileAttrOut struct {
	Key           string `json:"key"`
	Value         string `json:"value"`
	EpistemicType string `json:"epistemic_type"` // observed|inferred|predicted|suggested
	Status        string `json:"status"`         // active|pending
}

type profileEventOut struct {
	EventType  string     `json:"event_type"`
	Title      string     `json:"title"`
	OccurredAt *time.Time `json:"occurred_at,omitempty"` // 可能为 NULL(给不出确切时间)
	Status     string     `json:"status"`                // active|pending
}

type profileOut struct {
	Found       bool              `json:"found"` // owner/人物是否存在
	DisplayName string            `json:"display_name,omitempty"`
	Summary     string            `json:"summary,omitempty"`
	Attributes  []profileAttrOut  `json:"attributes"`
	Events      []profileEventOut `json:"events"`
}

// buildProfileOut 汇总某人物的属性 + 大事记（只取 active/pending，dismissed/superseded 不返回），
// 供 get_profile / get_person 复用。
func buildProfileOut(ctx context.Context, d MCPDeps, p *repo.Person) (profileOut, error) {
	out := profileOut{
		Found: true, DisplayName: p.DisplayName,
		Attributes: []profileAttrOut{}, Events: []profileEventOut{}, // 初始化为空数组(而非 null)，JSON 更友好
	}
	if p.Summary != nil {
		out.Summary = *p.Summary
	}
	attrs, err := d.PersonAttributes.ListByPerson(ctx, p.ID)
	if err != nil {
		return out, err
	}
	for _, a := range attrs {
		if a.Status != "active" && a.Status != "pending" {
			continue
		}
		out.Attributes = append(out.Attributes, profileAttrOut{
			Key: a.AttrKey, Value: a.ValueText, EpistemicType: a.EpistemicType, Status: a.Status,
		})
	}
	events, err := d.PersonEvents.ListByPerson(ctx, p.ID)
	if err != nil {
		return out, err
	}
	for _, e := range events {
		if e.Status != "active" && e.Status != "pending" {
			continue
		}
		out.Events = append(out.Events, profileEventOut{
			EventType: e.EventType, Title: e.Title, OccurredAt: e.OccurredAt, Status: e.Status,
		})
	}
	return out, nil
}

// getProfileArgs：无参（空 struct → object schema 无属性）。
type getProfileArgs struct{}

func getProfileHandler(d MCPDeps, userID int64) func(context.Context, *mcp.CallToolRequest, getProfileArgs) (*mcp.CallToolResult, any, error) {
	return func(ctx context.Context, _ *mcp.CallToolRequest, _ getProfileArgs) (*mcp.CallToolResult, any, error) {
		owner, err := d.Persons.GetOwner(ctx, userID)
		if err != nil {
			return nil, nil, err
		}
		if owner == nil {
			// owner 未 bootstrap：返回空画像、不报错（模型据此知道画像尚未建立）。
			return jsonResult(profileOut{Found: false, Attributes: []profileAttrOut{}, Events: []profileEventOut{}})
		}
		out, err := buildProfileOut(ctx, d, owner)
		if err != nil {
			return nil, nil, err
		}
		return jsonResult(out)
	}
}

type getPersonArgs struct {
	Name string `json:"name" jsonschema:"要查询的人物姓名(精确匹配显示名)"`
}

func getPersonHandler(d MCPDeps, userID int64) func(context.Context, *mcp.CallToolRequest, getPersonArgs) (*mcp.CallToolResult, any, error) {
	return func(ctx context.Context, _ *mcp.CallToolRequest, a getPersonArgs) (*mcp.CallToolResult, any, error) {
		name := strings.TrimSpace(a.Name)
		if name == "" {
			return nil, nil, fmt.Errorf("get_person 需给出 name")
		}
		p, err := d.Persons.FindByNameOrAlias(ctx, userID, name) // 别名兜底（2026-08-31）
		if err != nil {
			return nil, nil, err
		}
		if p == nil {
			return jsonResult(map[string]any{"found": false})
		}
		out, err := buildProfileOut(ctx, d, p)
		if err != nil {
			return nil, nil, err
		}
		return jsonResult(out)
	}
}

// ---- get_metrics（读，第 5 平面 person_metric）：按 metric_key 分组的时序测点 ----

// metricPointOut 是单个测点（曲线上的一点）：measured_at + 值(数值/类别) + 状态。
// value_num/value_text 用指针 + omitempty：数值型指标只出 value_num，类别型只出 value_text。
type metricPointOut struct {
	MeasuredAt time.Time `json:"measured_at"`
	ValueNum   *float64  `json:"value_num,omitempty"`
	ValueText  *string   `json:"value_text,omitempty"`
	Status     string    `json:"status"` // active|pending
}

// metricGroupOut 是一个指标键的一组测点 + 目录元数据（label/unit/numeric 取 profile.MetricDefOf）。
type metricGroupOut struct {
	Key     string           `json:"key"`     // 指标键（metric_key）
	Label   string           `json:"label"`   // 中文名（目录）
	Unit    string           `json:"unit"`    // 单位（目录；无量纲则空串）
	Numeric bool             `json:"numeric"` // 数值型(value_num 可画曲线) / 类别型(value_text)
	Points  []metricPointOut `json:"points"`  // 测点，按 measured_at 升序
}

type metricsOut struct {
	Found   bool             `json:"found"` // owner 是否存在
	Metrics []metricGroupOut `json:"metrics"`
}

type getMetricsArgs struct {
	MetricKey string `json:"metric_key,omitempty" jsonschema:"可选指标键过滤: emotion|weight|sleep|mood_energy|diet|health|height|waist|chest|hip|body_fat；留空返回全部指标"`
}

// getMetricsHandler 读 owner「我」的时序指标测点（只取 active/pending，见 ListByPerson），
// 按 metric_key 分组返回；owner 未建立时返回 {found:false}（仿 get_profile）。
func getMetricsHandler(d MCPDeps, userID int64) func(context.Context, *mcp.CallToolRequest, getMetricsArgs) (*mcp.CallToolResult, any, error) {
	return func(ctx context.Context, _ *mcp.CallToolRequest, a getMetricsArgs) (*mcp.CallToolResult, any, error) {
		owner, err := d.Persons.GetOwner(ctx, userID)
		if err != nil {
			return nil, nil, err
		}
		if owner == nil {
			return jsonResult(metricsOut{Found: false, Metrics: []metricGroupOut{}})
		}
		rows, err := d.PersonMetrics.ListByPerson(ctx, owner.ID)
		if err != nil {
			return nil, nil, err
		}
		filter := strings.TrimSpace(a.MetricKey)
		// 按 metric_key 分组。ListByPerson 已按 (metric_key, measured_at) 升序返回，同键连续；
		// 用 idx 记每键在 groups 里的下标，健壮于排序假设。label/unit/numeric 取目录定义。
		groups := []metricGroupOut{}
		idx := map[string]int{}
		for _, m := range rows {
			if filter != "" && m.MetricKey != filter {
				continue
			}
			gi, ok := idx[m.MetricKey]
			if !ok {
				def := profile.MetricDefOf(m.MetricKey)
				groups = append(groups, metricGroupOut{
					Key: m.MetricKey, Label: def.Label, Unit: def.Unit, Numeric: def.Numeric,
					Points: []metricPointOut{},
				})
				gi = len(groups) - 1
				idx[m.MetricKey] = gi
			}
			groups[gi].Points = append(groups[gi].Points, metricPointOut{
				MeasuredAt: m.MeasuredAt, ValueNum: m.ValueNum, ValueText: m.ValueText, Status: m.Status,
			})
		}
		return jsonResult(metricsOut{Found: true, Metrics: groups})
	}
}

// isKnownAttrKey 判断 attr_key 是否画像目录内的合法字段键。注意不能用 profile.Def()——
// 它对目录外 key 返回「其他」组默认定义(不报错)，无法用于拒绝。故遍历 profile.All() 精确匹配。
func isKnownAttrKey(key string) bool {
	if key == "" {
		return false
	}
	for _, def := range profile.All() {
		if def.Key == key {
			return true
		}
	}
	return false
}

// ---- propose_profile_attr（profile_attr）：只建 pending 提议，绝不写画像（§8 根防线）----
type proposeProfileAttrArgs struct {
	AttrKey   string `json:"attr_key" jsonschema:"画像属性键，必须是画像目录内的合法字段(如 occupation/city/hobbies)"`
	Value     string `json:"value" jsonschema:"要设置的新值(非空)"`
	Rationale string `json:"rationale,omitempty" jsonschema:"给用户看的修改理由"`
}

func proposeProfileAttrHandler(d MCPDeps, userID int64) func(context.Context, *mcp.CallToolRequest, proposeProfileAttrArgs) (*mcp.CallToolResult, any, error) {
	return func(ctx context.Context, _ *mcp.CallToolRequest, a proposeProfileAttrArgs) (*mcp.CallToolResult, any, error) {
		attrKey := strings.TrimSpace(a.AttrKey)
		value := strings.TrimSpace(a.Value)
		// 校验：attr_key 必须在画像目录内（拒绝目录外 key）；value 非空。非法 → tool-error 供模型读。
		if !isKnownAttrKey(attrKey) {
			return nil, nil, fmt.Errorf("非法画像属性键: %q（不在画像目录内）", a.AttrKey)
		}
		if value == "" {
			return nil, nil, fmt.Errorf("propose_profile_attr 需给出非空 value")
		}
		owner, err := d.Persons.GetOwner(ctx, userID)
		if err != nil {
			return nil, nil, err
		}
		if owner == nil {
			return nil, nil, fmt.Errorf("尚未建立画像 owner「我」，无法提议修改画像属性")
		}
		// 读现值组 {old:{value}}（只读，绝不写）；new 含 attr_key + value 供确认时落库与前端 diff。
		newFields := map[string]any{"attr_key": attrKey, "value": value}
		payloadMap := map[string]any{"new": newFields}
		if cur, _ := d.PersonAttributes.FindActiveByKey(ctx, owner.ID, attrKey); cur != nil {
			payloadMap["old"] = map[string]any{"value": cur.ValueText}
		}
		payload, _ := json.Marshal(payloadMap)
		oid := owner.ID // target_id = owner person id（confirm 时作 personID）
		return proposeAndReturn(ctx, d, userID, &repo.AgentProposal{
			Kind: "profile_attr", TargetKind: "profile", TargetID: &oid,
			Payload: json.RawMessage(payload), Rationale: a.Rationale,
		})
	}
}

// ---- propose_profile_event（profile_event）：只建 pending 提议，绝不写画像 ----
type proposeProfileEventArgs struct {
	EventType  string `json:"event_type" jsonschema:"事件类型: 里程碑|聚会|会议|旅行|健康|成就|挫折|负面|其他"`
	Title      string `json:"title" jsonschema:"事件标题(非空)"`
	OccurredAt string `json:"occurred_at,omitempty" jsonschema:"发生时间(可选)，如 2026-07-20 / 2026-07 / RFC3339；解析失败则不记时间"`
	Rationale  string `json:"rationale,omitempty" jsonschema:"给用户看的记录理由"`
}

func proposeProfileEventHandler(d MCPDeps, userID int64) func(context.Context, *mcp.CallToolRequest, proposeProfileEventArgs) (*mcp.CallToolResult, any, error) {
	return func(ctx context.Context, _ *mcp.CallToolRequest, a proposeProfileEventArgs) (*mcp.CallToolResult, any, error) {
		eventType := strings.TrimSpace(a.EventType)
		title := strings.TrimSpace(a.Title)
		// 校验：event_type 合法 + title 非空。非法 → tool-error 供模型读。
		if !profile.ValidEventTypes[eventType] {
			return nil, nil, fmt.Errorf("非法事件类型: %q（合法: 里程碑|聚会|会议|旅行|健康|成就|挫折|负面|其他）", a.EventType)
		}
		if title == "" {
			return nil, nil, fmt.Errorf("propose_profile_event 需给出非空 title")
		}
		owner, err := d.Persons.GetOwner(ctx, userID)
		if err != nil {
			return nil, nil, err
		}
		if owner == nil {
			return nil, nil, fmt.Errorf("尚未建立画像 owner「我」，无法提议记录大事记")
		}
		newFields := map[string]any{"event_type": eventType, "title": title}
		if oc := strings.TrimSpace(a.OccurredAt); oc != "" {
			newFields["occurred_at"] = oc // 原始字符串，confirm 时经 parseEventAt 尽力解析
		}
		payload, _ := json.Marshal(map[string]any{"new": newFields})
		oid := owner.ID
		return proposeAndReturn(ctx, d, userID, &repo.AgentProposal{
			Kind: "profile_event", TargetKind: "profile", TargetID: &oid,
			Payload: json.RawMessage(payload), Rationale: a.Rationale,
		})
	}
}

// ---- propose_profile_relationship（profile_relationship）：只建 pending 提议，绝不写画像 ----
// 入参用 related_person_name（自然：agent 说「我朋友李四」）而非 id。绝不写 person/relationship 表——
// 只校验 + 读现值（GetOwner + 可选 FindByName 供确认卡展示提示）+ Create 一条 pending 提议。
// 确认时才在单事务内「解析或新建关联人 + 写关系 + Resolve」（apply-once，见 proposals.go）。
type proposeProfileRelationshipArgs struct {
	RelationType      string `json:"relation_type" jsonschema:"关系类型(必填): 配偶|子女|父母|兄弟姐妹|亲戚|朋友|同事|领导|下属|客户|供应商|合作方|组织|其他"`
	RelatedPersonName string `json:"related_person_name,omitempty" jsonschema:"关联到的人物姓名(如「李四」)。与 org_name 至少给一个；确认时若该人不存在会自动新建"`
	OrgName           string `json:"org_name,omitempty" jsonschema:"关联到的组织名(如「某某公司」)。与 related_person_name 至少给一个"`
	Direction         string `json:"direction,omitempty" jsonschema:"关系方向(可选): upstream|downstream|peer"`
	Label             string `json:"label,omitempty" jsonschema:"自由称呼(可选)，如「大儿子」「张总」"`
	Rationale         string `json:"rationale,omitempty" jsonschema:"给用户看的修改理由"`
}

func proposeProfileRelationshipHandler(d MCPDeps, userID int64) func(context.Context, *mcp.CallToolRequest, proposeProfileRelationshipArgs) (*mcp.CallToolResult, any, error) {
	return func(ctx context.Context, _ *mcp.CallToolRequest, a proposeProfileRelationshipArgs) (*mcp.CallToolResult, any, error) {
		relationType := strings.TrimSpace(a.RelationType)
		relatedName := strings.TrimSpace(a.RelatedPersonName)
		orgName := strings.TrimSpace(a.OrgName)
		direction := strings.TrimSpace(a.Direction)
		label := strings.TrimSpace(a.Label)
		// 校验（非法 → tool-error 供模型读）：
		// ① relation_type 必须在合法关系枚举内；
		if !profile.ValidRelations[relationType] {
			return nil, nil, fmt.Errorf("非法关系类型: %q（合法: 配偶|子女|父母|兄弟姐妹|亲戚|朋友|同事|领导|下属|客户|供应商|合作方|组织|其他）", a.RelationType)
		}
		// ② related_person_name 与 org_name 至少给一个（都空则无从建立关系对端）；
		if relatedName == "" && orgName == "" {
			return nil, nil, fmt.Errorf("propose_profile_relationship 需给出 related_person_name 或 org_name 至少一个")
		}
		// ③ direction 若给出需 ∈ upstream|downstream|peer。
		if direction != "" && direction != "upstream" && direction != "downstream" && direction != "peer" {
			return nil, nil, fmt.Errorf("非法 direction: %q（合法: upstream|downstream|peer）", a.Direction)
		}
		owner, err := d.Persons.GetOwner(ctx, userID)
		if err != nil {
			return nil, nil, err
		}
		if owner == nil {
			return nil, nil, fmt.Errorf("尚未建立画像 owner「我」，无法提议新增关系")
		}
		// new 字段：确认时据此「解析或新建关联人 + 写关系」。related_person_name 是自然名，
		// 确认时经 FindByNameExt 命中已有人物 / 未命中 ManualCreatePersonExt 新建（见 proposals.go）。
		newFields := map[string]any{"relation_type": relationType}
		if relatedName != "" {
			newFields["related_person_name"] = relatedName
		}
		if orgName != "" {
			newFields["org_name"] = orgName
		}
		if direction != "" {
			newFields["direction"] = direction
		}
		if label != "" {
			newFields["label"] = label
		}
		// 为确认卡展示：若给 related_person_name，只读地看该人是否已存在（绝不写库），
		// 据此在 rationale 里追加「关联到已有人物X / 将新建人物X」提示，帮用户判断确认后果。
		rationale := strings.TrimSpace(a.Rationale)
		if relatedName != "" {
			ex, err := d.Persons.FindByNameOrAlias(ctx, userID, relatedName) // 别名兜底（2026-08-31）
			if err != nil {
				return nil, nil, err
			}
			var hint string
			if ex != nil {
				hint = fmt.Sprintf("将关联到已有人物「%s」", relatedName)
			} else {
				hint = fmt.Sprintf("将新建人物「%s」并建立关系", relatedName)
			}
			if rationale == "" {
				rationale = hint
			} else {
				rationale = rationale + "（" + hint + "）"
			}
		}
		payload, _ := json.Marshal(map[string]any{"new": newFields})
		oid := owner.ID // target_id = owner person id（confirm 时作 personID）
		return proposeAndReturn(ctx, d, userID, &repo.AgentProposal{
			Kind: "profile_relationship", TargetKind: "profile", TargetID: &oid,
			Payload: json.RawMessage(payload), Rationale: rationale,
		})
	}
}

// ---- propose_profile_metric（profile_metric）：只建 pending 提议，绝不写 person_metric（§8 根防线）----
// 仿 proposeProfileEventHandler：校验 + GetOwner + Create 一条 pending 提议；确认时才在单事务内
// 经 profile.Service.ManualAddMetricExt 落库（apply-once，见 proposals.go 的 profile_metric case）。
type proposeProfileMetricArgs struct {
	MetricKey  string   `json:"metric_key" jsonschema:"指标键(必填): emotion|weight|sleep|mood_energy|diet|health|height|waist|chest|hip|body_fat"`
	ValueNum   *float64 `json:"value_num,omitempty" jsonschema:"数值读数：数值型指标(体重/睡眠/情绪/精力)必填，如体重 70、睡眠 7.5、情绪 -1..1"`
	ValueText  string   `json:"value_text,omitempty" jsonschema:"类别读数：类别型指标(饮食/健康)必填，如饮食「火锅」、健康「感冒」"`
	Unit       string   `json:"unit,omitempty" jsonschema:"单位(可选)，留空取目录默认，如 kg/h"`
	MeasuredAt string   `json:"measured_at,omitempty" jsonschema:"测量时间(可选)，如 2026-07-20 / 「2026-07-20 15:04」/ RFC3339；解析失败或留空取当前时间"`
	Rationale  string   `json:"rationale,omitempty" jsonschema:"给用户看的记录理由"`
}

func proposeProfileMetricHandler(d MCPDeps, userID int64) func(context.Context, *mcp.CallToolRequest, proposeProfileMetricArgs) (*mcp.CallToolResult, any, error) {
	return func(ctx context.Context, _ *mcp.CallToolRequest, a proposeProfileMetricArgs) (*mcp.CallToolResult, any, error) {
		metricKey := strings.TrimSpace(a.MetricKey)
		// 校验（非法 → tool-error 供模型读）：
		// ① metric_key 必须在指标目录内；
		if !profile.ValidMetricKey(metricKey) {
			return nil, nil, fmt.Errorf("非法指标键: %q（合法: emotion|weight|sleep|mood_energy|diet|health|height|waist|chest|hip|body_fat）", a.MetricKey)
		}
		// ② 数值型指标必须给 value_num；类别型指标必须给 value_text（与 ManualAddMetricExt 硬约束一致）。
		valueText := strings.TrimSpace(a.ValueText)
		if profile.MetricDefOf(metricKey).Numeric {
			if a.ValueNum == nil {
				return nil, nil, fmt.Errorf("数值指标 %s 需给出 value_num", metricKey)
			}
		} else if valueText == "" {
			return nil, nil, fmt.Errorf("类别指标 %s 需给出 value_text", metricKey)
		}
		owner, err := d.Persons.GetOwner(ctx, userID)
		if err != nil {
			return nil, nil, err
		}
		if owner == nil {
			return nil, nil, fmt.Errorf("尚未建立画像 owner「我」，无法提议记录指标")
		}
		// new 字段：确认时据此经 ManualAddMetricExt 落库。value_num 有值才放(JSON 数字，
		// confirm 侧从 map[string]any 取 float64)；value_text/unit/measured_at 非空才放。
		newFields := map[string]any{"metric_key": metricKey}
		if a.ValueNum != nil {
			newFields["value_num"] = *a.ValueNum
		}
		if valueText != "" {
			newFields["value_text"] = valueText
		}
		if u := strings.TrimSpace(a.Unit); u != "" {
			newFields["unit"] = u
		}
		if mt := strings.TrimSpace(a.MeasuredAt); mt != "" {
			newFields["measured_at"] = mt // 原始字符串，confirm 时尽力解析、失败/空取 now
		}
		payload, _ := json.Marshal(map[string]any{"new": newFields})
		oid := owner.ID // target_id = owner person id（confirm 时作 personID）
		return proposeAndReturn(ctx, d, userID, &repo.AgentProposal{
			Kind: "profile_metric", TargetKind: "profile", TargetID: &oid,
			Payload: json.RawMessage(payload), Rationale: a.Rationale,
		})
	}
}

// ---- propose_profile_cycle（profile_cycle）：只建 pending 提议，落库在 confirm 单事务里经
// profile.Service.ManualAddCycleExt 完成（apply-once，见 proposals.go 的 profile_cycle case）。
type proposeProfileCycleArgs struct {
	CycleType    string `json:"cycle_type" jsonschema:"周期类型(必填): menstrual|medication|injection|followup"`
	Label        string `json:"label,omitempty" jsonschema:"名称，如药名/针名；生理期可留空"`
	AnchorDate   string `json:"anchor_date,omitempty" jsonschema:"上次开始日 YYYY-MM-DD"`
	PeriodDays   int    `json:"period_days,omitempty" jsonschema:"周期天数(如生理期 28)"`
	DurationDays int    `json:"duration_days,omitempty" jsonschema:"单次持续天数"`
	Dosage       string `json:"dosage,omitempty" jsonschema:"剂量，如 1片"`
	Frequency    string `json:"frequency,omitempty" jsonschema:"频次，如 每日一次"`
	Rationale    string `json:"rationale,omitempty" jsonschema:"给用户看的记录理由"`
}

func proposeProfileCycleHandler(d MCPDeps, userID int64) func(context.Context, *mcp.CallToolRequest, proposeProfileCycleArgs) (*mcp.CallToolResult, any, error) {
	return func(ctx context.Context, _ *mcp.CallToolRequest, a proposeProfileCycleArgs) (*mcp.CallToolResult, any, error) {
		cycleType := strings.TrimSpace(a.CycleType)
		if !profile.ValidCycleTypes[cycleType] { // 非法 → tool-error 供模型读
			return nil, nil, fmt.Errorf("非法周期类型: %q（合法: menstrual|medication|injection|followup）", a.CycleType)
		}
		owner, err := d.Persons.GetOwner(ctx, userID)
		if err != nil {
			return nil, nil, err
		}
		if owner == nil {
			return nil, nil, fmt.Errorf("尚未建立画像 owner「我」，无法提议记录周期")
		}
		// new 字段：确认时据此经 ManualAddCycleExt 落库。period/duration 仅 >0 才放（<=0 视为未给）。
		newFields := map[string]any{"cycle_type": cycleType}
		if v := strings.TrimSpace(a.Label); v != "" {
			newFields["label"] = v
		}
		if v := strings.TrimSpace(a.AnchorDate); v != "" {
			newFields["anchor_date"] = v
		}
		if a.PeriodDays > 0 {
			newFields["period_days"] = a.PeriodDays
		}
		if a.DurationDays > 0 {
			newFields["duration_days"] = a.DurationDays
		}
		if v := strings.TrimSpace(a.Dosage); v != "" {
			newFields["dosage"] = v
		}
		if v := strings.TrimSpace(a.Frequency); v != "" {
			newFields["frequency"] = v
		}
		payload, _ := json.Marshal(map[string]any{"new": newFields})
		oid := owner.ID
		return proposeAndReturn(ctx, d, userID, &repo.AgentProposal{
			Kind: "profile_cycle", TargetKind: "profile", TargetID: &oid,
			Payload: json.RawMessage(payload), Rationale: a.Rationale,
		})
	}
}

// ---- propose_profile_activity（profile_activity）：只建 pending 提议，落库在 confirm 单事务里经
// profile.Service.ManualAddActivityExt 完成（apply-once，见 proposals.go 的 profile_activity case）。
type proposeProfileActivityArgs struct {
	Activity    string `json:"activity" jsonschema:"做什么(必填)，如 写代码/打球/通勤"`
	Tool        string `json:"tool,omitempty" jsonschema:"工具/载体，如 电脑/手机/健身房"`
	Location    string `json:"location,omitempty" jsonschema:"地点"`
	CommuteMode string `json:"commute_mode,omitempty" jsonschema:"通勤方式，如 地铁/开车/步行"`
	StartedAt   string `json:"started_at,omitempty" jsonschema:"开始时间 YYYY-MM-DD/RFC3339，留空取当前"`
	DurationMin int    `json:"duration_min,omitempty" jsonschema:"时长分钟"`
	Rationale   string `json:"rationale,omitempty" jsonschema:"给用户看的记录理由"`
}

func proposeProfileActivityHandler(d MCPDeps, userID int64) func(context.Context, *mcp.CallToolRequest, proposeProfileActivityArgs) (*mcp.CallToolResult, any, error) {
	return func(ctx context.Context, _ *mcp.CallToolRequest, a proposeProfileActivityArgs) (*mcp.CallToolResult, any, error) {
		activity := strings.TrimSpace(a.Activity)
		if activity == "" {
			return nil, nil, fmt.Errorf("activity 不能为空")
		}
		owner, err := d.Persons.GetOwner(ctx, userID)
		if err != nil {
			return nil, nil, err
		}
		if owner == nil {
			return nil, nil, fmt.Errorf("尚未建立画像 owner「我」，无法提议记录活动")
		}
		newFields := map[string]any{"activity": activity}
		if v := strings.TrimSpace(a.Tool); v != "" {
			newFields["tool"] = v
		}
		if v := strings.TrimSpace(a.Location); v != "" {
			newFields["location"] = v
		}
		if v := strings.TrimSpace(a.CommuteMode); v != "" {
			newFields["commute_mode"] = v
		}
		if v := strings.TrimSpace(a.StartedAt); v != "" {
			newFields["started_at"] = v
		}
		if a.DurationMin > 0 {
			newFields["duration_min"] = a.DurationMin
		}
		payload, _ := json.Marshal(map[string]any{"new": newFields})
		oid := owner.ID
		return proposeAndReturn(ctx, d, userID, &repo.AgentProposal{
			Kind: "profile_activity", TargetKind: "profile", TargetID: &oid,
			Payload: json.RawMessage(payload), Rationale: a.Rationale,
		})
	}
}
