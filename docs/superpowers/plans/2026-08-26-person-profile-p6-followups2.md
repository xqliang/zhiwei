# 用户画像 P6（spec §13 第二批跟进）实现计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 清掉 spec §13 剩余可做项：F4（枚举/类型写入端校验）、P2a③（event 标题归一化去重）、F5 反向边（归档级联 pending 反向关系边）、F6（测试 snowflake nodeID 隔离，解锁并行测试）。

**不做并保留**：F7（独处时间——需 activity 增同场人物维度，单独设计）、F5 反向边中的 active 反向边（归档不篡改对端人物画像——已记 spec，本计划只级联 pending 反向边这种未确认噪声）、F6 中的 ListPending N+1（队列规模小）与属性自然键 SQL 精确比较（ParseFacts 已 trim，P4 起 activity 也靠 `<=>` 兜住了 NULL 语义）。

**Architecture:** 纯后端 + 测试基建，零前端改动（无需 hash）。F4 走 catalog 单点校验函数（LLM 路径违规 skip 宁少勿错 / 手动路径报错）；P2a③ 镜像 attribute 平面的「归一化比较 reaffirm」既有模式；F5 反向边是 P5 级联的一个补充 UPDATE；F6 给测试进程分配互异 snowflake node。

**工作目录：** worktree `.worktrees/person-p6`（分支 `feat/person-p6`，基线 main=4f3c7ef）。

**验证约定：** `make init-testdb` + `TEST_MYSQL_DSN="zhiwei:zhiwei@tcp(127.0.0.1:3307)/zhiwei_test?parseTime=true&charset=utf8mb4&multiStatements=true" go test -p 1 ./... -count=1`（Task 4 完成后**不带 -p 1 也要全绿**——那正是它的验收标准）。

---

### Task 1: F4 写入端校验/规范化

**Files:** Modify `internal/profile/catalog.go`（或新建 `internal/profile/validate.go`——校验逻辑独立成文件更清晰）+ `service.go`（LLM 路径）+ `service_manual.go`（手动路径）+ 测试

**现状**：catalog 的 ValueType/EnumOptions 对写入端纯建议性——LLM 出 gender=「男性」、smokes=「是」、birthday=「八月三号」都会原样落库，数据脏了以后按 enum/bool 前端渲染与查询都失配。

**做法**：
- 新函数（放 validate.go，详细注释）：
```go
// NormalizeAttrValue 按 catalog 的 ValueType 校验并规范化属性值（F4：写入端单点校验）。
// 返回规范化后的值；违规返回 error（描述原因，手动路径直接 400 给用户看）。
//   enum  : TrimSpace 后须精确命中 EnumOptions 之一（gender=「男性」→ 报错，别猜映射）
//   bool  : 接受 true/false（大小写不敏感，归一为小写）；「是/否」不接受——宁少勿错
//   date  : 经 parseEventAt 可解析且能重排为 YYYY-MM-DD（八月三号→报错；2026-08-03/2026/08/03→2026-08-03）
//   number: strconv.ParseFloat 可解析，归一为 %g 格式串（与 formatMetricValue 同法）
//   text  : 仅 TrimSpace 透传
// catalog 外的 key（Def 回退 text）不受校验——自造 key 的值域未知，校验反而误伤。
func NormalizeAttrValue(d AttrDef, value string) (string, error)
```
- **LLM 路径**（applyAttributeFact 或 ParseFacts——选 service 层：ParseFacts 不知道 catalog 语义边界。applyAttributeFact 开头调 NormalizeAttrValue，error → `st.Skipped++` return nil，注释「宁少勿错：脏值不落库不进队列，后续会话抽到规范值仍可落」）。注意 reaffirm/冲突比较链用的都是规范化后的值（先规范化再走闸门）
- **手动路径**（ManualAddAttribute 开头）：error 直接 return err（API 层已把 service error 转 400/500——核对 AddAttribute handler 的错误码路径，若纯 500 需为校验错误回 400：看既有 ManualAddMetric 的「非法指标类型」怎么映射状态码，对齐它）
- **测试**：单测 NormalizeAttrValue 全类型矩阵（合法/非法各若干）；集成测：LLM 路径脏值 skip + 规范值落库；手动路径脏值报错。既有 attribute 测试全绿（既有合法值不受影响——**先跑一遍存量测试确认没有依赖脏值通过的用例**，有则那是用例脏，修用例数据）

Commit: `feat(profile): 属性值写入端校验/规范化——enum/bool/date/number 单点闸（F4）`

### Task 2: P2a③ event 标题归一化去重

**Files:** Modify `internal/repo/person_event.go` + `internal/profile/service.go`（applyEventFact）+ `internal/profile/extractor.go`（factKey）+ 测试

**现状**：事件自然键对 title 原始精确匹配——LLM 同一事件两次出「去云南旅游一周」/「去云南旅游」会建两条 active 而非佐证（P2a 简化③记录在案）。

**做法**：
- `repo.NormalizeTitle`（trim+小写+去标点，**保留汉字**——unicode.IsLetter 含 Han）已存在且被 attribute 的 reaffirm 比较使用（DecideAttribute 里 `NormalizeTitle(existing.ValueText) == NormalizeTitle(f.Value)`）
- **镜像 attribute 模式**：person_event.go 加 `FindActiveByNormalizedTitleExt(ctx, ext, personID, eventType, title string)`——按 (person_id, event_type, status='active') 拉候选行（单人物单类型行数很小），Go 侧 NormalizeTitle 比较命中返回；未命中 nil
- **applyEventFact**：dedup（精确自然键，防同 session 重跑）之后、DecideEvent 之前，加归一化 existing 查询——命中走 Reaffirm（同值佐证语义）。注意与精确 dedup 的顺序：dedupHit 优先 Skip（幂等），其次归一化命中 Reaffirm，最后新建
- **DecideEvent 不用改**（existing != nil → Reaffirm 分支已有）——把归一化查询结果作为 existing 传入即可。核对 FindActiveByKeyExt（若 event 已有精确版 active 查询）与归一化版的关系：归一化版是超集（精确相等必归一相等），直接用归一化版替代精确版（一次查询两用）
- **factKey**（extractor.go event case）：`f.EventType + "\x00" + repo.NormalizeTitle(f.EventTitle)`——批内去重与 DB 自然键同步归一化（P3a「factKey 镜像 DB 自然键」教训：若 factKey 用原始 title 而 DB 用归一化，跨窗口近重复标题不塌缩、到 Service 又被归一化 reaffirm 吞掉，统计口径漂移）。注意 extractor.go import repo 是否引入循环依赖（profile→repo 已存在，无环）
- **测试**：同 person 同 type 两条近重复标题（不同 session）→ 第二条 Reaffirmed 而非新建 active；同 session 重跑仍 Skip（精确 dedup 不破）；factKey 近重复标题批内塌缩

Commit: `feat(profile): event 标题归一化去重——近重复事件走佐证而非重复建行（P2a③）`

### Task 3: F5 反向边——归档级联 pending 反向关系边

**Files:** Modify `internal/repo/person_relationship.go` + `internal/profile/service_manual.go`（ManualSetPersonStatus 级联段）+ 测试

**现状**：归档 A 后，他人指向 A 的 **pending** 反向关系边（C→A，`related_person_id=A`）仍留在确认队列——对着一半已归档的人物让用户确认关系，噪声。**active** 反向边刻意不动（那是 C 的画像数据，归档 A 不篡改 C——P5 已记 spec，本任务不改该决策）。

**做法**：
- person_relationship.go 加 `DismissPendingReverseExt(ctx, ext, personID) (int64, error)`：`UPDATE person_relationship SET status='dismissed' WHERE related_person_id=? AND status='pending'`（注释：只 pending——active 反向边是对端画像不动；事件 related_person_ids 是 JSON 列无法索引级联，留 spec 跟进）
- ManualSetPersonStatus 的 dismissed 分支在六平面级联后追加调用，行数并入汇总审计 Note（如「反向 pending 关系边 N 条」追加在原 Note 后）
- **测试**：C→A pending 反向边 + A 自身 active 属性 → 归档 A → 反向边 dismissed、审计含反向计数；C→A active 反向边 → 归档后仍 active
- spec §13 F5 反向边跟进句更新：pending 反向边已级联（Task 5 收尾时一并做，或本任务顺手做——本任务做，收尾只做核对）

Commit: `feat(profile): 归档级联 pending 反向关系边——清确认队列孤儿噪声（F5 补充）`

### Task 4: F6 测试 snowflake nodeID 隔离

**Files:** Modify 各测试文件的 `ids.Init(1)` 调用点（internal/{profile,api,pipeline,voiceprint,repo}/*_test.go，共 **23 处**）+ 新增 `internal/ids/testinit.go`（导出测试辅助 `InitForTest`）

**现状**：每个测试二进制 `ids.Init(1)`——`go test` 并行跑多包时（默认 -p=CPU 数）各进程同 nodeID=1，同毫秒同 step 生成相同 ID，撞共享 zhiwei_test 库主键（本周期两次踩坑，靠 `-p 1` 规避）。

**做法**：
- 先读 internal/ids 包：node 位数/取值范围/Init 语义（幂等？重复 Init 报错？）→ **实测**：`Init` 幂等（每次 `snowflake.NewNode` 重建并覆盖全局 node，不因重复调用报错）；`New()` 仅在 `Init` 从未调用时 panic（nil node）；snowflake 默认 NodeBits=10，node 值域 [0,1023]，`NewNode` 仅在 node<0 或 >1023 时报错。
- 测试侧统一改为**进程唯一 node**。**辅助函数落点**：实测调用点共 **23 处（跨 5 包：profile/api/pipeline/voiceprint/repo，其中 api 占 19 处）**，远超「≤6 则内联」阈值 → 选**导出 `ids.InitForTest() error`**（放 `internal/ids/testinit.go` 普通源文件——Go 不跨包导出 `_test.go` 符号，api/repo 等包的测试要 import 它必须放非 test 文件；生产不调用，注释说明）。实现 = `Init(int64(os.Getpid()) % (1<<snowflake.NodeBits))`（PID 取模 1024）；返回 error 与 `Init` 同签名，故 `ids.Init(1)` 两种写法（`_ = …` 与 `if err := …`）均原地替换、不动结构。碰撞面：并行进程数=CPU 数（个位数），PID 对 1024 同余概率极低，退化也不比旧方案差（注释已论证）。
- 注意：node 改变不影响既有断言（ID 值本身从不被断言——grep 确认没有测试硬编码 ID 前缀/数值断言）✅

**实测结论（2026-08-26，采纳方案 a）**：nodeID 隔离是**必要但不充分**——原验收标准（不带 `-p 1` 连跑 3 次全绿）**无法由本任务达成**，实际达成的事实如下：
- ✅ `go build ./...` + `go vet ./...` 绿；**每个包单独跑绿**；`internal/pipeline` **单包**连跑 3 次 3/3 绿（包内测试无 `t.Parallel`、串行执行且自带 user_id=1 同名行预清理，内部稳定）；全套 **`-p 1`** 连跑绿。
- ❌ **不带 `-p 1`**：iter1 绿，**iter2/iter3 失败**（`TestStagesASRAndSegment`、`TestStageExtractCommit`）。失败**与 nodeID 无关**（无主键撞库错误），是另两个**独立且跨包**的根因——即 `-p 1` 暂留的真实原因：
  1. **job 抢占**：`internal/repo/job.go` 的 `ClaimNext` = `SELECT id FROM pipeline_job WHERE status='pending' ORDER BY id LIMIT 1`——**全局领最老 pending job**，不按 session/user 限定。`TestStagesASRAndSegment` 起真实 Pool、`OnDone` 对任意完成 job 触发；并行包（如 repo 的 job 测试）造的 pending job 被抢走 → 该测试读自己 session 的 transcript 得 `sql: no rows`。（Makefile 注释早有记录：repo `TestJobLifecycle` 与 pipeline pool 测试互抢。）
  2. **user_id=1 去重污染**：extract 按归一化标题**跨 session 对 `user_id=1`** 佐证/去重 memory；并行的 api/profile 测试造同名 active memory → `TestStageExtractCommit` 的候选被佐证并入他人行 → `memories=1`（期望 2）。
- **后续任务需做**（完整并行解锁）：job 领取按 owner/session 限定（或测试用独立 Pool），且每包或每测试独立 `user_id`（或独立库/schema）；两者都超出「nodeID 隔离」范围，且 user_id 隔离本就是计划显式推后项。

Commit: `test: 测试进程 snowflake node 隔离——消除跨进程 ID 撞库（F6 部分）`

### Task 5: 收尾核对

1. 全套：`make init-testdb` + DSN go test ./... -count=1（无 -p 1）全绿 ×2
2. `go build ./...` + `go vet ./...`
3. spec §13 更新：F4/P2a③/F6 标「✅ 已于 P6 解决（2026-08-26）」；F5 反向边句更新（pending 已级联，active 仍刻意不动 + event related_person_ids JSON 无法级联留跟进）；F6 条目里测试隔离 user_id 项标注（nodeID 已解，user_id 隔离仍留——除非 Task 4 顺带做了，没做就保留）
4. 无前端改动 → 不动 hash（核对 `bash scripts/hash-web.sh` 后 git diff 为空）
5. Commit: `docs: P6 收尾——spec §13 第二批已解决标注`

---

## 计划自检

1. **覆盖**：F4/P2a③/F5反向边(pending)/F6(nodeID) 四项全落位；不做项（F7、active 反向边、N+1、属性自然键 SQL、user_id 隔离除非顺手）在头部明确记录。
2. **模式一致**：F4 校验失败 LLM skip 对齐「宁少勿错」；P2a③ 完全镜像 attribute 的 NormalizeTitle reaffirm 模式；F5 补充沿用 P5 级联事务结构。
3. **风险点已核**：NormalizeTitle 保留汉字（unicode.IsLetter 含 Han，实测文件在案）；factKey 归一化与 DB 自然键同步（P3a 教训）；手动路径校验错误的状态码映射对齐既有「非法指标类型」处理；存量测试可能依赖脏值——先跑存量再改。

### Task 1b: F4 前端配套——属性值受控输入（Task 1 双审发现的 UX 回归，必做）

**背景**：Task 1 写入端上闸后，存量自由文本属性值输入会硬失败——bool 八键（是否吸烟等）用户自然输「是/否」全 400、enum 须精确命中、birthday 自由文本无日期选择器。

**Files:** Modify `internal/api/person.go`（或 router 合适处）+ `web/app.js` + `web/index.html`

**做法**：
- 后端：`GET /api/profile/catalog` → `{catalog: [{key, label, group, value_type, enum_options, cardinality}]}`（profile.Catalog 导出遍历，静态数据无 DB）。注册进 RegisterPerson
- 前端：`attrCatalog = ref([])`，人物 tab 挂载/懒加载一次；`attrDefOf(key)` 查 def（找不到回退 text 自由文本——目录外 key 兼容）
- 加属性表单 + 就地编辑值输入：`value_type==='enum'` → `<select>`（空 option + enum_options）；`'bool'` → `<select>` 是/否（提交映射 true/false）；`'date'` → `type="date"`；其余保持自由文本。attr_key 的 datalist 可顺带用 catalog 的 key+label 增强（可选）
- 受控输入提交的值天然合法，自由文本路径（text/目录外）不经过闸门变化
- 验证：node --check；起本 worktree 临时服务（ZW_PORT=8099 对 zhiwei_test）冒烟 GET catalog；hash 同步留 Task 5 收尾一并
- Commit: `feat(web): 属性值按类型受控输入——enum/bool/date 控件化（F4 配套）`
