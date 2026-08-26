# 用户画像 P4（生活轨迹：activity 平面）实现计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 补齐画像第六个数据平面 person_activity（生活轨迹源：什么时间/多长时间/什么工具/做什么/地点/通勤），打通 迁移→repo→抽取闸门→确认队列→API→前端时间线 全链路。

**Architecture:** activity = **测点流语义**（同 metric 平面）：追加式、无「当前值」、无冲突/佐证路径——纯置信闸门 + 自然键幂等防重跑。spec §4.7 DDL 落地；`Fact`/`ParseFacts`/`factKey`/`DecideActivity`/`applyActivityFact`/`confirm.go` 六处按平面 switch 对称演进（新增平面须同时加 case 的既有约定）。**独处时间派生指标不实现**（spec §399：activity 无同场人物字段、数据稀疏时不展示——记入 spec §13 跟进项）。前端为**时间线列表**而非曲线：活动是类别身份，画成连续线是撒谎（dataviz skill 结论，P3b 已引用）。

**Tech Stack:** Go chi+sqlx+雪花 ID / MySQL 迁移 000010 / Vue3 CDN 无构建 / 集成测试共库 zhiwei_test（make init-testdb + t.Cleanup）。

**工作目录：** worktree `.worktrees/person-activity`（分支 `feat/person-activity`，基线 main=502473b）。dev 端口 **8081**。

**契约（本计划新增）：**
- `GET /api/persons/{id}/activities?from=&to=` → `{activities:[...]}`（升序，全状态；行含 `id/activity/tool?/location?/commute_mode?/started_at/duration_min?/source/status`）
- `POST /api/persons/{id}/activities` `{activity, tool?, location?, commute_mode?, started_at?, duration_min?}` → `{activity: row}`
- `DELETE /api/persons/{id}/activities/{aid}`（软删 status=dismissed，同 metric）
- 队列：kind=activity（value=activity 串/started_at）

---

### Task 1: 迁移 000010 + PersonActivityRepo

**Files:** Create `migrations/000010_activity.up.sql` + `.down.sql`、`internal/repo/person_activity.go`、`internal/repo/person_activity_test.go`

**迁移 up**（横切字段逐列对齐 000009 的 person_metric——抄列定义，不写「同上」）：

```sql
-- 画像 P4：activity（生活轨迹）平面（spec §4.7）。
-- 活动流：每条 = 某时开始、（可选）持续多久的一次活动（做什么/工具/地点/通勤）。
-- 测点流语义（同 person_metric）：无「当前值」、无 supersedes_id——改口就是新活动或 dismiss 旧条。
CREATE TABLE person_activity (
  id           BIGINT PRIMARY KEY,
  user_id      BIGINT NOT NULL DEFAULT 1,
  person_id    BIGINT NOT NULL,
  activity     VARCHAR(256) NOT NULL,              -- 做什么（开会/写代码/打球…）
  tool         VARCHAR(128) NULL,                  -- 什么工具（手机/电脑/健身房/汽车…）
  location     VARCHAR(256) NULL,
  commute_mode VARCHAR(24) NULL,                   -- 通勤方式（中文短串：地铁/开车/步行…；不做枚举强校验）
  started_at   DATETIME(3) NOT NULL,               -- 开始时间（LLM 未给则落 session 时间）
  duration_min INT NULL,                           -- 持续分钟数
  -- 横切字段（与既有平面一致，spec §3；无 supersedes_id，见顶部注释）
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
  KEY idx_person_time (person_id, started_at),
  KEY idx_user_status (user_id, status),
  KEY idx_session (session_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
```

down：`DROP TABLE IF EXISTS person_activity;`

**repo**：镜像 `internal/repo/person_metric.go` 逐函数改表名/字段（先读该文件再写）：
- `PersonActivity` struct（`Tool/Location/CommuteMode *string` 可空、`StartedAt time.Time`、`DurationMin *int`；注释说明测点流语义与双存无关、duration 可空）
- `CreateExt`（显式 16 列 INSERT，零值兜底同 metric：conf=0.8/observed/manual/active/version=1）
- `Get`（(nil,nil) 约定）
- `ListByPerson(personID, from, to *time.Time)`（半开区间 [from,to)，`ORDER BY started_at ASC, id ASC` 升序注释同 metric——轨迹按时间从早到晚铺开）
- `FindByNaturalKeyExt(sessionID, personID, activity, tool, location, commuteMode *string, startedAt time.Time, durationMin *int)`——自然键 = (session, person, activity, tool, location, commute_mode, started_at, duration_min)；四个可空串列用 `<=>`（绑定 SQL NULL 命中 IS NULL，抄 metric 的 vt any 技法，四个可空各做一个 any 中转）；duration 可空也用 `<=>`
- `SetStatusExt`/`SetStatus`、`ListPending(userID)`（ORDER BY id）、`CountPendingByPerson(personID)`

**测试**（镜像 person_metric_test.go 的测试组织：`make init-testdb` 前置 + 公共行 t.Cleanup）：Create+Get 往返（含可空列 NULL）；ListByPerson 升序+时间窗半开边界（from 含/to 不含）；FindByNaturalKeyExt 四可空列 NULL 命中（tool/location/commute/duration 全 NULL 的行能被 nil 参数命中——`<=>` 验证）；SetStatus；ListPending/CountPendingByPerson。

Commit: `feat(repo): person_activity 表 + repo（迁移 000010，测点流语义）`

### Task 2: profile 平面——Fact/ParseFacts/factKey/gate/service/confirm

**Files:** Modify `internal/profile/fact.go`、`gate.go`、`extractor.go`、`service.go`、`confirm.go`、`service_manual.go`；测试 `internal/profile/*_test.go` 追加 case

**fact.go**：
- `Fact` 加段：
```go
	// ---- activity 平面（P4 生活轨迹）----
	ActivityText string // 做什么（开会/写代码/打球…）
	Tool         string // 什么工具（手机/电脑/健身房…）
	Location     string // 与 event 平面 EventLocation 同风格的自由文本
	CommuteMode  string // 通勤方式中文短串（地铁/开车/步行…；不做枚举强校验）
	StartedAt    string // 原始日期串（YYYY-MM-DD/RFC3339），解析在 service 层 parseEventAt
	DurationMin  int    // 持续分钟
```
- `rawFact` 加 `ActivityText string `json:"activity"``、`Tool/Location/CommuteMode/StartedAt string` json 标签 `tool/location/commute_mode/started_at`、`DurationMin int `json:"duration_min"``
- `validPlanes` 加 `"activity": true`
- `ParseFacts` switch 加：
```go
		case "activity":
			// 活动流：仅强制 activity 非空（started_at 可空，service 落 session 时间；tool 等
			// 全可空——「下午去游泳了」没说工具地点也是有效活动）。
			if f.ActivityText == "" {
				continue
			}
```
（赋值段同步加 6 字段 trim；DurationMin 不 trim 同 cycle 的 int）

**gate.go**：
```go
// DecideActivity 活动闸门：测点流语义（完全对齐 DecideMetric）——无当前值/无冲突/无佐证，
// 纯置信闸门 + 自然键防重跑。同活动不同时刻是两条独立记录（各自成行）。
func DecideActivity(f Fact, dedupHit bool, cfg GateConfig) Decision {
	if dedupHit {
		return DecisionSkip
	}
	if autoWritable(f, cfg) {
		return DecisionCreateActive
	}
	return DecisionCreatePending
}
```

**extractor.go factKey**：switch 加 case（镜像 DB 自然键，**不含** confidence 等）：
```go
	case "activity":
		return "activity\x00" + subj + "\x00" + f.ActivityText + "\x00" + f.Tool + "\x00" +
			f.Location + "\x00" + f.CommuteMode + "\x00" + f.StartedAt + "\x00" + strconv.Itoa(f.DurationMin)
```
（extractor.go 需 import strconv 若未有；顶注释的自然键清单追加一行 `activity : subject + activity + tool + location + commute_mode + started_at + duration_min`）

**service.go**：
- `Service` struct 加 `Activities *repo.PersonActivityRepo`（找 Metrics/Cycles 字段处并列）
- `applyFact` 的 plane switch 加 `case "activity"` 分流（对齐 metric：传 sessionTime）
- `applyActivityFact`（对齐 applyMetricFact 逐行写）：
  - startedAt := sessionTime；parseEventAt(f.StartedAt) 命中则覆盖
  - 四个可空串 trim 空串→nil（对齐 cycle label 的 `<=>` 约定）；duration>0 才落（≤0 视为未给→nil，同 cycle period/duration「未给不设」）
  - `FindByNaturalKeyExt` dedup → `DecideActivity` → Skip/Active/Pending → `activityRow` + `CreateExt` + `createActivityLog` + 计数
- `activityRow(userID, personID, f, tool/location/commute *string, durationMin *int, startedAt, status, memID, prov)` 行构造（对齐 metricRow）
- `createActivityLog`（对齐 createMetricLog：EntityKind "activity"、ChangeType "create"）
- `ApplyStats` 无需新字段（Active/Pending/Skipped 复用）

**confirm.go**：`ConfirmPending`/`DismissPending` switch 各加 `case "activity"`（抄 metric case 改字段：Get→判 nil→SetStatusExt active/dismissed→createLog changed_by=user；无 supersedes 分支，注释同 metric「测点无版本取代语义」）；错误信息枚举串追加 `|activity`。

**service_manual.go**：`ManualAddActivity(ctx, personID, activity, tool, location, commuteMode, startedAt string, durationMin int)`（对齐 ManualAddMetric：activity trim 非空校验；可空串 trim 空→nil；duration>0 才落；startedAt 解析失败 `time.Now()` 兜底——手动录入没对话时刻可依；conf=1.0/observed/manual/active + create 审计）。

**测试**（各文件既有测试函数追加 case 或新增函数，共享库约定）：
- ParseFacts：activity 合法条目全字段解析；空 activity 丢弃；非法 plane 仍丢
- DecideActivity：dedupHit→Skip；高置信 observed→Active；低置信→Pending；predicted 高置信→Pending
- factKey：同活动不同 started_at 不塌缩；同键重提塌缩（保高置信）
- applyActivityFact（集成）：高置信落 active + change_log；同 session 重跑 Skip（幂等）；低置信 pending；自然键含可空 NULL 命中
- ConfirmPending/DismissPending kind=activity 流转
- ManualAddActivity 往返

Commit: `feat(profile): activity 平面全链路——Fact/闸门/抽取/落库/确认/手动录入`

### Task 3: prompt v3 加 activity 平面 + API 路由 + 接线

**Files:** Modify `prompts/profile_extraction_v3.md`、`internal/api/person.go`、`cmd/zhiwei-server/main.go`；测试 `internal/api/person_test.go` 追加

**prompt v3**（不升版本号，文件名/引用路径不变）：
- 第 32 行 plane 枚举追加 `activity`（生活轨迹）
- 平面字段说明区（metric/cycle 段后）追加：
```
- activity 平面字段（日常活动轨迹：什么时间、多长时间、什么工具、做什么）：
  - activity：做什么（开会/写代码/打羽毛球/通勤…，必填）
  - tool：工具/载体（手机/电脑/健身房/汽车…，可空）
  - location：地点（可空）
  - commute_mode：通勤方式（地铁/开车/步行…，仅通勤类活动，可空）
  - started_at：开始时间 YYYY-MM-DD 或 YYYY-MM-DD HH:MM（可空=对话当天）
  - duration_min：持续分钟数（整数，可空）
```
- 抽取范围排除清单（第 8-9 行附近）检查措辞不与新平面矛盾
- few-shot 示例对话**第 4 行**追加一句含活动的台词（如「4|我|今天早上坐地铁去上班，路上四十分钟，上午一直在写代码」），示例 facts 追加：
```json
  {"plane":"activity","subject":{"kind":"self"},"activity":"通勤","commute_mode":"地铁","started_at":"2026-08-20","duration_min":40,"confidence":0.95,"epistemic_type":"observed","block_index":4},
  {"plane":"activity","subject":{"kind":"self"},"activity":"写代码","tool":"电脑","location":"公司","started_at":"2026-08-20","confidence":0.9,"epistemic_type":"observed","block_index":4},
```
（与示例对话内容自洽——P2a 的 few-shot 不自洽教训）
- 文件顶部「排除」清单若把日常流水排除，需改为「一般流水由记忆系统负责，但**日常活动**（plane=activity）要抽」

**api/person.go**：
- `PersonHandler` 加 `Activities *repo.PersonActivityRepo`（Metrics/Cycles 旁）
- 路由（metrics/cycles 旁）：
```go
	r.Get("/api/persons/{id}/activities", h.ListActivities)
	r.Post("/api/persons/{id}/activities", h.AddActivity)
	r.Delete("/api/persons/{id}/activities/{aid}", h.DeleteActivity)
```
- `ListActivities`（抄 ListMetrics：parseDateParam from/to → ListByPerson → `{activities: list}`；注意活动无 metric_key 维度）
- `AddActivity`（抄 AddMetric：body `{activity, tool?, location?, commute_mode?, started_at?, duration_min?}` → `Service.ManualAddActivity` → `{activity: row}`；duration_min 用 json int 的 0=未给）
- `DeleteActivity`（抄 DeleteMetric：软删 dismissed）
- pending 列表（ListPending 函数 ~890 行 metrics/cycles 段后）加 activities 段：`h.Activities.ListPending` → pendingItem 追加（kind="activity"、value=a.Activity、occurred_at=a.StartedAt、id/status/source 字段齐——抄 metric 段逐字段对照）
- 名册角标 CountPendingByPerson 汇总处（~215 行 mp/cp 旁）加 ap 求和

**main.go**：装配区加 `Activities: &repo.PersonActivityRepo{DB: db}`（找 Metrics/Cycles 装配处并列；**P2a 教训：Service/Handler 依赖都在本任务接线，不留到下任务**）。

**测试**：
- prompt 文件不改代码逻辑，但跑一次既有 profile 抽取集成测试确认 prompt 仍被正确加载
- API 测试：POST/GET/DELETE activities 往返（含升序断言）；pending 列表含 activity 条目；角标计数

Commit: `feat(api): activities 三端点 + 队列 activity 条目 + 名册角标`

### Task 4: 前端活动时间线区 + hash + 冒烟 + 手动清单

**Files:** Modify `web/app.js`、`web/index.html`、`docs/superpowers/plans/2026-08-24-person-p1b-manual-checklist.md`

**app.js**（活动区放「健康周期」区之后，模式全对齐 metric/cycle 既有惯例）：
- 状态：`activities = ref([])`、`activityLoading`、`showAddActivity`、`addActivityForm = reactive({ activity:'', tool:'', location:'', commute_mode:'', started_at:'', duration_min:'' })`、`addingActivity`、`deletingActivityId`
- `loadActivities()`：详情打开时拉 `GET /activities`（对齐 loadMetrics：**带 pid guard**——await 后校验 person id，晚到旧响应丢弃，final review LOW-1 模式）；挂接 togglePerson 详情拉取成功后（loadMetrics 调用旁）
- `toggleAddActivity` 对称草稿（收起重置）、`resetAddActivityForm` 全 6 字段
- `submitAddActivity`：activity 必填 toast；可选字段非空才发；duration_min Number() 转换；POST 后 reload `loadActivities()+loadPersons()`（角标可能变）
- `askDeleteActivity/confirmDeleteActivity` 2 步删除
- `closePersonDetail` 追加清活动态（6 项）
- 队列：`pendingKindText` 加 `activity: '活动'`；`pendingSummary` 加分支 `if (it.kind === 'activity') return it.value || '';`（value=activity 串）
- 导出全部标识符

**index.html**（健康周期区后）：
- 区头「生活轨迹」+ 副说明 muted 小字（活动时间线，含 AI 抽取）
- `.seg` 列表行：**加粗 activity** + 来源徽标（AI/人工）+ pending 徽标 + meta 行（`fmtEventDate(started_at, true)` · duration_min→「N 分钟」（可空跳过）· tool · location · commute_mode 各 v-if 跳空）+ 2 步删除
- 空态「暂无活动记录。从对话自动抽取或手动添加。」
- 加活动表单（activity 必填 input / tool / location / commute_mode / started_at type=date / duration_min type=number min=1 step=1）+ 收起对称
- 队列 kind 徽标已有 pendingKindText 覆盖，无需模板改动

**Task 4 收尾**：
1. `node --check web/app.js`
2. `bash scripts/hash-web.sh` + `git add web/index.html`
3. 冒烟（curl 8081）：POST 3 条不同日期活动（乱序）→ GET 断言升序 → DELETE 软删（GET 行仍在但 status=dismissed，前端过滤由 status!==dismissed 保证——**列表模板对 dismissed 行 v-if 过滤**，对齐 metric 区 metricCategoryRows 模式，用 computed `activityRows` 滤 dismissed）；清理
4. 手动清单追加（P3b 段后）：
```markdown
## P4 生活轨迹验收（2026-08-26 追加）

26. 详情「生活轨迹」：手动加「通勤·地铁·40分钟」→ 列表出现（人工徽标 + meta 行）
27. 加 3 条不同日期活动 → 按时间升序排列；删除 2 步确认
28. 队列出现「活动」条目 → 确认流转；名册角标联动
29. 切换人物再切回：活动表单草稿不残留
```
5. Commit: `feat(web): 生活轨迹时间线区 + 队列活动条目`；收尾 commit `docs(web): P4 手动验收清单 + hash 同步`

---

## 计划自检

1. **覆盖**：spec §4.7 表结构（列全量落）+ §12 P4 行（活动流录入/抽取✓、轨迹可视化=时间线列表✓、独处时间→明确不实现记跟进）；六处平面 switch 对称演进点全列出。
2. **不实现并记录**：独处时间派生指标（无同场人物字段，spec §399 稀疏不展示）——Task 4 收尾时在 spec §13 跟进清单追加一条。
3. **模式一致**：repo/闸门/apply/manual/API/前端全镜像 metric 平面（同为测点流）；可空列 `<=>` 自然键、UTC 锚定、软删、对称草稿、pid guard 全部沿既有约定。
4. **类型一致**：Go `ActivityText` json 标签 `activity`（前端/API 字段名一致）；`DurationMin int` DB `duration_min INT NULL` 用 *int；前端 duration_min 字符串表单 Number() 转。
