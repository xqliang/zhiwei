-- 个人智能体/chatbot P1 数据层（设计见 docs/superpowers/specs/2026-08-24-agent-chatbot-system-design.md §6）。
-- 复用现有 agent_message / daily_review；本迁移新增 4 表 + 扩展 agent_message。
-- 注意：memory 的 session_id 可空化 + conversation_id 列留到 Plan 3（对话转记忆）再加。
--
-- ⚠️【合并协调点：迁移号 000005 + 000006 双撞号】main 已占用 000005_speaker_name_candidate /
-- 000006_person / 000007_segment_embedding / 000008_event（现 v8）。本分支的 000005_agent 与
-- 000006_conversation_memory 与之撞号——【合并到 main 时两个都要重编号】到 v8 之后（如
-- 000005_agent→000009_agent、000006_conversation_memory→000010_conversation_memory；两者仅依赖
-- 000001 期的 memory/agent_message/daily_review，排在 v8 后安全）。golang-migrate 按数字前缀去重：
-- 不重编号会 "duplicate migration version" 中止；在已到 v8 的既有库上则会【静默跳过本迁移】→ agent 表不建。

-- 会话分组：一个「问知微」对话 = 一行；映射到 dsh sessionId。
CREATE TABLE agent_conversation (
  id             BIGINT PRIMARY KEY,
  user_id        BIGINT NOT NULL DEFAULT 1,
  title          VARCHAR(256) NOT NULL DEFAULT '',
  dsh_session_id VARCHAR(64) NOT NULL,
  status         VARCHAR(16) NOT NULL DEFAULT 'active', -- active|archived
  created_at     DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  last_active_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  KEY idx_user_active (user_id, last_active_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

-- 写入闸门：agent 提议的每处修改，人审前只落这里（绝不静默写）。
CREATE TABLE agent_proposal (
  id              BIGINT PRIMARY KEY,
  user_id         BIGINT NOT NULL DEFAULT 1,
  conversation_id BIGINT NULL,
  message_id      BIGINT NULL,
  kind            VARCHAR(32) NOT NULL, -- memory_update|memory_dismiss|topic_rename|topic_confirm|topic_dismiss|todo_create|todo_status
  target_kind     VARCHAR(16) NOT NULL, -- memory|topic|todo
  target_id       BIGINT NULL,
  payload         JSON NOT NULL,
  rationale       TEXT NOT NULL,
  status          VARCHAR(16) NOT NULL DEFAULT 'pending', -- pending|applied|dismissed|expired
  applied_ref     BIGINT NULL,
  created_at      DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  resolved_at     DATETIME(3) NULL,
  KEY idx_user_status (user_id, status),
  KEY idx_conv (conversation_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

-- 周报（与现有 daily_review 平行）。
CREATE TABLE weekly_review (
  id         BIGINT PRIMARY KEY,
  user_id    BIGINT NOT NULL DEFAULT 1,
  week_start DATE NOT NULL,
  week_end   DATE NOT NULL,
  content    JSON NULL,
  status     VARCHAR(16) NOT NULL DEFAULT 'pending',
  created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  UNIQUE KEY uk_user_week (user_id, week_start)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

-- 话题/项目状态快照（进展/todo/风险）。
CREATE TABLE topic_status (
  id           BIGINT PRIMARY KEY,
  user_id      BIGINT NOT NULL DEFAULT 1,
  topic_id     BIGINT NOT NULL,
  content      JSON NULL,
  generated_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  KEY idx_topic_time (topic_id, generated_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

-- 扩展现有 agent_message（原列：id,user_id,role,content,citations,created_at）。
ALTER TABLE agent_message ADD COLUMN conversation_id BIGINT NULL AFTER user_id;
ALTER TABLE agent_message ADD COLUMN kind VARCHAR(16) NOT NULL DEFAULT 'text' AFTER role; -- text|tool_call|tool_result|card
ALTER TABLE agent_message ADD COLUMN tool_payload JSON NULL AFTER citations;
ALTER TABLE agent_message ADD COLUMN dsh_seq INT NULL AFTER tool_payload;
ALTER TABLE agent_message ADD KEY idx_am_conversation (conversation_id, id);
