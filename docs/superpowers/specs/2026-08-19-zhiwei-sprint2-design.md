# 知微云端 MVP · Sprint 2 设计文档（Memory 抽取 / Todo / Topic）

- 日期：2026-08-19
- 状态：已确认
- 上游文档：`docs/superpowers/specs/2026-08-18-zhiwei-cloud-mvp-design.md`（MVP 总设计）、`docs/superpowers/plans/2026-08-18-zhiwei-sprint0-1.md`（Sprint 0-1 实现计划，已验收）
- 本文档范围：Sprint 2——pipeline 的 extract 阶段（Memory 抽取 + 质量闸门 + Topic 归类 + Todo 提取）、memory/todo/topic API 与 Web 卡片 UI。Embedding 与检索不在本期（推迟 Sprint 3）。

---

## 1. 目标与非目标

### 1.1 目标

打通产品核心价值链路：一段真实对话音频上传后，几分钟内系统产出——

- 带类型/重要度/置信度的 **Memory 卡片**（时间线可见）
- 从对话中提取的 **Todo**（独立待办页可确认/完成/忽略）
- 自动归类的 **Topic**（组织层，列表 + 详情时间线，AI 新建建议可确认/改名/忽略）

让用户第一次体验「它居然记得」。

### 1.2 非目标

- Embedding 向量化与混合检索（Sprint 3，届时为存量 memory 回填）
- Agent 问答、证据引用（Sprint 3）
- Daily Review（Sprint 4）
- Person / Project / Risk 实体、Memory Consolidation、纠错学习
- 多用户、认证

---

## 2. 对上游 spec 的三处修订（均已确认）

| 决策点 | 上游 spec 表述 | 本期结论 | 理由 |
|---|---|---|---|
| Embedding 归属 | §3.2 commit 含批量 embedding；§10 又划入 Sprint 3（自相矛盾） | **推迟 Sprint 3**，`memory.embedding` 列留空，Sprint 3 做回填 | Sprint 2 聚焦抽取与组织层价值；Sprint 3 统一处理存量回填 |
| LLM 调用粒度 | §3.2 每个对话块送 flash | **混合窗口切分**：≤8 块一次调用；>8 块按每 10 块切窗口 | 逐块调用成本高且跨块上下文缺失；整段调用有输出 token 上限风险 |
| stage 结构 | §3.2 拆 extract/quality/commit 三个 stage | **合并为一个 extract stage**（handler 内部完成抽取→闸门→commit 单事务） | 中间产物无存储位置（9 张表无候选表）；quality 纯规则无独立重试价值；失败重试重调一次 flash 成本可接受 |

---

## 3. Pipeline 扩展（extract stage）

`Flow.Stages` 从 `asr, segment` 扩为 `asr, segment, extract`。extract handler 内部五个子步骤，全部失败在同 stage 重试（指数由现有状态机管理，上限 3 次）：

### 3.1 对话块聚合（纯内存计算）

从 `transcript_segment` 读取分段，**连续同 speaker 的相邻段**聚合为一个「对话块」：

```text
Block { SpeakerLabel, Text(段文本拼接), StartMS, EndMS, SegmentIDs[] }
```

- 相邻两段时间间隔 > 30s 时强制切块（防长静默缝合两个话题）
- 空文本段跳过
- 不扩展 segment stage（保持其「全文汇总」语义），聚合在 extract handler 内完成

### 3.2 窗口切分（混合粒度）

- 块数 ≤ 8 → 一次 LLM 调用
- 块数 > 8 → 按每 10 块切窗口（`ZW_EXTRACT_WINDOW` 可配），逐窗口调用，候选合并去重（同 title+content 视为重复，保留置信度高者）
- 每窗口在 prompt 中限制候选数 ≤ 10，防输出 token 超限

### 3.3 LLM 抽取

- 模型：`doubao-seed-1.6-flash`（Tier 1，沿用现有 `provider.LLMProvider` 接口与 Ark 实现）
- prompt 版本化：`prompts/extraction_v1.md`，版本号记入 job.trace
- 输入：对话块（带说话人标签与时间）+ 当前 Topic 列表（active + suggested，上限 30 个，超出取最近更新的 30 个）
- 输出 JSON：

```json
{"candidates": [{
  "type": "event|fact|decision|idea|problem|preference",
  "title": "…", "content": "…",
  "epistemic_type": "observed|inferred|suggested",
  "importance": 0.5, "confidence": 0.9,
  "is_todo": true, "todo_due": "2026-08-20T10:00:00Z | null",
  "topic_id": "已有 topic 的 id | null",
  "suggested_topic_name": "新主题建议 | null"
}]}
```

- 解析容错：剥掉 markdown 代码围栏、截取首个 `{` 到末个 `}` 再 unmarshal
- 解析失败 = stage 失败，走重试；解析成功后逐条校验，不合法条目丢弃（不失败整个 stage）

### 3.4 质量闸门（纯规则，阈值 env 可配）

| 规则 | 默认值 | 动作 |
|---|---|---|
| `confidence < 0.6` | `ZW_QUALITY_MIN_CONF` | 丢弃 |
| `content` 少于 8 个字符 | — | 丢弃（防碎片） |
| type / epistemic_type 枚举外 | — | 丢弃 |
| `is_todo` 且 `confidence ≥ 0.85` | `ZW_QUALITY_TODO_CONF` | todo 入库为 `confirmed` |
| `is_todo` 且 `confidence < 0.85` | 同上 | todo 入库为 `suggested`（降级，「要不要加入 Todo？」） |

### 3.5 commit（单事务，幂等）

一个 DB 事务内依次：

1. **幂等清理**：删除本 session 已有的 memory 与 todo（stage 重跑不产生重复数据）
2. **Topic 解析**（每条候选）：
   - 带合法 `topic_id`（属于本 user 且非 dismissed）→ 直接挂
   - 带 `suggested_topic_name` → 查本 user 同名（active/suggested）topic，命中则挂（同名合并）；否则创建 `status=suggested, created_by=ai`
   - 都没有 → `topic_id NULL`（未归类）
   - 幂等清理不删已创建的 suggested topic：同名查重天然幂等，且 topic 可能已被用户确认使用
3. **批量插 memory**：`transcript_segment_ids` 存块内 segment id 数组（provenance）；`event_at` = session.created_at + 块 start_ms 毫秒偏移（录音时间近似值，精确回放靠 segment 关联）
4. **插 todo**：`source_memory_id` 关联产生它的 memory，`topic_id` 继承该 memory 的归属，`status` 由闸门决定，`confidence` 同 memory

无有效文字的会话（块聚合后总文本为空）：跳过 LLM，事务为空提交，session 照常 completed（上游 spec 既有约定）。

trace 记录：各子步骤耗时、模型名、prompt 版本、token 用量、窗口数、候选数（产出/丢弃）。

### 3.6 代码落点

```text
internal/pipeline/stage_extract.go   # extract stage 编排（读 segments → 调领域逻辑 → 事务提交）
internal/memory/extract.go           # 对话块聚合、窗口切分、候选解析、质量闸门（纯逻辑，可单测）
internal/repo/memory.go / todo.go / topic.go   # 三张表 DAO（事务由 stage 层开启传入）
prompts/extraction_v1.md             # 抽取 prompt（版本化）
```

---

## 4. API 设计

沿用现有约定：chi 路由、雪花 ID 走 URL 路径且 JSON 中为字符串、单用户免登录。错误处理沿用 `http.Error` + 中文消息；不存在 ID 返回 404，请求体非法 400。

**Memories**

- `GET /api/memories` — 列表。过滤参数 `type` / `topic_id` / `since`，按 event_at 倒序，`limit`/`offset` 分页。响应含 topic 名称（前端卡片直接展示归属）
- `PATCH /api/memories/{id}` — body `{title?, content?, status?}`。改 title/content 则 `version+1`（整体替换，不做字段级 diff）；`status: "dismissed"` 即删除语义

**Todos**

- `GET /api/todos` — 列表。过滤参数 `topic_id` / `status`，按 created_at 倒序
- `PATCH /api/todos/{id}` — body `{status}`。状态机：`suggested → confirmed`、`confirmed → done`，任意非 dismissed 状态可 `dismissed`；非法流转返回 409

**Topics**

- `GET /api/topics` — 列表。每个 topic 附 memory 计数与未完成（confirmed）todo 计数（一条聚合 SQL），按计数倒序
- `POST /api/topics` — body `{name, description?}`。`status=active, created_by=user`；与现有（active/suggested）topic 重名返回 409
- `GET /api/topics/{id}` — 详情 = topic 本体 + 关联 memory 时间线（event_at 倒序）+ 关联 todo
- `PATCH /api/topics/{id}` — body `{status?, name?}`。确认（`suggested→active`）/ 改名 / 忽略，单字段操作。确认**不连带**确认 topic 下的 suggested todo——todo 独立确认

**既有接口扩展**

- `GET /api/sessions/{id}` — 详情响应追加 `memories`、`todos` 数组，时间线详情页一次请求渲染全部卡片

---

## 5. Web UI

沿用 Vue 3 CDN 无构建单页，现有样式体系（卡片/徽标/配色）。标签栏扩为四个：**时间线 / 录音 / Topics / 待办**（问知微、今日留待 Sprint 3/4）。

现有 `index.html` 约 400 行，本期做轻拆分（仍无构建）：`web/index.html`（结构+样式）+ `web/app.js`（Vue 应用）。

**时间线详情（扩展）**：转写文本下方渲染——

- Memory 卡片：类型徽标（事件/事实/决定/想法/问题/偏好，颜色区分）、标题、内容、重要度、所属 Topic 名、时间；右上角 ✕ 忽略（PATCH dismiss，卡片淡出）
- Todo 卡片：标题、截止时间、状态徽标。只读展示（操作统一在待办页），保持时间线「回放」语义

**Topics 标签页（新）**：

- 列表：名称、memory 计数、未完成 todo 计数；suggested 状态带黄色「待确认」徽标，行内按钮确认/忽略；点击名称进入详情
- 详情（页内切换，无路由）：关联 memory 时间线（卡片同时间线样式）+ 该 topic 的 todo；顶部内联编辑改名
- 「+ 新建 Topic」行内表单（名称必填、描述可选）

**待办标签页（新）**：按状态分三组——

- **待确认**（suggested）：黄底卡片，标题 + 截止时间 + 来源（点击跳时间线对应会话），按钮「加入」/「忽略」
- **进行中**（confirmed）：按钮「完成」/「忽略」，过期截止时间标红
- **已完成**（done）：默认折叠，点击展开

所有操作即时 PATCH + 本地乐观更新（失败回滚并提示），不整页刷新。

---

## 6. 测试策略

沿用两级测试约定：`make test`（纯逻辑，无外部依赖）/ `make test-integration`（需 `TEST_MYSQL_DSN`，fake LLM + 真 MySQL）。

**单元测试（TDD 重点）**

- 对话块聚合：连续同 speaker 合并、超 30s 间隔切块、空文本跳过、segment_ids 收集正确
- 窗口切分：≤8 块单次调用、>8 块按窗口边界切、末窗不足整窗的边界
- LLM 响应解析容错：围栏剥离、枚举外字段丢弃、非法 JSON 报错
- 质量闸门：confidence 阈值丢弃、短内容丢弃、todo 按置信度定 suggested/confirmed
- Topic 归属决策（纯函数部分）：合法 topic_id 直挂、同名合并、无归属置空
- Todo 状态机：合法流转通过、非法流转拒绝

**集成测试**

- extract stage 端到端：fake LLM 返回固定候选 JSON → pipeline 跑到 done → 断言 memory/todo/topic 落库字段正确；**重跑幂等**（再次执行不产生重复数据）
- API handler（httptest）：三组资源的列表过滤、PATCH 流转、404/409 路径

**真实验收（手动，不进 CI）**

- `scripts/e2e.sh` 扩展：轮询到 done 后追加断言 `GET /api/memories` 非空
- Sprint Done 标准：真实录音（含明确待办 + 至少一个可归类主题）上传后——时间线出现 memory/todo 卡片、Topic 页出现归类或建议、待办页可走完确认→完成闭环

**Prompt 调优**：实现期迭代，不算独立任务；prompt 版本号记入 job.trace。

---

## 7. 风险与应对

| 风险 | 应对 |
|---|---|
| flash 模型输出 JSON 不稳定（围栏、多余文本、枚举漂移） | 解析容错 + 逐条校验丢弃；解析彻底失败走 stage 重试；prompt 迭代不改代码 |
| 抽取质量差（垃圾卡片/漏抽） | 闸门阈值 env 可配；prompt 版本化迭代；e2e 用固定真人语音回归 |
| 长会话窗口切分后同话题跨窗口、topic 归属不一致 | 同名 suggested_topic_name 在 commit 时统一合并；候选去重规则兜底 |
| 重跑幂等清理误删用户手动数据 | 清理只删 `session_id` 匹配的 memory/todo（用户手动创建的 todo 无 session 来源，不受影响） |
