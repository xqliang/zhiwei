-- 反向回滚，仅开发环境。
-- 注意：把 session_id 收回 NOT NULL 前，必须先清掉 session_id IS NULL 的对话记忆，
--       否则 MODIFY ... NOT NULL 会因存在 NULL 行失败。
ALTER TABLE memory DROP KEY idx_mem_conversation;
DELETE FROM memory WHERE conversation_id IS NOT NULL OR session_id IS NULL; -- 清对话来源记忆
ALTER TABLE memory DROP COLUMN conversation_id;
ALTER TABLE memory MODIFY COLUMN session_id BIGINT NOT NULL;
