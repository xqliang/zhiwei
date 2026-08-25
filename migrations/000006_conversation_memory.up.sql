-- 对话转记忆（Plan 3b）数据层：见 spec §6.3 / §12。
-- 承接 000005_agent 顶部预告：memory 加 conversation_id + session_id 放宽可空。
-- 【合并注意】迁移号 000006 与 person-profile / speaker-name-inference 并行分支撞号，
--            合并时统一重编号（项目已知协调点）。

-- 1) 新增对话溯源列（可空；录音来源的记忆此列为 NULL）。
ALTER TABLE memory ADD COLUMN conversation_id BIGINT NULL AFTER session_id;

-- 2) 放宽 session_id 为可空（对话来源的记忆此列为 NULL；录音来源仍写 session_id）。
--    无 FK，直接 MODIFY；保持 BIGINT 类型与其余属性不变。
ALTER TABLE memory MODIFY COLUMN session_id BIGINT NULL;

-- 3) conversation_id 检索/幂等删除用索引（按会话删旧记忆、按会话查）。
ALTER TABLE memory ADD KEY idx_mem_conversation (conversation_id);
