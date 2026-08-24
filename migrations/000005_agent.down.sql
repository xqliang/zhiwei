-- 反向回滚。MySQL 不支持 DROP COLUMN IF EXISTS，down 仅用于开发环境。
ALTER TABLE agent_message DROP KEY idx_am_conversation;
ALTER TABLE agent_message DROP COLUMN dsh_seq;
ALTER TABLE agent_message DROP COLUMN tool_payload;
ALTER TABLE agent_message DROP COLUMN kind;
ALTER TABLE agent_message DROP COLUMN conversation_id;

DROP TABLE IF EXISTS topic_status;
DROP TABLE IF EXISTS weekly_review;
DROP TABLE IF EXISTS agent_proposal;
DROP TABLE IF EXISTS agent_conversation;
