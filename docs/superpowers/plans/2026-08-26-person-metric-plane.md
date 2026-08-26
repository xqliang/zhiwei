# person_metric 画像指标平面 实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: 用 superpowers:subagent-driven-development 逐任务执行；步骤用 `- [ ]` 复选框跟踪。

**Goal:** 新增画像第 5 平面 person_metric（时间序列个人指标），端到端接入抽取/闸门/手动/前端曲线/agent。

**Architecture:** 与 event 平面同构，但按 6 条「连续时序」硬约束特殊处理（append-only、自然键含 measured_at、DecideMetric 恒 create、数值+单位+全精度时间、confidence≠value、数值才画线）。收敛点在 `profile.Service.ApplyFacts`；stage 层零改动。

**Tech Stack:** Go(chi/sqlx/MySQL8/snowflake)、golang-migrate、Vue3 CDN、Ark LLM 抽取、MCP。

关联设计：`docs/superpowers/specs/2026-08-26-person-metric-plane-design.md`（含 6 硬约束、catalog、闸门归属）。

**执行波次（按 Go 包编译依赖）**：
- Wave 0（协调者）：T1 迁移 + apply 到测试库。
- Wave A：T2 repo（其它包都 import 它，须先行）。
- Wave B：T3 profile 平面（catalog+fact+service+gate+manual+confirm+prompt，单包，依赖 repo）。
- Wave C（并行）：T4 api + T5 agent（都依赖 profile.Service/repo，互不同包）。
- Wave D：T6 前端（协调者或单 agent，无并发）+ T7 main 装配（协调者）。

**约束**：各任务只改各自包、只 test 自己包、不跑 git、不碰 `main.go`/`web/*`（除 T6）。隔离库 `zhiwei_agentchat_test`（`TEST_MYSQL_DSN`，Wave 0 后已到 000011）+ `t.Cleanup`。中文详细注释。

---

## Task 1 — 迁移 000011_metric（协调者 Wave 0）

**Files:** Create `migrations/000011_metric.up.sql`, `migrations/000011_metric.down.sql`

- [ ] up.sql（克隆 000008_event 的横切字段块 + metric 专属列）：

```sql
-- person_metric：画像第 5 平面（时间序列个人指标：情绪/体重/睡眠等）。
-- 与 person_event 同构，但为「连续测点」特化：数值列 value_num + 单位 unit +
-- 全精度 measured_at；append-only（每测点一行，不单值 supersede）；
-- 自然键 (person_id, metric_key, measured_at) 含时间，同次抽取多读数不塌缩。
CREATE TABLE person_metric (
  id BIGINT PRIMARY KEY,
  user_id BIGINT NOT NULL DEFAULT 1,
  person_id BIGINT NOT NULL,
  metric_key VARCHAR(32) NOT NULL,            -- emotion|weight|sleep|mood_energy|diet|health（catalog）
  value_num DECIMAL(10,3) NULL,               -- 数值（体重kg/情绪-1..1/睡眠h）；曲线只画非空者
  value_text VARCHAR(256) NULL,               -- 类别描述（情绪='焦虑'/饮食='火锅'）
  unit VARCHAR(16) NULL,                       -- kg|h|…
  measured_at DATETIME(3) NOT NULL,            -- 测点时间（全精度，勿抹平到当天）
  confidence DECIMAL(4,3) NOT NULL DEFAULT 1.000,
  epistemic_type VARCHAR(16) NOT NULL DEFAULT 'observed',
  source VARCHAR(16) NOT NULL DEFAULT 'manual',      -- manual|extract
  status VARCHAR(16) NOT NULL DEFAULT 'active',       -- active|pending|superseded|dismissed
  session_id BIGINT NULL,
  memory_id BIGINT NULL,
  transcript_segment_ids JSON NULL,
  supersedes_id BIGINT NULL,                   -- 仅手动纠错用（正常写入不置）
  note VARCHAR(512) NULL,
  version INT NOT NULL DEFAULT 1,
  created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  updated_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  KEY idx_person_metric_time (person_id, metric_key, measured_at),
  KEY idx_person_metric_status (person_id, status)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
```
- [ ] down.sql：`DROP TABLE IF EXISTS person_metric;`
- [ ] 协调者：`migrate -path migrations -database "mysql://root:root@tcp(127.0.0.1:3307)/zhiwei_agentchat_test" up` → 到 000011；并对全新库验证 000001–000011 连续 migrate 通过。

---

## Task 2 — repo person_metric.go（Wave A）

**Files:** Create `internal/repo/person_metric.go`, `internal/repo/person_metric_test.go`。模板：`internal/repo/person_event.go`。

- [ ] `PersonMetric` 结构体（db tag 对齐上表所有列）：
```go
type PersonMetric struct {
	ID         ids.ID   `db:"id"`
	UserID     int64    `db:"user_id"`
	PersonID   ids.ID   `db:"person_id"`
	MetricKey  string   `db:"metric_key"`
	ValueNum   *float64 `db:"value_num"`   // 可空：value_text-only 指标
	ValueText  string   `db:"value_text"`
	Unit       string   `db:"unit"`
	MeasuredAt time.Time `db:"measured_at"`
	Confidence    float64 `db:"confidence"`
	EpistemicType string  `db:"epistemic_type"`
	Source        string  `db:"source"`
	Status        string  `db:"status"`
	SessionID *ids.ID `db:"session_id"`
	MemoryID  *ids.ID `db:"memory_id"`
	TranscriptSegmentIDs ids.List `db:"transcript_segment_ids"`
	SupersedesID *ids.ID `db:"supersedes_id"`
	Note      string    `db:"note"`
	Version   int       `db:"version"`
	CreatedAt time.Time `db:"created_at"`
	UpdatedAt time.Time `db:"updated_at"`
}
```
（`value_text`/`unit`/`note` 用值类型 string——列 NOT NULL? 上表 value_text/unit/note 是 NULL 列。sqlx safe 模式扫 NULL 进 string 会报错 → **这三列改用 `*string` 或在 SELECT 里 COALESCE**。选 `*string` 最简：`ValueText/Unit/Note *string`。实现者据此定字段类型，与既有 person_event 的可空处理一致。)

- [ ] `PersonMetricRepo struct{ DB *sqlx.DB }` + 方法（仿 person_event.go，`ExecerContext`/`QueryerContext`）：
  - `CreateExt(ctx, ext, *PersonMetric) error`（id 为空则 `ids.New()`；INSERT 全列）
  - `Get(ctx, id) (*PersonMetric, error)`
  - `ListByPerson(ctx, personID) ([]PersonMetric, error)`（`WHERE person_id=? AND status IN ('active','pending') ORDER BY metric_key, measured_at`）
  - `SetStatusExt(ctx, ext, id, status) error`
  - `ListPending(ctx, userID) ([]PersonMetric, error)`（`status='pending'`）
  - **`FindByPointExt(ctx, ext, personID, metricKey, measuredAt, valueNum *float64, valueText string) (*PersonMetric, error)`**：自然键去重——`WHERE person_id=? AND metric_key=? AND measured_at=? AND status!='dismissed'` 且 value 相等（value_num 用 `<=>` NULL 安全等或分支）；命中返回，供幂等跳过。
- [ ] 测试（隔离库 + `testDB(t)` + `t.Cleanup`）：Create→Get 往返（含 value_num 空/非空）；ListByPerson 按 measured_at 序；**同 person+key 两个不同 measured_at 各建一行、都在 ListByPerson**（锁定 append-only）；FindByPointExt 完全同点命中、value 不同不命中。
- [ ] 验证：`go build ./internal/repo/`、`TEST_MYSQL_DSN=… go test ./internal/repo/ -run PersonMetric -count=1`、`gofmt -l`。（勿跑整包，避开 TestJobLifecycle 自旋。）

---

## Task 3 — profile 平面（catalog + fact + service + gate + manual + confirm + prompt）（Wave B）

**Files:** Create `internal/profile/metric.go`（catalog）；Modify `internal/profile/fact.go`、`service.go`、`gate.go`、`service_manual.go`、`confirm.go`；Create `prompts/profile_extraction_v3.md`；Modify tests。依赖 Task 2 repo。

### 3a. catalog `internal/profile/metric.go`
```go
package profile

// MetricDef 描述一个画像指标键（仿 event 的 ValidEventTypes 目录化）。
type MetricDef struct {
	Key     string
	Label   string // 中文名
	Unit    string // 单位，无则 ""
	Numeric bool   // true=要求 value_num；false=value_text
}

var MetricCatalog = map[string]MetricDef{
	"emotion":     {"emotion", "情绪", "", true},       // value_num valence −1..1
	"weight":      {"weight", "体重", "kg", true},
	"sleep":       {"sleep", "睡眠时长", "h", true},
	"mood_energy": {"mood_energy", "精力", "", true},   // 0..1
	"diet":        {"diet", "饮食", "", false},
	"health":      {"health", "健康", "", false},
}

func ValidMetricKey(k string) bool { _, ok := MetricCatalog[k]; return ok }
func MetricDefOf(k string) MetricDef { return MetricCatalog[k] } // 目录外返回零值（调用方先 ValidMetricKey）
```
测试 `metric_test.go`：ValidMetricKey 命中/不命中；Numeric 标志正确。

### 3b. fact.go
- `Fact`（`fact.go:23`）加 metric 段：`MetricKey string`、`ValueNum *float64`、`ValueText string`（注意与 attribute 的 Value 区分，metric 专用）、`Unit string`、`MeasuredAt string`（原始字符串，applyMetricFact 里解析）。
- `rawFact`（`fact.go:87`）加对应 json tag 字段：`metric_key`/`value_num`/`value_text`/`unit`/`measured_at`。
- `validPlanes`（`fact.go:56`）加 `"metric": true`。
- `ParseFacts` 的 switch（`fact.go:161`）加 `case "metric"`：校验 `ValidMetricKey(MetricKey)`；`MetricDefOf(key).Numeric` 为 true 时要求 `ValueNum != nil`，否则要求 `ValueText != ""`（二者按 def 至少一）。非法条目跳过（与既有条目级校验一致）。
- 测试：metric 事实解析通过；非法 key / Numeric 键缺 value_num → 跳过。

### 3c. service.go
- `Service` 结构体（`service.go:23`）加 `Metrics *repo.PersonMetricRepo`。
- `applyFact`（`service.go:104`）分派加：`if f.Plane == "metric" { return s.applyMetricFact(ctx, tx, personID, f, prov) }`（在 event/relationship 分支旁）。
- 新增 `applyMetricFact`（仿 `applyEventFact` `service.go:247`，但**按 6 硬约束特化**）：
  1. 解析 `measured_at`：新增 `parseMetricAt(f.MeasuredAt, fallback time.Time) time.Time`——能解析 RFC3339/`YYYY-MM-DD HH:MM`/`YYYY-MM-DD` 就用（**保留时刻精度**），否则回退到 fallback（传入 session/录音时间或抽取当天；调用方给）。**不复用 parseEventAt（那个抹平到当天零点）**。
  2. **幂等去重**：`Metrics.FindByPointExt(tx, personID, key, measuredAt, valueNum, valueText)` 命中 → 直接 return（不重复插，无 reaffirm）。
  3. 否则 `DecideMetric(f.Confidence, f.EpistemicType)` 定 status（active/pending）→ 构 `metricRow`（conf=f.Confidence、source=extract、value_num/value_text/unit/measured_at）→ `Metrics.CreateExt` → `createMetricLog`（change_log，entity_kind='metric'，new_value=metric_key+值摘要）。**无 supersede/conflict 分支**。
- 新增 `metricRow(...)` 行构造 + `createMetricLog(...)`（仿 `eventRow` `service.go:456` / `createEventLog` `service.go:484`）。
- 注意约束 5：confidence 存 f.Confidence（抽取确定性），不塞进 value。

### 3d. gate.go
- 新增 `DecideMetric(confidence float64, epistemic string) string`（仿 `DecideEvent` `gate.go:82`，但**只有 autoWritable→"active" 否则 "pending"，无冲突/现值分支**）。复用 `autoWritable`（`gate.go:33`）。

### 3e. service_manual.go
- `ManualAddMetric(ctx, personID, metricKey string, valueNum *float64, valueText, unit string, measuredAt time.Time) (*repo.PersonMetric, error)`（自持事务 → Ext → Commit）。
- `ManualAddMetricExt(ctx, tx, ...同参...) (*repo.PersonMetric, error)`（仿 `ManualAddEventExt` `service_manual.go:316`）：校验 `ValidMetricKey`（非法报错）+ Numeric 键要 valueNum（否则要 valueText）；构行 conf=1.0/source=manual/epistemic=observed/status=active + `ChangeLogs.CreateExt`（entity_kind='metric', changed_by='user'）。**measured_at 必填**（零值报错）。
- `ManualDeleteMetric(ctx, id) error`（→ status=dismissed + delete 审计，仿 `ManualDeleteEvent` `service_manual.go:359`）。

### 3f. confirm.go
- `ConfirmPending`（`confirm.go:21` switch）加 `case "metric"`（仿 event case `confirm.go:110`：读 pending 行→置 active；metric **无 supersedes 旧行**处理，直接置 active 即可）。
- `DismissPending`（`confirm.go:160` switch）加 `case "metric"`（置 dismissed）。

### 3g. prompt `prompts/profile_extraction_v3.md`
- 基于 v2 复制；**删掉 v2:9 的「情绪、健康等时序状态（P3…本版忽略）」排除句**；在平面 schema 里加 `metric` 平面：字段 `plane:"metric"`, `metric_key`(emotion|weight|sleep|mood_energy|diet|health), `value_num`(数值), `value_text`(类别描述), `unit`, `measured_at`(能给出的时刻/日期)；给 2-3 个示例（「今天心情很焦虑」→emotion value_num≈-0.6 value_text焦虑；「体重 70 公斤」→weight value_num 70 unit kg；「昨晚睡了 5 小时」→sleep value_num 5 unit h measured_at 昨天）。版本号 = `profile_extraction_v3`。
- （main.go 指向 v3 由 Task 7 协调者做。）

- [ ] 验证：`go build ./internal/profile/`、`TEST_MYSQL_DSN=… go test ./internal/profile/ -count=1`（含既有用例不回归）、`gofmt -l`。测试新增：applyMetricFact append-only（两点两行）、幂等去重、DecideMetric active/pending、ManualAddMetric 落 active+审计、confirm/dismiss metric pending。

---

## Task 4 — api person.go metrics 端点（Wave C，与 Task 5 并行）

**Files:** Modify `internal/api/person.go`；extend `internal/api/person_test.go`。依赖 Task 2/3。模板：event 端点（ListEvents `:471` / AddEvent `:496` / DeleteEvent `:542`）。

- [ ] `PersonHandler`（`person.go:20`）加 `Metrics *repo.PersonMetricRepo`。
- [ ] 路由（仿 events `:41-43`）：`GET /api/persons/{id}/metrics`（ListMetrics→`Metrics.ListByPerson`，支持 `?metric_key=`/`?status=` 过滤）、`POST /api/persons/{id}/metrics`（AddMetric→`Service.ManualAddMetric`；body: metric_key/value_num/value_text/unit/measured_at）、`DELETE /api/persons/{id}/metrics/{mid}`（DeleteMetric→`Service.ManualDeleteMetric`）。
- [ ] 详情页嵌入：`personDetailResp`（`person.go:119`）加 `Metrics []metricGroup`（按 metric_key 分组：`{key,label,unit,numeric,points:[{measured_at,value_num,value_text,status}]}`，points 按 measured_at 升序，供前端画线）；`Get`（`person.go:189` 附近）读 `Metrics.ListByPerson` 填充（只 active+pending，计 pending 数）。
- [ ] pending 队列：`validPendingKinds`（`person.go:58`）加 `"metric"`；`ListPending`（`person.go:646`）加 metric 循环（`Metrics.ListPending` → pendingItem，展示 metric_key+值+measured_at）。
- [ ] 测试：POST 建 metric（active）→ GET 详情含该 metric 分组；DELETE → dismissed；pending metric 出现在 /api/profile/pending。
- [ ] 验证：`go build ./internal/api/`、相关 test（`-run Metric`）、`gofmt -l`。

---

## Task 5 — agent 工具（Wave C，与 Task 4 并行）

**Files:** Modify `internal/agent/mcp_server.go`、`mcp_profile_tools.go`、`proposals.go`；extend `internal/agent/proposals_test.go`。依赖 Task 2/3。模板：propose_profile_event（`mcp_profile_tools.go:223`）+ applyInTx profile_event（`proposals.go:278`）。

- [ ] `MCPDeps`（`mcp_server.go`）加 `PersonMetrics *repo.PersonMetricRepo`。
- [ ] `mcp_profile_tools.go`：
  - `get_metrics`（读工具）：入参 `metric_key`(可选，空=全部)；读 `d.PersonMetrics.ListByPerson(owner.ID)`，按 key 分组返回时间序列（含 value_num/value_text/measured_at/status）。
  - `propose_profile_metric`（写-提议，仿 propose_profile_event）：入参 `metric_key`/`value_num`(可选)/`value_text`(可选)/`unit`(可选)/`measured_at`(可选)/`rationale`。校验 `profile.ValidMetricKey` + Numeric 键要 value_num（否则 value_text）；`GetOwner`；构 `{new:{metric_key,value_num,value_text,unit,measured_at}}`；`proposeAndReturn(Kind:"profile_metric", TargetKind:"profile", TargetID:&owner.ID)`。**绝不写库**。
- [ ] `proposals.go` `applyInTx`（`:142` switch）加 `case "profile_metric"`：校验 metric_key（双保险）→ 解析 measured_at（缺省用当前时间或 now）→ `d.Profile.ManualAddMetricExt(ctx, tx, *p.TargetID, key, valueNum, valueText, unit, measuredAt)` → 返回 `&row.ID`。（`ProposalDeps.Profile` 已有，无需新依赖。value_num 从 payload 解析：注意 JSON 数字进 `map[string]any` 是 float64。）
- [ ] 测试（proposals_test.go）：propose_profile_metric 零写入 + 非法 key/Numeric 缺值报错；confirm → owner 新增 active metric + apply-once（重复 confirm 不重复）。
- [ ] 验证：`go build ./...`、`TEST_MYSQL_DSN=… go test ./internal/agent/ -run 'ProfileMetric|Metric' -count=1`、`gofmt -l`。

---

## Task 6 — 前端（Wave D，协调者或单 agent，无并发）

**Files:** Modify `web/app.js`、`web/index.html`（末尾 `bash scripts/hash-web.sh` + `node --check`）。模板：event 前端三件套（渲染 `index.html:1323-1352` / 表单 `:1354-1374` / 状态+handler `app.js:1216-1285`）+ 曲线 `chartGeom` `app.js:2036` + SVG 块 `index.html:892-911`。

- [ ] 画像详情卡「大事记」块之后加**「指标」区块**：`personDetail.metrics`（来自详情 API）按 key 分组渲染；每组标题=Label(+unit)，列出测点（measured_at + value_num/value_text + 状态 chip）；**Numeric 组**额外渲染一条趋势曲线：`chartGeom(points.map(p=>p.value_num), points.map(p=>p.measured_at))` + 克隆 SVG polyline 块。
- [ ] 手动新增表单（仿 addEventForm）：metric_key 下拉（从一份前端 catalog 常量，与后端 6 键一致）+ value_num/value_text/unit/measured_at 输入 → POST `/api/persons/{id}/metrics`；删除按钮 → DELETE。
- [ ] pending 队列渲染加 metric kind（若前端有 pending 列表 UI）。
- [ ] proposal 确认卡：`PROPOSAL_KINDS`/`PROPOSAL_TITLES`(「记录指标」)/`PROPOSAL_FIELD_LABELS`(metric_key/value_num/value_text/unit/measured_at) + `TOOL_LABELS`(propose_profile_metric) 加 `profile_metric`（三处 kind 串与后端一致）。
- [ ] `node --check web/app.js` + `bash scripts/hash-web.sh`。

---

## Task 7 — main 装配（协调者，最后）

**Files:** Modify `cmd/zhiwei-server/main.go`

- [ ] `personMetrics := &repo.PersonMetricRepo{DB: db}`（仿 `:76` personEvents）。
- [ ] 注入：`profileSvc.Metrics = personMetrics`（或构造处加字段，`:172` 附近）；`PersonHandler{... Metrics: personMetrics}`（`:242` 附近）；`MCPDeps{... PersonMetrics: personMetrics}`（`:276` 附近）。
- [ ] prompt 指向 v3：`main.go:128`（promptPath 或 profile 抽取 prompt 变量）改 `prompts/profile_extraction_v3.md`，版本号自动从文件名截取（`:132`）。
- [ ] `go build ./...` + `go vet ./...` 全绿；fresh 库 migrate 000001–000011 通过。

---

## 验收 & 自检
- 抽取：「心情焦虑/体重70kg/睡5小时」→ metric 事实 → 高置信 active、低置信 pending。
- **append-only**：同 key 不同 measured_at 各一行；重跑不重复插（FindByPointExt 幂等）。
- 手动 + agent 双闸门落库；confirm apply-once。
- 前端画像页显示指标分组 + Numeric 组趋势曲线；pending 可确认/放弃。
- agent get_metrics 读趋势、propose_profile_metric 提议（走 agent 闸门确认卡）。
- kind 串 `profile_metric` / plane `metric` / pending kind `metric` 三处/多处一致。
- `go build ./...`+vet+各包 test + fresh 库 000001–000011 全过。
- 6 硬约束落实自查：无单值 supersede、自然键含 measured_at、DecideMetric 恒 create、value_num+unit+全精度 measured_at、confidence≠value、曲线仅 Numeric。
