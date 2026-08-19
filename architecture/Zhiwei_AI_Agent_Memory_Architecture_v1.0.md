# 知微 Zhiwei AI Agent & Memory Architecture
## 工程设计文档 V1.0

> 产品：知微 Zhiwei  
> 定位：AI 全时生活记忆与个人智能体  
> 核心链路：Sensing → Perception → Memory → World Model → Agent → Action → Review → Learning  
> 文档目标：将知微从产品方案进一步落到可直接开工的工程架构、数据模型、Agent Workflow、Memory Engine、检索、Harness、API 与 MQ 设计。

---

# 1. 设计目标

## 1.1 核心目标

知微不是“录音 + ASR + 总结”，而是建立一个长期运行的 Personal World Model：

```text
硬件持续感知
    ↓
音频/事件流
    ↓
ASR + Speaker
    ↓
Event / Fact / Decision / Todo
    ↓
Memory Engine
    ↓
Personal World Model
    ↓
Topic / Project / Goal / Person
    ↓
Agent Harness
    ↓
Action / Reminder / Review
    ↓
User Correction
    ↓
Memory Learning
```

## 1.2 工程原则

1. Raw Audio、Transcript、Memory、World Model 分层存储。
2. 所有 AI 派生信息必须保留 provenance。
3. Agent 不允许直接访问 MySQL。
4. Agent 只能通过 Domain Tool 修改业务状态。
5. AI 修改采用 Proposal → Validate → Commit。
6. MySQL 是 Source of Truth；ES 是搜索索引；Redis 是缓存。
7. Vector Search、Keyword Search、Graph Search、Time Search 组合召回。
8. 事实与推断必须区分。
9. 低置信度结论不能自动升级为长期事实。
10. 所有重要 Memory 都可追溯到原始 Transcript。
11. 所有用户修改都进入 Correction Log。
12. Agent Workflow 必须可观测、可回放、可评估。

---

# 2. 总体系统架构

```text
                         ┌─────────────────┐
                         │ Zhiwei Hardware │
                         └────────┬────────┘
                                  │ BLE / Wi-Fi
                                  ▼
                         ┌─────────────────┐
                         │ Zhiwei Mobile   │
                         │ Flutter + Native │
                         └────────┬────────┘
                                  │ HTTPS / WS
                                  ▼
                         ┌─────────────────┐
                         │ API Gateway     │
                         │ tRPC-Go         │
                         └────────┬────────┘
                                  │
             ┌────────────────────┼────────────────────┐
             │                    │                    │
             ▼                    ▼                    ▼
       Audio Service         Core Service        Agent Service
             │                    │                    │
             ▼                    ▼                    ▼
           OSS/S3               MySQL          Agent Harness
             │                    │                    │
             └──────────────┐     │     ┌──────────────┘
                            ▼     ▼     ▼
                             MQ / Event Bus
                                  │
               ┌──────────────────┼──────────────────┐
               ▼                  ▼                  ▼
              ASR              Speaker         Understanding
               │                  │                  │
               └──────────────────┼──────────────────┘
                                  ▼
                         Memory Engine
                                  │
                     ┌────────────┼────────────┐
                     ▼            ▼            ▼
                   Topic         Todo         Risk
                     │            │            │
                     └────────────┼────────────┘
                                  ▼
                          World Model / Graph
                                  │
                                  ▼
                          Review / Insight
                                  │
                                  ▼
                               User
```

---

# 3. 服务拆分

## 3.1 API / Core Service

技术：

- Go
- tRPC-Go
- MySQL
- Redis

职责：

- User
- Device
- Timeline
- Memory
- Person
- Topic
- Project
- Todo
- Reminder
- Review
- Agent Conversation

---

## 3.2 Audio Service

职责：

- 音频上传
- Chunk 管理
- Session 管理
- 校验
- OSS 写入
- 音频生命周期
- 上传幂等
- 断点续传

---

## 3.3 AI Gateway

统一模型入口：

```text
ASR
LLM
Embedding
Vision
TTS
Image Generation
```

能力：

- Model Router
- Fallback
- Timeout
- Retry
- Cost Control
- Rate Limit
- Model Version
- Prompt Version
- A/B Test

---

## 3.4 Memory Engine

职责：

- Memory Candidate
- Memory Validation
- Memory Merge
- Memory Conflict
- Memory Version
- Memory Provenance
- Memory Retrieval
- Memory Decay
- Memory Consolidation

这是知微的核心基础设施。

---

## 3.5 World Model Service

维护：

- Person
- Topic
- Project
- Goal
- Decision
- Todo
- Risk
- Relationship
- State

提供 Graph-like 查询。

---

## 3.6 Agent Service

负责：

- Agent Session
- Context Assembly
- Tool Calling
- Planning
- Workflow
- Permission
- Proposal
- Action
- Audit

Agent Runtime 可以与 DeepSeek-Harness 集成。

---

# 4. 音频数据 Pipeline

```text
Hardware
  ↓
VAD
  ↓
Opus Chunk
  ↓
Local Encrypted Queue
  ↓
BLE
  ↓
Mobile Queue
  ↓
HTTPS Upload
  ↓
OSS
  ↓
audio.uploaded
  ↓
ASR
  ↓
Speaker Diarization
  ↓
Semantic Understanding
```

## 4.1 Chunk

建议：

- 5～15 秒逻辑 Chunk
- sequence_no
- session_id
- start_at
- end_at
- checksum

必须支持：

- Retry
- Resume
- Deduplication
- Out-of-order
- ACK

---

# 5. MQ Event

推荐 RocketMQ 或 Kafka。

Topic：

```text
zw.audio.uploaded
zw.audio.processed

zw.asr.completed
zw.speaker.completed

zw.transcript.created
zw.memory.candidate.created
zw.memory.committed

zw.topic.updated
zw.todo.created
zw.todo.updated

zw.risk.detected
zw.review.requested

zw.agent.action.proposed
zw.agent.action.committed

zw.user.correction.created
```

Event 示例：

```json
{
  "event_id": "evt_123",
  "event_type": "transcript.created",
  "user_id": "u_123",
  "session_id": "s_123",
  "trace_id": "trace_123",
  "occurred_at": "2026-08-18T10:30:00Z",
  "payload": {}
}
```

所有消费者必须：

- 幂等
- 可重试
- DLQ
- Trace ID
- Event Version

---

# 6. MySQL 数据模型

## 6.1 user

```sql
CREATE TABLE user (
  id BIGINT PRIMARY KEY,
  status TINYINT NOT NULL,
  timezone VARCHAR(64),
  locale VARCHAR(16),
  created_at DATETIME(3),
  updated_at DATETIME(3)
);
```

## 6.2 device

```sql
CREATE TABLE device (
  id BIGINT PRIMARY KEY,
  user_id BIGINT NOT NULL,
  device_sn VARCHAR(128) NOT NULL,
  firmware_version VARCHAR(64),
  battery_level TINYINT,
  last_seen_at DATETIME(3),
  status TINYINT,
  created_at DATETIME(3),
  updated_at DATETIME(3),
  UNIQUE KEY uk_device_sn(device_sn),
  KEY idx_user(user_id)
);
```

## 6.3 audio_session

```sql
CREATE TABLE audio_session (
  id BIGINT PRIMARY KEY,
  user_id BIGINT NOT NULL,
  device_id BIGINT,
  start_at DATETIME(3),
  end_at DATETIME(3),
  duration_ms BIGINT,
  status VARCHAR(32),
  oss_key VARCHAR(512),
  checksum VARCHAR(128),
  created_at DATETIME(3),
  updated_at DATETIME(3),
  KEY idx_user_time(user_id, start_at)
);
```

## 6.4 audio_chunk

```sql
CREATE TABLE audio_chunk (
  id BIGINT PRIMARY KEY,
  session_id BIGINT NOT NULL,
  sequence_no INT NOT NULL,
  start_at DATETIME(3),
  end_at DATETIME(3),
  duration_ms INT,
  oss_key VARCHAR(512),
  checksum VARCHAR(128),
  upload_status VARCHAR(32),
  created_at DATETIME(3),
  UNIQUE KEY uk_session_seq(session_id, sequence_no)
);
```

---

# 7. Transcript Schema

```sql
CREATE TABLE transcript (
  id BIGINT PRIMARY KEY,
  user_id BIGINT NOT NULL,
  session_id BIGINT,
  language VARCHAR(16),
  text TEXT NOT NULL,
  start_at DATETIME(3),
  end_at DATETIME(3),
  confidence DECIMAL(5,4),
  status VARCHAR(32),
  created_at DATETIME(3),
  updated_at DATETIME(3),
  KEY idx_user_time(user_id, start_at)
);
```

```sql
CREATE TABLE transcript_segment (
  id BIGINT PRIMARY KEY,
  transcript_id BIGINT NOT NULL,
  speaker_id BIGINT,
  text TEXT NOT NULL,
  start_at DATETIME(3),
  end_at DATETIME(3),
  confidence DECIMAL(5,4),
  speaker_confidence DECIMAL(5,4),
  sequence_no INT,
  created_at DATETIME(3),
  KEY idx_transcript(transcript_id)
);
```

---

# 8. Speaker / Voiceprint

## speaker

```sql
CREATE TABLE speaker (
  id BIGINT PRIMARY KEY,
  user_id BIGINT NOT NULL,
  name VARCHAR(128),
  type VARCHAR(32),
  confidence DECIMAL(5,4),
  status VARCHAR(32),
  created_at DATETIME(3),
  updated_at DATETIME(3),
  KEY idx_user(user_id)
);
```

## voiceprint

```sql
CREATE TABLE voiceprint (
  id BIGINT PRIMARY KEY,
  speaker_id BIGINT NOT NULL,
  embedding_ref VARCHAR(512),
  model_version VARCHAR(64),
  sample_count INT,
  quality_score DECIMAL(5,4),
  created_at DATETIME(3),
  updated_at DATETIME(3)
);
```

声纹原始 embedding 不建议直接暴露给业务层。

---

# 9. Memory 数据模型

这是最重要的核心表。

```sql
CREATE TABLE memory (
  id BIGINT PRIMARY KEY,
  user_id BIGINT NOT NULL,

  type VARCHAR(32) NOT NULL,
  title VARCHAR(512),
  content TEXT NOT NULL,

  source_type VARCHAR(32),
  source_id BIGINT,

  event_at DATETIME(3),
  observed_at DATETIME(3),

  importance DECIMAL(5,4),
  confidence DECIMAL(5,4),

  epistemic_type VARCHAR(32),
  status VARCHAR(32),

  topic_id BIGINT,
  project_id BIGINT,

  persistence_policy VARCHAR(32),

  version INT NOT NULL DEFAULT 1,

  created_at DATETIME(3),
  updated_at DATETIME(3),

  KEY idx_user_time(user_id, event_at),
  KEY idx_user_type(user_id, type),
  KEY idx_topic(topic_id),
  KEY idx_project(project_id)
);
```

---

# 10. Memory 类型

```text
event
fact
decision
todo
idea
preference
relationship
goal
milestone
problem
insight
```

---

# 11. Epistemic Type

每条 Memory 必须标记：

```text
observed
inferred
predicted
suggested
```

例如：

```json
{
  "type": "fact",
  "content": "项目 X 计划周五上线",
  "epistemic_type": "observed",
  "confidence": 0.96
}
```

---

# 12. Memory Provenance

```sql
CREATE TABLE memory_source (
  id BIGINT PRIMARY KEY,
  memory_id BIGINT NOT NULL,
  source_type VARCHAR(32),
  source_id BIGINT,
  contribution DECIMAL(5,4),
  created_at DATETIME(3),
  KEY idx_memory(memory_id)
);
```

支持：

```text
Memory
 ↓
Transcript 1
Transcript 2
Transcript 8
```

用于解释：

> “为什么知微这样判断？”

---

# 13. Memory Version

```sql
CREATE TABLE memory_version (
  id BIGINT PRIMARY KEY,
  memory_id BIGINT NOT NULL,
  version INT NOT NULL,
  content TEXT,
  properties JSON,
  change_type VARCHAR(32),
  changed_by VARCHAR(32),
  source_id BIGINT,
  created_at DATETIME(3),
  KEY idx_memory_version(memory_id, version)
);
```

用户纠错必须产生版本，而不是覆盖原数据。

---

# 14. Memory Relation

```sql
CREATE TABLE memory_relation (
  id BIGINT PRIMARY KEY,
  user_id BIGINT NOT NULL,
  source_memory_id BIGINT NOT NULL,
  relation_type VARCHAR(64),
  target_memory_id BIGINT NOT NULL,

  confidence DECIMAL(5,4),

  valid_from DATETIME(3),
  valid_to DATETIME(3),

  status VARCHAR(32),

  created_at DATETIME(3),

  KEY idx_source(source_memory_id),
  KEY idx_target(target_memory_id),
  KEY idx_relation(user_id, relation_type)
);
```

---

# 15. Person

```sql
CREATE TABLE person (
  id BIGINT PRIMARY KEY,
  user_id BIGINT NOT NULL,
  name VARCHAR(128),
  aliases JSON,
  speaker_id BIGINT,
  relationship_type VARCHAR(64),
  importance DECIMAL(5,4),
  created_at DATETIME(3),
  updated_at DATETIME(3)
);
```

---

# 16. Topic

```sql
CREATE TABLE topic (
  id BIGINT PRIMARY KEY,
  user_id BIGINT NOT NULL,

  name VARCHAR(256),
  description TEXT,

  type VARCHAR(32),

  goal TEXT,
  status VARCHAR(32),

  progress DECIMAL(5,4),
  health_score DECIMAL(5,4),

  start_at DATETIME(3),
  target_at DATETIME(3),

  created_by VARCHAR(32),

  created_at DATETIME(3),
  updated_at DATETIME(3)
);
```

---

# 17. Project / Goal

Project：

```sql
CREATE TABLE project (
  id BIGINT PRIMARY KEY,
  user_id BIGINT NOT NULL,
  topic_id BIGINT,
  name VARCHAR(256),
  description TEXT,
  status VARCHAR(32),
  progress DECIMAL(5,4),
  deadline DATETIME(3),
  created_at DATETIME(3),
  updated_at DATETIME(3)
);
```

Goal：

```sql
CREATE TABLE goal (
  id BIGINT PRIMARY KEY,
  user_id BIGINT NOT NULL,
  topic_id BIGINT,
  title VARCHAR(256),
  description TEXT,
  status VARCHAR(32),
  target_value DECIMAL(18,4),
  current_value DECIMAL(18,4),
  target_at DATETIME(3),
  created_at DATETIME(3),
  updated_at DATETIME(3)
);
```

---

# 18. Todo / Risk / Decision

## Todo

```sql
CREATE TABLE todo (
  id BIGINT PRIMARY KEY,
  user_id BIGINT NOT NULL,
  title VARCHAR(512),
  description TEXT,
  source_memory_id BIGINT,
  topic_id BIGINT,
  project_id BIGINT,
  priority TINYINT,
  status VARCHAR(32),
  due_at DATETIME(3),
  confidence DECIMAL(5,4),
  created_by VARCHAR(32),
  created_at DATETIME(3),
  updated_at DATETIME(3)
);
```

## Risk

```sql
CREATE TABLE risk (
  id BIGINT PRIMARY KEY,
  user_id BIGINT NOT NULL,
  topic_id BIGINT,
  project_id BIGINT,
  title VARCHAR(512),
  description TEXT,
  severity TINYINT,
  probability DECIMAL(5,4),
  confidence DECIMAL(5,4),
  status VARCHAR(32),
  source_memory_id BIGINT,
  created_at DATETIME(3),
  updated_at DATETIME(3)
);
```

## Decision

```sql
CREATE TABLE decision (
  id BIGINT PRIMARY KEY,
  user_id BIGINT NOT NULL,
  topic_id BIGINT,
  project_id BIGINT,
  title VARCHAR(512),
  decision TEXT,
  decided_at DATETIME(3),
  status VARCHAR(32),
  source_memory_id BIGINT,
  created_at DATETIME(3),
  updated_at DATETIME(3)
);
```

---

# 19. World Model / Graph

第一阶段不引入独立图数据库。

使用 MySQL：

```text
entity
relation
```

## Entity

```sql
CREATE TABLE entity (
  id BIGINT PRIMARY KEY,
  user_id BIGINT NOT NULL,
  entity_type VARCHAR(64),
  ref_id BIGINT,
  name VARCHAR(256),
  properties JSON,
  created_at DATETIME(3),
  updated_at DATETIME(3),
  KEY idx_user_type(user_id, entity_type),
  UNIQUE KEY uk_ref(user_id, entity_type, ref_id)
);
```

## Relation

```sql
CREATE TABLE relation (
  id BIGINT PRIMARY KEY,
  user_id BIGINT NOT NULL,

  source_entity_id BIGINT NOT NULL,
  relation_type VARCHAR(64),
  target_entity_id BIGINT NOT NULL,

  confidence DECIMAL(5,4),

  valid_from DATETIME(3),
  valid_to DATETIME(3),

  source_memory_id BIGINT,

  status VARCHAR(32),

  created_at DATETIME(3),

  KEY idx_source(source_entity_id, relation_type),
  KEY idx_target(target_entity_id, relation_type)
);
```

---

# 20. Graph Relation 示例

```text
User
 ├── knows → Alice
 ├── works_on → Project A
 ├── learning → Rust
 │
Project A
 ├── owned_by → User
 ├── depends_on → Alice
 ├── has_risk → Risk 123
 └── has_todo → Todo 456
```

关系必须支持时间：

```text
valid_from
valid_to
```

避免历史事实覆盖当前事实。

---

# 21. Elasticsearch Mapping

建议至少：

```text
zw_transcript_v1
zw_memory_v1
zw_todo_v1
zw_topic_v1
zw_person_v1
zw_project_v1
```

Memory 文档：

```json
{
  "id": "m123",
  "user_id": "u123",
  "type": "decision",
  "title": "项目 X 周五上线",
  "content": "用户与 Alice 决定项目 X 周五上线",
  "event_at": "2026-08-18T10:30:00Z",
  "importance": 0.92,
  "confidence": 0.96,
  "topic_id": "t123",
  "project_id": "p123",
  "person_ids": ["person_1"],
  "embedding": [ ... ]
}
```

---

# 22. Hybrid Retrieval

查询：

> “我最近和 Alice 讨论过哪些项目？”

并行：

```text
1. BM25
2. Vector
3. Person Filter
4. Time Filter
5. Graph Traversal
```

得到候选：

```text
Candidate Set
```

再进行：

```text
Rerank
```

最后：

```text
Context Builder
```

---

# 23. Retrieval Pipeline

```text
User Query
   ↓
Query Understanding
   ↓
Intent / Entity / Time Extraction
   ↓
┌────────────┬────────────┬────────────┐
│            │            │
BM25       Vector       Graph
│            │            │
└────────────┴────────────┘
             ↓
       Candidate Merge
             ↓
          Reranker
             ↓
       Diversity Filter
             ↓
      Context Compression
             ↓
            LLM
```

---

# 24. Retrieval Score

初始版本：

```text
score =
0.30 * semantic_score
+ 0.20 * keyword_score
+ 0.15 * recency_score
+ 0.15 * importance_score
+ 0.10 * topic_score
+ 0.10 * confidence_score
```

后续使用用户行为学习权重。

---

# 25. Time Retrieval

时间是知微特有的重要维度。

Query：

> “去年我为什么开始学 Rust？”

必须转换：

```json
{
  "time_range": {
    "start": "2025-01-01",
    "end": "2025-12-31"
  },
  "semantic_query": "开始学习 Rust 的原因"
}
```

---

# 26. Memory Consolidation

每天执行：

```text
Recent Memories
       ↓
Cluster
       ↓
Entity Resolution
       ↓
Duplicate Detection
       ↓
Conflict Detection
       ↓
Long-term Memory Update
```

例如：

```text
M1：我要学 Rust
M2：开始看 ownership
M3：最近没时间
M4：暂停 Rust
```

最终：

```text
Topic = Rust Learning
Status = Paused
Reason = Work Pressure
```

---

# 27. Memory Conflict Resolution

如果出现：

```text
Deadline = Friday
Deadline = Wednesday
```

不要删除旧数据。

状态：

```text
Friday
status = superseded

Wednesday
status = active
```

同时记录：

```text
superseded_by
source_memory
valid_from
valid_to
```

---

# 28. Agent 总体架构

不做“一个超级 Agent”。

采用：

```text
Agent Runtime
    +
Workflow
    +
Domain Tools
    +
Memory Engine
```

逻辑 Agent：

```text
Perception Agent
Memory Consolidator
Topic Agent
Todo Agent
Risk Agent
Review Agent
User Agent
```

运行时统一由 Agent Harness 承载。

---

# 29. Event-driven Agent Workflow

```text
audio.uploaded
      ↓
ASR Workflow
      ↓
Speaker Workflow
      ↓
Memory Extraction Workflow
      ↓
Memory Consolidation
      ↓
World Model Update
      ↓
Topic/Todo/Risk Update
```

这些是确定性 Workflow。

LLM 只负责：

```text
Extraction
Classification
Reasoning
Planning
Generation
```

不要让 LLM 决定整个系统流程。

---

# 30. User Agent Workflow

```text
User Query
    ↓
Intent Detection
    ↓
Entity / Time Extraction
    ↓
Memory Retrieval
    ↓
World Model Retrieval
    ↓
Context Assembly
    ↓
LLM Reasoning
    ↓
Tool Call?
   /     \
 No      Yes
 |        |
Answer   Proposal
           ↓
        Validator
           ↓
         Commit
           ↓
         Answer
```

---

# 31. Memory Transaction

Agent 不直接 UPDATE DB。

流程：

```text
BEGIN
 ↓
Read
 ↓
Reason
 ↓
Proposed Change
 ↓
Validate
 ↓
Permission
 ↓
Commit
```

Proposal：

```json
{
  "action": "update_project_deadline",
  "target": {
    "type": "project",
    "id": "p123"
  },
  "changes": {
    "deadline": "2026-08-26T23:59:59Z"
  },
  "evidence": [
    "memory_123"
  ],
  "confidence": 0.94,
  "requires_confirmation": false
}
```

---

# 32. Tool Schema

## search_memory

```json
{
  "name": "search_memory",
  "description": "Search user's memories",
  "input_schema": {
    "query": "string",
    "time_range": "optional",
    "topic_id": "optional",
    "person_id": "optional",
    "limit": "integer"
  }
}
```

## get_topic

```json
{
  "name": "get_topic",
  "input_schema": {
    "topic_id": "string"
  }
}
```

## update_todo

```json
{
  "name": "update_todo",
  "input_schema": {
    "todo_id": "string",
    "status": "optional",
    "due_at": "optional",
    "title": "optional"
  }
}
```

---

# 33. Tool Permission

## Read

自动：

```text
search_memory
get_memory
get_topic
get_project
search_todo
get_person
```

## Write

默认建议模式：

```text
create_todo
update_todo
create_topic
create_reminder
```

## High Risk

必须确认：

```text
delete_memory
bulk_update
export_data
share_memory
delete_person
```

---

# 34. DeepSeek-Harness 集成

如果采用 DeepSeek-Harness 作为 Agent Harness，则定位为：

> Agent Runtime / Control Plane

而不是业务数据库访问层。

架构：

```text
Zhiwei Agent Service
        ↓
DeepSeek-Harness
        ↓
Planner / Context / Tool / State
        ↓
Zhiwei Domain Tools
        ↓
Memory / Topic / Todo Service
        ↓
MySQL / ES / Redis
```

Harness 负责：

- Agent State
- Workflow
- Context
- Tool Calling
- Retry
- Timeout
- Execution Trace
- Evaluation
- Model Routing

Zhiwei 负责：

- Memory
- World Model
- Business State
- Permissions
- User Data
- Domain Tools

---

# 35. Harness Tool Boundary

错误：

```text
Agent → SQL → MySQL
```

正确：

```text
Agent
 ↓
Harness
 ↓
Tool
 ↓
Domain Service
 ↓
MySQL
```

这样未来替换模型或 Harness，不影响核心数据层。

---

# 36. Agent State

Topic Agent 示例：

```json
{
  "agent_id": "topic_agent",
  "topic_id": "topic_123",
  "state": "active",
  "goal": "完成 Rust 学习",
  "current_phase": "ownership",
  "progress": 0.42,
  "next_review_at": "2026-08-20T20:00:00Z",
  "risk_ids": [],
  "todo_ids": ["todo_1", "todo_2"]
}
```

---

# 37. 长周期 Topic Workflow

```text
Topic Created
      ↓
Goal Definition
      ↓
Initial Plan
      ↓
Observe Events
      ↓
Update Progress
      ↓
Detect Risk
      ↓
Suggest Action
      ↓
Wait
      ↓
Re-observe
      ↓
Review
      ↓
Complete / Continue
```

这就是知微最重要的 Long-running Agent 场景。

---

# 38. Monitoring Agent

不需要持续调用 LLM。

采用：

```text
Event / Scheduler
      ↓
Rule Filter
      ↓
Candidate Generation
      ↓
LLM Reasoning
```

例如：

```text
Project deadline < 3 days
AND
progress < 60%
```

才触发 LLM。

这样降低成本。

---

# 39. Risk Workflow

```text
New Event
 ↓
Rule Engine
 ↓
Risk Candidate
 ↓
LLM Validation
 ↓
Risk
 ↓
Notify?
```

Risk 类型：

```text
deadline
stalled
dependency
resource
goal_conflict
behavior
```

---

# 40. Daily Review Workflow

每天固定时间：

```text
Scheduler
 ↓
Get Today's Memories
 ↓
Get Active Topics
 ↓
Get Todos
 ↓
Get Decisions
 ↓
Get Risks
 ↓
Get Important People
 ↓
Review Agent
 ↓
Daily Review
 ↓
Notification
```

---

# 41. Review 输出 Schema

```json
{
  "date": "2026-08-18",
  "headline": "今天主要在推进项目 X",
  "highlights": [],
  "completed": [],
  "todos": [],
  "decisions": [],
  "risks": [],
  "insights": [],
  "tomorrow": [],
  "comic_story": []
}
```

---

# 42. Prompt 分层

Prompt 不写死代码。

建议：

```text
system/
  agent_base.md
  safety.md
  memory_policy.md

memory/
  extraction_v1.md
  consolidation_v1.md
  conflict_v1.md

topic/
  detection_v1.md
  progress_v1.md
  risk_v1.md

review/
  daily_v1.md
  weekly_v1.md
```

---

# 43. Context Assembly

Agent Context：

```text
System
+
User Profile
+
Current Conversation
+
Recent Memories
+
Relevant Memories
+
Active Topics
+
Current Todos
+
Current Risks
+
Relevant People
+
Recent Corrections
```

不要：

```text
把所有历史 Memory 全塞进去
```

---

# 44. Context Budget

建议分层：

```text
Recent Context
20%

Relevant Memory
40%

World Model
20%

User Profile
10%

Task / Instruction
10%
```

实际比例由模型和任务动态调整。

---

# 45. Correction Learning

用户：

> “不是 Alice，是 Bob。”

记录：

```sql
CREATE TABLE user_correction (
  id BIGINT PRIMARY KEY,
  user_id BIGINT NOT NULL,
  target_type VARCHAR(32),
  target_id BIGINT,
  field_name VARCHAR(64),
  old_value JSON,
  new_value JSON,
  reason TEXT,
  created_at DATETIME(3)
);
```

这些数据用于：

- 后续召回
- Speaker Correction
- Entity Resolution
- Prompt Personalization
- Evaluation

---

# 46. 用户个性化 Memory

用户长期纠正：

```text
“项目”默认指 Project X
Alice = Alice Zhang
Rust 学习通常指 Rust Programming
```

可以形成：

```text
User Preference / User Semantic Dictionary
```

---

# 47. Entity Resolution

同一个人可能出现：

```text
Alice
Alice Zhang
小爱
爱姐
speaker_03
```

统一：

```text
person_id = p123
```

使用：

```text
Alias
Voiceprint
Context
Co-occurrence
User Correction
```

---

# 48. Memory 写入策略

不要所有 Transcript 都成为 Memory。

采用：

```text
Transcript
 ↓
Information Density
 ↓
Candidate
```

高价值：

```text
Decision
Goal
Todo
Problem
Idea
Preference
Relationship Change
```

低价值：

```text
寒暄
重复
电视
背景音乐
无意义聊天
```

---

# 49. Memory Quality Gate

每条 Candidate 必须经过：

```text
Is it factual?
Is it useful?
Is it new?
Is it stable?
Is confidence enough?
Does it conflict?
```

评分：

```text
value_score
novelty_score
confidence_score
stability_score
```

只有达到阈值才进入长期 Memory。

---

# 50. Storage Strategy

```text
Audio
 → OSS

Transcript
 → MySQL + ES

Memory
 → MySQL + ES + Embedding

Entity / Relation
 → MySQL

Cache
 → Redis

Agent State
 → Redis + MySQL

Review
 → MySQL + OSS(optional)
```

---

# 51. Redis Key

```text
zw:user:{uid}:session
zw:user:{uid}:recent_memory
zw:user:{uid}:active_topics
zw:user:{uid}:todos
zw:device:{device_id}:state
zw:agent:{session_id}:state
zw:lock:{resource}
```

TTL：

- Session：30min
- Recent Memory：1h
- Active Topics：10min
- Agent State：24h～7d

---

# 52. tRPC-Go API

建议：

```text
user.*
device.*
audio.*
transcript.*
speaker.*
memory.*
person.*
topic.*
project.*
todo.*
risk.*
review.*
agent.*
search.*
```

---

# 53. API 示例

## memory.search

```json
{
  "query": "我最近为什么没有学习 Rust",
  "time_range": {
    "start": "2026-07-18",
    "end": "2026-08-18"
  },
  "limit": 20
}
```

## topic.get

```json
{
  "topic_id": "topic_123"
}
```

## agent.chat

```json
{
  "conversation_id": "conv_123",
  "message": "我最近为什么没有学习 Rust？"
}
```

返回：

```json
{
  "message_id": "msg_123",
  "answer": "...",
  "citations": [
    {
      "memory_id": "m123"
    }
  ],
  "actions": []
}
```

---

# 54. Agent Citation

Agent 回答必须尽量引用 Memory：

```json
{
  "answer": "最近主要是工作项目占用了晚上时间。",
  "evidence": [
    {
      "memory_id": "m1",
      "reason": "用户在 8 月 12 日提到工作加班"
    },
    {
      "memory_id": "m2",
      "reason": "用户在 8 月 15 日提到没有时间学习"
    }
  ]
}
```

前端可以点击：

> 查看原始记录。

这是建立信任的关键。

---

# 55. 可观测性

每一次 AI Pipeline 都必须有：

```text
trace_id
user_id
session_id
workflow_id
agent_run_id
model
model_version
prompt_version
latency
token_usage
cost
input_ids
output
error
```

---

# 56. Agent Trace

```text
Agent Run
 ├── Plan
 ├── Retrieve
 │    ├── BM25
 │    ├── Vector
 │    └── Graph
 │
 ├── Tool Call
 ├── Observation
 ├── Reason
 └── Final Answer
```

必须可以回放。

---

# 57. Evaluation System

建立离线评测集：

```text
Audio
Transcript
Speaker
Expected Memory
Expected Todo
Expected Topic
Expected Risk
Expected Answer
```

指标：

```text
ASR WER
Speaker Accuracy
Memory Precision
Memory Recall
Todo Precision
Topic Precision
Retrieval Recall@K
Rerank NDCG
Agent Success Rate
Action Error Rate
```

---

# 58. 核心产品指标

技术指标之外，需要：

### Memory Acceptance Rate

用户确认 / 保存 / 引用的 Memory 比例。

### Correction Rate

用户修改 AI 信息的比例。

### Recall Success Rate

用户查询后认为：

> “找到了。”

的比例。

### Agent Resolution Rate

无需用户再次解释即可解决问题的比例。

### Weekly Valuable Memory

每周用户真正使用的 Memory 数量。

---

# 59. MVP 技术范围

## P0

```text
Hardware
BLE
Audio Upload
ASR
Speaker
Transcript
Memory
ES Search
Hybrid Retrieval
Agent Chat
Todo
Daily Review
```

## P1

```text
Topic
Project
Person
Risk
Memory Consolidation
Graph Relation
Weekly Review
Correction
```

## P2

```text
Personal Insight
Long-running Topic Agent
Comic
Calendar Integration
Health Integration
```

---

# 60. 第一阶段工程团队建议

## Client

- Flutter
- iOS Native Audio/BLE
- Android Native Audio/BLE
- SQLite / Drift

## Backend

- Go
- tRPC-Go
- MySQL
- Redis
- Elasticsearch
- RocketMQ/Kafka

## AI

- AI Gateway
- Streaming ASR
- Speaker Embedding
- DeepSeek / other LLM
- Embedding
- Image Generation

## Agent

- DeepSeek-Harness
- Workflow Engine
- Domain Tool Framework
- Agent Trace

---

# 61. 第一阶段推荐项目目录

```text
zhiwei/
├── apps/
│   ├── mobile/
│   └── admin/
│
├── services/
│   ├── api/
│   ├── audio/
│   ├── asr/
│   ├── speaker/
│   ├── understanding/
│   ├── memory/
│   ├── worldmodel/
│   ├── agent/
│   └── review/
│
├── packages/
│   ├── schema/
│   ├── proto/
│   ├── tools/
│   ├── prompts/
│   └── evaluation/
│
├── deploy/
│   ├── docker/
│   ├── k8s/
│   └── terraform/
│
└── docs/
    ├── architecture/
    ├── api/
    ├── memory/
    ├── agent/
    └── evaluation/
```

---

# 62. 第一阶段最重要的 4 个工程资产

## 1. Zhiwei Memory Engine

负责：

```text
Extract
Store
Merge
Conflict
Version
Retrieve
Provenance
Correction
```

## 2. Zhiwei World Model

负责：

```text
Person
Topic
Project
Goal
Todo
Risk
Decision
Relation
```

## 3. Zhiwei Agent Runtime

基于 DeepSeek-Harness：

```text
State
Workflow
Context
Tools
Policy
Execution
Evaluation
```

## 4. Zhiwei Evaluation Platform

负责：

```text
Dataset
Annotation
Offline Evaluation
Online Evaluation
Regression
Prompt Version
Model Version
```

---

# 63. 推荐的第一条完整闭环

不要一开始同时做所有 Agent。

先实现：

```text
Hardware
 ↓
Audio
 ↓
ASR
 ↓
Speaker
 ↓
Memory Candidate
 ↓
Memory Validation
 ↓
MySQL
 ↓
ES
 ↓
Hybrid Retrieval
 ↓
Agent Chat
 ↓
Evidence Citation
```

这条链路打通以后，再加入：

```text
Todo
 ↓
Topic
 ↓
Risk
 ↓
Review
 ↓
Long-running Agent
```

---

# 64. 最终架构原则

知微的核心不是：

```text
LLM
```

也不是：

```text
Knowledge Graph
```

而是：

```text
                    Personal World Model
                           │
            ┌──────────────┼──────────────┐
            │              │              │
        Episodic       Semantic       Goal State
         Memory         Memory
            │              │              │
            └──────────────┼──────────────┘
                           ↓
                    Temporal Relations
                           ↓
                   Hybrid Retrieval
                           ↓
                    Agent Harness
                           ↓
                  Long-running Workflow
                           ↓
                       Action
                           ↓
                       Review
                           ↓
                    User Correction
                           │
                           └────────→ Memory
```

最终形成知微自己的技术飞轮：

> **记录越多 → Memory 越丰富 → World Model 越准确 → Agent 越懂用户 → 用户越愿意使用 → Correction 越多 → Memory 越准确。**

---

# 65. 下一步工程拆解

建议按以下顺序进入开发：

### Sprint 1：基础设施

- tRPC-Go
- MySQL Schema
- Redis
- ES
- MQ
- OSS
- AI Gateway
- Trace

### Sprint 2：Audio Pipeline

- Hardware → Mobile
- Upload
- Chunk
- ASR
- Speaker
- Transcript

### Sprint 3：Memory Engine

- Candidate
- Validation
- Provenance
- Version
- Search
- Embedding
- Hybrid Retrieval

### Sprint 4：Agent

- DeepSeek-Harness
- Tool Framework
- Context Builder
- Agent Chat
- Evidence Citation
- Proposal / Commit

### Sprint 5：World Model

- Person
- Topic
- Project
- Todo
- Decision
- Risk
- Relation

### Sprint 6：Proactive Agent

- Topic Monitor
- Risk Detection
- Todo Follow-up
- Daily Review
- Weekly Review

### Sprint 7：Quality

- Evaluation Dataset
- Annotation
- Replay
- Regression
- Cost Optimization
- Latency Optimization

---

# 66. 最终判断

知微最值得长期投入的不是“模型能力”，而是：

> **Memory Engine + Personal World Model + Agent Harness**

三者关系：

```text
Memory Engine
    ↓
“我记得你发生过什么”

World Model
    ↓
“我理解你现在处于什么状态”

Agent Harness
    ↓
“我可以长期帮你把事情推进下去”
```

这三层建立起来后，ASR、LLM、Embedding、Image Model 都可以替换，而知微的核心资产仍然存在。

**知微最终应该从“AI 录音硬件”进化成真正的 Personal AI Operating System。**
