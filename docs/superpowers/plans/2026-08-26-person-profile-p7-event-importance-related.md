# 用户画像 P7（P2a①②：事件重要度 + 多人事件）实现计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 落地 spec §13 P2a 简化①②——事件重要度脱离 confidence 代偿独立建模；多人事件 related_person_ids 支持多元素写入与录入。

**现状（已核实）**：
- `person_event.importance DECIMAL(5,4) DEFAULT 0.5` 列已存在；LLM 路径 `eventRow` 填 `f.Confidence`（代偿），手动路径固定 1.0——**列在、语义错**
- `related_person_ids` 是 `ids.List`（JSON 数组列），但 LLM 路径只解析单个 `f.Related`（prompt「同场的主要人物」单数），手动路径 `ManualAddEvent` 单 `relatedPersonID *ids.ID`，API body 单 `related_person_id`——**数组列在、写入端单元素**
- 前端展示侧**已支持多人**（`index.html:932`：`ev.related_person_ids.map(id => personNameOf(id)).join('、')`），只有加事件表单是单选 select

**Architecture:** 零迁移（两列都在，只改写入语义与 UI）。importance 走「LLM 显式给值 > 类型默认 > confidence 兜底」三级；related_people 走新 json 数组字段（`related` 保留给 relationship 平面与 event 单人向后兼容）。

**工作目录：** worktree `.worktrees/person-p7`（分支 `feat/person-p7`，基线 main=5a7aad7）。

**验证约定：** `make init-testdb` + `TEST_MYSQL_DSN="zhiwei:zhiwei@tcp(127.0.0.1:3307)/zhiwei_test?parseTime=true&charset=utf8mb4&multiStatements=true" go test -p 1 ./... -count=1`；前端 `node --check` + hash。

---

### Task 1: P2a① importance 独立建模

**Files:** Modify `internal/profile/fact.go`、`service.go`、`service_manual.go`、`prompts/profile_extraction_v3.md` + 测试

**做法**：
- fact.go：`Fact` event 段加 `EventImportance float64`（json 标签 `importance`——rawFact 加 `EventImportance float64 \`json:"importance"\``；注意 rawFact 现有字段名冲突检查：confidence 已有、importance 无冲突）；ParseFacts 赋值段 `f.EventImportance = clamp01(rf.EventImportance)`（0=未给）
- 新函数（service.go 或 gate.go 选近处）：
```go
// defaultImportance 事件类型的默认重要度（P2a①：importance 不再用 confidence 代偿）。
// 类型分级依据 spec §4.4 事件语义：里程碑/成就（女儿出生/考上研究生）是人生大事 0.9；
// 挫折/负面/健康（被骗/生病）影响深远 0.8；旅行/聚会/会议 是经历 0.5；其他日常 0.4。
func defaultImportance(eventType string) float64
```
- service.go `eventRow`：Importance 取值链 `f.EventImportance > 0 ? f.EventImportance : defaultImportance(f.EventType)`——不再用 confidence（注释说明 P2a① 决策：重要度=人生分量，置信度=抽取把握，两者正交）
- service_manual.go `ManualAddEvent`：签名加 `importance float64` 参数（0=用类型默认；>0 clamp 到 (0,1]）；API `AddEvent` body 加可选 `importance`
- prompt v3：event 平面字段说明加 `importance：事件的人生分量 0~1（女儿出生 0.9、日常聚会 0.5、小事 0.3；不确定可不填走类型默认）`；few-shot 事件条目补 `"importance":0.8`
- 测试：LLM 路径——显式 importance 落库、未给走类型默认（里程碑 0.9/其他 0.4）、confidence 不再影响 importance；手动路径——0 走默认、传值落值；既有 event 测试核对（若有断言 importance==confidence 的用例需更新为类型默认）

Commit: `feat(profile): 事件重要度独立建模——类型默认+LLM 显式值，脱离 confidence 代偿（P2a①）`

### Task 2: P2a② 多人事件后端

**Files:** Modify `internal/profile/fact.go`、`service.go`、`service_manual.go`、`prompts/profile_extraction_v3.md`、`internal/api/person.go` + 测试

**做法**：
- fact.go：`Fact` event 段加 `EventRelated []Subject`；rawFact 加 `EventRelated []rawSubject \`json:"related_people"\``（**新标签 related_people，不动 `related`**——related 是 relationship 平面与 event 单人主人物的共用字段，同 location 共用先例）；ParseFacts 赋值段逐个 trimSubject
- service.go `applyEventFact` related 解析段改：
```go
		// related 解析（可选，多人）：related_people 数组逐个解析（单人解析失败跳过该人，
		// 不阻断事件——多人场景宁少记一人不丢事件）；空数组时回退旧 related 单人字段
		// （prompt few-shot/历史输出兼容）。解析不到存空。
		var relatedIDs ids.List
		if len(f.EventRelated) > 0 {
			for _, sub := range f.EventRelated {
				if rid, err := s.resolveSubject(ctx, tx, sub, prov); err == nil && rid != 0 {
					relatedIDs = append(relatedIDs, rid)
				}
			}
		} else if f.Related.Kind != "" {
			// 旧单人路径原样保留
		}
```
- service_manual.go `ManualAddEvent`：`relatedPersonID *ids.ID` 改 `relatedPersonIDs []ids.ID`（空切片=无同行）；调用方 AddEvent handler 同步
- api AddEvent body：`related_person_id`（单，向后兼容保留）之外加 `related_person_ids []string`（数组优先，单字段非空时并入）；两者都空=无
- prompt v3：event 字段说明 `related` 行改为「related_people：同场人物数组（每项同 subject 结构，最多 3 人；「和家人去」→ 配偶+子女两个元素）」；few-shot 旅行事件改用 `"related_people":[{"kind":"mentioned","name":"Alice"},{"kind":"relation","relation":"子女"}]`（台词「带家人去云南自驾」自洽）
- **factKey event case 不含 related**（现状已不含——核对即可，多人不破坏批内去重）
- 测试：LLM 路径 related_people 两人落两元素（一人解析失败仍落另一人）；旧 related 单人兼容；手动路径数组 body；API 单字段兼容

Commit: `feat(profile): 多人事件——related_people 数组解析+手动多人录入（P2a②）`

### Task 3: 前端——importance 视觉分层 + 加事件多选

**Files:** Modify `web/app.js` + `web/index.html`

**做法**：
- 大事记列表行（index.html ~932 附近）：按 `ev.importance` 分层——≥0.8 加粗+左侧 accent 竖条或 ★（重大）、0.6~0.8 常规、<0.6 muted 淡显（具体样式对齐既有 .seg/chip 体系，dataviz「数据是唯一主角」：不引入新色，用字重/透明度分层）；行内小字标 `重要度 {{(ev.importance*10).toFixed(0)}}` 可选（评估：数值暴露还是纯视觉——纯视觉更干净，列表 hover title 提示）
- 加事件表单：related select 改 `multiple`（v-model 数组 `addEventForm.related_person_ids: []`；提示「可多选」）；submitAddEvent body 发 `related_person_ids`（空数组不发）；可选 importance——加一个简化输入：三档按钮（重大/一般/日常 映射 0.9/0.5/0.3，默认「一般」）比数字输入更贴近语义
- resetAddEventForm/closePersonDetail 对称清理数组字段
- 队列 pendingSummary event 分支不变（value=title）
- node --check + hash（`bash scripts/hash-web.sh` 提交 index.html）

Commit: `feat(web): 大事记重要度视觉分层 + 同行人物多选录入`

### Task 4: 收尾

1. 全套：make init-testdb + DSN go test -p 1 ./... -count=1 ×2；go build ./... + go vet ./...
2. 冒烟（8081 重启后或临时服务）：POST 事件带 importance+related_person_ids 两人 → GET events 断言 importance 值与 related_person_ids 长度 2；旧单 related_person_id 兼容
3. 手动清单追加：
```markdown
## P7 P2a①② 验收（2026-08-26 追加）

40. 加「里程碑」事件不填重要度 → 默认 0.9（重大视觉样式）；「其他」类型 → 淡显
41. 加事件三档重要度按钮（重大/一般/日常）→ 保存后列表视觉分层对应
42. 抽取「和家人去旅游」→ 事件详情显示「与 X、Y 同行/同场」两人
43. 加事件同行人物多选两人 → 保存后显示两名；清空重开表单不残留
44. 旧数据（单 related）事件 → 仍正常显示单人
```
4. spec §13：P2a 简化条目标注「✅ ①② 已于 P7 解决（2026-08-26）」（③ 已 P6）
5. hash 核对（bash scripts/hash-web.sh 后 git diff 空）

Commit: `docs: P7 收尾——手动清单 40-44 + spec §13 P2a①② 已解决标注`

---

## 计划自检

1. **覆盖**：P2a①（importance 三级取值链+prompt+手动可选）②（related_people 数组全链路+兼容单人）全落位；零迁移（两列已存在）。
2. **类型一致**：rawFact 新标签 `importance`/`related_people` 无同层冲突（confidence/related 各自独立）；ManualAddEvent 签名变更的唯一调用方是 AddEvent handler（grep 核对无其他调用点）。
3. **风险点已核**：factKey event case 不含 related（多人不破坏批内去重）；前端展示侧已支持多人 join（index.html:932 实测在案）；既有 event 测试可能断言 importance==confidence——Task 1 先跑存量。
