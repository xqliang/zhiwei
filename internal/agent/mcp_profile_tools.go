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
// 全部限 user_id=1（toolUserID），写目标恒为 owner「我」。「owner 概要注入对话头」由
// ProfileContext.Head 承担（见 context.go / orchestrator.go），不在此工具层。
func registerProfileTools(s *mcp.Server, d MCPDeps) {
	// ---- 读工具 ----
	mcp.AddTool(s, &mcp.Tool{
		Name:        "get_profile",
		Description: "读取我（画像 owner「我」）的个人画像：显示名、简介、属性列表(键/值/认知类型/状态) 与大事记(类型/标题/时间/状态)。无参。owner 尚未建立时返回空画像({found:false})。",
	}, getProfileHandler(d))

	mcp.AddTool(s, &mcp.Tool{
		Name:        "get_person",
		Description: "按姓名读取某个人物的画像(属性 + 大事记)。入参 name(精确匹配显示名)。找不到返回 {found:false}。",
	}, getPersonHandler(d))

	// ---- 写-提议工具 ----
	mcp.AddTool(s, &mcp.Tool{
		Name:        "propose_profile_attr",
		Description: "提议更新我的一条画像属性(不立即生效，返回待确认提议；用户确认后才落库)。attr_key 必须是画像目录内的合法字段键(如 occupation/city/hobbies)。",
	}, proposeProfileAttrHandler(d))

	mcp.AddTool(s, &mcp.Tool{
		Name:        "propose_profile_event",
		Description: "提议给我记录一条大事记(不立即生效，返回待确认提议)。event_type 必须是合法事件类型(里程碑|聚会|会议|旅行|健康|成就|挫折|负面|其他)，title 非空。",
	}, proposeProfileEventHandler(d))

	mcp.AddTool(s, &mcp.Tool{
		Name:        "propose_profile_relationship",
		Description: "提议给我(画像 owner「我」)新增一条人物关系或组织关系(不立即生效，返回待确认提议；用户确认后才落库)。relation_type 必须合法(配偶|子女|父母|兄弟姐妹|亲戚|朋友|同事|领导|下属|客户|供应商|合作方|组织|其他)；related_person_name(关联到某人，如「我朋友李四」) 与 org_name(关联到某组织) 至少给一个。确认时若 related_person_name 对应人物不存在会自动新建。",
	}, proposeProfileRelationshipHandler(d))
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

func getProfileHandler(d MCPDeps) func(context.Context, *mcp.CallToolRequest, getProfileArgs) (*mcp.CallToolResult, any, error) {
	return func(ctx context.Context, _ *mcp.CallToolRequest, _ getProfileArgs) (*mcp.CallToolResult, any, error) {
		owner, err := d.Persons.GetOwner(ctx, toolUserID)
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

func getPersonHandler(d MCPDeps) func(context.Context, *mcp.CallToolRequest, getPersonArgs) (*mcp.CallToolResult, any, error) {
	return func(ctx context.Context, _ *mcp.CallToolRequest, a getPersonArgs) (*mcp.CallToolResult, any, error) {
		name := strings.TrimSpace(a.Name)
		if name == "" {
			return nil, nil, fmt.Errorf("get_person 需给出 name")
		}
		p, err := d.Persons.FindByName(ctx, toolUserID, name)
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

func proposeProfileAttrHandler(d MCPDeps) func(context.Context, *mcp.CallToolRequest, proposeProfileAttrArgs) (*mcp.CallToolResult, any, error) {
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
		owner, err := d.Persons.GetOwner(ctx, toolUserID)
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
		return proposeAndReturn(ctx, d, &repo.AgentProposal{
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

func proposeProfileEventHandler(d MCPDeps) func(context.Context, *mcp.CallToolRequest, proposeProfileEventArgs) (*mcp.CallToolResult, any, error) {
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
		owner, err := d.Persons.GetOwner(ctx, toolUserID)
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
		return proposeAndReturn(ctx, d, &repo.AgentProposal{
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

func proposeProfileRelationshipHandler(d MCPDeps) func(context.Context, *mcp.CallToolRequest, proposeProfileRelationshipArgs) (*mcp.CallToolResult, any, error) {
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
		owner, err := d.Persons.GetOwner(ctx, toolUserID)
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
			ex, err := d.Persons.FindByName(ctx, toolUserID, relatedName)
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
		return proposeAndReturn(ctx, d, &repo.AgentProposal{
			Kind: "profile_relationship", TargetKind: "profile", TargetID: &oid,
			Payload: json.RawMessage(payload), Rationale: rationale,
		})
	}
}
