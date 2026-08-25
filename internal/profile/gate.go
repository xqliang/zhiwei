package profile

import (
	"zhiwei/internal/repo"
)

// GateConfig 是画像闸门阈值（来自 ZW_PROFILE_AUTO_CONFIDENCE 配置）。
type GateConfig struct {
	AutoConf float64 // 自动写入 active 的置信阈值；<=0 用默认 0.75
}

// autoConf 返回生效阈值：配置 <=0 时兜底默认 0.75（spec §5 阈值默认值）。
func (g GateConfig) autoConf() float64 {
	if g.AutoConf <= 0 {
		return 0.75
	}
	return g.AutoConf
}

// Decision 是一条事实的落库决策（spec §5 闸门规则）。
type Decision string

const (
	DecisionCreateActive    Decision = "create_active"    // 无现值且高置信 observed/inferred → 直接 active
	DecisionCreatePending   Decision = "create_pending"   // 无现值低置信 → pending 待人工确认
	DecisionReaffirm        Decision = "reaffirm"         // 同值已存在 → 佐证（属性平面为置信度上调 +0.05 封顶 0.99；关系平面为 touch 不上调）
	DecisionConflictPending Decision = "conflict_pending" // 单值冲突 → pending（supersedes 指向现值），绝不静默覆盖
	DecisionSkip            Decision = "skip"             // 自然键已处理过（同 session 同值）→ 幂等跳过
)

// autoWritable 判定「无现值」候选能否直接写 active：置信度达阈值 且 认知类型可自动写入。
// 只有 observed（观察到）/inferred（可推断）允许自动 active；predicted/suggested 一律进 pending。
func autoWritable(f Fact, cfg GateConfig) bool {
	return f.Confidence >= cfg.autoConf() &&
		(f.EpistemicType == "observed" || f.EpistemicType == "inferred")
}

// DecideAttribute 属性闸门（spec §5.2-5.4）。
// existing 的语义按 cardinality 区分：
//
//	单值型 = 该 key 当前 active 行（值可能不同 → 冲突路径）；
//	列表型 = 该 key 同值 active 行（无则 nil；列表元素的 existing 必同值 → 只有 reaffirm）。
//
// dedupHit = 自然键 (session,person,key,value) 已存在任意 status 行。
func DecideAttribute(f Fact, existing *repo.PersonAttribute, isList bool, dedupHit bool, cfg GateConfig) Decision {
	if dedupHit {
		return DecisionSkip
	}
	if existing != nil {
		// 归一化后比较：去空格/标点、转小写，使「 工程师 」与「工程师」视为同值。
		if repo.NormalizeTitle(existing.ValueText) == repo.NormalizeTitle(f.Value) {
			return DecisionReaffirm
		}
		return DecisionConflictPending // 只有单值型会走到这里（列表型 existing 即同值行）
	}
	if autoWritable(f, cfg) {
		return DecisionCreateActive
	}
	return DecisionCreatePending
}

// DecideRelationship 关系闸门：关系天然多条（多个子女/朋友/同事并存），无冲突路径——
// 同键（主体,类型,对端）已 active → 佐证；新键按置信度 create。
// 注：同类型不同对端（如两位「配偶」）在 P1 会并存两行 active，用户可在队列里放弃其一；
// 更精细的唯一性约束（配偶唯一）留给后续按 relation_type 配置。
func DecideRelationship(f Fact, existing *repo.PersonRelationship, dedupHit bool, cfg GateConfig) Decision {
	if dedupHit {
		return DecisionSkip
	}
	if existing != nil {
		return DecisionReaffirm
	}
	if autoWritable(f, cfg) {
		return DecisionCreateActive
	}
	return DecisionCreatePending
}

// DecideEvent 事件闸门（spec §4.4/§5）：事件天然多条追加（同 relationship 无冲突路径）——
// 同键（person,类型,title）已 active → 佐证（事件无置信佐证语义，持久化效果=审计
// 条目，同 relationship reaffirm 模式）；新键按置信度 create。
func DecideEvent(f Fact, existing *repo.PersonEvent, dedupHit bool, cfg GateConfig) Decision {
	if dedupHit {
		return DecisionSkip
	}
	if existing != nil {
		return DecisionReaffirm
	}
	if autoWritable(f, cfg) {
		return DecisionCreateActive
	}
	return DecisionCreatePending
}

// DecideMetric 指标闸门：测点流无当前值/无冲突/无佐证——纯置信闸门，自然键防重跑。
// 与其余四平面不同，metric 是「采样」而非「当前态」：同 key 同值不是佐证而是两次独立采样
// （各自成行），不同值不是冲突而是时序变化——故既无 reaffirm 也无 conflict/existing 分支。
// 唯一去重是自然键 (session,person,key,value,measured_at) 防同 session 重跑（dedupHit）。
func DecideMetric(f Fact, dedupHit bool, cfg GateConfig) Decision {
	if dedupHit {
		return DecisionSkip
	}
	if autoWritable(f, cfg) {
		return DecisionCreateActive
	}
	return DecisionCreatePending
}

// DecideCycle 周期闸门：单值语义（同 person+type+label 至多一条 active）——
// 有 active 现值 → 冲突 pending（supersedes 指向现值，绝不静默覆盖，对齐 attribute 单值模式）；
// 无 → 按置信度。无 reaffirm（周期更新即冲突路径：用药/周期调整都要人工确认取代现值）。
func DecideCycle(f Fact, existing *repo.PersonCycle, dedupHit bool, cfg GateConfig) Decision {
	if dedupHit {
		return DecisionSkip
	}
	if existing != nil {
		return DecisionConflictPending
	}
	if autoWritable(f, cfg) {
		return DecisionCreateActive
	}
	return DecisionCreatePending
}
