# person_metric 画像指标平面 设计

- 日期：2026-08-26
- 分支/worktree：`feat/agent-chatbot`
- 范围：新增画像**第 5 个平面 person_metric**（时间序列个人指标：情绪/体重/睡眠/健康等），端到端接入：迁移 + repo + 抽取(LLM) + 闸门 + 手动 CRUD + 前端(列表+趋势曲线) + agent 工具（读 + 提议写）。
- 关联规格：`docs/superpowers/specs/2026-08-24-person-profile-system-design.md` §4.5（person_metric schema 草图）；agent-chatbot spec §P3「曲线可视化增强（接 person_metric）」。
- 模板：**event 平面**（append-only、有时间、跨 stage/api/agent 全链路），person_metric 与之并列同构。
- 现状：无任何 metric 脚手架（纯绿地）；下一个迁移号 **000011**；`prompts/profile_extraction_v2.md:9` 显式排除时序状态、留给本平面。

---

## 核心设计：时间序列 ≠ 单值/事件（6 条硬约束）

现有平面的语义对「连续测点」不适用，person_metric **必须**特殊处理（否则会塌缩/误判）：

1. **Append-only，绝不单值 supersede**：attribute 的「改值=旧行 superseded、只留一条 active」对指标是错的——每个测点都要保留。metric 照 event 的 append 列表语义，**正常写入路径不走 supersede**（`supersedes_id`/`superseded` 仅留给「手动纠错」）。
2. **自然键必须含 measured_at**：身份 = `(person_id, metric_key, measured_at)`。否则同一次抽取里两条同指标读数会塌缩成一条。
3. **DecideMetric 恒 create、无 reaffirm/conflict 分支**：两次不同心情读数不是「互相佐证的同一事实」，是两个数据点。只在 `(metric_key, measured_at, value)` 完全重复时幂等跳过（防重跑重复插）。置信度 active/pending 闸门仍用，但「现值冲突/佐证」分支对指标无意义。
4. **数值 + 单位 + 全精度时间**：event 无数值列、`occurred_at` 被 `parseEventAt` 抹平到 UTC 当日零点。metric 需 `value_num DECIMAL(10,3)` + `value_text` + `unit` + **全精度 `measured_at DATETIME(3)`**（不复用 parseEventAt 的日精度抹平；给不出确切时刻时回退到事件/录音时间或当天，但保留 datetime 列）。
5. **confidence 与 value 分离**：confidence 只表「抽取确定性」，主载荷是 value_num/value_text。不像 event 把 confidence 当 importance 混用。
6. **曲线要数值**：趋势曲线只吃数值序列，故只有 `value_num` 非 NULL 的指标能画线（`value_text`-only 的如「饮食=火锅」只进列表、不进曲线）。

---

## metric_key 目录（catalog，仿 ValidEventTypes/ValidRelations）

`profile` 包加 `MetricDef{Key, Label, Unit, Numeric bool}` + `MetricCatalog map[string]MetricDef` + `ValidMetricKeys`。首批（对齐 spec §4.5，可扩）：

| key | 中文 Label | Unit | 数值? | value_num 语义 | value_text 例 |
|---|---|---|---|---|---|
| `emotion` | 情绪 | （无） | 是 | valence −1..1 | 焦虑/平静/开心 |
| `weight` | 体重 | kg | 是 | 千克 | — |
| `sleep` | 睡眠时长 | h | 是 | 小时 | — |
| `mood_energy` | 精力 | （无） | 是 | 0..1 | 疲惫/充沛 |
| `diet` | 饮食 | （无） | 否 | — | 火锅/清淡 |
| `health` | 健康 | （无） | 否/是 | 可选 | 感冒/头痛 |

校验：`metric_key ∈ ValidMetricKeys`（propose 端 + confirm 端双保险，与 attr/event/relationship 一致）。`Numeric=true` 的键要求 value_num 非空；否则 value_text 非空。

---

## 数据流（端到端，各段与 event 同构）

### 1. 迁移 `migrations/000011_metric.up.sql`（+down）
克隆 `000008_event.up.sql`，person_metric 表：
```sql
CREATE TABLE person_metric (
  id BIGINT PRIMARY KEY,
  user_id BIGINT NOT NULL DEFAULT 1,
  person_id BIGINT NOT NULL,
  metric_key VARCHAR(32) NOT NULL,
  value_num DECIMAL(10,3) NULL,
  value_text VARCHAR(256) NULL,
  unit VARCHAR(16) NULL,
  measured_at DATETIME(3) NOT NULL,          -- 全精度测点时间
  -- 横切字段块（照 event）：
  confidence DECIMAL(4,3) NOT NULL DEFAULT 1.000,
  epistemic_type VARCHAR(16) NOT NULL DEFAULT 'observed',  -- observed|inferred|...
  source VARCHAR(16) NOT NULL DEFAULT 'manual',            -- manual|extract
  status VARCHAR(16) NOT NULL DEFAULT 'active',            -- active|pending|superseded|dismissed
  session_id BIGINT NULL, memory_id BIGINT NULL,
  transcript_segment_ids JSON NULL,
  supersedes_id BIGINT NULL,                 -- 仅手动纠错用
  note VARCHAR(512) NULL,
  version INT NOT NULL DEFAULT 1,
  created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  updated_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  KEY idx_person_metric_time (person_id, metric_key, measured_at),
  KEY idx_person_metric_status (person_id, status)
);
```

### 2. repo `internal/repo/person_metric.go`
克隆 `person_event.go`：`PersonMetric` 结构体 + `PersonMetricRepo`：`CreateExt`/`Get`/`ListByPerson`(ORDER BY measured_at)/`SetStatusExt`/`ListPending` + **`FindByPointExt(ctx,tx,personID,metricKey,measuredAt,valueNum,valueText)`**（自然键含 measured_at+value，供幂等去重，替代 event 的 FindByNaturalKey）。

### 3. 抽取 `internal/profile/fact.go` + prompt v3
- `Fact`/`rawFact` 加 metric 段：`MetricKey/ValueNum *float64/ValueText/Unit/MeasuredAt`。
- `validPlanes` 加 `"metric"`；`ParseFacts` 加 `case "metric"`（校验 metric_key 合法 + Numeric 键要 value_num）。
- `prompts/profile_extraction_v3.md`：加 metric 平面 schema + 示例（情绪/体重/睡眠），**删掉 v2:9 的排除句**；`main.go:128/132` 指向 v3。

### 4. service `internal/profile/service.go` + `gate.go` + `service_manual.go` + `confirm.go`
- `Service` 加 `Metrics *repo.PersonMetricRepo`。
- `applyFact` 分派加 `metric → applyMetricFact`；`applyMetricFact`（仿 applyEventFact，但**恒 create**：先 `FindByPointExt` 命中完全同点则幂等跳过，否则按 `DecideMetric` 定 active/pending 建行 + change_log；无 supersede/reaffirm 分支）。
- `gate.go` 加 `DecideMetric`（仅 autoWritable → active 否则 pending，无冲突分支）。
- `service_manual.go` 加 `ManualAddMetric/ManualAddMetricExt/ManualDeleteMetric`（仿 event：conf=1.0/manual/active + 审计；measured_at 必填）。
- `confirm.go` 的 `ConfirmPending`/`DismissPending` 各加 `case "metric"`。

### 5. api `internal/api/person.go`
- `PersonHandler` 加 `Metrics *repo.PersonMetricRepo`；路由加 `GET/POST/DELETE /api/persons/{id}/metrics[/{mid}]`（仿 events）。
- `personDetailResp` 加 `Metrics []metricView`（按 metric_key 分组 + 每组时间序列）；`Get` 嵌入。
- `validPendingKinds` 加 `"metric"`；`ListPending` 加 metric 循环。

### 6. 前端 `web/app.js` + `web/index.html`
- 画像详情卡「大事记」块之后加**「指标」区块**：按 metric_key 分组，每组显示最新值 + （Numeric 键）一条趋势曲线（复用 `chartGeom(series, labels)` + 克隆 `index.html:892-911` 的 SVG 块，series=该 key 按 measured_at 升序的 value_num、labels=日期）。+ 手动新增表单（仿 event 表单：metric_key 下拉 + value + measured_at）。
- pending 队列渲染加 metric kind。
- proposal 确认卡三处（`PROPOSAL_KINDS`/`TITLES`/`FIELD_LABELS` + TOOL_LABELS）加 `profile_metric`。

### 7. agent `internal/agent/`
- `MCPDeps` 加 `PersonMetrics *repo.PersonMetricRepo`（main 装配）。
- `mcp_profile_tools.go`：`get_metrics`（读某 metric_key 时间序列，或 buildProfileOut 附最近指标）+ `propose_profile_metric`（校验 metric_key + value + measured_at → 建 `Kind:"profile_metric"` pending 提议，绝不写库）。
- `proposals.go` `applyInTx` 加 `case "profile_metric"` → `d.Profile.ManualAddMetricExt(...)`（confirm 单事务 apply-once）。
- `ProposalDeps` 已有 `Profile`——无需新依赖。

### 8. 装配 `cmd/zhiwei-server/main.go`
`personMetrics := &repo.PersonMetricRepo{DB: db}` → 注入 profileSvc / PersonHandler / MCPDeps。

---

## 闸门归属
- **抽取来源**（LLM）：走 profile 自带 pending 队列（auto-active if conf≥ProfileAutoConfidence 否则 pending），与 attr/event/rel 一致。
- **agent 来源**：走 agent_proposal 闸门 + 聊天确认卡（`propose_profile_metric`→confirm 单事务 apply-once），与我已建的 propose_profile_attr/event/relationship 一致。

## 明确不做（YAGNI / 范围外）
- 不把 metric 注入对话上下文头（event 也没注入，保持一致；后续可选）。
- 不做周报趋势与 metric 的融合（周报趋势另有其活动量语义；本平面是「个人画像指标」，曲线在画像页）。
- 首批 catalog 6 个 key，不做任意自定义 key（校验拒绝目录外）。
- 不做 person_cycle（周期，敏感，P3 独立）/ person_activity（P4）。

## 验收
- 抽取：对话里「我今天心情很焦虑/体重 70kg/睡了 5 小时」→ 抽出 metric 事实 → 高置信 active、低置信 pending。
- append-only：同一天两条体重读数各建一行（不塌缩）；重跑抽取不重复插（自然键含 measured_at+value 幂等）。
- 手动 + agent 两条闸门都能落库；confirm apply-once。
- 前端画像页显示指标分组 + 数值 key 的趋势曲线；pending 队列可确认/放弃。
- agent 能 get_metrics 读趋势、propose_profile_metric 提议记录。
- `go build ./...` + vet + 各包测试 + fresh 库 migrate 000001–000011 全过。

## 需你定夺的设计岔口（我已给默认，可否决）
1. **metric_key 首批目录**：上表 6 个（emotion/weight/sleep/mood_energy/diet/health）。要加/减？
2. **曲线位置**：画像详情页「指标」区块（默认），不进周报。
3. **闸门**：抽取走 profile pending 队列、agent 走 agent 闸门（双轨，默认，与现有一致）。
4. **measured_at 缺省**：LLM 给不出确切时刻时，回退到 session/录音时间或抽取当天 00:00（保留 datetime 列，不同于 event 的纯日精度）。
