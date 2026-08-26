# feat/agent-chatbot ↔ main 对账合并计划

- 日期：2026-08-26
- 现状：`main` 与 `feat/agent-chatbot` 已**分叉**（merge-base `0e356c4`，migrations 到 `000008_event`）。`base..main`=57 提交/61 文件（人物画像 P2a/P7 续建：**metric_cycle / activity / person_restore** + 事件重要度 + 多人事件）；`base..HEAD`=69 提交/140 文件（本会话：person_metric / 向量检索 / 对话转记忆 / agent 流式+WS / 报告 / **多用户鉴权+租户** / dsh cancel）。**非 FF**。
- 依据：`git merge-tree --write-tree main HEAD`（内存试合并）判定 **16 真冲突 + 2 假干净真会炸 + 5 测试 auto-merge**。
- 执行位置：**须在仓库根 `zhiwei-water18-0822/` 或新集成 worktree**（隔离 feat worktree 里 checkout main 会被拒）。

---

## 一、冲突面总览

### 真冲突（16）+ 隐患（2）
- **profile 五件套（主战场，语义需人工决策）**：`fact.go`（main 6 平面 `MetricValue` 单串 + Cycle*/Activity* 字段、`ValidMetricKeys` map；feat 4 平面 `ValueNum`+`MetricValueText` 分离、`MetricCatalog`）、`gate.go`（`DecideMetric` 签名不同 + main 有 `DecideActivity`）、`service.go`（main `applyCycle/ActivityFact`+`FindByNaturalKeyExt`+数值双存+单用户；feat `parseMetricAt`+`FindByPointExt`+num/text 分列+**resolver 全带 userID**）、`service_manual.go`、`confirm.go`（main 7 kind；feat 4 kind + `assertPersonOwner` IDOR）。
- **repo add/add**：`internal/repo/person_metric.go`（两套实现，见二）、`person_metric_test.go`。
- **api**：`person.go`（feat metric 内嵌详情 num/text 分组；main cycle/activity 全套 handler + CountPending 角标 + 扁平 metric `?from/to`）、`person_test.go`、`query_test.go`。
- **prompt add/add**：`prompts/profile_extraction_v3.md`（两边各建，抽取 schema 不同）。
- **web**：`app.js`（main 按键 `?metric_key=`+cycle/activity；feat `METRIC_CATALOG` 内嵌分组曲线）、`index.html`（main 面板；feat agent 聊天+报告 UI+metric 面板）。
- **cmd**：`main.go`（feat 加 auth/agent/retrieve/MCP/orchestrator/authGate 装配；main 往 `profile.Service{}`/`PersonHandler{}` 加 Cycles/Activities）。
- **测试 auto-merge（仅需过编译）**：`api/{audio,memory,todo,topic}_test.go`、`profile/confirm_test.go`。
- **⚠️ 假干净真会炸（auto-merge 抓不到）**：
  - `internal/repo/person.go`：feat 把 `Get(ctx,id)`→`Get(ctx,userID,id)`（多用户），main 只加 `ListDismissed`。合并后取 feat 签名 → **main 带来的所有 `Persons.Get(ctx,id)` 调用（cycle/activity/restore/DeleteMetric）编译失败**。多用户 userID 贯穿是跨切改动，波及 main 每一处相关 repo 调用。
  - `internal/api/query.go`：需编译核验。

### 修正用户先前假设
- `internal/config/config.go`、`go.mod`/`go.sum` — main **从未动**，feat 独有、**干净带入**（非"必冲突"）。

### feat 独有、main 未碰 → 干净引入（无冲突）
`internal/agent/`、`internal/auth/`、`internal/retrieve/`、`internal/review/`、`internal/memory/conversation.go`、`internal/provider/{llm.go改,embed_vision.go新}`、`services/agent-sidecar/`（含 dsh cancel patch）、`cmd/zhiwei-adduser/`、config/go.mod/.gitignore。repo 层唯一与 main 重叠的是 `person_metric.go`(冲突)+`person.go`(隐患)。**只要保 feat 的 person_metric，agent/auth/retrieve/review 零改动可编译**。

---

## 二、两套 person_metric（核心决策）

**同一设计**（append-only 时序测点，spec §4.5），差异：
- 词表：main `emotion|state|weight|sleep_late|diet|health` vs feat `emotion|weight|sleep|mood_energy|diet|health`。
- feat 独有：`supersedes_id`、`note`、catalog `MetricDef{Numeric}`、`FindByPointExt`(num/text `<=>` 去重)、多用户 `ManualAddMetric(userID,...)+Ext`、**agent MCP 工具 get_metrics/propose_profile_metric + ProfileContext**、详情内嵌分组、num/text 分存。
- main 独有：`pre_dismiss_status`（restore 用）、`DismissAllByPersonExt`/`RestoreArchivedExt` 级联、`CountPendingByPerson` 角标、`ListByPerson(key,from,to)` 时间窗、数值双存、且属自洽 **6 平面**（额外 person_cycle + person_activity）。

**推荐：保 feat 的 person_metric，移植 main 的运营特性**（`pre_dismiss_status` 列并进 feat CREATE + dismiss/restore 级联 + CountPending + 可选时间窗查询）。理由：feat 的 person_metric 被 agent 工具/多用户/前端/catalog 依赖，改动面小；反向（保 main、改 feat agent 层）代价大得多。

---

## 三、迁移重编号

base 到 `000008_event`；两边 000009-000011 三重撞号。**推荐**（画像平面 main 打底 → agent/auth feat 叠加）：

| 最终 | 来源 | 原名 | 处理 |
|---|---|---|---|
| 000009 | main | metric_cycle | 删其 `person_metric` CREATE，只留 person_cycle，改名 `000009_cycle`（若引入 cycle） |
| 000010 | main | activity | 原样（若引入 activity） |
| 000011 | main | person_restore | 删针对 person_metric 的 ALTER（该列并进下方 feat metric CREATE），保留其余 |
| 000012 | feat | 000009_agent | 顺延 |
| 000013 | feat | 000010_conversation_memory | 顺延 |
| 000014 | feat | 000011_metric | 顺延，**并把 `pre_dismiss_status VARCHAR(16) NULL` 并进本 CREATE** |
| 000015 | feat | 000012_auth | 顺延 |

丢弃 main 的 person_metric 定义（不跑其 CREATE）。**若决定不引入 cycle/activity**（见决策 #1）：main 000009/000010/000011 相关平面搁置，feat 四迁移直接顺延 000009-000012（最简）。

---

## 四、执行路线（在仓库根 / 新集成 worktree，非隔离 feat worktree）

1. 起集成分支（勿在 main/feat 原地）：`git switch -c integrate/merge-main feat/agent-chatbot`。
2. **merge commit 而非 rebase**：两边各 57/69 提交且深改同批 profile 文件，rebase 会让每个 feat 提交反复冲突；`git merge main` 一次性解可控。
3. 解冲突顺序：① 先跑通 §一"干净子系统"（agent/auth/retrieve/review 等本就无冲突）；② **先定 metric 设计与平面集（决策 #1-#4）**，再解 profile 五件套（fact/gate/service/service_manual/confirm）；③ 迁移重编号（§三）；④ api/web；⑤ **补 main 带来的代码的 `userID`**（`person.go` Get 签名 + cycle/activity repo&service，编译强制暴露）。
4. **务必全量编译 + 跑测试**（`repo/person.go` 的 Get 签名破坏是 auto-merge 抓不到的）；fresh 库 migrate 000001–000015 全过；各包测试绿。
5. 通过后把 `integrate/merge-main` 合回 main。

---

## 五、7 决策点 —— 已定（2026-08-26 用户确认）
1. **平面范围**：**全范围**（用户 2026-08-26 改定，推翻先前"缩范围"）——cycle/activity 一并并入，把 main 的这两平面从「单用户 6 平面」移植进 feat 的「多用户 + catalog + agent」架构（fact 字段 / gate.DecideCycle&Activity / applyCycle&ActivityFact / manual+Ext / confirm kind / api handler+userID / web 面板；repo person_cycle/person_activity 补多用户 Ext）。**person_metric 仍用 feat 版**（decision #2）——故 main 的 000009_metric_cycle 里 person_metric CREATE 丢弃、只留 person_cycle。
2. person_metric：**保 feat** + 移植 main 运营特性（pre_dismiss_status 列并进 feat CREATE + dismiss/restore 级联 + CountPending）。
3. metric_key 词表：以 feat 为准（emotion/weight/sleep/mood_energy/diet/health），按需补 main 的 state/sleep_late。
4. 数值存法：**feat num/text 分存**（与 catalog Numeric 一致）。
5. 多用户为合并后默认：**是**——main 带来的代码（person_restore/事件重要度/多人事件相关的 repo/service/api 调用）须补 `userID`（编译强制暴露，尤其 `Persons.Get(ctx,userID,id)`）。
6. person_restore 级联扩到 feat metric：**是**（cycle/activity 因决策 1 不含）。
7. profile_extraction_v3.md：以 feat 版为基（4 平面 num/text），保留 main 的事件重要度/多人事件抽取（related_people 数组 + 事件 importance）。

**全范围迁移重编号**：base 到 000008；feat 四迁移原样顺延 **000009_agent / 000010_conversation_memory / 000011_metric / 000012_auth**；main 三迁移改号并入：**000013_cycle**（= main 000009_metric_cycle，**删 person_metric CREATE**[与 feat 000011 撞表]，只留 person_cycle）/ **000014_activity**（= main 000010_activity）/ **000015_person_restore**（= main 000011_person_restore，6 平面 pre_dismiss_status ALTER 全留；ALTER person_metric 作用于 feat 000011 建的表）。

## 七、仓库根执行脚本（由用户在 `zhiwei-water18-0822/` 跑；隔离 worktree 无法执行）
> 分叉大、需人工解冲突，脚本给骨架，冲突处按 §一/§二/§五 手解。

```
cd /Users/jyxc-dz-0100360/work/fun/zhiwei-water18-0822      # main 所在的主 checkout
git fetch --all
git switch -c integrate/merge-main feat/agent-chatbot        # 集成分支，勿动 main/feat
git merge main                                               # 一次性 merge commit；下面手解冲突
# 手解（参照本计划）：
#  - profile 五件套(fact/gate/service/service_manual/confirm)：取 feat 的 4 平面+多用户+catalog，
#    剔除 cycle/activity；保留 main 的事件重要度/多人事件(related_people)。
#  - repo/person_metric.go & person_metric_test.go(add/add)：取 feat 版，补 pre_dismiss_status
#    列 + DismissAll/RestoreArchived 级联 + CountPending。
#  - api/person.go：取 feat metric 内嵌契约；剔除 cycle/activity handler；保留 main 事件重要度/多人。
#  - web/app.js & index.html：取 feat（含 agent 聊天/报告/metric 面板）；剔除 cycle/activity 面板；
#    合入 main 的事件重要度视觉分层+多人录入。
#  - prompts/profile_extraction_v3.md(add/add)：feat 版为基 + main 事件重要度/多人事件抽取。
#  - cmd/main.go：并 feat 全套装配；剔除 Cycles/Activities 字段。
#  - 迁移：删掉 merge 带入的 000009_metric_cycle/000010_activity/000011_person_restore；
#    给 000011_metric.up.sql 加 `pre_dismiss_status VARCHAR(16) NULL`；确认最终 000009_agent..000012_auth 连续。
#  - repo/person.go：Get 用 feat 多用户签名；main 带来的所有 Get 调用补 userID（编译报错处逐一改）。
# 解完：
go build ./... && go vet ./...
# fresh 库验证 migrate 000001-000012 连续、各包测试绿（隔离库 -p 1）
git switch main && git merge --ff-only integrate/merge-main    # 集成分支验证通过后 FF 回 main
```


---

## 六、规模与风险
中大型、判断密集。低风险机械 union：cmd/main.go（字面量并字段+装配块并列）、index.html（面板并列）。高风险语义决策：profile 五件套（4 vs 6 平面 + 单 vs 多用户 userID 贯穿）、person_metric add/add、api/person.go（metric 契约分叉）、web/app.js（数据流分叉）、prompt。**主风险 = profile service 层结构性分叉 + main 代码需补多用户 userID（编译强制暴露）**。首次合并强烈建议按决策 #1 推荐**缩范围**（不含 cycle/activity）以降复杂度。
