# 转写详情 timeline 展示 profile 平面变更 — 设计规格

日期：2026-08-28
状态：已批准（brainstorming 对齐）
分支：worktree-timeline-profile-changes

## 目标

在转写详情页右栏（「提取的记忆」卡片下方的红框空白区）新增一个区块，展示**这条录音触发的 profile 平面变更事件流**——让用户一眼看到「这次提取到底动了哪些画像、怎么动的」。

## 需求（brainstorming 已确认）

1. **展示形态**：变更事件流（change_log 语义），非实体当前状态、非字段级 diff。
2. **范围与组织**：覆盖全部 8 个平面，**按平面分组**（每个有变更的平面一个小区，空平面不占位）。
3. 每条变更展示：平面类型标签 + 实体摘要 + 动作（新增/合并更新/佐证/待确认）+ 来源（LLM/手动）+ 置信 + 时间。
4. 只读展示（审计不在 timeline 编辑）；待确认变更提供「去确认」入口跳转 profile 待确认队列。

## 现状（已探明）

- 前端右栏 `detail-aside` 依次为：记忆卡片 → 记忆整理草稿 → 待办卡片，红框位于待办之后、`/detail-aside` 之前。
- `person_change_log` 已记录 8 平面（person/attribute/relationship/event/metric/cycle/activity/pet）的所有变更，字段含 `entity_kind`、`change_type`(create/reaffirm/...)、`changed_by`(llm/user)、`old_value`/`new_value`（实体摘要快照）、`confidence`、`note`、`session_id`、`created_at`。
- `PersonChangeLogRepo` **只有 `ListByPerson`，没有按 session 查询的方法**。
- `GET /api/sessions/{id}` 的 detail handler 已用 `resp["memories"]=ListBySession`、`resp["todos"]=ListBySession` 模式返回卡片数据。
- `QueryHandler` **未装配 `ChangeLogs` repo**；`cmd/zhiwei-server/main.go` 已有 `personLogs` 实例（供 profile Service 用）。

## 关键限制（需知晓）

change_log 的 `new_value` 存的是**实体摘要快照**（如 `petSummary`「泡泡（猫·布偶）」），**不是字段级 diff**——能看到「合并更新了泡泡」，但看不到「性别 公→母」的前后对比。本次实现为**摘要版**；字段级 diff 需额外按 `entity_id` 关联实体逐字段比对，列为后续迭代（YAGNI，本次不做）。

## 变更动作归一规则（后端已存，前端据此归类）

`change_type` 只有 `create`/`reaffirm` 等粗值，具体动作语义在 `note`；`new_value` 为实体摘要 JSON 字符串（带引号，前端需 `JSON.parse` 去引号，失败原样显示）。前端 `profileChangeAction(log)` 归一：

| 场景 | change_type | note | 动作标签 |
|---|---|---|---|
| 新增实体 | create | NULL/空 | 新增 |
| 合并更新（高置信覆盖，如重新提取改性别） | create | 含「合并更新」 | 更新 |
| 同值/同参佐证 | reaffirm | 含「佐证」 | 佐证 |
| 冲突待确认（低置信，supersedes 现值） | create | 含「conflict」或「待人工确认」 | 待确认 |

**限制**：新建低置信 pending（DecisionCreatePending）与新增 active 的 change_log **无法区分**（均 create + note 空，实体 status 不入审计）。故「去确认」按钮仅对 note 含「conflict/待人工确认」的**冲突类 pending** 显示；新建类 pending 不单独标记，用户到 profile pending 总览页查看全部待确认项。

## 架构与数据流

### 后端

1. `internal/repo/person_change_log.go`：新增 `ListBySession(ctx, sessionID) ([]PersonChangeLog, error)`——按 `session_id` 查，`created_at` 升序。
2. `internal/api/query.go`：`QueryHandler` 加 `ChangeLogs *repo.PersonChangeLogRepo` 字段（与 Memories/Todos 并列，注释标注「详情附带 profile 变更」）。detail handler 加 `resp["profile_changes"] = h.ChangeLogs.ListBySession(sid)`（repo 为空则跳过，兼容旧装配，对齐 memories/todos 写法）。
3. `cmd/zhiwei-server/main.go`：装配 `ChangeLogs: personLogs` 到 QueryHandler。

> 备选（不采用）：新端点 `GET /api/sessions/{id}/profile-changes`。解耦但多一次前端请求、多一个端点，与现有 memories/todos 内联模式不一致。

### 前端

4. `web/app.js`：detail 加载后把 `resp.profile_changes` 存入响应式状态（如 `detail.value.profile_changes`）。新增 `profileChangeGroups` computed：按 `entity_kind` 分组，空分组过滤，组内按 `created_at` 排序（后端已排序）。新增 `profileChangeMeta(kind)`：平面→{图标, 中文名, 颜色} 映射（对齐现有 `typeMeta`）。新增 `profileChangeAction(log)`：据 `change_type`+`note` 归一出动作标签与 badge 样式（映射见「变更动作归一规则」）。
5. `web/index.html`：右栏 `detail-aside` 底部（todo 卡片 `</template>` 之后、`</div><!-- /detail-aside -->` 之前）插入新区块：
   - 标题行「涉及的画像变更」（复用 `todo-group-title` 样式）。
   - `v-if` 有变更才渲染。
   - 按分组 `v-for`：每组一个小区标题（图标+平面名）+ 组内 `card sunken` 逐条：实体摘要（`new_value` 经 `JSON.parse` 去引号，失败原样显示）+ 动作 badge + 来源（🤖 LLM / ✋ 手动，`changed_by`）+ 置信 + 时间（复用 `muted`）。
   - 冲突类待确认（note 含「conflict/待人工确认」，见「变更动作归一规则」）附加「去确认」按钮，跳转 `/profile` pending 总览页（复用现有路由）。

### 边界

- 无 profile 变更（纯闲聊录音）：整个区块 `v-if` 不渲染。
- 手动改值的 change_log（`session_id` 为空）不被 `ListBySession` 命中，不显示——符合「这条录音涉及的变更」语义。
- 变更摘要为空时降级显示 `entity_kind`。

## 测试

- 后端：`internal/repo` 或 `internal/api` 加 `ListBySession` 单测（造 2 条不同 session 的 change_log，断言只返回目标 session 的、按时间升序）。
- 前端：`profileChangeGroups`/`profileChangeAction` 纯函数可单测；渲染走现有模式。
- 手动验证：dev 起服务，打开泡泡那条录音的详情页，确认右栏出现「涉及的画像变更」区块，展示宠物平面的变更条目。
