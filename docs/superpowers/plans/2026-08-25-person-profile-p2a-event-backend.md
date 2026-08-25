# 用户画像 P2a（大事记后端）实现计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 画像系统 P2 的后端：新增 **event 平面**（person_event 表 + repo + LLM fact 解析扩展 + 闸门 + Service 落库 + API），支撑「大事记」——结婚/毕业/升学/生子/旅行/聚会/会议/生病/学会…有日期的人生事件。媒体史（书/影视/音乐/游戏）的 list 属性 key 在 P1 catalog 已有，P1b datalist 已可录入，无需后端改动。

**Architecture:** 完全复用 P1a 的平面扩展模式（attribute/relationship → 加 event）：fact.go 加 event 平面字段与解析；gate 加 DecideEvent（事件天然追加、无冲突路径，同 relationship 模式）；Service 加 applyEventFact；确认队列/pending 并集/确认放弃扩 event kind；详情响应加 events。**LLM prompt 升 v2**（加 event 平面说明）。

**Tech Stack:** 同 P1a（Go/chi/sqlx/MySQL/雪花 ID）。迁移编号 **000008**（000007 已被 main 的 segment_embedding 占用）。

**设计决策（沿用 spec §4.4，两处 MVP 简化）：**
1. `related_person_ids` JSON 列存单元素列表或空——LLM fact 只给**单个** Related 指代（多人事件取最主要的，多对多留后续）；解析不到就空。
2. `occurred_at` 解析链：RFC3339 → `2006-01-02` → `2006-01` → 失败置 NULL（标题里常含时间信息，事件仍创建；精度问题前端展示兜底）。
3. event_type 枚举 9 种（spec §4.4）：里程碑|聚会|会议|旅行|健康|成就|挫折|负面|其他。
4. 闸门：事件天然多条追加（同 relationship 无冲突路径）；自然键 (session, person, event_type, title)；同键 active → 佐证（不 bump 置信，事件无置信佐证语义，touch 注释对齐 relationship 的「审计即佐证」）；按置信度 active/pending。

**工作目录：** worktree `.worktrees/person-event`（分支 `feat/person-event`），`cd /Users/jyxc-dz-0100360/work/fun/zhiwei-water18-0822/.worktrees/person-event`

**测试命令约定：** 同 P1a（`make test` 单元；集成 `make init-testdb` + `TEST_MYSQL_DSN="zhiwei:zhiwei@tcp(127.0.0.1:3307)/zhiwei_test?parseTime=true&charset=utf8mb4&multiStatements=true" go test -p 1 ./internal/xxx/ -count=1`；testdata/speech.wav 已复制）。

---

### Task 1: 迁移 000008_event

**Files:** Create `migrations/000008_event.up.sql` + `.down.sql`

- [ ] up（spec §4.4 schema + 横切字段）：

```sql
-- 画像 P2：event 平面（人物大事记，spec §4.4）。
-- 有日期的一次性事件（结婚/毕业/旅行/聚会/会议/生病/学会…）；
-- 与 list 属性（看过的书等速览）互补：属性记「有过的」，event 记「某次发生的」。
CREATE TABLE person_event (
  id            BIGINT PRIMARY KEY,
  user_id       BIGINT NOT NULL DEFAULT 1,
  person_id     BIGINT NOT NULL,
  event_type    VARCHAR(32) NOT NULL,              -- 里程碑|聚会|会议|旅行|健康|成就|挫折|负面|其他
  title         VARCHAR(512) NOT NULL,
  description   TEXT NULL,
  occurred_at   DATETIME(3) NULL,                  -- 事件发生时间（可能只精确到日/月，解析失败为 NULL）
  end_at        DATETIME(3) NULL,                  -- 跨天事件（旅行/会议）
  location      VARCHAR(256) NULL,
  related_person_ids JSON NULL,                    -- 同场人物（MVP 单元素或空，见计划头决策 1）
  importance    DECIMAL(5,4) NOT NULL DEFAULT 0.5,
  -- 横切字段（与 attribute/relationship 平面一致，spec §3）
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
  KEY idx_person_time (person_id, occurred_at),
  KEY idx_user_status (user_id, status),
  KEY idx_session (session_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
```

- [ ] down：`DROP TABLE IF EXISTS person_event;`
- [ ] `make compose-up && make init-testdb` 验证
- [ ] Commit: `feat(profile): 迁移 000008_event——人物大事记表`

### Task 2: repo——PersonEventRepo

**Files:** Create `internal/repo/person_event.go` + `internal/repo/person_event_test.go`

结构体（字段同迁移列；RelatedPersonIDs ids.List；OccurredAt/EndAt/Location *string/*time.Time 可空）+ 方法（Ext 模式、(nil,nil)、零值兜底同兄弟 repo）：
- CreateExt/Create（INSERT 全 20 列）
- Get
- ListByPerson(ctx, personID)（全状态 ORDER BY occurred_at DESC, id DESC——时间倒序的大事记）
- FindActiveByKeyExt(ctx, ext, personID, eventType, title)（单值型当前 active 行：person_id+event_type+title+status='active'）
- FindByNaturalKeyExt(ctx, ext, sessionID, personID, eventType, title)（幂等：任意 status）
- SetStatusExt/SetStatus
- ListPending(ctx, userID)
- **手动 CRUD 不建 repo 方法**（走 Service.ManualAddEvent 等，见 Task 6——与 attribute 模式一致：Service 持有 repo 组合）

测试：单值查询命中/未命中、自然键命中、ListByPerson 时间倒序、ListPending、SetStatus、Get 的 (nil,nil)。

集成测试命令与提交模式同 P1a。Commit: `feat(profile): PersonEventRepo（时间倒序/自然键/…）`

### Task 3: fact.go 扩 event 平面

**Files:** Modify `internal/profile/fact.go` + `fact_test.go`

- Fact struct 加 event 平面字段：

```go
	// ---- event 平面（P2 大事记）----
	EventType   string // 里程碑|聚会|会议|旅行|健康|成就|挫折|负面|其他
	Title       string
	Description string
	OccurredAt  string // 原始字符串（YYYY-MM-DD / YYYY-MM / RFC3339），解析放 service
	EndAt       string
	Location    string
```

- rawFact 加对应 json 标签：event_type/title/description/occurred_at/end_at/location
- `validPlanes` 加 `"event": true`
- 新校验表 `validEventTypes`（9 项）+ 导出 `ValidEventTypes`
- ParseFacts 的 switch 加 event 分支：`if !validEventTypes[f.EventType] || f.Title == "" { continue }`（title 必填；occurred_at 允许空）
- factKey 加 EventType+Title 判别字段（防批内塌缩）
- 测试：合法 event fact 解析（含 trim）、非法 event_type/空 title 丢弃、markdown 围栏容错

Commit: `feat(profile): Fact 扩 event 平面（9 类型枚举/occurred_at 原始串）`

### Task 4: gate.go 加 DecideEvent

**Files:** Modify `internal/profile/gate.go` + `gate_test.go`

```go
// DecideEvent 事件闸门：事件天然多条追加（同 relationship 无冲突路径）——
// 同键（person,类型,title）已 active → 佐证（事件无置信佐证语义，持久化效果=审计，
// 同 relationship reaffirm 模式）；新键按置信度 create。
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
```

测试：active/skip/reaffirm/低置信 pending/默认阈值。Commit: `feat(profile): DecideEvent 事件闸门`

### Task 5: prompt 升 v2（event 平面说明）

**Files:** Create `prompts/profile_extraction_v2.md`；Modify `cmd/zhiwei-server/main.go` prompt 路径

- v2 = v1 全文 + 以下增补：
  - 「只抽」清单更新：一次性**事件**（带日期的人生大事）**现在要抽**（plane=event）
  - plane 枚举：`attribute` / `relationship` / `event`
  - event 字段说明：event_type 9 枚举、title（短句概括）、description（可选细节）、occurred_at（YYYY-MM-DD 尽量给；只知道月份给 YYYY-MM；不确定留空）、end_at（跨天）、location、related（同场主要人物 subject，可选）
  - 属性目录不变
  - 示例加一条：`{"plane":"event","subject":{"kind":"self"},"event_type":"旅行","title":"去云南旅游一周","occurred_at":"2026-07-20","end_at":"2026-07-27","location":"云南","confidence":0.9,"epistemic_type":"observed","block_index":2}`
- main.go 的 prompt 路径改 `prompts/profile_extraction_v2.md`
- Commit: `feat(profile): 画像抽取 prompt v2——event 大事记平面`

### Task 6: Service 扩 event——applyEventFact + ManualAddEvent/ManualDeleteEvent

**Files:** Modify `internal/profile/service.go`（struct 加 Events 依赖 + applyFact 分流 + applyEventFact + eventRow/eventLog 构造 + parseEventAt 工具）；Create `internal/profile/service_manual.go` 的事件段（追加到文件末尾）；Modify `service_test.go`（扩 TestApplyFactsGatePaths 或新增 TestApplyEventFacts）

核心逻辑：

```go
// parseEventAt 尽力解析事件时间：RFC3339 → YYYY-MM-DD → YYYY-MM；失败返回零值（调用方存 NULL）。
func parseEventAt(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	for _, layout := range []string{time.RFC3339, "2006-01-02", "2006-01"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}
```

- applyFact 分流：`if f.Plane == "event" { return s.applyEventFact(...) }`
- applyEventFact：resolveSubject（subject 必解析到，0 → Skipped）；related 解析（可 0，RelatedPersonIDs 空）；FindActiveByKeyExt/FindByNaturalKeyExt（键 = type+title）；DecideEvent 决策执行（Skip/Reaffirm=审计/Active/Pending）；eventRow 构造（occurred_at/end_at 经 parseEventAt，失败 NULL；importance 用 f.Confidence 代——MVP 简化，注释说明）；事件日志构造 createEventLog
- ManualAddEvent(ctx, personID, eventType, title, description, occurredAt, endAt, location, relatedPersonID)（校验 ValidEventTypes；active/manual conf=1.0 + 审计）
- ManualDeleteEvent(ctx, id)（dismissed + 审计）
- Service struct 加 `Events *repo.PersonEventRepo`
- 测试 TestApplyEventFacts：高置信旅行事件→active（occurred_at 解析成功）；低置信→pending；同键重跑→skip；同键另一 session→reaffirm；occurred_at 烂串→NULL 仍创建；手动加/删事件
- Commit: `feat(profile): Service 扩 event 平面——大事记落库+手动 CRUD`

### Task 7: confirm.go 扩 event kind + 详情响应加 events

**Files:** Modify `internal/profile/confirm.go`（ConfirmPending/DismissPending 加 "event" 分支）；`internal/profile/confirm_test.go` 补事件确认用例

- event 分支：仅 pending 可确认；→active + confirm 审计；Dismiss → dismissed + 审计（模式照抄 relationship 分支）
- 测试：event pending 确认/放弃
- Commit: `feat(profile): 确认队列扩 event kind`

### Task 8: API——events 端点 + pending 并集 + 详情 events + 手动事件

**Files:** Modify `internal/api/person.go` + `person_test.go`

- `GET /api/persons/{id}/events`（ListByPerson 全状态，status 过滤 query 可选）→ `{events:[...]}`；handler struct 加 Events 依赖
- `POST /api/persons/{id}/events`（ManualAddEvent；event_type 校验 400）
- `DELETE /api/persons/{id}/events/{eid}`（ManualDeleteEvent；404）
- ListPending 加 event 平面段（kind="event"；Value=title；补 event_type/occurred_at 字段进 pendingItem）
- pendingItem 加 `EventType string \`json:"event_type,omitempty"\`` 与 `OccurredAt *time.Time \`json:"occurred_at,omitempty"\``
- 详情 personDetailResp 加 `Events []repo.PersonEvent \`json:"events"\``（active+pending 过滤，时间倒序——直接 ListByPerson 后过滤）
- 测试：events 端点列表/手动建事件 400 合法路径/详情含 events/pending 含 event 条目并确认
- Commit: `feat(profile): 人物大事记 API——events 端点+队列并集+详情`

### Task 9: 装配 + 全量回归

**Files:** Modify `cmd/zhiwei-server/main.go`（repo 装配 personEvents + Service.Events + PersonHandler.Events）；`README.md`（API 一览加 3 条 events 路由）

- main.go：`personEvents := &repo.PersonEventRepo{DB: db}`；Service 与 PersonHandler 的 Events 字段接线
- `make test && make test-integration` 全绿
- Commit: `feat(profile): main 装配 event 平面 + README`

---

## 计划自检

1. **覆盖**：spec §4.4 schema/§12 P2 后端范围全落位（迁移/repo/解析/闸门/编排/确认/API/装配）；媒体史 list 属性无需改动（catalog 已有）。
2. **一致性**：平面扩展完全对齐 attribute/relationship 既有模式（Ext/nil,nil/横切字段/审计/闸门决策序）。
3. **P2b 前端**（下一计划）：人物详情大事记时间线（按年分组）+ 队列 event 条目 + 手动加事件表单——本计划不涉及。

## 执行交接

沿用 Subagent-Driven：每任务实现 + spec/质量双审；prompt v2 的真实 LLM 效果由用户冒烟验证（不进 CI）。
