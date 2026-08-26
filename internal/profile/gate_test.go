package profile

import (
	"testing"

	"zhiwei/internal/repo"
)

func TestDecideAttribute(t *testing.T) {
	cfg := GateConfig{AutoConf: 0.75}
	base := Fact{Plane: "attribute", AttrKey: "occupation", Value: "工程师",
		Confidence: 0.9, EpistemicType: "observed"}

	// 无现值、高置信 observed → 直接 active
	if d := DecideAttribute(base, nil, false, false, cfg); d != DecisionCreateActive {
		t.Fatalf("高置信无现值应为 create_active: %v", d)
	}
	// 无现值、低置信 → pending
	low := base
	low.Confidence = 0.6
	if d := DecideAttribute(low, nil, false, false, cfg); d != DecisionCreatePending {
		t.Fatalf("低置信应为 create_pending: %v", d)
	}
	// 高置信但 suggested（推测）→ pending（只有 observed/inferred 可自动写入）
	sugg := base
	sugg.EpistemicType = "suggested"
	if d := DecideAttribute(sugg, nil, false, false, cfg); d != DecisionCreatePending {
		t.Fatalf("suggested 应为 create_pending: %v", d)
	}
	// 高置信 inferred（可推断）→ active（inferred 与 observed 并列在自动写入白名单——
	// 钉死谓词第二个操作数：删掉 `|| inferred` 后这条会失败）
	inf := base
	inf.EpistemicType = "inferred"
	if d := DecideAttribute(inf, nil, false, false, cfg); d != DecisionCreateActive {
		t.Fatalf("inferred 高置信应为 create_active: %v", d)
	}
	// 高置信 predicted（预测）→ pending（predicted 不在自动写入白名单，须人工确认）
	pred := base
	pred.EpistemicType = "predicted"
	if d := DecideAttribute(pred, nil, false, false, cfg); d != DecisionCreatePending {
		t.Fatalf("predicted 应为 create_pending: %v", d)
	}
	// 边界值：confidence 恰好等于阈值 0.75（cfg.AutoConf=0.75）→ active，钉死 `>=` 而非 `>`
	edge := base
	edge.Confidence = 0.75
	if d := DecideAttribute(edge, nil, false, false, cfg); d != DecisionCreateActive {
		t.Fatalf("confidence==阈值应为 create_active（>= 语义）: %v", d)
	}
	// 同 session 同值已处理 → skip（幂等）
	if d := DecideAttribute(base, nil, false, true, cfg); d != DecisionSkip {
		t.Fatalf("dedupHit 应 skip: %v", d)
	}
	// 有现值同值 → reaffirm（佐证）
	same := &repo.PersonAttribute{ValueText: "工程师"}
	if d := DecideAttribute(base, same, false, false, cfg); d != DecisionReaffirm {
		t.Fatalf("同值应 reaffirm: %v", d)
	}
	// 有现值不同值（单值型）→ 冲突 pending，绝不静默覆盖
	diff := &repo.PersonAttribute{ValueText: "教师"}
	if d := DecideAttribute(base, diff, false, false, cfg); d != DecisionConflictPending {
		t.Fatalf("单值冲突应 conflict_pending: %v", d)
	}
	// 列表型：existing 只会是同值行，无值 → 按置信度 create
	lowList := Fact{Plane: "attribute", AttrKey: "hobbies", Value: "游泳",
		Confidence: 0.6, EpistemicType: "observed"}
	if d := DecideAttribute(lowList, nil, true, false, cfg); d != DecisionCreatePending {
		t.Fatalf("列表低置信应 create_pending: %v", d)
	}
	highList := lowList
	highList.Confidence = 0.9
	if d := DecideAttribute(highList, nil, true, false, cfg); d != DecisionCreateActive {
		t.Fatalf("列表高置信应 create_active: %v", d)
	}
	// 阈值兜底：AutoConf<=0 时用默认 0.75
	if d := DecideAttribute(base, nil, false, false, GateConfig{}); d != DecisionCreateActive {
		t.Fatalf("默认阈值 0.75，0.9 应 active: %v", d)
	}
	// 值归一化比较：现值「 工程师 」与新值「工程师」视为同值
	spaced := &repo.PersonAttribute{ValueText: " 工程师 "}
	if d := DecideAttribute(base, spaced, false, false, cfg); d != DecisionReaffirm {
		t.Fatalf("归一化后同值应 reaffirm: %v", d)
	}
}

func TestDecideRelationship(t *testing.T) {
	cfg := GateConfig{AutoConf: 0.75}
	f := Fact{Plane: "relationship", RelationType: "配偶",
		Related:    Subject{Kind: "mentioned", Name: "Alice"},
		Confidence: 0.9, EpistemicType: "observed"}

	if d := DecideRelationship(f, nil, false, cfg); d != DecisionCreateActive {
		t.Fatalf("高置信新关系应 create_active: %v", d)
	}
	if d := DecideRelationship(f, nil, true, cfg); d != DecisionSkip {
		t.Fatalf("dedupHit 应 skip: %v", d)
	}
	if d := DecideRelationship(f, &repo.PersonRelationship{}, false, cfg); d != DecisionReaffirm {
		t.Fatalf("同键关系应 reaffirm: %v", d)
	}
	low := f
	low.Confidence = 0.6
	if d := DecideRelationship(low, nil, false, cfg); d != DecisionCreatePending {
		t.Fatalf("低置信应 create_pending: %v", d)
	}
}

func TestDecideEvent(t *testing.T) {
	cfg := GateConfig{AutoConf: 0.75}
	f := Fact{Plane: "event", EventType: "旅行", EventTitle: "去云南旅游",
		Subject:    Subject{Kind: "self"},
		Confidence: 0.9, EpistemicType: "observed"}

	if d := DecideEvent(f, nil, false, cfg); d != DecisionCreateActive {
		t.Fatalf("高置信新事件应 create_active: %v", d)
	}
	if d := DecideEvent(f, nil, true, cfg); d != DecisionSkip {
		t.Fatalf("dedupHit 应 skip: %v", d)
	}
	if d := DecideEvent(f, &repo.PersonEvent{}, false, cfg); d != DecisionReaffirm {
		t.Fatalf("同键事件应 reaffirm: %v", d)
	}
	low := f
	low.Confidence = 0.6
	if d := DecideEvent(low, nil, false, cfg); d != DecisionCreatePending {
		t.Fatalf("低置信应 create_pending: %v", d)
	}
	if d := DecideEvent(f, nil, false, GateConfig{}); d != DecisionCreateActive {
		t.Fatalf("默认阈值 0.75，0.9 应 active: %v", d)
	}
}

func TestDecideMetric(t *testing.T) {
	cfg := GateConfig{AutoConf: 0.75}
	f := Fact{Plane: "metric", MetricKey: "weight", MetricValue: "72.5",
		Subject:    Subject{Kind: "self"},
		Confidence: 0.9, EpistemicType: "observed"}

	// 高置信 observed → 直接 active（纯置信闸门）
	if d := DecideMetric(f, false, cfg); d != DecisionCreateActive {
		t.Fatalf("高置信测点应 create_active: %v", d)
	}
	// 低置信 → pending
	low := f
	low.Confidence = 0.6
	if d := DecideMetric(low, false, cfg); d != DecisionCreatePending {
		t.Fatalf("低置信应 create_pending: %v", d)
	}
	// 自然键已处理（同 session 重跑）→ skip（幂等）
	if d := DecideMetric(f, true, cfg); d != DecisionSkip {
		t.Fatalf("dedupHit 应 skip: %v", d)
	}
	// metric 无 reaffirm/conflict 语义：DecideMetric 签名不含 existing 参数——同 key 同值是
	// 「两次独立采样各自成行」而非佐证，不同值是「时序变化」而非冲突。故只有 active/pending/skip
	// 三条路径可测（无第四条 existing 分支），此处不存在 reaffirm/conflict 断言。
	// 默认阈值兜底：AutoConf<=0 用 0.75，0.9 应 active
	if d := DecideMetric(f, false, GateConfig{}); d != DecisionCreateActive {
		t.Fatalf("默认阈值 0.75，0.9 应 active: %v", d)
	}
}

func TestDecideCycle(t *testing.T) {
	cfg := GateConfig{AutoConf: 0.75}
	f := Fact{Plane: "cycle", CycleType: "medication", CycleLabel: "降压药",
		Subject:    Subject{Kind: "self"},
		Confidence: 0.9, EpistemicType: "observed"}

	// 无现值、高置信 → active
	if d := DecideCycle(f, nil, false, cfg); d != DecisionCreateActive {
		t.Fatalf("高置信新周期应 create_active: %v", d)
	}
	// 无现值、低置信 → pending
	low := f
	low.Confidence = 0.6
	if d := DecideCycle(low, nil, false, cfg); d != DecisionCreatePending {
		t.Fatalf("低置信应 create_pending: %v", d)
	}
	// 自然键已处理 → skip（幂等）
	if d := DecideCycle(f, nil, true, cfg); d != DecisionSkip {
		t.Fatalf("dedupHit 应 skip: %v", d)
	}
	// 有 active 现值 → 冲突 pending（单值语义，supersedes 指向现值，绝不静默覆盖）。
	// 注意即使高置信也走冲突路径（无 reaffirm——周期更新即取代）：钉死 existing!=nil 分支优先于置信度。
	if d := DecideCycle(f, &repo.PersonCycle{}, false, cfg); d != DecisionConflictPending {
		t.Fatalf("有现值应 conflict_pending: %v", d)
	}
	// 默认阈值兜底：AutoConf<=0 用 0.75，0.9 应 active
	if d := DecideCycle(f, nil, false, GateConfig{}); d != DecisionCreateActive {
		t.Fatalf("默认阈值 0.75，0.9 应 active: %v", d)
	}
}

func TestDecideActivity(t *testing.T) {
	cfg := GateConfig{AutoConf: 0.75}
	f := Fact{Plane: "activity", ActivityText: "写代码", Tool: "电脑",
		Subject:    Subject{Kind: "self"},
		Confidence: 0.9, EpistemicType: "observed"}

	// 高置信 observed → 直接 active（纯置信闸门，测点流语义同 metric）
	if d := DecideActivity(f, false, cfg); d != DecisionCreateActive {
		t.Fatalf("高置信活动应 create_active: %v", d)
	}
	// 低置信 → pending
	low := f
	low.Confidence = 0.6
	if d := DecideActivity(low, false, cfg); d != DecisionCreatePending {
		t.Fatalf("低置信应 create_pending: %v", d)
	}
	// 高置信 predicted（预测）→ pending（predicted 不在自动写入白名单，须人工确认）
	pred := f
	pred.EpistemicType = "predicted"
	if d := DecideActivity(pred, false, cfg); d != DecisionCreatePending {
		t.Fatalf("predicted 应 create_pending: %v", d)
	}
	// 自然键已处理（同 session 重跑）→ skip（幂等）
	if d := DecideActivity(f, true, cfg); d != DecisionSkip {
		t.Fatalf("dedupHit 应 skip: %v", d)
	}
	// activity 无 reaffirm/conflict 语义：DecideActivity 签名不含 existing 参数——同活动不同时刻是
	// 「两次独立记录各自成行」而非佐证/冲突（同 metric）。故只有 active/pending/skip 三条路径可测。
	// 默认阈值兜底：AutoConf<=0 用 0.75，0.9 应 active
	if d := DecideActivity(f, false, GateConfig{}); d != DecisionCreateActive {
		t.Fatalf("默认阈值 0.75，0.9 应 active: %v", d)
	}
}
