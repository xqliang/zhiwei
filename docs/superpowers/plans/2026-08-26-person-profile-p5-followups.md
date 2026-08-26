# 用户画像 P5（合并后跟进项清理）实现计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 清掉画像模块 P1-P4 合并后积累的高价值跟进项：P1b final 三项前端体验（错误提示被吞/回填无反馈/声纹 id 手抄）+ spec §13 的 F2（关系先行）/F3（profile stage 非致命化）/F5（归档级联）。

**不做并保留跟进**：F4（枚举写入端校验——面大，单独一期）、F6（N+1/自然键 SQL 精确比较/测试隔离 user_id——均「可接受」级）、F7（独处时间——需数据模型扩展）、P2a 简化①②③（重要度代偿/多人事件/标题归一化——均有既定代偿设计）。

**Architecture:** 六个小任务互不依赖（除 Task 6 收尾），每个独立可测可交付。前端两项沿用既有惯例（toast/showError/懒加载/pid guard）；后端三项均为小切口的既有函数改造（ApplyFacts 循环改两趟 / stageProfile 错误分支改非致命 / ManualSetPersonStatus 加级联），不动表结构。

**工作目录：** worktree `.worktrees/person-followups`（分支 `feat/person-followups`，基线 main=9c7765e）。dev 端口 **8081**（主仓跑旧二进制——前端冒烟后仍可打 8081 验证 api() 错误透传，因为该 fix 纯前端）。

**验证约定：** 前端 `node --check` + hash；后端 `make init-testdb` + `TEST_MYSQL_DSN=... go test`（Makefile 51-52 行模式）。

---

### Task 1: 前端 api() 错误透传 + 回填过程提示

**Files:** Modify `web/app.js` + `web/index.html`

**问题**（P1b final 记录）：Go 的 `http.Error` 回**纯文本**错误体（如「display_name 不能为空」「该声纹已绑定人物「张三」」），而 api() 只尝试 `JSON.parse(text).error`——解析失败静默回落通用「请求失败」，具体原因全被吞。

**app.js 修复**（api() 内，~40-44 行）：
```js
      if (!r.ok) {
        // Go http.Error 回纯文本，JSON API 回 {"error":...}——两种都透传：
        // JSON 解析失败时直接用响应文本（trim 截断到 200 字防超长 HTML 刷屏）。
        let msg = '';
        try { msg = (text ? JSON.parse(text).error : '') || ''; } catch (e) {}
        if (!msg && text) { msg = text.trim().slice(0, 200); }
        throw new Error(msg || '请求失败 ' + r.status);
      }
```

**回填过程提示**（runBackfill 附近 + 模板）：backfilling 期间模板已有 spinner（核实），补一行 muted 小字「正在处理最近 50 个会话的画像抽取，期间可能无响应，请稍候…」——回填是同步端点，长跑时用户需要知道没卡死。挂 `v-if="backfilling"`。

**验证**：node --check；curl 打 8081 的 400 端点（如 POST /api/persons 空 display_name）拿到纯文本体，确认新代码路径会透传（逻辑走查即可，浏览器手动验收入清单）。

Commit: `fix(web): api() 透传纯文本错误体 + 回填过程提示`

### Task 2: 新建人物声纹下拉

**Files:** Modify `web/app.js` + `web/index.html`

**问题**：新建人物表单的 speaker_id 是自由文本输入，placeholder「从声纹 tab 复制 id，可留空」——用户要跨 tab 手抄雪花 ID。

**做法**：
- app.js：`newPersonSpeakers = ref([])` + `loadNewPersonSpeakers()`（GET /api/speakers，模板同 loadAllSpeakers）；toggleNewPerson **打开时**懒加载一次（若已加载跳过——刷新名册场景少，简单缓存即可）；cancelNewPerson 不清缓存（无泄漏风险，纯只读列表）
- index.html（759 行输入框替换为 select）：
```html
<select class="txt" v-model="newPerson.speaker_id" style="margin-bottom:8px">
  <option value="">不关联声纹（可留空）</option>
  <option v-for="sp in newPersonSpeakers" :key="sp.id" :value="sp.id">{{ sp.name }}（{{ sp.id }}）</option>
</select>
```
- createPerson 的 `newPerson.value.speaker_id.trim()` 保留（select 值无空格，兼容无害）；409 冲突错误经 Task 1 的透传现在能显示具体「已绑定人物「张三」」

**验证**：node --check；GET /api/speakers 响应行结构核对（sp.id/sp.name 字段名以 speaker.go List 实际返回为准——实现前先 curl 8081 或读代码确认）。

Commit: `feat(web): 新建人物声纹改下拉选择（免手抄雪花 ID）`

### Task 3: F3 profile stage 非致命化

**Files:** Modify `internal/pipeline/stage_profile.go` + 测试

**问题**：profile 是 pipeline 末段，ExtractSession 失败（如 LLM 超时）会把整个 session 置 failed——但 transcript/memory 已落库且完好，画像只是增强数据。

**做法**（stage_profile.go 22-25 行错误分支）：
```go
		res, err := d.Profile.ExtractSession(ctx, sessionID)
		if err != nil {
			// F3（spec §13）：非致命化——transcript/memory 已落库且完好，画像失败不应把
			// 整个 session 置 failed。记 trace（Error 字段）+ 日志后放行；后续「从历史回填」
			// 端点可重跑该 session 的画像（ApplyFacts 幂等，重跑安全）。
			appendTrace(j, repo.TraceEntry{
				Stage: "profile", MS: msSince(begin),
				Model: d.Profile.Model, PromptVersion: d.Profile.PromptVersion,
				Error: fmt.Sprintf("画像抽取失败（非致命，可回填重跑）: %v", err),
			})
			log.Printf("[profile] session=%s 抽取失败（非致命）: %v", sessionID, err)
			return nil
		}
```
（`d.Profile == nil` 的装配错误保持致命——那是部署错误不是运行时抖动。）

**测试**：找 stage_profile 既有测试（internal/pipeline/*_test.go）加「ExtractSession 返回 error → handler 返回 nil 且 trace 有 Error」用例；若无现成 mock 结构，按包内既有测试模式补。

**验证**：`go build ./...` + `go test ./internal/pipeline/ -count=1`。

Commit: `fix(pipeline): profile stage 失败非致命化——trace 记错后放行（F3）`

### Task 4: F2 ApplyFacts 关系先行两趟落库

**Files:** Modify `internal/profile/service.go` + 测试

**问题**：`subject:relation:TYPE` 的属性/事件事实（如「我老婆是儿科医生」）靠 resolveSubject 查 active 关系对端解析归属；同批里关系事实排在属性之后时（LLM 乱序），该属性因查不到关系被 Skipped。

**做法**（ApplyFacts 97-103 行单循环改两趟，同事务内）：
```go
	// F2（spec §13）：两趟落库——relationship 平面先行。subject=relation:TYPE 的事实
	// 依赖同批先落的关系行（resolveSubject 查 active 对端）；LLM 输出顺序不保证关系在前，
	// 乱序时这些事实会被跳过（非破坏但丢一次抽取机会）。两趟都在同一事务，失败整体回滚。
	relPass := make([]Fact, 0, len(facts))
	restPass := make([]Fact, 0, len(facts))
	for _, f := range facts {
		if f.Plane == "relationship" {
			relPass = append(relPass, f)
		} else {
			restPass = append(restPass, f)
		}
	}
	for _, f := range append(relPass, restPass...) { // append 共享底层数组时的顺序陷阱：relPass/restPass 各自独立切片，append 到 relPass 若扩容会复制，不会踩 restPass——但为稳妥用两次循环
		...
	}
```
（**用两次独立 for 循环**替代上面 append 写法——清晰且零陷阱；循环体照抄原样。）

**测试**：service 层集成用例：一批 facts = [属性(subject=relation:配偶), 关系(self↔mentioned:Alice 配偶)]（属性在前故意乱序）→ 断言属性成功落库到 Alice（而非 Skipped）。既有测试跑全绿防回归。

**验证**：make init-testdb + `TEST_MYSQL_DSN=... go test ./internal/profile/ -count=1`。

Commit: `fix(profile): ApplyFacts 两趟落库——relationship 先行解决乱序依赖（F2）`

### Task 5: F5 人物归档级联

**Files:** Modify `internal/profile/service_manual.go`（ManualSetPersonStatus）+ `internal/repo/person*.go`（如需批量接口）+ 测试

**问题**：归档（dismiss）人物后其六平面行原样保留 active——名册不可见但数据仍会进抽取闸门/队列，留孤儿引用。

**做法**（ManualSetPersonStatus 内，status=="dismissed" 分支级联）：
- 六平面各需「把该 person 的 active|pending 行批量置 dismissed」。优先在各 repo 加 `DismissAllByPersonExt(ctx, tx, personID) (int64, error)`（UPDATE ... SET status='dismissed' WHERE person_id=? AND status IN ('active','pending')）——六个 repo 各一个方法（对齐既有 SetStatusExt 风格，详细注释）
- 级联行数记入 person 的 change_log Note（如「人物归档：级联 dismissed 属性 N1/关系 N2/大事记 N3/指标 N4/周期 N5/活动 N6 行」）；**不逐行写审计**（归档是显式用户意图，一行汇总审计即可，避免审计表爆量——注释说明该取舍）
- 非 dismissed 流转不级联（恢复人物不自动恢复已级联的行——恢复语义复杂，留跟进；注释记此决策）

**测试**：造人物+六平面各一行 active/pending → ManualSetPersonStatus dismissed → 断言各行 status=dismissed + change_log 有一条汇总审计；恢复 active 后各行仍 dismissed（不回滚）。

**验证**：make init-testdb + `TEST_MYSQL_DSN=... go test ./internal/profile/ ./internal/repo/ -count=1`。

Commit: `feat(profile): 人物归档级联六平面 dismissed + 汇总审计（F5）`

### Task 6: hash + 冒烟 + 清单 + spec 更新

1. `node --check web/app.js`；`bash scripts/hash-web.sh` + `git add web/index.html`
2. 冒烟（8081，纯前端项可直接验；后端项需本 worktree 起临时服务 ZW_PORT=8099 对 zhiwei_test，P4 Task 4 同法）：
   - POST /api/persons 空 display_name → 400 纯文本体（Task 1 走查依据）
   - POST /api/persons 带 speaker_id 已被绑定的 → 409 具体文案
   - 归档一个测试人物 → GET 其 attributes/events 等行 status 全 dismissed
3. 手动清单追加：
```markdown
## P5 跟进项验收（2026-08-26 追加）

30. 新建人物填空 display_name 提交 → 报错显示后端原文「display_name 不能为空」（非通用「请求失败」）
31. 新建人物表单声纹是下拉（含说话人名）；选已绑定声纹提交 → 409 显示「已绑定人物「XX」」
32. 「从历史抽取画像」点击后 spinner + 提示文案；完成后统计条出现
33. 归档测试人物 → 详情不可进、名册消失；DB 里其六平面行全 dismissed
34. 录音处理中 LLM 抖动（模拟）→ session 不再 failed，trace 记「画像抽取失败（非致命）」
```
4. spec §13：F2/F3/F5 三条标注「✅ 已于 P5 解决（2026-08-26）」（不删条目，保留历史）
5. Commits: `docs(web): P5 手动验收清单 + hash 同步`

---

## 计划自检

1. **覆盖**：P1b final 三项（错误透传/回填提示/声纹下拉）+ F2/F3/F5 全落位；不做的（F4/F6/F7/P2a简化）在头部明确记录。
2. **类型一致**：speaker select 值 sp.id 与 POST body speaker_id 字段一致；DismissAllByPersonExt 命名对齐 SetStatusExt 惯例。
3. **风险点已核**：Task 4 两趟循环用独立 for（避 append 别名陷阱）；Task 5 审计取舍（一行汇总）有注释依据；Task 3 区分装配错误（致命）与运行时错误（非致命）。
