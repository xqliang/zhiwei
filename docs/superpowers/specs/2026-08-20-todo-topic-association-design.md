# 知微云端 MVP · 代办/记忆 ↔ Topic 多对多关联 设计文档

- 日期：2026-08-20
- 状态：待审阅
- 上游文档：`docs/superpowers/specs/2026-08-19-zhiwei-sprint2-design.md`（Sprint 2：Memory/Todo/Topic 单值 topic 归属，本期修订其单值设计）、`docs/superpowers/specs/2026-08-18-zhiwei-cloud-mvp-design.md`（MVP 总设计）
- 范围：把 `todo`↔`topic` 与 `memory`↔`topic` 从单值外键改为多对多；生成代办/记忆时由抽取 LLM 自动关联多个 topic；支持手动加/删 topic 关联（代办与记忆均支持）；extract 重跑保留手动关联。

---

## 1. 目标与非目标

### 1.1 目标
- `todo`↔`topic`、`memory`↔`topic` 均为多对多（关联表 `todo_topic` / `memory_topic`）。
- 生成时自动关联：抽取 LLM 为每个候选输出 0..N 个 topic 关联（已有 topic_id 或新主题建议），memory 与其派生 todo 共享同一组 topic。
- 手动关联：代办与记忆均可手动加/删 topic 关联。
- 关联来源可区分（`source`：`ai`/`user`）。
- extract 重跑（删旧重建）时，`source='user'` 的手动关联按稳定自然键恢复。

### 1.2 非目标
- Person/Project/Risk 等实体（Sprint 3+）。
- topic 之间的层级/父子关系。
- 手动创建 todo（仍仅由 extract 产出；无 `POST /api/todos`）。
- embedding/检索（Sprint 3）。

---

## 2. 对 Sprint 2 spec 的修订

Sprint 2 §3.5 将 `memory.topic_id`、`todo.topic_id` 定为单值可空外键，且 todo 继承来源 memory 的 topic。本期修订为：两列删除，改用关联表；候选 topic 字段由单值升级为数组；todo 与 memory 共享候选的 topic 集合（1:1 派生关系下天然共享）。这是一次有意的对称化重构，非遗漏。

---

## 3. Schema（`migrations/000002_todo_topic.up.sql` / `.down.sql`）

现库无外键约束（仅列级 `ON UPDATE CURRENT_TIMESTAMP`），沿用风格：雪花 ID、`DATETIME(3)`、索引而非 FK。

### 3.1 新增关联表

```sql
CREATE TABLE memory_topic (
  memory_id  BIGINT NOT NULL,
  topic_id   BIGINT NOT NULL,
  source     VARCHAR(8) NOT NULL DEFAULT 'ai',  -- ai=抽取自动, user=手动
  created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  PRIMARY KEY (memory_id, topic_id),
  KEY idx_topic (topic_id)
);

CREATE TABLE todo_topic (
  todo_id  BIGINT NOT NULL,
  topic_id BIGINT NOT NULL,
  source   VARCHAR(8) NOT NULL DEFAULT 'ai',
  created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  PRIMARY KEY (todo_id, topic_id),
  KEY idx_topic (topic_id)
);
```

`source` 列记录关联来源，用于重跑保留手动关联（§6）与前端区分展示。

### 3.2 数据迁移（up 内）

```sql
INSERT IGNORE INTO memory_topic(memory_id, topic_id, source, created_at)
  SELECT id, topic_id, 'ai', created_at FROM memory WHERE topic_id IS NOT NULL;
INSERT IGNORE INTO todo_topic(todo_id, topic_id, source, created_at)
  SELECT id, topic_id, 'ai', created_at FROM todo WHERE topic_id IS NOT NULL;
ALTER TABLE memory DROP KEY idx_topic, DROP COLUMN topic_id;
ALTER TABLE todo    DROP KEY idx_topic, DROP COLUMN topic_id;
```

### 3.3 down

反向：重建 `topic_id` 列 + `idx_topic`，从关联表回填一个 topic（多→单取任一，多余关联丢失，固有损失，注明），再 `DROP TABLE memory_topic / todo_topic`。

---

## 4. 抽取 LLM / 候选 / 归属（auto 多关联）

### 4.1 prompt
`prompts/extraction_v1.md` → `extraction_v2.md`：候选的 `topic_id`(单) + `suggested_topic_name`(单) 改为 `topics: [{topic_id?, suggested_name?}]`（0..N，每项二选一：已存在 topic 的 id，或新主题建议名）。prompt 版本号 +1，记入 `job.trace`。

### 4.2 Candidate
`internal/memory/candidate.go`：删除 `TopicID *ids.ID` / `SuggestedTopicName string`，新增：

```go
type TopicRef struct {
    TopicID       *ids.ID  // 已有 topic（active/suggested）
    SuggestedName string   // 新主题建议名
}
type Candidate struct {
    ... // 原有字段不变
    Topics []TopicRef
}
```

JSON 解析容错沿用（剥围栏、截首尾 `{`…`}`、逐条校验丢弃非法项）；`topics` 缺失/空/枚举外 → 该候选 topic 集为空。

### 4.3 ResolveTopics
`internal/memory/topic.go`：从「每候选解析 1 个 topic」改为遍历 `Candidate.Topics[]`，对每个 `TopicRef` 应用现有三规则（直挂合法 topic_id / 同名合并 / 收集为新建 suggested），结果去重，返回该候选的 resolved topic IDs（memory 与 todo 共用）。建议 topic 的创建仍在 commit 事务内（同名查重天然幂等，幂等清理不删已确认的 suggested topic——沿用 Sprint 2 §3.5 约定）。

### 4.4 commit（`stage_extract.go` 单事务）
沿用 Sprint 2 五步，扩展为：

1. **快照手动关联**（新增）：查本 session 待删 memory 的 `memory_topic(source='user')` 与待删 todo 的 `todo_topic(source='user')`，按自然键 `K`（§6.1）存成 `map[K][]topicID`。
2. **幂等清理**：删本 session 派生 todo（`source_memory_id` 子查询）→ 显式 `DELETE FROM todo_topic WHERE todo_id IN (...)`；删本 session memory → 显式 `DELETE FROM memory_topic WHERE memory_id IN (...)`。（无 FK，显式删。）
3. 建 suggested topic（同名查重合并，沿用）。
4. 插 memory：批量插 `memory_topic(memory_id, topic_id, 'ai')`（resolved topics，`INSERT IGNORE` 兜重）。
5. 插 todo：topic 继承 = 同候选的 resolved topics，批量插 `todo_topic(todo_id, topic_id, 'ai')`。
6. **重链手动关联**（新增）：对每条新 memory/todo 算 `K`，命中快照则 `INSERT IGNORE … source='user'` 补回。

trace 记录新增「手动关联快照/恢复条数」。

---

## 5. 手动关联 API

沿用现有约定（chi 路由、雪花 ID 走 URL 与 JSON 字符串、单用户免登录、`http.Error` + 中文消息、404/400/409）。现有 `PATCH /api/todos/{id}`（status）、`PATCH /api/memories/{id}`（title/content/status）不动，避免与状态机/版本耦合。

**Todos**
- `POST /api/todos/{id}/topics` body `{topic_id}` → 加关联，`source='user'`。幂等（已存在返 200）；todo 不存在 404；topic 不存在/已 dismissed 返 400/404。
- `DELETE /api/todos/{id}/topics/{topic_id}` → 移除关联。不存在 404（或幂等 204）。

**Memories**（同型）
- `POST /api/memories/{id}/topics` body `{topic_id}` → 加关联，`source='user'`，同上校验。
- `DELETE /api/memories/{id}/topics/{topic_id}` → 移除关联。

**列表/详情响应**
- `GET /api/todos`、`GET /api/memories`：每条带 `topics: [{id,name,status,source}]`（LEFT JOIN 关联表 + topic 聚合）。`topic_id` 过滤参数改 `WHERE id IN (SELECT ..._id FROM ..._topic WHERE topic_id=?)`。
- `GET /api/topics/{id}` 详情：挂载的 memories/todos 改 JOIN 关联表取。
- `GET /api/topics` 计数：memory/todo 计数子查询改 `JOIN ..._topic`。

---

## 6. 幂等与重跑保留手动关联

### 6.1 稳定自然键
`memory` 存有 `transcript_segment_ids`（候选所属对话块的 segment id 数组，provenance，Sprint 2 §3.5）。segment 来自 asr/segment stage，extract 重跑不动 segment → segment_ids 跨重跑稳定。自然键：

```
K = canonical(排序后 segment_ids, title)
```

todo 与其 source memory 1:1（一候选最多一 todo），复用 source memory 的 `K`。`canonical` = segment_ids 排序后定长拼接 + `\x1f` 分隔 + title。

### 6.2 流程
见 §4.4 步骤 1 与 6：删旧前快照 `source='user'` 行（按 `K`），重建后按 `K` 命中 `INSERT IGNORE … source='user'` 补回。`source='ai'` 行不快照（每次重建）。

### 6.3 边界
- LLM 重跑 title 漂移 → `K` 变 → 该条手动关联不恢复（todo 实质已变，合理）。
- 同块内 title 撞名 → 多条候选共享 `K` → 都补同样手动关联（罕见，可接受；如成问题再加 content 前缀入键）。
- segment_ids 为空（不应发生）→ 退化为 title-only 键。

---

## 7. Web UI

沿用 Vue 3 CDN 无构建单页、现有卡片/徽标/配色。
- 时间线 memory/todo 卡片、待办页、Topics 详情：topic 由单徽标改为多徽标（多个 topic 名 chip）。
- 待办编辑/记忆编辑：内联「+ 关联 Topic」选择器（从 active+suggested topic 选），点 × 移除；即时 POST/DELETE + 乐观更新，失败回滚提示。
- 徽标可标 `source`（手动关联加小标识），可选。

---

## 8. 测试策略

沿用两级：`make test`（纯逻辑，无外部依赖）/ `make test-integration`（`TEST_MYSQL_DSN`，fake LLM + 真 MySQL）。

**单元**
- ResolveTopics 多 ref：直挂/同名合并/新建 suggested/去重；空 topics、非法 ref 丢弃。
- Candidate JSON 解析：`topics` 数组缺失/空/枚举外容错。
- 自然键 canonical：segment_ids 排序稳定、与 title 组合。
- 快照+重链纯逻辑（用内存 map 模拟）：命中恢复、漂移不恢复、撞名都补。

**集成**
- extract 端到端：fake LLM 返回 `topics:[...]` → 断言 `memory_topic`+`todo_topic(source='ai')` 落库正确（memory/todo 共享同组）。
- 重跑幂等：再跑一次不产生重复关联行。
- **重跑保留手动关联**：先 POST 手动加 topic 关联 → 重跑 extract → 断言 `source='user'` 行按自然键恢复、`source='ai'` 行重建不重复；构造 title 漂移场景断言该条不复原。
- API（httptest）：POST/DELETE todos/memories 的 topic 关联（幂等、404/400）；GET 列表返 `topics[]`；`topic_id` 过滤走 JOIN；GET topics 计数正确。
- 迁移：现有 `topic_id` 数据迁到关联表后，按 topic 列表/过滤语义不变。

**真实验收（手动，不进 CI）**
- e2e 扩展：真实录音（含可归类多主题的待办）上传 → 断言代办/记忆关联多个 topic；待办页手动加/删 topic 生效。

---

## 9. 风险与应对

| 风险 | 应对 |
|---|---|
| flash 输出 `topics` 数组不稳定（围栏、枚举漂移、重复 ref） | 解析容错 + 逐条校验丢弃；彻底失败走 stage 重试；`INSERT IGNORE` 兜重复 |
| 自然键撞名误补关联 | 同块撞名罕见，接受；如成问题再加 content 前缀入键 |
| 重跑 LLM 漂移导致手动关联丢失 | 符合预期（todo 实质变更）；trace 记恢复条数便于排查 |
| 迁移删 `topic_id` 列影响存量查询 | 迁移内回填关联表，同步改所有查询为 JOIN，集成测试覆盖 |
| 无 FK 导致关联表悬空 | 删 memory/todo 时显式删关联行（commit 事务内） |

---

## 10. 代码落点

```
migrations/000002_todo_topic.{up,down}.sql
internal/repo/todo_topic.go        (新: AddLink/RemoveLink/ListByIDs/BatchInsert/DeleteByIDs)
internal/repo/memory_topic.go      (新: 同型)
internal/repo/todo.go              (去 TopicID; List/ListByTopic 改 JOIN; InsertExt 不写 topic_id)
internal/repo/memory.go           (去 TopicID; 过滤改 JOIN)
internal/repo/topic.go             (ListWithCounts/Get 改 JOIN)
internal/memory/candidate.go       (Candidate.Topics []TopicRef)
internal/memory/topic.go           (ResolveTopics 遍历多 ref)
internal/memory/naturalkey.go      (新: canonical(segment_ids, title))
internal/pipeline/stage_extract.go (commit: 快照→清理→建 topic→插 memory_topic/todo_topic(ai)→重链 user)
internal/api/todo.go               (GET 返 topics[]; POST/DELETE topics 子路由)
internal/api/memory.go             (GET 返 topics[]; POST/DELETE topics 子路由)
internal/api/topic.go              (计数/详情改 JOIN)
prompts/extraction_v2.md           (候选 topics 数组, 版本+1)
web/app.js                         (多 topic 徽标; 编辑加/删 topic)
```

---

## 11. 实现方式

spec 获批后用 writing-plans 写实现计划；执行阶段按用户偏好在 **git worktree** 内、用 **subagent** 并行推进独立切片（如 ①迁移+repo ②pipeline+prompt ③API+web），汇合并跑 `make test` / `make test-integration` / `make e2e`。
