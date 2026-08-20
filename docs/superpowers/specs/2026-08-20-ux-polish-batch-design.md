# UX 打磨批次设计：手动合并 + 实体编辑/删除 + 时间线卡片增强

- 日期：2026-08-20
- 状态：待审阅
- 范围：F1 手动合并主题、F2 记忆 inplace 编辑、F3 待办编辑+删除、F4 topic 删除、F5 时间线卡片增强+删除时间线。
- 推进方式：一个 spec/plan 覆盖全部 5 特性。

## 背景/现状（已落 main 的 API 面）

- 记忆 `Memory`：`PATCH /api/memories/{id}` 已支持 `{title, content, status}`（改 content→version+1，`MemoryRepo.Save` 只写 title/content/status/version）。无 DELETE（用 status=dismissed 软删）。模型有 title+content。
- 待办 `Todo`：`PATCH /api/todos/{id}` 只做状态机 `{status}`（`CanTransition`：suggested→confirmed→done，任意→dismissed；`TodoRepo.UpdateStatus`）。无 title 编辑、无 DELETE。模型只有 title（无 content 字段）。`GET /api/todos`、`GET /api/todos/{id}/topics`、关联 AddTopic/RemoveTopic。
- 主题 `Topic`：`PATCH /api/topics/{id}` 支持 `{status, name}`（`UpdateName`/`UpdateStatus`）。已有 `忽略`(dismiss)。无 DELETE。`POST /api/topics/merge {groups:[{canonical_name, member_ids}]}` → `TopicRepo.MergeGroups`（找/建 canonical、迁 memory_topic/todo_topic 关联、member 置 dismissed）。
- 会话 `AudioSession`：仅 GET。`GET /api/sessions`（ListSessions 返回 `AudioSession`+`job_status`/`job_stage`，无 ASR 文本、无记忆/待办计数）。`GET /api/sessions/{id}`（详情：session+transcript+segments+memories+todos+job）。`SessionRepo`：Create/Get/List/UpdateStatus/SetJobID，无 Delete。模型 `AudioSession`：Filename/StoragePath/DurationMS/Mime/Status/JobID/CreatedAt。
- 关联表 `memory_topic`(PK memory_id,topic_id,source)、`todo_topic`(PK todo_id,topic_id,source) 均无 FK（靠应用层顺序）。
- 前端：`web/app.js`（Vue 3 CDN，`api()` 助手 + `showError`）、`web/index.html`。Topics 列表有「智能合并」(LLM) 按钮 + `mergeDraft` 面板（`startConsolidate`/`applyMerge`，member 用 `{id,name,checked}`）。时间线 session 卡片只显示 filename+created_at+status；展开后才有 segments(ASR)+memory+todo 卡片。

## 共享模式

- **inplace 编辑**：点文本→变 `<input>`/`<textarea>`→保存(PATCH)→reload 列表；Esc/取消→还原。记忆改 title+content；待办改 title。
- **2 步删除确认**：点 `删除`→同按钮变 `确认删除?`+`取消`→再点→DELETE→reload。**行内按钮变换**，不用浏览器 `confirm()`。
- **删除=硬删除**（删行+关联，区别于既有 `忽略`/dismiss 软删）。单事务级联。
- 后端新增 DELETE 端点返回 204（幂等：不存在也不报错）。

---

## F1. 手动合并主题（纯前端，复用 /api/topics/merge）

- Topics 列表视图：「智能合并」旁加「手动合并」按钮。
- 点「手动合并」→进入选择模式：每个 topic 卡片出现勾选框；按钮变「开始合并」+ 出现「取消」（退出选择模式）。
- 勾选 ≥2 个 topic（<2 时「开始合并」禁用/不响应）。
- 点「开始合并」→弹输入框输新规范名（默认填**第一个勾选** topic 的名）。
- 确认→`POST /api/topics/merge {groups:[{canonical_name: <新名>, member_ids: [<全部勾选 id>]}]}`（复用 MergeGroups：找/建同名 canonical、迁 memory_topic/todo_topic 关联、member 置 dismissed）。
- 成功→`loadTopics()` 刷新 + 退出选择模式。「取消」或合并后退出选择模式。
- 无后端改动（MergeGroups 已处理 canonical 找/建 + 关联迁移 + member dismissed）。

---

## F2. 记忆内容 inplace 编辑（纯前端，复用 PATCH /api/memories）

- 复用 `PATCH /api/memories/{id} {title, content}`（已存在，`Save` 改 content→version+1）。
- 时间线 memory 卡片（`web/index.html` session 详情）+ topic 详情 memory 卡片：title 与 content 均可 inplace 编辑。
- 点 title/content→输入框→保存→`PATCH /api/memories/{id}`（只发被改字段：title 或 content 或两者）→成功→`reloadSession`（时间线）/`openTopic`（topic 详情）刷新。
- 取消→还原。空 title 不允许（前端 trim 校验，空则不发）。
- 无后端改动。

---

## F3. 待办编辑 + 删除（后端+前端）

### F3.1 后端
- 扩展 `PATCH /api/todos/{id}`：body `{title?, status?}`（至少一个非空，否则 400）。
  - `title` 非空→`TodoRepo.UpdateTitle(ctx, id, title)`：`UPDATE todo SET title = ? WHERE id = ?`（trim 后非空）。
  - `status` 非空→既有的 `CanTransition` + `UpdateStatus` 流转（不变）。
  - 两者可同 body：先应用 title 更新，再走 status 流转（title 永远写、status 需 CanTransition）；返回更新后的 `{todo: td}`。
  - **不复用既有「status 必填」校验**：改为 title/status 任一非空即合法；保留 status 枚举校验（status 非空时必须合法枚举）。
- 新增 `DELETE /api/todos/{id}`：`TodoRepo.Delete(ctx, id)` 单事务级联：
  - `DELETE FROM todo_topic WHERE todo_id = ?`
  - `DELETE FROM todo WHERE id = ?`
  - 返回 204（不存在也不报错）。
- `TodoRepo` 新增 `UpdateTitle` + `Delete`。

### F3.2 前端
- 待办卡（待办 tab + 时间线 todo 卡）：title inplace 编辑→`PATCH /api/todos/{id} {title}`→`loadTodos`/`reloadSession`。
- 待办卡加 `删除` 按钮（2 步确认）→`DELETE /api/todos/{id}`→reload。
- 注意：既有 todo 卡已有「忽略」(dismiss) 按钮；新增「删除」(硬删) 并列。

---

## F4. topic 删除（后端+前端）

### F4.1 后端
- 新增 `DELETE /api/topics/{id}`：`TopicRepo.Delete(ctx, id)` 单事务级联：
  - `DELETE FROM memory_topic WHERE topic_id = ?`
  - `DELETE FROM todo_topic WHERE topic_id = ?`
  - `DELETE FROM topic WHERE id = ?`
  - 返回 204。
- `TopicRepo` 新增 `Delete`。
- 注意：既有「忽略」(PATCH dismissed) 保留；新增「删除」(硬删 topic+关联)。

### F4.2 前端
- topic 卡（Topics 列表 + topic 详情）加 `删除` 按钮（2 步确认）→`DELETE /api/topics/{id}`→`loadTopics`/`closeTopicDetail`。

---

## F5. 时间线卡片增强 + 删除时间线（后端+前端）

### F5.1 后端 — ListSessions 富化
- `GET /api/sessions` 每行增加：
  - `asr_preview`（string）：该 session 的转写文本前 ~120 字符（concat 各 segment.text，按 start_ms 排序，截断 120）。
  - `memory_count`（int）：该 session 的 active 记忆数。
  - `todo_count`（int）：该 session 派生的非 dismissed 待办数（todo.source_memory_id IN session's memories）。
- 实现：`ListSessions` 用**单条 SQL**（带 3 个相关子查询）一次取齐，避免 N+1：
  ```sql
  SELECT s.*,
    (SELECT COUNT(*) FROM memory WHERE session_id = s.id AND status = 'active') AS memory_count,
    (SELECT COUNT(*) FROM todo WHERE source_memory_id IN (SELECT id FROM memory WHERE session_id = s.id) AND status != 'dismissed') AS todo_count,
    (SELECT GROUP_CONCAT(seg.text ORDER BY seg.start_ms SEPARATOR '')
       FROM transcript_segment seg JOIN transcript tr ON tr.id = seg.transcript_id
       WHERE tr.session_id = s.id) AS asr_full
  FROM audio_session s ORDER BY s.id DESC LIMIT ?
  ```
  Go 把 `asr_full` 截断到 120 runes 得 `asr_preview`（GROUP_CONCAT 默认上限 1024 够取前 120；超长转写后续可调 `group_concat_max_len` 或分页）。
- `QueryHandler.ListSessions` 的 `row` 结构（内嵌 `repo.AudioSession`）加 `MemoryCount int \`db:"memory_count"\``、`TodoCount int \`db:"todo_count"\``、`AsrFull string \`db:"asr_full"\``（JSON 输出 `asr_preview`/`memory_count`/`todo_count`，`asr_full` 不外泄）。

### F5.2 后端 — DELETE /api/sessions/{id}
- 新增 `DELETE /api/sessions/{id}`：handler 先 `SessionRepo.Get` 拿 `StoragePath`，再 `SessionRepo.Delete(ctx, id)` 单事务级联（DB 行），最后 `os.Remove(StoragePath)` best-effort（库外，失败仅 log 不阻断）。
- `SessionRepo.Delete(ctx, id)` 单事务（`BeginTxx`+defer Rollback+Commit），按依赖顺序删（子表先于主表，子查询依赖主表行仍存在）：
  1. `DELETE FROM memory_topic WHERE memory_id IN (SELECT id FROM memory WHERE session_id = ?)`
  2. `DELETE FROM todo_topic WHERE todo_id IN (SELECT id FROM todo WHERE source_memory_id IN (SELECT id FROM memory WHERE session_id = ?))`
  3. `DELETE FROM todo WHERE source_memory_id IN (SELECT id FROM memory WHERE session_id = ?)`
  4. `DELETE FROM memory WHERE session_id = ?`
  5. `DELETE FROM transcript_segment WHERE transcript_id IN (SELECT id FROM transcript WHERE session_id = ?)`
  6. `DELETE FROM transcript WHERE session_id = ?`
  7. （若 session.JobID != nil）`DELETE FROM job WHERE id = ?`
  8. `DELETE FROM audio_session WHERE id = ?`
  - 返回 error（404 if Get 不存在；204 on success）。
- `RegisterQuery` 加 `r.Delete("/api/sessions/{id}", h.DeleteSession)`。

### F5.3 前端
- 折叠 session 卡：filename + `asr_preview`（截断 + 省略号）+ `{{memory_count}} 条记忆 · {{todo_count}} 个待办` + 处理状态徽标 + `删除` 按钮（2 步确认）。
- `删除`→`DELETE /api/sessions/{id}`→`loadSessions` 刷新。
- 展开详情不变（仍 segments+memory+todo 卡）。
- `ListSessions` 返回的新字段直接在卡片渲染（`s.asr_preview`/`s.memory_count`/`s.todo_count`）。

---

## 取舍（已拍板）

1. **删除=硬删除**：todo/topic/session 删除都是删行+级联关联，区别于 `忽略`(dismiss)。理由：用户明确「删除」与既有「忽略」区分；硬删释放数据；删 session 级联清干净。audit 靠 memory 的 superseded/version + extract trace，不靠 todo/topic 软删。
2. **2 步删除行内确认**：不用浏览器 `confirm()`，用按钮就地变 `确认删除?`+`取消`。与应用按钮风格一致、无弹窗。
3. **asr_preview 截断 120**：够卡片预览；GROUP_CONCAT 默认 1024 够取前 120。超长转写后续优化。
4. **F1 手动合并 canonical=新名输入**（默认第一个勾选）：不复用既有 canonical 选择，用户输新名，MergeGroups 找/建同名。
5. **F2/F3 编辑复用/扩展现有 PATCH**：记忆复用 `PATCH /api/memories`（title/content）；待办扩展 `PATCH /api/todos` 加 title。不新增 edit 端点。
6. **session 删除级联单事务 + 文件 best-effort**：DB 行原子删；音频文件库外删（失败不阻断，仅 log）。

## 顺序与非目标

- 实现顺序：F2(纯前端,最小)→F1(纯前端)→F3(后端+前端)→F4(后端+前端)→F5(后端+前端,最大)。或按依赖：先纯前端两个，再后端 CRUD（F3/F4），最后 F5 级联。
- 非目标：不做记忆硬删除（记忆用 dismiss/superseded，不硬删）；不做 todo content 编辑（Todo 模型无 content 字段）；不做批量删除；不改状态机（todo 的 CanTransition 不变，删除独立于状态）；不引入软删 deleted_at 列；不做删除撤销/回收站。
