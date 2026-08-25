# 用户画像 P3a（状态&健康后端）实现计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 画像系统 P3 的后端：新增 **metric 平面**（时序指标：情绪/状态/体重/饮食/健康/熬夜——时间戳+数值或文本的测点流）与 **cycle 平面**（周期/日程：生理期/用药/打针/慢病随访——含下次预测，**敏感数据**）。迁移 000009；fact/gate/service/confirm/API 四层平面扩展；prompt 升 v3。

**Architecture:** 完全复用平面扩展模式（P2a event 的路径）。关键差异：

**metric（时序测点流）语义决策**：
- 每个测点独立一行（无「当前值」概念，无冲突路径、无 reaffirm——不是「当前态」是「采样」）
- 自然键 `(session, person, metric_key, value_text, measured_at 原始串)`；同 session 重跑 → skip
- measured_at：LLM 可给原始日期串（复用 parseEventAt）；未给/解析失败 → **落 session 的 created_at**（对话发生时即测点时刻——比 NULL 更符合时序语义，注释说明）
- 闸门 = 纯置信闸门（高置信 active / 低置信 pending），其余无
- value：value_num（数值型如体重 kg）与 value_text（类别如情绪='焦虑'）二选一由 LLM 给；metric_key 枚举 6 种（spec §4.5）

**cycle（周期）语义决策**：
- 同 `(person, cycle_type, label)` 至多一条 active——新建议走 **attribute 单值模式**（有 active 现值且参数不同 → pending+supersedes 绝不静默覆盖；无 → 按置信度）
- `next_predicted_at` = anchor_date + period_days 天（后端算，纯日期加法；**非医疗建议**，API/前端带免责文案——spec §9）
- 敏感：数据本地存（单用户 MVP）；API 不做特殊加密但 cycle 端点注释标注敏感；前端折叠是 P3b 的事

**工作目录：** worktree `.worktrees/person-metric`（分支 `feat/person-metric`），`cd /Users/jyxc-dz-0100360/work/fun/zhiwei-water18-0822/.worktrees/person-metric`

**测试约定：** 同前（make test / make init-testdb + TEST_MYSQL_DSN … go test -p 1 -count=1；testdata 已就位）。

---

### Task 1: 迁移 000009_metric_cycle

**Files:** `migrations/000009_metric_cycle.up.sql` + `.down.sql`

```sql
-- 画像 P3：metric（时序指标）+ cycle（周期/日程，敏感）两平面（spec §4.5/§4.6）。
-- metric 是测点流：每个时间戳一行（情绪/体重/熬夜…），无「当前值」概念。
-- cycle 含下次预测（anchor+period，非医疗建议），敏感数据：本地存储、前端默认折叠（spec §9）。
CREATE TABLE person_metric (
  id           BIGINT PRIMARY KEY,
  user_id      BIGINT NOT NULL DEFAULT 1,
  person_id    BIGINT NOT NULL,
  metric_key   VARCHAR(32) NOT NULL,               -- emotion|state|weight|sleep_late|diet|health
  value_num    DECIMAL(10,3) NULL,                 -- 数值型（体重 kg、熬夜 0/1）；类别型为 NULL
  value_text   VARCHAR(256) NULL,                  -- 类别/描述（情绪='焦虑'、饮食='火锅'）；数值型为 NULL
  unit         VARCHAR(16) NULL,
  measured_at  DATETIME(3) NOT NULL,               -- 测点时间（LLM 未给则落 session 时间）
  -- 横切字段（与既有平面一致，spec §3）
  confidence    DECIMAL(5,4) NOT NULL DEFAULT 0.8,
  epistemic_type VARCHAR(16) NOT NULL DEFAULT 'observed',
  source        VARCHAR(8) NOT NULL DEFAULT 'manual',
  status        VARCHAR(16) NOT NULL DEFAULT 'active',
  session_id    BIGINT NULL,
  memory_id     BIGINT NULL,
  transcript_segment_ids JSON NULL,
  version       INT NOT NULL DEFAULT 1,
  created_at    DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  updated_at    DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  KEY idx_person_key_time (person_id, metric_key, measured_at),
  KEY idx_user_status (user_id, status),
  KEY idx_session (session_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE person_cycle (
  id              BIGINT PRIMARY KEY,
  user_id         BIGINT NOT NULL DEFAULT 1,
  person_id       BIGINT NOT NULL,
  cycle_type      VARCHAR(16) NOT NULL,            -- menstrual|medication|injection|followup
  label           VARCHAR(128) NULL,               -- 药名/针名/'生理期'（自然键成分，NULL 视为 ''）
  anchor_date     DATE NULL,                       -- 上次起始（预测锚点）
  period_days     INT NULL,                        -- 周期天数
  duration_days   INT NULL,                        -- 单次持续
  dosage          VARCHAR(64) NULL,
  frequency_text  VARCHAR(64) NULL,                -- 频次（'每日两次'）
  next_predicted_at DATE NULL,                     -- = anchor+period；估算非医疗建议（spec §9）
  -- 横切字段同上
  confidence    DECIMAL(5,4) NOT NULL DEFAULT 0.8,
  epistemic_type VARCHAR(16) NOT NULL DEFAULT 'observed',
  source        VARCHAR(8) NOT NULL DEFAULT 'manual',
  status        VARCHAR(16) NOT NULL DEFAULT 'active',
  session_id    BIGINT NULL,
  memory_id     BIGINT NULL,
  transcript_segment_ids JSON NULL,
  supersedes_id BIGINT NULL,
  version       INT NOT NULL DEFAULT 1,
  created_at    DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  updated_at    DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  KEY idx_person_type (person_id, cycle_type, status),
  KEY idx_user_status (user_id, status),
  KEY idx_session (session_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
```

down：两表 DROP IF EXISTS。验证 `make compose-up && make init-testdb`（9/u metric_cycle）。Commit: `feat(profile): 迁移 000009_metric_cycle——时序指标+周期两表`

### Task 2: repo——PersonMetricRepo

**Files:** `internal/repo/person_metric.go` + 测试（照 person_event.go 模式）

方法：CreateExt/Create（INSERT 18 列；零值兜底 UserID/Confidence=0.8/EpistemicType/Source/Status/Version）；Get(nil,nil)；**ListByPerson(ctx, personID, metricKey string, from, to *time.Time)**——时序查询：metric_key 空不过滤；from/to 半开区间 `[from, to)`；ORDER BY measured_at ASC（图表要升序）；**ListPending**；SetStatusExt/SetStatus；**FindByNaturalKeyExt(session, person, metricKey, valueText, measuredAt)**（幂等——注意 value_num 类测点 valueText 传 fmt 数值串，Go 侧统一转 string 比较，注释说明）。**注意 metric 无 FindActiveByKey**（无当前值概念，注释声明）。

测试：数值/类别两类测点、时序过滤（from/to 边界）、ASC 排序、自然键命中（数值型经字符串化）、pending、(nil,nil)。

### Task 3: repo——PersonCycleRepo

**Files:** `internal/repo/person_cycle.go` + 测试

方法：CreateExt/Create（20 列）；Get(nil,nil)；ListByPerson(personID)（全状态按 cycle_type,id）；**FindActiveByKeyExt(person, cycleType, label)**（label NULL 安全 `<=>`）；**FindByNaturalKeyExt(session, person, cycleType, label)**（任意 status）；SetStatusExt/SetStatus；ListPending。

测试：NULL label 匹配（`<=>`）、自然键、pending、(nil,nil)。

### Task 4: fact.go 扩 metric/cycle 平面

**Files:** `internal/profile/fact.go` + 测试

- Fact 加字段：
```go
	// ---- metric 平面（P3 时序指标）----
	MetricKey   string // emotion|state|weight|sleep_late|diet|health
	MetricValue string // LLM 原始值（数值或类别），Go 侧分流 value_num/value_text
	MetricUnit  string
	MeasuredAt  string // 原始日期串；空则 service 落 session 时间

	// ---- cycle 平面（P3 周期/日程，敏感）----
	CycleType      string // menstrual|medication|injection|followup
	CycleLabel     string
	AnchorDate     string // YYYY-MM-DD 原始串
	PeriodDays     int
	DurationDays   int
	Dosage         string
	FrequencyText  string
```
- rawFact 加对应 json 标签（metric_key/metric_value/metric_unit/measured_at/cycle_type/cycle_label/anchor_date/period_days/duration_days/dosage/frequency）
- validPlanes 加 metric/cycle；导出 `ValidMetricKeys`（6）与 `ValidCycleTypes`（4）
- ParseFacts 分支：metric——`!ValidMetricKeys[key] || MetricValue == ""` 丢；cycle——`!ValidCycleTypes[t]` 丢（label/anchor 可空——纯随访记录）
- factKey 追加 MetricKey+MetricValue+MeasuredAt 与 CycleType+CycleLabel+AnchorDate 判别
- 测试：两类 fact 解析（含 trim）、非法枚举丢弃、cycle 空合法

### Task 5: gate——DecideMetric / DecideCycle

**Files:** `internal/profile/gate.go` + 测试

```go
// DecideMetric 指标闸门：测点流无当前值/无冲突/无佐证——纯置信闸门，自然键防重跑。
func DecideMetric(f Fact, dedupHit bool, cfg GateConfig) Decision {
	if dedupHit { return DecisionSkip }
	if autoWritable(f, cfg) { return DecisionCreateActive }
	return DecisionCreatePending
}
// DecideCycle 周期闸门：单值语义（同 person+type+label 至多一条 active）——
// 有 active 现值 → 冲突 pending（supersedes，绝不静默覆盖，对齐 attribute 单值模式）；
// 无 → 按置信度。无 reaffirm（周期更新即冲突路径）。
func DecideCycle(f Fact, existing *repo.PersonCycle, dedupHit bool, cfg GateConfig) Decision {
	if dedupHit { return DecisionSkip }
	if existing != nil { return DecisionConflictPending }
	if autoWritable(f, cfg) { return DecisionCreateActive }
	return DecisionCreatePending
}
```

测试各路径（含 metric 无 reaffirm 的语义注释）。

### Task 6: prompt v3

**Files:** Create `prompts/profile_extraction_v3.md`；main.go 路径切 v3

v2 全文 + 增补：
- 「只抽」清单：删「情绪、健康等时序状态（P3…本版忽略）」——现在要抽（plane=metric/cycle）
- plane 枚举加 `/ metric（时序指标）/ cycle（周期日程）`
- metric 字段说明：metric_key 6 枚举、metric_value（数值或中文短语）、metric_unit、measured_at（尽量给日期；对话当下的状态可留空）
- cycle 字段说明：cycle_type 4 枚举、cycle_label（药名/针名；生理期留空）、anchor_date（上次开始的日期）、period_days（周期天数，如 28）、duration_days、dosage、frequency
- 示例加两条：
```json
  {"plane":"metric","subject":{"kind":"self"},"metric_key":"emotion","metric_value":"焦虑",
   "confidence":0.85,"epistemic_type":"observed","block_index":3},
  {"plane":"cycle","subject":{"kind":"self"},"cycle_type":"medication","cycle_label":"降压药",
   "anchor_date":"2026-08-01","frequency":"每日一次","dosage":"1片","confidence":0.9,
   "epistemic_type":"observed","block_index":3}
```
（示例对话第 3 行须补相关内容保持 few-shot 自洽——如「最近有点焦虑，降压药还是每天一片」。）
- v2 的 extractor.go/stage_profile_test 引用同步 v3（grep 全仓 profile_extraction_v2）

### Task 7: Service 扩 metric/cycle

**Files:** Modify `service.go`/`service_manual.go`/`service_test.go`

- Service struct 加 Metrics/Cycles repo
- **applyMetricFact**：dedup 自然键（valueText = f.MetricValue——统一串比较，repo 层注释）；DecideMetric；**measured_at 解析链：parseEventAt(f.MeasuredAt) 失败 → session 时间**（ApplyFacts 需拿 session created_at——applyFact 签名加 sessionTime time.Time 参数，从 ApplyFacts 开头 Sessions.Get 取一次）；value_num/value_text 分流（strconv.ParseFloat 成功 → num + unit 保留，否则 text）
- **applyCycleFact**：FindActiveByKey/FindByNaturalKey；DecideCycle；**next_predicted_at = anchor_date + period_days**（两者都非空才算，date 加法注释「估算非医疗建议」）；conflict pending 带 supersedes
- 构造器 metricRow/cycleRow + 审计 createMetricLog/createCycleLog（entity_kind=metric/cycle；注意 change_log 的 entity_kind VARCHAR(16) 够用）
- Manual：ManualAddMetric（key 校验/value/unit/measuredAt 原始串）/ManualDeleteMetric/ManualAddCycle（type 校验/label/anchor/period/duration/dosage/frequency，算 next_predicted）/ManualDeleteCycle
- 测试 TestApplyMetricFacts（数值+类别、measured_at 空→session 时间、重跑 skip、pending）、TestApplyCycleFacts（新 cycle active、同键冲突 pending+supersedes、anchor+period→next_predicted、重跑 skip）、手动 CRUD；owner 清理 t.Cleanup（metric/cycle 行 + entity_kind 审计行）

### Task 8: confirm 扩 kind + API

**Files:** Modify `confirm.go`/`confirm_test.go`/`api/person.go`/`api/person_test.go`

- confirm/dismiss 加 metric/cycle 分支（cycle 带 supersedes 处理照 attribute；metric 无 supersede）
- API：
  - `GET /api/persons/{id}/metrics?metric_key=&from=&to=`（时序查询，升序；from/to 为 YYYY-MM-DD 解析失败 400）→ `{metrics:[...]}`
  - `POST /api/persons/{id}/metrics`（ManualAddMetric）+ `DELETE .../metrics/{mid}`
  - `GET /api/persons/{id}/cycles` → `{cycles:[...]}`（**响应头加免责不是 JSON 的做法——响应体附 `note: "周期预测为估算，非医疗建议"` 字段**，P3b 前端同款文案）
  - `POST /api/persons/{id}/cycles` + `DELETE .../cycles/{cid}`
  - ListPending 并集扩 metric/cycle（pendingItem 加 MetricKey/CycleType 字段；Value 用 value_text 或 fmt 数值/cycle label）
  - validPendingKinds 加 metric/cycle
- 测试：metrics 时序端点（from/to 过滤）、cycles CRUD+next_predicted 断言、队列 metric/cycle 条目、确认流转；setup 加两 repo + owner 清理

### Task 9: 装配 + 回归

main.go：personMetrics/personCycles repo + Service 两字段 + PersonHandler 两字段；README API 一览（4 组路由 + 队列描述加「指标/周期」）；`make test && make test-integration` 全绿。

---

## 计划自检

1. **覆盖**：spec §4.5/§4.6/§9/§12 P3 后端全落位；图表/折叠 UI 是 P3b。
2. **模式一致性**：metric=「无当前值的纯追加平面」（新形态，闸门最简）；cycle=「attribute 单值模式 + next 计算」；均已声明。
3. **敏感**：本地存（现状即满足）、响应体免责文案、spec §9 引用；加密/权限多用户不在 MVP。
